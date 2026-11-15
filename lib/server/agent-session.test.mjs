import assert from "node:assert/strict"
import { randomUUID } from "node:crypto"
import { EventEmitter, once } from "node:events"
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import test, { after } from "node:test"
import { fileURLToPath, pathToFileURL } from "node:url"
import sdkPackage from "@agent-cloud/agent-ui/package.json" with { type: "json" }
import { AgentSessionReductionError } from "@agent-cloud/agent-ui/server"
import { createAgentCommandEnvelope } from "@agent-cloud/agent-ui/runtime"
import { AgentEventSchema, CommandReceiptState, SessionSnapshotSchema, SessionStatus } from "@agent-cloud/agent-ui/protocol"
import { create } from "@bufbuild/protobuf"
import TypeScript from "typescript"
import { WebSocketServer } from "ws"

const root = fileURLToPath(new URL("../../", import.meta.url))
assert.equal(sdkPackage.version, "0.1.4")
const fixture = mkdtempSync(join(root, ".agent-session-test-"))
const compiled = new Map()
after(() => rmSync(fixture, { recursive: true, force: true }))

// Keep fixtures under the checkout so untouched package imports resolve normally.
function sourceModule(filename) {
  filename = resolve(filename)
  if (compiled.has(filename)) return compiled.get(filename)
  const output = join(fixture, `${compiled.size}.mjs`)
  const url = pathToFileURL(output).href
  compiled.set(filename, url)
  let javascript = TypeScript.transpile(readFileSync(filename, "utf8"), {
    module: TypeScript.ModuleKind.ESNext, target: TypeScript.ScriptTarget.ES2022,
  }, filename)
  const parsed = TypeScript.createSourceFile(filename, javascript, TypeScript.ScriptTarget.ES2022, true, TypeScript.ScriptKind.JS)
  const imports = parsed.statements.filter((statement) =>
    (TypeScript.isImportDeclaration(statement) || TypeScript.isExportDeclaration(statement)) && statement.moduleSpecifier)
  for (const statement of imports.reverse()) {
    const specifier = statement.moduleSpecifier
    const name = specifier.text
    if (!name.startsWith(".")) continue
    const resolved = TypeScript.resolveModuleName(name, filename, {
      moduleResolution: TypeScript.ModuleResolutionKind.Bundler, module: TypeScript.ModuleKind.ESNext,
    }, TypeScript.sys).resolvedModule
    assert.ok(resolved && resolved.resolvedFileName.startsWith(root) && resolved.resolvedFileName.endsWith(".ts"),
      `Missing DoOps TypeScript source: ${name} from ${filename}`)
    const replacement = sourceModule(resolved.resolvedFileName)
    javascript = javascript.slice(0, specifier.getStart(parsed)) + JSON.stringify(replacement) + javascript.slice(specifier.end)
  }
  writeFileSync(output, javascript)
  return url
}

const { authorizedAgentSession, executeAgentCommand } = await import(sourceModule(join(root, "lib/server/agent-session.ts")))
const { storeAgentAttachment } = await import(sourceModule(join(root, "lib/server/agent-attachments.ts")))
const { AgentGatewayError } = await import(sourceModule(join(root, "lib/server/agent-gateway-rpc.ts")))

const prompt = { case: "sendPrompt", value: { text: "synthetic prompt", attachments: [] } }
const apply = { case: "extension", value: {
  namespace: "doops.operations", semanticType: "apply", schemaVersion: "1",
  payload: { mediaType: "application/json", data: new TextEncoder().encode(JSON.stringify({ input: {
    release: "synthetic", namespace: "synthetic", chart: "https://example.test/chart.tgz",
    values: "", message: "functional test", credentials: "",
  } })) },
} }

function gatewayReport(outcome, toolText = outcome === "succeeded" ? "release deployed: revision 7" : "helm verification failed") {
  return {
    kind: "doops.verified-mutation", verified: true, outcome, summary: "synthetic response",
    canonicalScope: { token: "private-scope-canary" },
    payload: "private-payload-canary",
    mutations: [{
      tool: "doops_helm", tool_call_id: "helm-call-1", status: outcome === "succeeded" ? "completed" : "failed",
      verified: true, canonicalScope: { server: "private-scope-canary" }, rawOutput: "private-attestation-canary",
      result: {
        isError: outcome !== "succeeded",
        content: [{ type: "text", text: toolText }],
        structuredContent: {
          phase: outcome === "unknown" ? "execution" : "validation",
          executionStarted: outcome !== "failed",
          executionOutcome: outcome === "unknown" ? "unknown" : undefined,
          payload: "private-result-payload-canary",
        },
      },
    }],
  }
}

function systemReceiptTexts(hosted) {
  return hosted.source.getSnapshot().history
    .filter((item) => item.content?.content.case === "system")
    .flatMap((item) => item.content.content.value.content)
    .filter((block) => block.content.case === "text")
    .map((block) => block.content.value.text)
}

async function command(hosted, payload) {
  const snapshot = hosted.source.getSnapshot()
  return createAgentCommandEnvelope({
    sessionId: snapshot.sessionId, streamEpoch: snapshot.streamEpoch, commandId: randomUUID(),
    issuedAt: new Date().toISOString(), command: payload,
  })
}

async function fakeRPC(t) {
  const server = new WebSocketServer({ host: "127.0.0.1", port: 0, path: "/v1/rpc" })
  t.after(async () => {
    for (const socket of server.clients) socket.terminate()
    await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()))
  })
  await once(server, "listening")
  const operations = [], requests = [], dispatched = new EventEmitter()
  // Sessions with a dispatched prompt or apply that has not ended, across every connection.
  const running = new Set()
  let metadataFailure
  let read = 0
  const send = (socket, message) => socket.send(JSON.stringify({ jsonrpc: "2.0", ...message }))
  server.on("connection", (socket, request) => {
    assert.equal(request.headers.authorization, "Bearer synthetic-token")
    assert.equal(new URL(request.url, "http://localhost").searchParams.get("cluster"), "test")
    assert.equal(new URL(request.url, "http://localhost").searchParams.get("instance"), "node")
    let operation
    socket.on("message", (raw) => {
      const message = JSON.parse(raw.toString())
      if (message.method === "initialize") {
        send(socket, { id: message.id, result: {} })
      } else if (message.method === "tools/call") {
        requests.push(message.params)
        assert.equal(message.params.name, "doops_agent_prompt")
        const args = message.params.arguments
        if (args.mode === "metadata") {
          // Like the Gateway (tunnel_hub.go resourceKeyForTool), a metadata read is an ask on its session and does
          // not enter while that session's prompt or apply is running.
          const failure = metadataFailure || (running.has(args.session_id) ? { code: -32005, message: "target busy: test/node" } : undefined)
          if (failure) {
            send(socket, { id: message.id, error: failure })
            return
          }
          send(socket, { id: message.id, result: { content: [{ type: "text", text: JSON.stringify({
            status: "connected", model: "synthetic-model", event_schema: "doops.sh/acp-events/v1", event_formats: ["acp"],
          }) }] } })
          return
        }
        assert.equal(args.mode, undefined, "No history or audit reads")
        assert.equal(args.event_format, "acp")
        if (args.attachments?.length) {
          assert.equal(args.attachments.length, 1)
          assert.equal(args.attachments[0].filename, "note.txt")
          assert.equal(args.attachments[0].media_type, "text/plain")
          assert.equal(args.attachments[0].base64_data, Buffer.from("uploaded document").toString("base64"))
        }
        let acknowledge
        operation = {
          args, socket, turnId: randomUUID(), ack: new Promise((resolve) => { acknowledge = resolve }),
          acknowledge,
          update(update) { send(socket, { method: "notifications/message", params: {
            request_id: message.id, sessionID: args.session_id, kind: "acp_update", data: JSON.stringify(update),
          } }) },
          error(code, text, structuredContent) {
            running.delete(args.session_id)
            send(socket, { id: message.id, error: {
              code, message: text, ...(structuredContent ? { data: { structuredContent } } : {}),
            } })
          },
          finish(canceled = false) {
            this.update({ sessionUpdate: "agent_message", turnId: this.turnId, content: { type: "text", text: "synthetic response" } })
            this.update({ sessionUpdate: "turn_finished", turnId: this.turnId,
              status: canceled ? "cancelled" : "completed", stopReason: canceled ? "cancelled" : "end_turn" })
            if (canceled) this.error(-32007, "operation canceled")
            else {
              running.delete(args.session_id)
              const report = args.operation === "apply" ? gatewayReport("succeeded") : undefined
              send(socket, { id: message.id, result: {
                content: [{ type: "text", text: report ? JSON.stringify(report) : "Operation complete." }],
                ...(report ? { structuredContent: report } : {}),
              } })
            }
          },
        }
        running.add(args.session_id)
        socket.on("close", () => running.delete(args.session_id))
        operations.push(operation)
        dispatched.emit("operation")
      } else {
        assert.equal(message.method, "tools/cancel")
        assert.equal(message.params.id, 2)
        assert.equal(message.id, 3)
        assert.ok(operation)
        send(socket, { id: message.id, result: { canceled: true } })
        operation.acknowledge()
      }
    })
  })
  return {
    operations, requests, running,
    // Every later metadata read gets this Gateway error; undefined restores the default replies.
    failMetadata(failure) { metadataFailure = failure },
    context: { gateway: `http://127.0.0.1:${server.address().port}`, token: "synthetic-token", cluster: "test", instance: "node", sessionId: randomUUID() },
    async nextOperation() {
      if (operations.length === read) await once(dispatched, "operation")
      return operations[read++]
    },
  }
}

async function authorize(t, context) {
  const hosted = await authorizedAgentSession(context, new AbortController().signal)
  const epoch = hosted.source.getSnapshot().streamEpoch
  const emit = hosted.source.emit
  t.mock.method(hosted.source, "emit", function (events, streamEpoch) {
    assert.equal(streamEpoch, epoch, "Every main emission must carry the owning epoch")
    return emit.call(this, events, streamEpoch)
  })
  return hosted
}

async function assertBlocked(hosted, context, rpc) {
  const dispatched = rpc.operations.length
  for (const payload of [prompt, apply]) {
    await assert.rejects(executeAgentCommand(hosted, context, await command(hosted, payload)),
      (error) => error instanceof AgentGatewayError && error.status === 409)
    assert.equal(rpc.operations.length, dispatched, "A blocked prompt/apply must not be dispatched")
  }
}

async function finishNext(hosted, context, rpc, payload = prompt) {
  const owned = await command(hosted, payload)
  assert.equal((await executeAgentCommand(hosted, context, owned)).state, CommandReceiptState.ACCEPTED)
  const active = hosted.active
  const operation = await rpc.nextOperation()
  assert.equal(operation.args.session_id, context.sessionId)
  assert.equal(operation.args.operation, payload.case === "extension" ? "apply" : "ask")
  operation.finish()
  await active.done
  assert.equal(hosted.active, undefined)
  assert.equal(hosted.source.getSnapshot().commandReceipts.find((receipt) => receipt.commandId === owned.commandId).state, CommandReceiptState.SUCCEEDED)
}

test("apply keeps one sanitized Gateway success receipt beside the model summary", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
  const owned = await command(hosted, apply)
  await executeAgentCommand(hosted, rpc.context, owned)
  const active = hosted.active, operation = await rpc.nextOperation()
  operation.finish()
  await active.done
  const snapshot = hosted.source.getSnapshot()
  const receipts = systemReceiptTexts(hosted)
  assert.equal(receipts.length, 1)
  assert.match(receipts[0], /doops\.verified-mutation/)
  assert.match(receipts[0], /release deployed: revision 7/)
  assert.doesNotMatch(receipts[0], /synthetic response/)
  assert.doesNotMatch(receipts[0], /private-(?:scope|payload|attestation|result)/)
  const summaries = snapshot.history
    .filter((item) => item.content?.content.case === "message" && item.content.content.value.role === 2)
    .flatMap((item) => item.content.content.value.content)
    .filter((block) => block.content.case === "text")
    .map((block) => block.content.value.text)
  assert.deepEqual(summaries, ["synthetic response"])
})

for (const [outcome, errorText, receiptCode] of [
  ["failed", "doagent_apply_tool_failed: doops_helm: helm_verification_failed", "doops_operation_failed"],
  ["unknown", "execution_outcome_unknown", "doops_apply_outcome_unknown"],
]) {
  test(`apply keeps the Gateway ${outcome} receipt in history`, { timeout: 5_000 }, async (t) => {
    const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
    const owned = await command(hosted, apply)
    await executeAgentCommand(hosted, rpc.context, owned)
    const active = hosted.active, operation = await rpc.nextOperation()
    operation.error(-32603, errorText, gatewayReport(outcome))
    await active.done
    const snapshot = hosted.source.getSnapshot()
    const receipts = systemReceiptTexts(hosted)
    assert.equal(receipts.length, 1)
    assert.match(receipts[0], new RegExp(`"outcome": "${outcome}"`))
    assert.doesNotMatch(receipts[0], /private-(?:scope|payload|attestation|result)/)
    assert.equal(snapshot.commandReceipts.find((receipt) => receipt.commandId === owned.commandId).error.code, receiptCode)
    if (outcome === "unknown") {
      const replay = await executeAgentCommand(hosted, rpc.context, owned)
      assert.equal(replay.state, CommandReceiptState.FAILED)
      assert.equal(rpc.operations.length, 1, "unknown apply must not be replayed")
    }
  })
}

test("ordinary ask keeps its model result without an extra Gateway receipt", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
  await finishNext(hosted, rpc.context, rpc, prompt)
  assert.deepEqual(systemReceiptTexts(hosted), [])
  const assistant = hosted.source.getSnapshot().history
    .filter((item) => item.content?.content.case === "message" && item.content.content.value.role === 2)
  assert.equal(assistant.length, 1)
})

test("a connected apply stays active after 60 seconds without its first event", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
  t.mock.timers.enable({ apis: ["setTimeout"] })
  const owned = await command(hosted, apply)
  await executeAgentCommand(hosted, rpc.context, owned)
  const active = hosted.active, operation = await rpc.nextOperation()
  t.mock.timers.tick(60_000)
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(operation.socket.readyState, 1, "a connected quiet operation must not be disconnected")
  assert.equal(hosted.active, active)
  assert.equal(hosted.source.getSnapshot().status, SessionStatus.RUNNING)
  operation.finish()
  await active.done
  assert.equal(hosted.source.getSnapshot().commandReceipts.find((receipt) => receipt.commandId === owned.commandId).state, CommandReceiptState.SUCCEEDED)
})

test("the client watchdog requests cancellation after the Gateway deadline and still waits for terminal evidence", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
  t.mock.timers.enable({ apis: ["setTimeout"] })
  const owned = await command(hosted, apply)
  await executeAgentCommand(hosted, rpc.context, owned)
  const active = hosted.active, operation = await rpc.nextOperation()
  operation.update({ sessionUpdate: "doops_notice", content: { type: "text", text: "waiting for Helm" } })
  // Ensure the progress notification is consumed before advancing the deadline.
  while (!hosted.source.getSnapshot().history.some((item) => item.content?.content.case === "system")) {
    await new Promise((resolve) => setImmediate(resolve))
  }
	t.mock.timers.tick(65 * 60_000)
  await operation.ack
  assert.equal(hosted.active, active, "cancellation ACK is not a terminal result")
  operation.finish(true)
  await active.done
  assert.equal(hosted.source.getSnapshot().commandReceipts.find((receipt) => receipt.commandId === owned.commandId).state, CommandReceiptState.CANCELLED)
})

test("cancel ACK keeps same-SID prompt/apply blocked until original RPC confirms cancellation", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
  const owned = await command(hosted, prompt)
  await executeAgentCommand(hosted, rpc.context, owned)
  const active = hosted.active, operation = await rpc.nextOperation()
  let settled = false
  const interruption = executeAgentCommand(hosted, rpc.context, await command(hosted, { case: "interrupt", value: { turnId: owned.commandId } }))
    .then((receipt) => { settled = true; return receipt })
  await operation.ack
  await assertBlocked(hosted, rpc.context, rpc)
  assert.equal(settled, false, "Cancel ACK cannot settle interrupt")
  assert.equal(hosted.active, active)
  operation.finish(true)
  assert.equal((await interruption).state, CommandReceiptState.SUCCEEDED)
  assert.equal(hosted.source.getSnapshot().commandReceipts.find((receipt) => receipt.commandId === owned.commandId).state, CommandReceiptState.CANCELLED)
  assert.equal(hosted.active, undefined)
  await finishNext(hosted, rpc.context, rpc)
  await finishNext(hosted, rpc.context, rpc, apply)
  assert.equal(rpc.operations.length, 3)
})

test("same-session admission stays exclusive while prompt arguments are still resolving", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
  const first = executeAgentCommand(hosted, rpc.context, await command(hosted, prompt))
  const second = executeAgentCommand(hosted, rpc.context, await command(hosted, prompt))
  assert.equal((await first).state, CommandReceiptState.ACCEPTED)
  await assert.rejects(second, (error) => error instanceof AgentGatewayError && error.status === 409)
  const active = hosted.active
  const operation = await rpc.nextOperation()
  operation.finish()
  await active.done
  assert.equal(rpc.operations.length, 1)
  assert.equal(hosted.active, undefined)
})

for (const ending of ["disconnect", "execution_outcome_unknown", "execution_outcome_unknown: cancellation not confirmed", "timeout"]) {
  test(`ACK-only ${ending} releases the active slot without replaying the uncertain command`, { timeout: 20_000 }, async (t) => {
    const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
    const owned = await command(hosted, prompt)
    await executeAgentCommand(hosted, rpc.context, owned)
    const active = hosted.active, operation = await rpc.nextOperation()
    const interruption = executeAgentCommand(hosted, rpc.context, await command(hosted, { case: "interrupt", value: { turnId: owned.commandId } }))
    await operation.ack
    await assertBlocked(hosted, rpc.context, rpc)
    if (ending === "disconnect") operation.socket.terminate()
    else if (ending !== "timeout") operation.error(-32603, ending)
    else operation.update({ sessionUpdate: "agent_message", turnId: operation.turnId, content: { type: "text", text: "ACK is not terminal" } })
    assert.equal((await interruption).state, CommandReceiptState.FAILED)
    await active.done
    assert.equal(hosted.active, undefined)
    const snapshot = hosted.source.getSnapshot()
    assert.equal(snapshot.commandReceipts.find((receipt) => receipt.commandId === owned.commandId).state, CommandReceiptState.FAILED)
    assert.notDeepEqual(snapshot.capabilities.commandSchemas, [])
    assert.notDeepEqual(snapshot.capabilities.extensions, [])
    assert.equal(await authorizedAgentSession(rpc.context, new AbortController().signal), hosted)
    const replay = await executeAgentCommand(hosted, rpc.context, owned)
    assert.equal(replay.state, CommandReceiptState.FAILED)
    assert.equal(rpc.operations.length, 1, "uncertain command was replayed")
    await finishNext(hosted, rpc.context, rpc)
    const otherContext = { ...rpc.context, sessionId: randomUUID() }
    const other = await authorize(t, otherContext)
    await finishNext(other, otherContext, rpc)
    assert.equal(hosted.active, undefined)
    assert.equal(rpc.operations.length, 3)
  })
}

test("ordinary original-RPC terminal errors release the session", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
  const owned = await command(hosted, prompt)
  await executeAgentCommand(hosted, rpc.context, owned)
  const active = hosted.active, operation = await rpc.nextOperation()
  operation.error(-32603, "doagent_execution_failed")
  await active.done
  assert.equal(active.uncertain, false)
  assert.equal(hosted.active, undefined)
  assert.equal(hosted.source.getSnapshot().commandReceipts.find((receipt) => receipt.commandId === owned.commandId).state, CommandReceiptState.FAILED)
  await finishNext(hosted, rpc.context, rpc)
})

test("conversation attachments are forwarded once and remain in the user timeline", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t)
  const hosted = await authorize(t, rpc.context)
  const reference = await storeAgentAttachment(
    rpc.context,
    { name: "note.txt", mediaType: "text/plain", bytes: new TextEncoder().encode("uploaded document") },
  )
  const payload = {
    case: "sendPrompt",
    value: {
      text: "请阅读这个文档",
      attachments: [{
        contentId: reference.id,
        identity: { namespace: "agentcloud.session-file", semanticKey: "attachment", schemaVersion: "1" },
        content: { case: "file", value: {
          artifactId: reference.id, representationId: "source", revision: 1n,
          mediaType: reference.mediaType, role: "input", filename: reference.name,
        } },
      }],
    },
  }
  await finishNext(hosted, rpc.context, rpc, payload)
  const user = hosted.source.getSnapshot().history.find((item) => item.content?.content.case === "message" && item.content.content.value.role === 1)
  assert.ok(user)
  assert.equal(user.content.content.value.content.some((block) => block.content.case === "file" && block.content.value.artifactId === reference.id), true)
})

test("a capable agent without a delivered model is not told to upgrade", async (t) => {
  const server = new WebSocketServer({ host: "127.0.0.1", port: 0, path: "/v1/rpc" })
  t.after(async () => {
    for (const socket of server.clients) socket.terminate()
    await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()))
  })
  await once(server, "listening")
  server.on("connection", (socket) => {
    socket.on("message", (raw) => {
      const message = JSON.parse(raw.toString())
      if (message.method === "initialize") {
        socket.send(JSON.stringify({ jsonrpc: "2.0", id: message.id, result: {} }))
        return
      }
      socket.send(JSON.stringify({ jsonrpc: "2.0", id: message.id, result: { content: [{ type: "text", text: JSON.stringify({
        status: "awaiting_model_provider", event_schema: "doops.sh/acp-events/v1", event_formats: ["acp"],
      }) }] } }))
    })
  })
  const context = {
    gateway: `http://127.0.0.1:${server.address().port}`, token: "synthetic-token",
    cluster: "test", instance: "node", sessionId: randomUUID(),
  }
  await assert.rejects(
    authorizedAgentSession(context, new AbortController().signal),
    (error) => error instanceof AgentGatewayError && error.status === 503 && error.message === "Gateway 还没有可下发到该节点的模型",
  )
})

test("released SDK artifact rejects missing/stale epochs before and inside the publication queue", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t)
  const hosted = await authorizedAgentSession(rpc.context, new AbortController().signal)
  const previous = hosted.source.getSnapshot()
  const next = create(SessionSnapshotSchema, { sessionId: previous.sessionId, streamEpoch: randomUUID(), status: SessionStatus.IDLE })
  const events = [{ itemId: "status", event: create(AgentEventSchema, {
    event: { case: "sessionStatusChanged", value: { status: SessionStatus.COMPLETED } },
  }).event }]
  const stale = (error) => error instanceof AgentSessionReductionError && error.code === "session_source_epoch_stale"
  const reset = hosted.source.resetEpoch(next)
  const queued = assert.rejects(hosted.source.emit(events, previous.streamEpoch), stale)
  await reset
  await queued
  await assert.rejects(hosted.source.emit(events, previous.streamEpoch), stale)
  await assert.rejects(hosted.source.emit(events), stale)
  const untouched = hosted.source.getSnapshot()
  assert.equal(untouched.status, SessionStatus.IDLE)
  assert.equal(untouched.revision, 0n)
  assert.equal(untouched.latestSequence, 0n)
  assert.deepEqual(untouched.history, [])
  await hosted.source.emit(events, next.streamEpoch)
  assert.equal(hosted.source.getSnapshot().status, SessionStatus.COMPLETED)
  assert.equal(hosted.source.getSnapshot().revision, 1n)
})

// The console re-authorizes every snapshot, stream and command request through metadata. The Gateway checks the token
// and the target grant before it admits a call, and answers a metadata read on a session whose apply is running with
// "busy" (tunnel_hub.go handleGatewayToolCall). That answer must not lock the caller out of its own running release.
test("refresh and stop during a running apply reach the hosted session", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
  const owned = await command(hosted, apply)
  await executeAgentCommand(hosted, rpc.context, owned)
  const active = hosted.active, operation = await rpc.nextOperation()
  assert.ok(rpc.running.has(rpc.context.sessionId))
  assert.equal(await authorizedAgentSession(rpc.context, new AbortController().signal), hosted, "a refresh reattaches to the running release")
  const stop = executeAgentCommand(await authorizedAgentSession(rpc.context, new AbortController().signal), rpc.context,
    await command(hosted, { case: "interrupt", value: { turnId: owned.commandId } }))
  await operation.ack
  operation.finish(true)
  assert.equal((await stop).state, CommandReceiptState.SUCCEEDED)
  await active.done
  assert.equal(hosted.source.getSnapshot().commandReceipts.find((receipt) => receipt.commandId === owned.commandId).state, CommandReceiptState.CANCELLED)
})

test("during a running apply only a busy answer re-authorizes the hosted session", { timeout: 10_000 }, async (t) => {
  for (const [name, failure, admitted] of [
    ["same session busy", undefined, true],
    ["cancellation pending", { code: -32005, message: "target busy: cancellation pending" }, true],
    ["queue timeout", { code: -32005, message: "target queue timeout: test/node" }, true],
    ["grant removed", { code: -32003, message: "forbidden: ask on test/node" }, false],
    ["token rejected", { code: -32001, message: "gateway_auth_expired" }, false],
    ["target offline", { code: -32004, message: "target offline: test/node" }, false],
    ["model withdrawn", { code: -32603, message: "model_provider_lookup_failed" }, false],
    // A refused admission creates no turn, so it can never re-authorize a hosted operation; the
    // older Gateway's unknown wrapper is fail-closed and must not reattach either.
    ["queued admission", { code: -32603, message: "remote error: doagent_prompt_queued" }, false],
    ["queue cleanup unknown", { code: -32603, message: "doagent_prompt_queue_cleanup_unknown" }, false],
    ["unknown wrapper around a queued admission", { code: -32603, message: "execution_outcome_unknown: remote error: doagent_prompt_queued" }, false],
  ]) {
    const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
    await executeAgentCommand(hosted, rpc.context, await command(hosted, apply))
    const active = hosted.active, operation = await rpc.nextOperation()
    rpc.failMetadata(failure)
    const authorization = authorizedAgentSession(rpc.context, new AbortController().signal)
    if (admitted) assert.equal(await authorization, hosted, name)
    else await assert.rejects(authorization, (error) => error instanceof AgentGatewayError && error.message === failure.message, name)
    rpc.failMetadata(undefined)
    operation.finish()
    await active.done
  }
})

test("a busy answer does not open a session the console does not host", { timeout: 5_000 }, async (t) => {
  const rpc = await fakeRPC(t)
  rpc.failMetadata({ code: -32005, message: "target busy: test/node" })
  await assert.rejects(authorizedAgentSession(rpc.context, new AbortController().signal),
    (error) => error instanceof AgentGatewayError && error.message === "target busy: test/node")
})

// R4-7: a hosted session stays cached for 30 minutes after its release ends. Another write on the same session
// (CLI write, Git push, Agent upgrade) also makes the metadata read answer busy; that must not reattach the page to
// the old snapshot and skip the metadata checks.
test("a busy answer reattaches only while the hosted session runs a command", { timeout: 10_000 }, async (t) => {
  const busy = { code: -32005, message: "session has an active operation; wait for terminal evidence: op-0123" }
  for (const [name, running, admitted] of [
    ["release running", true, true],
    ["release finished, other write on the session", false, false],
  ]) {
    const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
    await executeAgentCommand(hosted, rpc.context, await command(hosted, apply))
    const active = hosted.active, operation = await rpc.nextOperation()
    if (!running) {
      operation.finish()
      await active.done
      assert.equal(hosted.active, undefined, `${name}: nothing runs`)
    }
    rpc.failMetadata(busy)
    const authorization = authorizedAgentSession(rpc.context, new AbortController().signal)
    if (admitted) assert.equal(await authorization, hosted, name)
    else await assert.rejects(authorization, (error) => error instanceof AgentGatewayError && error.message === busy.message, name)
    rpc.failMetadata(undefined)
    if (running) {
      operation.finish()
      await active.done
    }
  }
})

// A queued admission provably created no turn: the hosted session must not stay uncertain or keep
// the doops_apply_outcome_unknown replay guard. An unconfirmed queue cleanup and a real unknown
// outcome may still have run, so those stay uncertain.
test("a queued admission leaves the session usable, an unconfirmed outcome does not", { timeout: 20_000 }, async (t) => {
  for (const [name, payload, end, code, uncertain] of [
    ["queued admission", apply, (operation) => operation.error(-32603, "remote error: doagent_prompt_queued"), "doops_operation_failed", false],
    ["queue cleanup never confirmed", apply, (operation) => operation.error(-32603, "doagent_prompt_queue_cleanup_unknown"), "doops_apply_outcome_unknown", true],
    ["unknown wrapper around a queued admission", apply, (operation) => operation.error(-32603, "execution_outcome_unknown: remote error: doagent_prompt_queued"), "doops_apply_outcome_unknown", true],
    ["prompt turn id missing", apply, (operation) => operation.error(-32603, "execution_outcome_unknown: prompt_turn_id_missing"), "doops_apply_outcome_unknown", true],
    ["empty prompt", prompt, (operation) => operation.error(-32602, "doagent_prompt_no_prompt"), "doops_operation_failed", false],
    ["pre-admission rejection", apply, (operation) => operation.error(-32003, "doagent_prompt_rejected"), "doops_operation_failed", false],
  ]) {
    const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
    const owned = await command(hosted, payload)
    await executeAgentCommand(hosted, rpc.context, owned)
    const active = hosted.active, operation = await rpc.nextOperation()
    end(operation)
    await active.done
    assert.equal(active.uncertain, uncertain, name)
    assert.equal(hosted.active, undefined, name)
    const snapshot = hosted.source.getSnapshot()
    assert.equal(snapshot.commandReceipts.find((receipt) => receipt.commandId === owned.commandId).error.code, code, name)
    assert.equal(snapshot.error.code, code, name)
  }
})

// The deployment page tells an unconfirmed release apart from one that failed: the target may already have changed.
test("an apply whose outcome is unconfirmed is reported as unknown, not as an ordinary failure", { timeout: 20_000 }, async (t) => {
  for (const [name, payload, end, code] of [
    ["apply cut off", apply, (operation) => operation.socket.terminate(), "doops_apply_outcome_unknown"],
    ["apply outcome unknown", apply, (operation) => operation.error(-32603, "execution_outcome_unknown"), "doops_apply_outcome_unknown"],
    ["apply refused", apply, (operation) => operation.error(-32603, "doagent_execution_failed"), "doops_operation_failed"],
    ["apply credential ambiguity", apply, (operation) => operation.error(-32003, "credential_reference_ambiguous"), "doops_operation_failed"],
    ["prompt cut off", prompt, (operation) => operation.socket.terminate(), "doops_operation_failed"],
  ]) {
    const rpc = await fakeRPC(t), hosted = await authorize(t, rpc.context)
    const owned = await command(hosted, payload)
    await executeAgentCommand(hosted, rpc.context, owned)
    const active = hosted.active, operation = await rpc.nextOperation()
    end(operation)
    await active.done
    const snapshot = hosted.source.getSnapshot()
    assert.equal(snapshot.commandReceipts.find((receipt) => receipt.commandId === owned.commandId).error.code, code, name)
    assert.equal(snapshot.error.code, code, name)
  }
})
