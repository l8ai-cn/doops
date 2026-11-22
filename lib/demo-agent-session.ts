"use client"

import { clone, create, type MessageInitShape } from "@bufbuild/protobuf"
import { applyDeltaBatch, applySessionSnapshot, type AgentSessionState } from "@agent-cloud/agent-ui/runtime"
import {
  AgentEventSchema,
  CommandReceiptSchema,
  CommandReceiptState,
  MessageRole,
  SessionCursorSchema,
  SessionDeltaBatchSchema,
  SessionHistoryPageSchema,
  SessionSnapshotSchema,
  SessionStatus,
  TimelineItemStatus,
  type AgentEvent,
  type CommandEnvelope,
  type CommandReceipt,
  type SessionCursor,
  type SessionDeltaBatch,
  type SessionSnapshot,
} from "@agent-cloud/agent-ui/protocol"
import type { AgentWorkbenchSessionTransport } from "@agent-cloud/agent-ui/runtime"
import { demoApplyReply, demoAssistantReply } from "./demo"

type EventInput = {
  itemId: string
  turnId?: string
  causationCommandId?: string
  event: AgentEvent["event"]
}

const DEMO_MODEL = "demo/ops-assistant"

export function createDemoAgentSessionTransport(sessionId: string): AgentWorkbenchSessionTransport {
  return new DemoAgentSession(sessionId)
}

class DemoAgentSession implements AgentWorkbenchSessionTransport {
  private state: AgentSessionState
  private batches: SessionDeltaBatch[] = []
  private listeners = new Set<() => void>()
  private publishing: Promise<void> = Promise.resolve()
  private active: AbortController | null = null

  constructor(sessionId: string) {
    const now = new Date().toISOString()
    const snapshot = create(SessionSnapshotSchema, {
      sessionId,
      streamEpoch: crypto.randomUUID(),
      status: SessionStatus.IDLE,
      configuration: { model: DEMO_MODEL },
      grants: [{
        grantId: crypto.randomUUID(),
        issuer: "demo://local",
        subject: "演示用户",
        sessionId,
        issuedAt: now,
        actions: ["session.send_prompt", "session.interrupt"],
      }],
      capabilities: {
        protocolVersion: "2",
        history: false,
        models: [DEMO_MODEL],
        commandSchemas: ["send_prompt", "interrupt"].map((semanticKey) => ({
          namespace: "agentcloud",
          semanticKey,
          schemaVersion: "1",
          actions: [`session.${semanticKey}`],
        })),
        extensions: [{ namespace: "doops.operations", semanticKey: "apply", schemaVersion: "1", actions: ["session.send_prompt"] }],
      },
    })
    const applied = applySessionSnapshot(snapshot)
    if (applied.status !== "ready") throw new Error("演示会话无法初始化")
    this.state = applied
  }

  async getSnapshot(): Promise<SessionSnapshot> {
    return clone(SessionSnapshotSchema, this.state.snapshot)
  }

  async getHistoryPage(_signal?: AbortSignal, beforeSequence = BigInt(0)) {
    // 演示会话只保留当前快照，没有更早的分页；返回空页保持传输接口完整。
    return create(SessionHistoryPageSchema, {
      sessionId: this.state.snapshot.sessionId,
      streamEpoch: this.state.snapshot.streamEpoch,
      nextBeforeSequence: beforeSequence,
    })
  }

  async *streamDeltas(cursor: SessionCursor, signal: AbortSignal): AsyncIterable<SessionDeltaBatch> {
    const position = clone(SessionCursorSchema, cursor)
    let wake = () => {}
    const abort = () => wake()
    signal.addEventListener("abort", abort, { once: true })
    try {
      while (!signal.aborted) {
        const ready = new Promise<void>((resolve) => { wake = resolve })
        const current = wake
        this.listeners.add(current)
        try {
          for (const batch of this.replay(position)) {
            if (signal.aborted) return
            position.revision = batch.revision
            position.sequence = batch.lastSequence
            yield clone(SessionDeltaBatchSchema, batch)
          }
          if (!signal.aborted) await ready
        } finally {
          this.listeners.delete(current)
        }
      }
    } finally {
      signal.removeEventListener("abort", abort)
    }
  }

  async execute(command: CommandEnvelope, signal?: AbortSignal): Promise<CommandReceipt> {
    signal?.throwIfAborted()
    const snapshot = this.state.snapshot
    if (command.sessionId !== snapshot.sessionId || command.streamEpoch !== snapshot.streamEpoch) {
      throw new Error("演示会话不匹配")
    }
    const existing = snapshot.commandReceipts.find((receipt) => receipt.commandId === command.commandId)
    if (existing) return clone(CommandReceiptSchema, existing)
    if (command.command.case === "interrupt") return this.interrupt(command)
    if (command.command.case !== "sendPrompt" && !isApply(command)) {
      throw new Error("演示模式不支持该会话命令")
    }
    if (this.active) throw new Error("当前会话仍有运行中的演示任务")
    const text = promptText(command)
    const controller = new AbortController()
    this.active = controller
    const accepted = receipt(command, CommandReceiptState.ACCEPTED)
    await this.emit([
      receiptEvent(accepted),
      statusEvent(command.commandId, SessionStatus.RUNNING),
      messageEvent(command.commandId, "user", MessageRole.USER, text),
    ])
    void this.finish(command, text, controller).catch(() => {
      controller.abort()
    })
    return accepted
  }

  private async finish(command: CommandEnvelope, text: string, controller: AbortController) {
    try {
      await wait(280, controller.signal)
      const answer = isApply(command) ? demoApplyReply() : demoAssistantReply(text)
      const done = receipt(command, CommandReceiptState.SUCCEEDED)
      await this.emit([
        messageEvent(command.commandId, "assistant", MessageRole.ASSISTANT, answer),
        receiptEvent(done),
        statusEvent(command.commandId, SessionStatus.COMPLETED),
      ])
    } catch (error) {
      if (!controller.signal.aborted) throw error
      const canceled = receipt(command, CommandReceiptState.CANCELLED, "演示任务已停止")
      await this.emit([
        receiptEvent(canceled),
        statusEvent(command.commandId, SessionStatus.IDLE),
      ])
    } finally {
      if (this.active === controller) this.active = null
    }
  }

  private async interrupt(command: CommandEnvelope): Promise<CommandReceipt> {
    const active = this.active
    if (!active) throw new Error("当前没有运行中的演示任务")
    active.abort()
    const done = receipt(command, CommandReceiptState.SUCCEEDED)
    await this.emit([receiptEvent(done)])
    return done
  }

  private replay(cursor: SessionCursor): SessionDeltaBatch[] {
    const snapshot = this.state.snapshot
    if (cursor.sessionId !== snapshot.sessionId || cursor.streamEpoch !== snapshot.streamEpoch) {
      throw new Error("演示会话游标不匹配")
    }
    if (cursor.revision === snapshot.revision && cursor.sequence === snapshot.latestSequence) return []
    return this.batches.filter((batch) => batch.baseRevision >= cursor.revision && batch.firstSequence > cursor.sequence)
  }

  private emit(events: EventInput[]): Promise<void> {
    const run = this.publishing.then(() => this.publish(events))
    this.publishing = run.then(() => undefined, () => undefined)
    return run
  }

  private publish(events: EventInput[]) {
    const snapshot = this.state.snapshot
    const createdAt = new Date().toISOString()
    const batch = create(SessionDeltaBatchSchema, {
      sessionId: snapshot.sessionId,
      streamEpoch: snapshot.streamEpoch,
      baseRevision: snapshot.revision,
      revision: snapshot.revision + 1n,
      firstSequence: snapshot.latestSequence + 1n,
      lastSequence: snapshot.latestSequence + BigInt(events.length),
      digest: `sha256:demo-${snapshot.revision + 1n}-${snapshot.latestSequence + BigInt(events.length)}`,
      events: events.map((input, index) => create(AgentEventSchema, {
        envelope: {
          sessionId: snapshot.sessionId,
          streamEpoch: snapshot.streamEpoch,
          revision: snapshot.revision + 1n,
          sequence: snapshot.latestSequence + BigInt(index) + 1n,
          itemId: input.itemId,
          turnId: input.turnId,
          causationCommandId: input.causationCommandId,
          createdAt,
        },
        event: input.event,
      })),
    })
    const next = applyDeltaBatch(this.state, batch)
    if (next.status !== "ready") throw new Error("演示会话事件无法应用")
    this.state = next
    this.batches.push(batch)
    for (const listener of this.listeners) listener()
  }
}

function isApply(command: CommandEnvelope): boolean {
  const payload = command.command
  return payload.case === "extension" && payload.value.namespace === "doops.operations" && payload.value.semanticType === "apply"
}

function promptText(command: CommandEnvelope): string {
  const payload = command.command
  if (payload.case === "sendPrompt") {
    const text = payload.value.text.trim()
    if (!text) throw new Error("请输入演示问题")
    return payload.value.text
  }
  return "发布演示工作负载"
}

function receipt(command: CommandEnvelope, state: CommandReceiptState, message?: string): CommandReceipt {
  return create(CommandReceiptSchema, {
    sessionId: command.sessionId,
    commandId: command.commandId,
    payloadDigest: command.payloadDigest,
    state,
    receivedAt: command.issuedAt,
    updatedAt: new Date().toISOString(),
    ...(command.command.case === "extension" ? { interactionKind: "extension" } : {}),
    ...(message ? { error: { code: "demo_cancelled", message } } : {}),
  })
}

function receiptEvent(value: CommandReceipt): EventInput {
  return {
    itemId: `receipt:${value.commandId}:${value.state}`,
    causationCommandId: value.commandId,
    event: create(AgentEventSchema, { event: { case: "commandReceiptChanged", value: { receipt: value } } }).event,
  }
}

function statusEvent(commandId: string, status: SessionStatus): EventInput {
  return {
    itemId: `status:${commandId}:${status}`,
    turnId: commandId,
    causationCommandId: commandId,
    event: create(AgentEventSchema, { event: { case: "sessionStatusChanged", value: { status } } }).event,
  }
}

function messageEvent(commandId: string, roleName: string, role: MessageRole, text: string): EventInput {
  const content: MessageInitShape<typeof AgentEventSchema> = {
    event: {
      case: "timelineItemAppended",
      value: {
        content: {
          content: {
            case: "message",
            value: {
              role,
              status: TimelineItemStatus.COMPLETED,
              content: [{ contentId: "text", content: { case: "text", value: { text } } }],
            },
          },
        },
      },
    },
  }
  return {
    itemId: `${roleName}:${commandId}`,
    turnId: commandId,
    causationCommandId: commandId,
    event: create(AgentEventSchema, content).event,
  }
}

function wait(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(resolve, ms)
    const abort = () => {
      clearTimeout(timer)
      reject(new DOMException("演示任务已停止", "AbortError"))
    }
    if (signal.aborted) abort()
    else signal.addEventListener("abort", abort, { once: true })
  })
}
