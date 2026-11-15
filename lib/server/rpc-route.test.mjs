import assert from "node:assert/strict"
import { once } from "node:events"
import { mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import test, { after } from "node:test"
import { fileURLToPath, pathToFileURL } from "node:url"
import TypeScript from "typescript"
import WebSocket, { WebSocketServer } from "ws"

const root = fileURLToPath(new URL("../../", import.meta.url))
const fixture = mkdtempSync(join(tmpdir(), "doops-rpc-route-test-"))
symlinkSync(join(root, "node_modules"), join(fixture, "node_modules"), "dir")
const compiled = new Map()
after(() => rmSync(fixture, { recursive: true, force: true }))

function sourceModule(filename) {
  filename = resolve(filename)
  if (compiled.has(filename)) return compiled.get(filename)
  const output = join(fixture, `${compiled.size}.mjs`)
  const url = pathToFileURL(output).href
  compiled.set(filename, url)
  let javascript = TypeScript.transpile(readFileSync(filename, "utf8"), {
    module: TypeScript.ModuleKind.ESNext,
    target: TypeScript.ScriptTarget.ES2022,
  }, filename)
  const parsed = TypeScript.createSourceFile(
    filename,
    javascript,
    TypeScript.ScriptTarget.ES2022,
    true,
    TypeScript.ScriptKind.JS,
  )
  for (const statement of parsed.statements.filter((statement) =>
    (TypeScript.isImportDeclaration(statement) || TypeScript.isExportDeclaration(statement)) &&
    statement.moduleSpecifier,
  ).reverse()) {
    const specifier = statement.moduleSpecifier
    const name = specifier.text
    if (!name.startsWith(".") && !name.startsWith("@/")) continue
    const source = name.startsWith("@/") ? join(root, name.slice(2)) : name
    const resolved = TypeScript.resolveModuleName(source, filename, {
      moduleResolution: TypeScript.ModuleResolutionKind.Bundler,
      module: TypeScript.ModuleKind.ESNext,
    }, TypeScript.sys).resolvedModule
    assert.ok(resolved && resolved.resolvedFileName.startsWith(root) && resolved.resolvedFileName.endsWith(".ts"),
      `Missing DoOps TypeScript source: ${name} from ${filename}`)
    const replacement = sourceModule(resolved.resolvedFileName)
    javascript = javascript.slice(0, specifier.getStart(parsed)) +
      JSON.stringify(replacement) +
      javascript.slice(specifier.end)
  }
  writeFileSync(output, javascript)
  return url
}

const { POST } = await import(sourceModule(join(root, "app/api/rpc/route.ts")))

function deferred() {
  let resolvePromise
  const promise = new Promise((resolve) => { resolvePromise = resolve })
  return { promise, resolve: resolvePromise }
}

function send(socket, message) {
  if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ jsonrpc: "2.0", ...message }))
}

async function localGateway(t, {
  onInitialize = (connection, message) => send(connection.socket, { id: message.id, result: { capabilities: {} } }),
  onCall = () => {},
  onCancel = (connection, message) => send(connection.socket, { id: message.id, result: { canceled: true } }),
} = {}) {
  const server = new WebSocketServer({ host: "127.0.0.1", port: 0, path: "/v1/rpc" })
  const connectionReady = deferred()
  const connections = []
  const calls = []
  const cancels = []

  server.on("connection", (socket, request) => {
    const connection = {
      socket,
      request,
      initialized: deferred(),
      call: deferred(),
      cancel: deferred(),
    }
    connections.push(connection)
    connectionReady.resolve(connection)
    socket.on("message", (raw) => {
      const message = JSON.parse(raw.toString())
      if (message.method === "initialize") {
        connection.initialized.resolve(message)
        onInitialize(connection, message)
      } else if (message.method === "tools/call") {
        calls.push(message)
        connection.call.resolve(message)
        onCall(connection, message)
      } else if (message.method === "tools/cancel") {
        cancels.push(message)
        connection.cancel.resolve(message)
        onCancel(connection, message)
      }
    })
  })

  await once(server, "listening")
  const address = server.address()
  assert.ok(address && typeof address === "object")
  const gateway = `http://127.0.0.1:${address.port}`

  t.after(async () => {
    for (const socket of server.clients) socket.terminate()
    await new Promise((resolve, reject) => {
      server.close((error) => {
        if (error && error.code !== "ERR_SERVER_NOT_RUNNING") reject(error)
        else resolve()
      })
    })
  })

  return {
    gateway,
    calls,
    cancels,
    connection: connectionReady.promise,
    connections,
  }
}

function sessionCookie(gateway) {
  return `doops_session_gateway=${encodeURIComponent(gateway)}; doops_session_token=synthetic-token`
}

function rpcRequest(gateway, body) {
  return new Request("http://console.test/api/rpc", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Origin: "http://console.test",
      Cookie: sessionCookie(gateway),
    },
    body: JSON.stringify(body),
  })
}

test("a different origin cannot call a tool with the console session", async () => {
  const response = await POST(new Request("http://console.test/api/rpc", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Origin: "http://notebook.test",
      Cookie: sessionCookie("http://127.0.0.1:9"),
    },
    body: JSON.stringify(callBody("req-origin")),
  }))
  assert.equal(response.status, 403)
  assert.deepEqual(await response.json(), { error: "跨站请求被拒绝" })
})

function callBody(requestId, sessionId = "rpc-session") {
  return {
    cluster: "test",
    instance: "node",
    tool: "doops_agent_prompt",
    arguments: { session_id: sessionId, instruction: "synthetic operation" },
    requestId,
  }
}

async function collectEvents(response) {
  assert.ok(response.body)
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  const events = []
  let buffer = ""
  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true })
      let index
      while ((index = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, index).trim()
        buffer = buffer.slice(index + 1)
        if (line) events.push(JSON.parse(line))
      }
    }
    buffer += decoder.decode()
    if (buffer.trim()) events.push(JSON.parse(buffer.trim()))
    return events
  } finally {
    reader.releaseLock()
  }
}

function sleep(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds))
}

async function cancelRequest(gateway, requestId, sessionId = "rpc-session") {
  return POST(rpcRequest(gateway, {
    cluster: "test",
    instance: "node",
    arguments: { session_id: sessionId },
    cancelRequestId: requestId,
  }))
}

test("quiet prompt waits for the total execution deadline instead of cancelling at 45 seconds", { timeout: 5_000 }, async (t) => {
  const gateway = await localGateway(t)
  t.mock.timers.enable({ apis: ["setTimeout"] })
  const response = await POST(rpcRequest(gateway.gateway, callBody("quiet-prompt")))
  const collected = collectEvents(response)
  const connection = await gateway.connection
  await connection.call.promise
  t.mock.timers.tick(60_000)
  await new Promise((resolve) => setImmediate(resolve))
  assert.equal(gateway.cancels.length, 0)
  assert.equal(connection.socket.readyState, WebSocket.OPEN)
  send(connection.socket, { id: 2, result: { content: [{ type: "text", text: "verified result" }] } })
  const events = await collected
  assert.ok(events.some((event) => event.type === "result"))
  assert.equal(events.some((event) => event.type === "error"), false)
})

test("cancellation ACK leaves the original RPC stream open until terminal evidence", { timeout: 5_000 }, async (t) => {
  const gateway = await localGateway(t)
  const requestId = "cancel-waits"
  const response = await POST(rpcRequest(gateway.gateway, callBody(requestId)))
  assert.equal(response.status, 200)
  let settled = false
  const eventsPromise = collectEvents(response).finally(() => { settled = true })
  const connection = await gateway.connection
  await connection.initialized.promise
  await connection.call.promise

  const cancellation = await cancelRequest(gateway.gateway, requestId)
  assert.equal(cancellation.status, 200)
  assert.deepEqual(await cancellation.json(), {
    request_id: requestId,
    cancel_requested: true,
  })
  await connection.cancel.promise
  assert.equal(settled, false, "cancel ACK must not close the original RPC stream")
  await sleep(25)
  assert.equal(settled, false, "the original stream must wait for terminal evidence")

  send(connection.socket, { id: 2, error: { code: -32007, message: "operation canceled" } })
  const events = await eventsPromise
  assert.equal(events.some((event) => event.type === "error" && event.code === -32007), true)
  assert.equal(events.some((event) => event.code === -32008), false)
  assert.equal(events.at(-1)?.type, "done")
})

test("ACK-only cancellation becomes unknown when the upstream disconnects", { timeout: 5_000 }, async (t) => {
  const gateway = await localGateway(t, {
    onCancel: (connection, message) => {
      send(connection.socket, { id: message.id, result: { canceled: true } })
      setTimeout(() => connection.socket.terminate(), 10)
    },
  })
  const requestId = "cancel-unknown"
  const response = await POST(rpcRequest(gateway.gateway, callBody(requestId)))
  const eventsPromise = collectEvents(response)
  const connection = await gateway.connection
  await connection.call.promise
  const cancellation = await cancelRequest(gateway.gateway, requestId)
  assert.equal(cancellation.status, 200)
  await connection.cancel.promise

  const events = await eventsPromise
  assert.deepEqual(events.filter((event) => event.type === "error").map((event) => event.code), [-32008])
  assert.equal(events.some((event) => event.type === "result"), false)
  assert.equal(events.at(-1)?.type, "done")
})

test("cross-wired notifications and cancellation requests are rejected", { timeout: 5_000 }, async (t) => {
  const gateway = await localGateway(t, {
    onCall: (connection, message) => {
      send(connection.socket, {
        method: "notifications/message",
        params: { sessionID: "other-session", request_id: message.id, data: "wrong session" },
      })
      send(connection.socket, {
        method: "notifications/message",
        params: { sessionID: "rpc-session", request_id: 999, data: "wrong request" },
      })
      send(connection.socket, {
        method: "notifications/message",
        params: { sessionID: "rpc-session", request_id: message.id, data: "accepted" },
      })
      connection.complete = () => send(connection.socket, {
        id: message.id,
        result: { isError: false, content: [{ text: "complete" }] },
      })
    },
  })
  const requestId = "cross-wire"
  const response = await POST(rpcRequest(gateway.gateway, callBody(requestId)))
  const eventsPromise = collectEvents(response)
  const connection = await gateway.connection
  await connection.call.promise

  const wrongCancellation = await cancelRequest(gateway.gateway, requestId, "other-session")
  assert.equal(wrongCancellation.status, 409)
  assert.equal(gateway.cancels.length, 0)
  connection.complete()

  const events = await eventsPromise
  assert.deepEqual(events.filter((event) => event.type === "output").map((event) => event.data), ["accepted"])
  assert.equal(events.some((event) => event.data === "wrong session" || event.data === "wrong request"), false)
  assert.equal(events.some((event) => event.type === "result"), true)
})

test("cancelling before dispatch prevents a late initialize response from dispatching the tool", { timeout: 5_000 }, async (t) => {
  const gateway = await localGateway(t, { onInitialize: () => {} })
  const requestId = "cancel-before-dispatch"
  const response = await POST(rpcRequest(gateway.gateway, callBody(requestId)))
  const eventsPromise = collectEvents(response)
  const connection = await gateway.connection
  await connection.initialized.promise

  const cancellation = await cancelRequest(gateway.gateway, requestId)
  assert.equal(cancellation.status, 200)
  send(connection.socket, { id: 1, result: { capabilities: {} } })
  await sleep(50)

  assert.equal(gateway.calls.length, 0)
  const events = await eventsPromise
  assert.deepEqual(events.filter((event) => event.type === "error").map((event) => event.code), [-32007])
})

const { AgentGatewayError, agentGatewayCall } = await import(sourceModule(join(root, "lib/server/agent-gateway-rpc.ts")))

// Gateway checks login and the target grant before the WebSocket upgrade, so it
// answers a refused caller with a plain HTTP status and a body we must not echo.
async function refusingGateway(t, status) {
  const server = new WebSocketServer({
    host: "127.0.0.1",
    port: 0,
    path: "/v1/rpc",
    verifyClient: (_info, done) => done(false, status, "synthetic gateway body"),
  })
  const connections = []
  server.on("connection", (socket) => connections.push(socket))
  await once(server, "listening")
  t.after(async () => {
    await new Promise((resolve, reject) => server.close((error) => {
      if (error && error.code !== "ERR_SERVER_NOT_RUNNING") reject(error)
      else resolve()
    }))
  })
  return { gateway: `http://127.0.0.1:${server.address().port}`, connections }
}

const refusedUpgrades = [
  { status: 401, code: -32001, httpStatus: 401, message: /^登录已失效，请重新登录（HTTP 401）$/ },
  { status: 403, code: -32003, httpStatus: 403, message: /^无权访问该目标，请联系管理员授权（HTTP 403）$/ },
  { status: 404, code: -32004, httpStatus: 404, message: /^目标不存在或已下线（HTTP 404）$/ },
  { status: 502, code: -32000, httpStatus: 502, message: /^Gateway 拒绝连接（HTTP 502）$/ },
]

test("a refused Gateway upgrade is a terminal, undispatched error with a stable code and Chinese reason", { timeout: 5_000 }, async (t) => {
  for (const c of refusedUpgrades) {
    const gateway = await refusingGateway(t, c.status)
    const requestId = `refused-${c.status}`
    const response = await POST(rpcRequest(gateway.gateway, callBody(requestId)))
    const events = await collectEvents(response)
    const errors = events.filter((event) => event.type === "error")
    assert.equal(errors.length, 1, `HTTP ${c.status}`)
    assert.equal(errors[0].code, c.code, `HTTP ${c.status}`)
    assert.match(errors[0].error, c.message, `HTTP ${c.status}`)
    assert.equal(events.some((event) => event.type === "open" || event.type === "result"), false, `HTTP ${c.status}`)
    assert.equal(events.at(-1).type, "done", `HTTP ${c.status}`)
    // Nothing was dispatched, so the outcome is known: never report it as unknown.
    assert.doesNotMatch(JSON.stringify(events), /execution_outcome_unknown|-32008|Unexpected server response|synthetic gateway body/, `HTTP ${c.status}`)
    assert.equal(gateway.connections.length, 0, `HTTP ${c.status}`)
    // The call is no longer registered, so a late stop cannot target it.
    assert.equal((await cancelRequest(gateway.gateway, requestId)).status, 409, `HTTP ${c.status}`)
  }
})

test("DoAgent Gateway calls refused at the upgrade keep the HTTP status and are not dispatched", { timeout: 5_000 }, async (t) => {
  for (const c of refusedUpgrades) {
    const gateway = await refusingGateway(t, c.status)
    const context = { gateway: gateway.gateway, token: "synthetic-token", cluster: "test", instance: "node", sessionId: "rpc-session" }
    await assert.rejects(
      agentGatewayCall(context, { mode: "metadata" }, new AbortController().signal),
      (error) => {
        assert.ok(error instanceof AgentGatewayError, `HTTP ${c.status}`)
        assert.equal(error.status, c.httpStatus, `HTTP ${c.status}`)
        assert.equal(error.outcome, "not_dispatched", `HTTP ${c.status}`)
        assert.match(error.message, c.message, `HTTP ${c.status}`)
        return true
      },
    )
  }
})

test("agentGatewayCall retains only the public structured receipt from a Gateway error", { timeout: 5_000 }, async (t) => {
  const report = {
    kind: "doops.verified-mutation", verified: true, outcome: "unknown",
    canonicalScope: { token: "private-scope-canary" },
    turn: {
      turnId: "partial-turn", status: "interrupted", stopReason: "budget_exceeded",
      failureClass: "budget_exceeded", failureReason: "completion review budget exhausted",
      failureDetails: { payload: "private-payload-canary" },
    },
    mutations: [{
      tool: "doops_helm", tool_call_id: "helm-call-1", status: "failed", verified: true,
      payload: "private-payload-canary",
      result: {
        isError: true, content: [{ type: "text", text: "execution_outcome_unknown" }],
        structuredContent: {
          phase: "execution", executionStarted: true, executionOutcome: "unknown",
          canonicalScope: { server: "private-scope-canary" },
        },
      },
    }],
  }
  const gateway = await localGateway(t, {
    onCall: (connection, message) => send(connection.socket, {
      id: message.id, error: { code: -32603, message: "execution_outcome_unknown", data: { structuredContent: report } },
    }),
  })
  const context = { gateway: gateway.gateway, token: "synthetic-token", cluster: "test", instance: "node", sessionId: "rpc-session" }
  await assert.rejects(
    agentGatewayCall(context, { operation: "apply" }, new AbortController().signal),
    (error) => {
      assert.ok(error instanceof AgentGatewayError)
      assert.equal(error.outcome, "unknown")
      assert.equal(error.structuredContent.kind, "doops.verified-mutation")
      assert.equal(error.structuredContent.mutations[0].result.structuredContent.executionOutcome, "unknown")
      assert.deepEqual(error.structuredContent.turn, {
        turnId: "partial-turn", status: "interrupted", stopReason: "budget_exceeded",
        failureClass: "budget_exceeded", failureReason: "completion review budget exhausted",
      })
      assert.equal("canonicalScope" in error.structuredContent, false)
      assert.equal("payload" in error.structuredContent.mutations[0], false)
      assert.equal("canonicalScope" in error.structuredContent.mutations[0].result.structuredContent, false)
      return true
    },
  )
})

test("RPC proxy preserves audit completion error data and original MCP evidence", { timeout: 5_000 }, async (t) => {
  const result = { isError: false, content: [{ text: "self-upgrade-accepted job=upgrade" }], structuredContent: { outcome: "accepted", job: "upgrade" } }
  const data = { audit_id: 42, upgrade_operation_id: "operation-42", result }
  const gateway = await localGateway(t, { onCall: (connection, message) => send(connection.socket, {
    id: message.id, error: { code: -32603, message: "audit_completion_failed", data },
  }) })
  const response = await POST(rpcRequest(gateway.gateway, callBody("upgrade-audit-error")))
  const events = await collectEvents(response)
  assert.deepEqual(events.find((event) => event.type === "error"), { type: "error", error: "audit_completion_failed", code: -32603, data })
  assert.equal(events.some((event) => event.type === "result"), false)
})

test("RPC proxy preserves native structured failure evidence", { timeout: 5_000 }, async (t) => {
  const result = { isError: true, content: [{ text: "self-upgrade-failed category=upgrade_job_failed" }], structuredContent: { outcome: "failed", category: "upgrade_job_failed" } }
  const gateway = await localGateway(t, { onCall: (connection, message) => send(connection.socket, { id: message.id, result }) })
  const events = await collectEvents(await POST(rpcRequest(gateway.gateway, callBody("upgrade-job-failed"))))
  assert.deepEqual(events.find((event) => event.type === "error").data, { result })
  assert.equal(events.some((event) => event.type === "result"), false)
})

test("upgrade verification requires the actual release commit and structuredSSH on the serving Agent", { timeout: 5_000 }, async (t) => {
  const expected = "a".repeat(40)
  const protocol = { schemaVersion: "doops.sh/structured-ssh/v1", tool: "doops_ssh", structuredArgv: true }
  for (const scenario of ["valid", "wrong-commit", "missing-commit", "missing-protocol"]) {
    const gateway = await localGateway(t, {
      onInitialize: (connection, message) => send(connection.socket, { id: message.id, result: {
        serverInfo: { releaseCommit: scenario === "valid" ? "b".repeat(40) : expected },
        capabilities: {
          releaseCommit: scenario === "wrong-commit" ? "b".repeat(40) : scenario === "missing-commit" ? undefined : expected,
          protocols: { structuredSSH: scenario === "missing-protocol" ? undefined : protocol },
        },
      } }),
      onCall: (connection, message) => send(connection.socket, { id: message.id, result: { content: [{ text: "self-upgrade-succeeded" }] } }),
    })
    const body = {
      ...callBody(`verify-${scenario}`), tool: "doops_agent_upgrade",
      arguments: { session_id: "upgrade-session", upgrade_operation_id: "operation-42", observe_job: "upgrade-job" },
      requiredProtocol: "structuredSSH", expectedReleaseCommit: expected,
    }
    const events = await collectEvents(await POST(rpcRequest(gateway.gateway, body)))
    const initialize = await (await gateway.connection).initialized.promise
    assert.deepEqual(initialize.params, {
      session_id: "upgrade-session", upgrade_operation_id: "operation-42",
      expected_release_commit: expected, required_protocol: "structuredSSH",
    })
    assert.equal(gateway.calls.length, scenario === "valid" ? 1 : 0, scenario)
    assert.equal(events.some((event) => event.type === "result"), scenario === "valid", scenario)
    assert.equal(events.some((event) => event.type === "error"), scenario !== "valid", scenario)
  }
})

test("only verification connections send allowlisted initialize audit metadata", { timeout: 5_000 }, async (t) => {
  const expected = "a".repeat(40)
  const protocol = { schemaVersion: "doops.sh/structured-ssh/v1", tool: "doops_ssh", structuredArgv: true }
  for (const mode of ["ordinary", "observe", "protocol", "commit", "both"]) {
    const gateway = await localGateway(t, {
      onInitialize: (connection, message) => send(connection.socket, { id: message.id, result: {
        serverInfo: { releaseCommit: "b".repeat(40) },
        capabilities: { releaseCommit: expected, protocols: { structuredSSH: protocol } },
      } }),
      onCall: (connection, message) => send(connection.socket, { id: message.id, result: { content: [{ text: "complete" }] } }),
    })
    const body = {
      ...callBody(`init-${mode}`), tool: mode === "ordinary" ? "doops_agent_prompt" : "doops_agent_upgrade",
      arguments: { session_id: "original-session", upgrade_operation_id: "operation-42", observe_job: "upgrade-job", expected_release_commit: expected, unrelated: "must-not-enter-initialize" },
      ...(["protocol", "both"].includes(mode) && { requiredProtocol: "structuredSSH" }),
      ...(["commit", "both"].includes(mode) && { expectedReleaseCommit: expected }),
    }
    const events = await collectEvents(await POST(rpcRequest(gateway.gateway, body)))
    const initialize = await (await gateway.connection).initialized.promise
    assert.deepEqual(initialize.params, ["ordinary", "observe"].includes(mode) ? {} : {
      session_id: "original-session", upgrade_operation_id: "operation-42",
      ...(body.requiredProtocol && { required_protocol: body.requiredProtocol }),
      ...(body.expectedReleaseCommit && { expected_release_commit: body.expectedReleaseCommit }),
    }, mode)
    assert.equal(gateway.calls.length, 1, mode)
    assert.equal(events.some((event) => event.type === "error"), false, mode)
  }
})

test("initialize verification errors retain code and original audit data without dispatching a tool", { timeout: 5_000 }, async (t) => {
  const expected = "a".repeat(40)
  const data = { audit_id: 42, upgrade_operation_id: "operation-42", result: { capabilities: { releaseCommit: expected } } }
  const gateway = await localGateway(t, { onInitialize: (connection, message) => send(connection.socket, {
    id: message.id, error: { code: -32603, message: "audit_completion_failed", data },
  }) })
  const body = {
    ...callBody("verify-audit-error"), tool: "doops_agent_upgrade", expectedReleaseCommit: expected,
    arguments: { session_id: "original-session", upgrade_operation_id: "operation-42", observe_job: "upgrade-job" },
  }
  const events = await collectEvents(await POST(rpcRequest(gateway.gateway, body)))
  assert.deepEqual(events.find((event) => event.type === "error"), { type: "error", error: "audit_completion_failed", code: -32603, data })
  assert.equal(events.some((event) => event.type === "result"), false)
  assert.equal(gateway.calls.length, 0)
})
