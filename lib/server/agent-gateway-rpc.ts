import WebSocket from "ws"
import { toWsBase } from "../gateway"
import { readGatewayUpgradeRejection } from "./gateway-upgrade"

export interface AgentGatewayContext {
  gateway: string
  token: string
  cluster: string
  instance: string
  sessionId: string
}

export interface AgentGatewayUpdate {
  kind: string
  data: string
}

export interface AgentGatewayMutationReceipt {
  tool: string
  tool_call_id: string
  status: string
  verified: boolean
  resolved_by?: string
  reason?: string
  result?: {
    isError: boolean
    text?: string
    structuredContent?: {
      phase?: string
      executionStarted?: boolean
      executionOutcome?: string
    }
  }
}

export interface AgentGatewayPublicReceipt {
  kind: "doops.verified-mutation"
  verified: boolean
  outcome: "succeeded" | "failed" | "unknown" | "cancelled"
  mutations: AgentGatewayMutationReceipt[]
  summary?: string
  turn?: {
    turnId?: string
    status?: string
    stopReason?: string
    failureClass?: string
    failureReason?: string
    error?: string
  }
}

export interface AgentGatewayCallResult {
  text: string
  structuredContent?: AgentGatewayPublicReceipt
}

export class AgentGatewayError extends Error {
  constructor(message: string, readonly status = 502, readonly outcome: "terminal" | "unknown" | "not_dispatched" = "unknown", readonly canceled = false,
    readonly structuredContent?: AgentGatewayPublicReceipt) {
    super(message)
  }
}

const metadataCallTimeoutMs = 45_000
// Gateway owns the 60-minute operation deadline. Keep a small client-side
// watchdog margin so the browser cannot cancel a still-running repair before
// the Gateway emits its terminal receipt.
const operationWatchdogTimeoutMs = 65 * 60_000

export function agentGatewayCall(
  context: AgentGatewayContext,
  args: Record<string, unknown>,
  signal: AbortSignal,
  onUpdate: (update: AgentGatewayUpdate) => void = () => {},
): Promise<AgentGatewayCallResult> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(new AgentGatewayError("请求派发前已停止", 499, "not_dispatched", true)); return }
    const url = new URL(`${toWsBase(context.gateway)}/v1/rpc`)
    url.searchParams.set("cluster", context.cluster)
    url.searchParams.set("instance", context.instance)
    const ws = new WebSocket(url, { headers: { Authorization: `Bearer ${context.token}` }, followRedirects: false })
    let timer = setTimeout(() => fail(new AgentGatewayError("Gateway 未返回连接结果")), 45_000)
    let dispatched = false
    let terminal = false
    const cleanup = () => {
      clearTimeout(timer)
      signal.removeEventListener("abort", abort)
      ws.close()
    }
    const fail = (error: Error) => {
      if (terminal) return
      terminal = true
      cleanup()
      reject(error instanceof AgentGatewayError && !dispatched
        ? new AgentGatewayError(error.message, error.status, "not_dispatched", error.canceled, error.structuredContent) : error)
    }
    ws.on("error", (error) => fail(new AgentGatewayError(error.message)))
    const abort = () => {
      if (!dispatched) { fail(new AgentGatewayError("请求派发前已停止", 499, "not_dispatched", true)); return }
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ jsonrpc: "2.0", id: 3, method: "tools/cancel", params: { id: 2 } }))
      }
      clearTimeout(timer)
      timer = setTimeout(() => fail(new AgentGatewayError("已请求停止，远端终态未确认", 409)), 15_000)
    }
    signal.addEventListener("abort", abort, { once: true })
    ws.on("open", () => ws.send(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: {} })))
    ws.on("message", (raw) => {
      if (terminal) return
      try {
        const message = JSON.parse(raw.toString())
        if (message.id === 1) {
          if (message.error) throw new AgentGatewayError(message.error.message || "Gateway initialize 失败")
          clearTimeout(timer)
          ws.send(JSON.stringify({ jsonrpc: "2.0", id: 2, method: "tools/call", params: {
            name: "doops_agent_prompt", arguments: { ...args, session_id: context.sessionId },
          } }))
          dispatched = true
          // Silence while preparing or running a tool is not a connection failure.
          // Bound the whole operation and drain its original result after cancellation.
          timer = setTimeout(abort, args.mode === "metadata" ? metadataCallTimeoutMs : operationWatchdogTimeoutMs)
          return
        }
        if (message.method === "notifications/message") {
          if (message.params?.sessionID !== context.sessionId || message.params?.request_id !== 2) {
            throw new AgentGatewayError("Agent 事件缺少请求或会话归属")
          }
          onUpdate({ kind: message.params.kind, data: message.params.data })
          return
        }
        if (message.id !== 2) return
        const error = message.error?.message || (message.result?.isError
          ? message.result.content?.map((item: { text?: string }) => item.text || "").join("") : "")
        const structuredContent = publicReceipt(message.error?.data?.structuredContent ?? message.result?.structuredContent)
        if (error) throw new AgentGatewayError(error, message.error?.code === -32001 ? 401 : message.error?.code === -32003 ? 403 : 502,
          error === "execution_outcome_unknown" || error.startsWith("execution_outcome_unknown:") ? "unknown" : "terminal",
          message.error?.code === -32007, structuredContent)
        if (!message.result || !Array.isArray(message.result.content)) throw new AgentGatewayError("Agent 执行结果无效")
        terminal = true
        const text = message.result.content.map((item: { text?: string }) => item.text || "").join("")
        cleanup()
        resolve({ text, structuredContent })
      } catch (error) {
        fail(error instanceof Error ? error : new AgentGatewayError("Agent 事件无效"))
      }
    })
    ws.on("unexpected-response", async (_request, response) => {
      const rejection = await readGatewayUpgradeRejection(response)
      fail(new AgentGatewayError(rejection.message, rejection.status, "not_dispatched"))
    })
    ws.on("close", () => fail(new AgentGatewayError("Gateway 已断开，远端终态未确认")))
  })
}

function publicReceipt(value: unknown): AgentGatewayPublicReceipt | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined
  const report = value as Record<string, unknown>
  if (report.kind !== "doops.verified-mutation" || typeof report.verified !== "boolean" ||
    !["succeeded", "failed", "unknown", "cancelled"].includes(report.outcome as string) ||
    !Array.isArray(report.mutations)) return undefined
  const mutations = report.mutations.map(publicMutation)
  if (mutations.some((mutation) => mutation === undefined)) return undefined
  const turn = report.turn && typeof report.turn === "object" && !Array.isArray(report.turn)
    ? Object.fromEntries(Object.entries(report.turn).filter(([key, value]) =>
      ["turnId", "status", "stopReason", "failureClass", "failureReason", "error"].includes(key) && typeof value === "string"))
    : undefined
  return {
    kind: "doops.verified-mutation",
    verified: report.verified,
    outcome: report.outcome as AgentGatewayPublicReceipt["outcome"],
    mutations: mutations as AgentGatewayMutationReceipt[],
    ...(typeof report.summary === "string" ? { summary: report.summary } : {}),
    ...(turn ? { turn } : {}),
  }
}

function publicMutation(value: unknown): AgentGatewayMutationReceipt | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined
  const mutation = value as Record<string, unknown>
  if (typeof mutation.tool !== "string" || typeof mutation.tool_call_id !== "string" ||
    typeof mutation.status !== "string" || typeof mutation.verified !== "boolean") return undefined
  const result = publicToolResult(mutation.result)
  return {
    tool: mutation.tool,
    tool_call_id: mutation.tool_call_id,
    status: mutation.status,
    verified: mutation.verified,
    ...(typeof mutation.resolved_by === "string" ? { resolved_by: mutation.resolved_by } : {}),
    ...(typeof mutation.reason === "string" ? { reason: mutation.reason } : {}),
    ...(result ? { result } : {}),
  }
}

function publicToolResult(value: unknown): AgentGatewayMutationReceipt["result"] | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined
  const result = value as Record<string, unknown>
  if (typeof result.isError !== "boolean") return undefined
  const content = Array.isArray(result.content)
    ? result.content.flatMap((item) => item && typeof item === "object" && !Array.isArray(item) &&
      typeof (item as Record<string, unknown>).text === "string" ? [(item as Record<string, unknown>).text as string] : [])
    : []
  const facts = publicToolFacts(result.structuredContent)
  return {
    isError: result.isError,
    ...(content.length ? { text: content.join("") } : {}),
    ...(facts ? { structuredContent: facts } : {}),
  }
}

function publicToolFacts(value: unknown): NonNullable<AgentGatewayMutationReceipt["result"]>["structuredContent"] | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined
  const facts = value as Record<string, unknown>
  const publicFacts = {
    ...(typeof facts.phase === "string" ? { phase: facts.phase } : {}),
    ...(typeof facts.executionStarted === "boolean" ? { executionStarted: facts.executionStarted } : {}),
    ...(typeof facts.executionOutcome === "string" ? { executionOutcome: facts.executionOutcome } : {}),
  }
  return Object.keys(publicFacts).length ? publicFacts : undefined
}
