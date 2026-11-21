import assert from "node:assert/strict"
import test from "node:test"
import { callTool, fetchAudit, login, persistTokenSession, restoreServerSession, sessionIsAdmin, runAgentUpgrade, upgradeStorageKey, RpcError } from "./client.ts"
import { DEMO_AUDIT } from "./demo.ts"

const session = { gateway: "https://gateway.example.test", token: "synthetic-token" }
const operation = { cluster: "test", instance: "node", tool: "doops_agent_prompt", arguments: { session_id: "browser-test", operation: "apply", instruction: "synthetic deployment" } }

test("demo audit uses an inclusive since and exclusive until across equivalent timestamp formats", async (t) => {
  const original = [...DEMO_AUDIT]
  t.after(() => DEMO_AUDIT.splice(0, DEMO_AUDIT.length, ...original))
  const times = [
    "2026-11-10T23:59:59.999Z", "2026-11-11T00:00:00Z", "2026-11-11T00:00:00.001Z",
    "2026-11-11T00:00:00.999Z", "2026-11-11T00:00:01.000Z", "2026-11-11T00:00:01Z",
    "2026-11-11T08:00:01+08:00", "2026-11-11T08:00:00+08:00",
  ]
  DEMO_AUDIT.splice(0, DEMO_AUDIT.length, ...times.map((started_at, index) => ({ id: index + 1, started_at, status: "success" })))
  t.mock.method(globalThis, "fetch", () => { throw new Error("demo audit must not call the Gateway") })
  for (const [since, until] of [
    ["2026-11-11T00:00:00Z", "2026-11-11T00:00:01Z"],
    ["2026-11-11T00:00:00.000Z", "2026-11-11T00:00:01.000Z"],
    ["2026-11-11T08:00:00+08:00", "2026-11-11T08:00:01+08:00"],
  ]) {
    const result = await fetchAudit({ ...session, demo: true }, { since, until })
    assert.deepEqual(result.events.map((event) => event.id), [8, 4, 3, 2])
    assert.equal(result.summary.total, 4)
  }
})

function mockResponse(t, values, status = 200) {
  t.mock.method(globalThis, "fetch", async (_url, request) => {
    assert.equal(request.headers["Content-Type"], "application/json")
    assert.equal(request.headers.Authorization, undefined)
    assert.equal(JSON.parse(request.body).arguments.operation, "apply")
    return new Response(status === 200 ? values.join("\n") + "\n" : JSON.stringify({ error: "Gateway authentication rejected" }), { status })
  })
}

test("browser receives assistant text separately from tool output and final result", async (t) => {
  mockResponse(t, [JSON.stringify({ type: "output", kind: "assistant_delta", data: "verified" }), JSON.stringify({ type: "result", result: { isError: false, content: [{ text: "Operation complete." }] } }), JSON.stringify({ type: "done" })])
  const events = []
  await callTool(session, operation, (event) => events.push(event))
  assert.equal(events[0].kind, "assistant_delta")
  assert.equal(events[1].type, "result")
})

test("explicit cancel uses a separate request and leaves the original RPC stream open", async (t) => {
  let cancel
  const requests = []
  t.mock.method(globalThis, "fetch", async (_url, request) => {
    const body = JSON.parse(request.body)
    requests.push(body)
    if (body.cancelRequestId) {
      return Response.json({ request_id: body.cancelRequestId, cancel_requested: true })
    }
    return new Response([
      JSON.stringify({ type: "output", data: "still draining\n" }),
      JSON.stringify({ type: "result", result: { isError: false, content: [{ text: "terminal evidence" }] } }),
      JSON.stringify({ type: "done" }),
    ].join("\n") + "\n")
  })
  const events = []
  const completion = callTool(session, {
    ...operation,
    onCancelReady: (requestCancel) => { cancel = requestCancel },
  }, (event) => events.push(event))
  assert.equal(typeof cancel, "function")
  await cancel()
  await completion
  assert.equal(requests.length, 2)
  assert.ok(requests[0].requestId)
  assert.equal(requests[1].cancelRequestId, requests[0].requestId)
  assert.equal(events.some((event) => event.type === "result"), true)
  assert.equal(events.some((event) => event.type === "error"), false)
})

test("remote cancellation uncertainty is terminal failure, not local abort completion", async (t) => {
  t.mock.method(globalThis, "fetch", async (_url, request) => {
    const body = JSON.parse(request.body)
    if (body.cancelRequestId) return Response.json({ request_id: body.cancelRequestId, cancel_requested: true })
    return new Response([
      JSON.stringify({ type: "error", error: "已请求停止，远端终态未确认", code: -32008 }),
      JSON.stringify({ type: "done" }),
    ].join("\n") + "\n")
  })
  const events = []
  let cancel
  const completion = callTool(session, {
    ...operation,
    onCancelReady: (requestCancel) => { cancel = requestCancel },
  }, (event) => events.push(event))
  await cancel()
  await assert.rejects(completion, /远端终态未确认/)
  assert.deepEqual(events.filter((event) => event.type === "error").map((event) => event.error), ["已请求停止，远端终态未确认"])
})

test("an immediate stop waits for admission and failed stop requests can be retried", async (t) => {
  let accept
  let finish
  let cancel
  let cancellations = 0
  const admitted = new Promise((resolve) => { accept = resolve })
  const encoder = new TextEncoder()
  const stream = new ReadableStream({
    start(controller) {
      finish = () => {
        controller.enqueue(encoder.encode(JSON.stringify({ type: "result", result: { content: [{ text: "completed" }] } }) + "\n"))
        controller.close()
      }
    },
  })
  t.mock.method(globalThis, "fetch", async (_url, request) => {
    const body = JSON.parse(request.body)
    if (!body.cancelRequestId) {
      await admitted
      return new Response(stream)
    }
    cancellations++
    if (cancellations === 1) return Response.json({ error: "synthetic stop failure" }, { status: 503 })
    return Response.json({ request_id: body.cancelRequestId, cancel_requested: true })
  })
  const completion = callTool(session, { ...operation, onCancelReady: (stop) => { cancel = stop } }, () => {})
  const firstStop = assert.rejects(cancel(), /synthetic stop failure/)
  await Promise.resolve()
  assert.equal(cancellations, 0)
  accept()
  await firstStop
  await cancel()
  assert.equal(cancellations, 2)
  finish()
  await completion
})

test("native MCP tool errors cannot become successful browser results", async (t) => {
  mockResponse(t, [JSON.stringify({ type: "result", result: { isError: true, content: [{ text: "helm verification failed" }] } }), JSON.stringify({ type: "done" })])
  const events = []
  await assert.rejects(callTool(session, operation, (event) => events.push(event)), /helm verification failed/)
  assert.equal(events.some((event) => event.type === "result"), false)
})

test("Gateway authentication failure rejects the operation promise", async (t) => {
  mockResponse(t, [], 401)
  await assert.rejects(callTool(session, operation, () => {}), /Gateway authentication rejected/)
})

test("a Gateway upgrade refusal reaches the caller as its Chinese reason and code", async (t) => {
  const refusal = { type: "error", error: "无权访问该目标，请联系管理员授权（HTTP 403）", code: -32003 }
  mockResponse(t, [JSON.stringify(refusal), JSON.stringify({ type: "done" })])
  const events = []
  await assert.rejects(callTool(session, operation, (event) => events.push(event)), (error) => {
    assert.equal(error.message, refusal.error)
    assert.doesNotMatch(error.message, /Unexpected server response/)
    return true
  })
  assert.deepEqual(events.filter((event) => event.type === "error"), [refusal])
})

test("closed or malformed event streams cannot be declared complete", async (t) => {
  for (const values of [[JSON.stringify({ type: "done" })], ["not json"], [JSON.stringify({ type: "unknown" })]]) {
    const context = t.mock
    context.method(globalThis, "fetch", async () => new Response(values.join("\n")))
    await assert.rejects(callTool(session, operation, () => {}))
    context.restoreAll()
  }
})

test("session restoration distinguishes missing sessions from HTTP and contract errors", async (t) => {
  for (const status of [401, 403, 502]) {
    t.mock.method(globalThis, "fetch", async () => Response.json({ session: null, error: "Gateway session rejected" }, { status }))
    await assert.rejects(restoreServerSession(), /Gateway session rejected/)
    t.mock.restoreAll()
  }
  t.mock.method(globalThis, "fetch", async () => Response.json({ session: null }))
  assert.equal(await restoreServerSession(), null)
  t.mock.restoreAll()
  t.mock.method(globalThis, "fetch", async () => Response.json({ session: {} }))
  await assert.rejects(restoreServerSession(), /Gateway 会话响应无效/)
})

test("only an explicit admin session or a default demo session shows the admin console", () => {
  const cases = [
    { name: "real session without the flag", session: { gateway: "https://gateway.example.test" }, want: false },
    { name: "real ordinary user", session: { gateway: "https://gateway.example.test", isAdmin: false }, want: false },
    { name: "real platform admin", session: { gateway: "https://gateway.example.test", isAdmin: true }, want: true },
    { name: "truthy non-boolean flag", session: { gateway: "https://gateway.example.test", isAdmin: "true" }, want: false },
    { name: "default demo session", session: { gateway: "demo://local", demo: true }, want: true },
    { name: "demo admin", session: { gateway: "demo://local", demo: true, isAdmin: true }, want: true },
    { name: "demo ordinary user", session: { gateway: "demo://local", demo: true, isAdmin: false }, want: false },
  ]
  for (const { name, session: value, want } of cases) {
    assert.equal(sessionIsAdmin(value), want, name)
  }
})

test("browser sessions take isAdmin only when the server returns true", async (t) => {
  const cases = [
    { name: "admin", flag: true, want: true },
    { name: "ordinary user", flag: false, want: false },
    { name: "missing flag", flag: undefined, want: false },
    { name: "string flag", flag: "true", want: false },
  ]
  for (const { name, flag, want } of cases) {
    const body = { authenticated: true, gateway: "https://gateway.example.test", username: "ops-zhang", isAdmin: flag }
    const entries = [
      { label: "login", run: () => login("https://gateway.example.test", "ops-zhang", "synthetic-password"), response: body },
      { label: "token", run: () => persistTokenSession("https://gateway.example.test", "synthetic-token"), response: body },
      { label: "restore", run: () => restoreServerSession(), response: { session: { gateway: body.gateway, username: body.username, isAdmin: flag } } },
    ]
    for (const entry of entries) {
      t.mock.method(globalThis, "fetch", async () => Response.json(entry.response))
      const result = await entry.run()
      assert.equal(result.isAdmin, want, `${entry.label}: ${name}`)
      t.mock.restoreAll()
    }
  }
})

const upgradeID = "12345678-1234-4123-8123-123456789abc"
const upgradeCommit = "a".repeat(40)
const upgradeTarget = { cluster: "test", instance: "node" }
const upgradeStart = {
  id: upgradeID, sessionId: "original-upgrade-session", phase: "preflight", status: "running",
  image: `registry.test/team/agent@sha256:${"b".repeat(64)}`,
  sourceCommit: upgradeCommit, expectedCommit: upgradeCommit, ciURL: "https://ci.test/builds/42",
  deadline: Date.now() + 60_000, output: "", error: "", auditError: "",
}

function upgradeResult(outcome, phase, jobPhase = phase, expectedCommit = upgradeCommit) {
  const structuredContent = {
    outcome, phase, job_phase: jobPhase, job: `job-${jobPhase}`, namespace: "test", release: "agent",
    expected_release_commit: expectedCommit, upgrade_operation_id: upgradeID,
  }
  return {
    isError: outcome === "failed",
    content: [{ type: "text", text: `self-upgrade-${outcome} job=job-${jobPhase} namespace=test\nsynthetic evidence` }],
    structuredContent,
  }
}

function upgradeGateway(t, response) {
  const calls = []
  t.mock.method(globalThis, "fetch", async (_url, request) => {
    const call = JSON.parse(request.body)
    calls.push(call)
    const event = response(call, calls.length)
    return new Response([JSON.stringify(event), JSON.stringify({ type: "done" })].join("\n"))
  })
  return calls
}

function normalUpgrade(call) {
  const args = call.arguments
  if (args.observe_job) return { type: "result", result: upgradeResult("succeeded", "observe", args.observe_job.slice(4)) }
  return { type: "result", result: upgradeResult("accepted", args.rollback ? "rollback" : args.preflight ? "preflight" : "apply") }
}

function verificationFault(category = "", changes = {}) {
  return { type: "error", code: -32603, error: "audit_completion_failed", data: {
    audit_id: 123, upgrade_operation_id: upgradeID,
    result: { isError: Boolean(category), structuredContent: {
      phase: "verify", upgrade_operation_id: upgradeID, outcome: category ? "error" : "success", category,
      expected_release_commit: upgradeCommit, release_commit: upgradeCommit,
      required_protocol: "structuredSSH", structured_ssh: !category, ...changes,
    } },
  } }
}

test("verify audit failure finishes promptly without rollback and survives resumed observation", async (t) => {
  const calls = upgradeGateway(t, (call) => call.requiredProtocol ? verificationFault() : normalUpgrade(call))
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, value => states.push(value)), /audit_completion_failed/)
  const saved = JSON.parse(JSON.stringify(states.at(-1)))
  assert.equal(saved.status, "audit_failed")
  assert.equal(saved.verificationComplete, true)
  assert.equal(saved.auditFailure.audit_id, 123)
  assert.equal(calls.length, 5)
  assert.equal(calls.filter(call => call.arguments.rollback).length, 0)
  t.mock.restoreAll()
  const resumed = upgradeGateway(t, normalUpgrade)
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, saved, new AbortController().signal, value => states.push(value), true), /audit_completion_failed/)
  assert.equal(states.at(-1).status, "audit_failed")
  assert.equal(states.at(-1).verificationComplete, true)
  assert.equal(states.at(-1).auditFailure.audit_id, 123)
  assert.equal(resumed.length, 2)
  assert.ok(resumed.every(call => call.arguments.observe_job === "job-apply"))
})

test("missing or inconsistent verify audit evidence cannot authorize rollback or success", async (t) => {
  for (const event of [
    { type: "error", code: -32603, error: "audit_completion_failed" },
    verificationFault("", { release_commit: "b".repeat(40) }),
    verificationFault("", { upgrade_operation_id: "different" }),
    verificationFault("", { structured_ssh: false }),
  ]) {
    const calls = upgradeGateway(t, call => call.requiredProtocol ? event : normalUpgrade(call))
    const states = []
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, value => states.push(value)), /audit_completion_failed/)
    assert.equal(states.at(-1).status, "audit_failed")
    assert.equal(states.at(-1).verificationComplete, false)
    assert.ok(states.at(-1).auditError)
    assert.equal(calls.filter(call => call.arguments.rollback).length, 0)
    t.mock.restoreAll()
  }
})

test("definite final target rejection rolls back once even when its audit write also fails", async (t) => {
  for (const auditFails of [false, true]) {
    const previousCommit = "c".repeat(40)
    const calls = upgradeGateway(t, call => {
      if (call.arguments.rollback || call.arguments.observe_job === "job-rollback") return {
        type: "result", result: upgradeResult(call.arguments.rollback ? "accepted" : "succeeded", call.arguments.rollback ? "rollback" : "observe", "rollback", previousCommit),
      }
      if (call.requiredProtocol) return auditFails ? verificationFault("upgrade_verification_protocol_missing") : {
        type: "error", code: -32003, error: "upgrade_verification_protocol_missing",
      }
      return normalUpgrade(call)
    })
    const states = []
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, value => states.push(value)), /upgrade_verification_protocol_missing/)
    assert.equal(calls.filter(call => call.arguments.rollback).length, 1)
    assert.equal(calls.filter(call => call.arguments.preflight === false).length, 1)
    assert.equal(calls.at(-1).expectedReleaseCommit, previousCommit)
    assert.equal(calls.at(-1).requiredProtocol, "structuredSSH")
    assert.equal(states.at(-1).verificationComplete, true)
    assert.equal(states.at(-1).status, auditFails ? "audit_failed" : "rolled_back")
    assert.match(states.at(-1).error, /已回滚并核验/)
    t.mock.restoreAll()
  }
})

test("transient or forbidden final verification never dispatches a speculative rollback", async (t) => {
  for (const event of [
    { type: "error", code: -32008, error: "execution_outcome_unknown" },
    { type: "error", code: -32003, error: "upgrade_verification_target_offline" },
    { type: "error", code: -32003, error: "upgrade_verification_forbidden" },
  ]) {
    const calls = upgradeGateway(t, call => call.requiredProtocol ? event : normalUpgrade(call))
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, () => {}))
    assert.equal(calls.length, 5)
    assert.equal(calls.filter(call => call.arguments.rollback).length, 0)
    t.mock.restoreAll()
  }
})

test("received audit failure survives a broken response stream and later resume", async (t) => {
  t.mock.method(globalThis, "fetch", async (_url, request) => {
    const call = JSON.parse(request.body)
    if (!call.requiredProtocol) return new Response(JSON.stringify(normalUpgrade(call)) + "\n")
    let delivered = false
    return new Response(new ReadableStream({ pull(controller) {
      if (!delivered) {
        delivered = true
        controller.enqueue(new TextEncoder().encode(JSON.stringify(verificationFault()) + "\n"))
      } else controller.error(new TypeError("response stream disconnected"))
    } }))
  })
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, value => states.push(value)))
  const saved = JSON.parse(JSON.stringify(states.at(-1)))
  assert.equal(saved.auditFailure.audit_id, 123)
  assert.equal(saved.status, "audit_failed")
  t.mock.restoreAll()
  const resumed = upgradeGateway(t, normalUpgrade)
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, saved, new AbortController().signal, value => states.push(value), true), /audit_completion_failed/)
  assert.equal(states.at(-1).status, "audit_failed")
  assert.equal(states.at(-1).verificationComplete, true)
  assert.equal(resumed.length, 2)
})

test("received audited Job acceptance stays resumable after reset or malformed trailing data", async (t) => {
  for (const phase of ["preflight", "apply", "rollback"]) for (const broken of ["reset", "malformed"]) {
    const calls = []
    t.mock.method(globalThis, "fetch", async (_url, request) => {
      const call = JSON.parse(request.body)
      calls.push(call)
      const normal = normalUpgrade(call)
      if (call.arguments.observe_job || normal.result.structuredContent.phase !== phase) return new Response(JSON.stringify(normal) + "\n")
      let delivered = false
      const event = { type: "error", code: -32603, error: "audit_completion_failed", data: {
        audit_id: 123, upgrade_operation_id: upgradeID, result: normal.result,
      } }
      return new Response(new ReadableStream({ pull(controller) {
        if (!delivered) {
          delivered = true
          controller.enqueue(new TextEncoder().encode(JSON.stringify(event) + "\n"))
        } else if (broken === "reset") controller.error(new TypeError("stream reset"))
        else { controller.enqueue(new TextEncoder().encode("invalid trailing event\n")); controller.close() }
      } }))
    })
    const states = []
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, { ...upgradeStart, phase: phase === "rollback" ? "rollback" : "preflight" }, new AbortController().signal, value => states.push(value)))
    const saved = JSON.parse(JSON.stringify(states.at(-1)))
    assert.equal(saved.status, "outcome_unknown")
    assert.equal(saved.job, `job-${phase}`)
    assert.equal(saved.auditFailure.audit_id, 123)
    t.mock.restoreAll()
    const resumed = upgradeGateway(t, normalUpgrade)
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, saved, new AbortController().signal, value => states.push(value), true), /audit_completion_failed/)
    assert.ok(resumed.every(call => call.arguments.observe_job === saved.job))
    assert.equal(states.at(-1).verificationComplete, true)
    assert.equal(states.at(-1).status, "audit_failed")
    t.mock.restoreAll()
  }
})

test("definitive failed Job remains failed with its earlier audit warning", async (t) => {
  const calls = upgradeGateway(t, call => {
    if (call.arguments.preflight === false) return { ...verificationFault(), data: {
      audit_id: 123, upgrade_operation_id: upgradeID, result: normalUpgrade(call).result,
    } }
    if (call.arguments.observe_job === "job-apply") return { type: "result", result: upgradeResult("failed", "observe", "apply") }
    return normalUpgrade(call)
  })
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, value => states.push(value)))
  assert.equal(states.at(-1).status, "failed")
  assert.equal(states.at(-1).auditFailure.audit_id, 123)
  assert.ok(states.at(-1).auditError)
  assert.equal(calls.length, 4)
})

test("definitive rollback refusal is failed without a nonexistent Job to resume", async (t) => {
  const calls = upgradeGateway(t, call => {
    if (call.arguments.rollback) return { type: "result", result: {
      isError: true, content: [{ text: "rollback baseline missing" }], structuredContent: {
        outcome: "failed", phase: "rollback", upgrade_operation_id: upgradeID,
      },
    } }
    if (call.requiredProtocol) return { type: "error", code: -32003, error: "upgrade_verification_commit_mismatch" }
    return normalUpgrade(call)
  })
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, value => states.push(value)), /rollback baseline missing/)
  assert.equal(calls.filter(call => call.arguments.rollback).length, 1)
  assert.equal(states.at(-1).phase, "rollback")
  assert.equal(states.at(-1).status, "failed")
  assert.equal(states.at(-1).job, undefined)
})

test("upgrade runs preflight before apply and carries one identity and provenance through final verification", async (t) => {
  const calls = upgradeGateway(t, normalUpgrade)
  const states = []
  await runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, (value) => states.push(value))
  assert.equal(calls.length, 5)
  assert.equal(calls[0].arguments.preflight, true)
  assert.equal(calls[1].arguments.observe_job, "job-preflight")
  assert.equal(calls[2].arguments.preflight, false)
  assert.equal(calls[3].arguments.observe_job, "job-apply")
  assert.equal(calls[4].requiredProtocol, "structuredSSH")
  assert.equal(calls[4].expectedReleaseCommit, upgradeCommit)
  for (const { arguments: args } of calls) {
    assert.equal(args.upgrade_operation_id, upgradeID)
    assert.equal(args.session_id, upgradeStart.sessionId)
    assert.equal(args.expected_release_commit, upgradeCommit)
    assert.equal(args.source_commit, upgradeCommit)
    assert.equal(args.ci_url, upgradeStart.ciURL)
  }
  assert.equal(states.at(-1).status, "verified")
})

test("failed preflight preserves its structured error and never applies", async (t) => {
  const calls = upgradeGateway(t, (call) => call.arguments.observe_job
    ? { type: "result", result: upgradeResult("failed", "observe", "preflight") }
    : normalUpgrade(call))
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, (value) => states.push(value)), (error) => {
    assert.ok(error instanceof RpcError)
    assert.equal(error.data.result.structuredContent.outcome, "failed")
    return true
  })
  assert.equal(calls.length, 2)
  assert.equal(states.at(-1).status, "failed")
  assert.match(states.at(-1).output, /synthetic evidence/)
})

test("audit completion failure preserves acceptance, finishes verification, and never declares success", async (t) => {
  const calls = upgradeGateway(t, (call) => {
    const normal = normalUpgrade(call)
    if (call.arguments.preflight === false) return { type: "error", code: -32603, error: "audit_completion_failed", data: { audit_id: 99, upgrade_operation_id: upgradeID, result: normal.result } }
    return normal
  })
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, (value) => states.push(value)), (error) => {
    assert.ok(error instanceof RpcError)
    assert.equal(error.message, "audit_completion_failed")
    assert.equal(error.data.audit_id, 99)
    return true
  })
  assert.equal(calls.length, 5)
  assert.equal(calls.at(-1).expectedReleaseCommit, upgradeCommit)
  assert.equal(states.at(-1).job, "job-apply")
  assert.equal(states.at(-1).status, "audit_failed")
  assert.match(states.at(-1).auditError, /99/)
  assert.equal(states.some((value) => value.status === "verified"), false)
  const saved = JSON.parse(JSON.stringify(states.at(-1)))
  assert.deepEqual(saved.auditFailure, { audit_id: 99, upgrade_operation_id: upgradeID, result: normalUpgrade(calls[2]).result })
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, saved, new AbortController().signal, (value) => states.push(value), true), (error) => {
    assert.equal(error.message, "audit_completion_failed")
    assert.deepEqual(error.data, saved.auditFailure)
    return true
  })
  assert.ok(calls.slice(5).every((call) => call.arguments.observe_job === "job-apply"))
  assert.equal(calls.filter((call) => call.arguments.preflight === false).length, 1)
})

test("preflight audit failure observes the Job but blocks subsequent apply", async (t) => {
  const calls = upgradeGateway(t, (call) => call.arguments.preflight
    ? { type: "error", code: -32603, error: "audit_completion_failed", data: { audit_id: 98, upgrade_operation_id: upgradeID, result: normalUpgrade(call).result } }
    : normalUpgrade(call))
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, (value) => states.push(value)), /audit_completion_failed/)
  assert.equal(calls.length, 2)
  assert.equal(calls[1].arguments.observe_job, "job-preflight")
  assert.equal(states.at(-1).status, "audit_failed")
  assert.match(states.at(-1).error, /停止后续 apply/)
})

test("only the exact audit completion error with valid correlation can continue verification", async (t) => {
  for (const change of [
    { error: "another_internal_error" },
    { error: "audit_completion_failed; execution outcome requires verification" },
    { code: -32000 },
    { audit_id: 0 },
    { audit_id: -1 },
    { audit_id: 1.5 },
    { audit_id: "99" },
  ]) {
    const calls = upgradeGateway(t, () => ({
      type: "error", error: change.error ?? "audit_completion_failed", code: change.code ?? -32603,
      data: { audit_id: change.audit_id ?? 99, upgrade_operation_id: upgradeID, result: upgradeResult("accepted", "preflight") },
    }))
    const states = []
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, (value) => states.push(value)))
    assert.equal(calls.length, 1, JSON.stringify(change))
    assert.equal(Boolean(states.at(-1).auditError), change.audit_id !== undefined, JSON.stringify(change))
    assert.equal(states.at(-1).auditFailure, undefined, "invalid evidence is never saved as recoverable")
    t.mock.restoreAll()
  }
})

test("audit failure during observation or verification remains a failure after a successful Job", async (t) => {
  for (const failedCall of [4, 5]) {
    const calls = upgradeGateway(t, (call, index) => index === failedCall
      ? { type: "error", code: -32603, error: "audit_completion_failed", data: { audit_id: 100 + index, upgrade_operation_id: upgradeID, result: normalUpgrade(call).result } }
      : normalUpgrade(call))
    const states = []
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, (value) => states.push(value)), /audit_completion_failed/)
    assert.equal(calls.length, 5)
    assert.equal(calls.filter((call) => call.arguments.preflight === false).length, 1)
    assert.equal(states.at(-1).status, "audit_failed")
    t.mock.restoreAll()
  }
})

test("resumed verification preserves an earlier audit failure without reapplying", async (t) => {
  const calls = upgradeGateway(t, normalUpgrade)
  const states = []
  const stored = { ...upgradeStart, phase: "apply", status: "audit_failed", job: "job-apply", namespace: "test", release: "agent", auditError: "审计终态写入失败（审计 ID 99）" }
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, stored, new AbortController().signal, (value) => states.push(value), true), /audit_completion_failed/)
  assert.equal(calls.length, 2)
  assert.ok(calls.every((call) => call.arguments.observe_job === "job-apply"))
  assert.equal(states.at(-1).status, "audit_failed")
  assert.equal(states.at(-1).auditError, stored.auditError)
})

test("audit error data from another operation cannot resume or trigger mutation", async (t) => {
  const calls = upgradeGateway(t, () => ({
    type: "error", code: -32603, error: "audit_completion_failed",
    data: { audit_id: 98, upgrade_operation_id: "other-operation", result: upgradeResult("accepted", "preflight") },
  }))
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, () => {}))
  assert.equal(calls.length, 1)
})

test("restoring a preflight or apply operation only observes the stored Job and original session", async (t) => {
  for (const phase of ["preflight", "apply", "rollback"]) {
    const calls = upgradeGateway(t, normalUpgrade)
    const states = []
    const stored = { ...upgradeStart, phase, status: "paused", job: `job-${phase}`, namespace: "test", release: "agent" }
    await runAgentUpgrade(session, upgradeTarget, stored, new AbortController().signal, (value) => states.push(value), true)
    assert.ok(calls.every((call) => call.arguments.observe_job === stored.job && call.arguments.session_id === stored.sessionId))
    assert.equal(states.at(-1).status, phase === "preflight" ? "preflight_passed" : "verified")
    assert.equal(calls.length, phase === "preflight" ? 1 : 2)
    t.mock.restoreAll()
  }
})

test("rollback uses the receipt commit and verifies both identity and structuredSSH", async (t) => {
  const restoredCommit = "c".repeat(40)
  const calls = upgradeGateway(t, (call) => ({
    type: "result",
    result: upgradeResult(call.arguments.observe_job ? "succeeded" : "accepted", call.arguments.observe_job ? "observe" : "rollback", "rollback", restoredCommit),
  }))
  const states = []
  await runAgentUpgrade(session, upgradeTarget, { ...upgradeStart, phase: "rollback", sourceCommit: "", ciURL: "", expectedCommit: "" }, new AbortController().signal, (value) => states.push(value))
  assert.equal(calls[0].arguments.rollback, true)
  assert.equal(calls.at(-1).expectedReleaseCommit, restoredCommit)
  assert.equal(calls.at(-1).arguments.expected_release_commit, restoredCommit)
  assert.equal(calls.at(-1).requiredProtocol, "structuredSSH")
  assert.equal(states.at(-1).status, "verified")
})

test("rollback without a receipt commit retains its Job but cannot pass verification", async (t) => {
  const calls = upgradeGateway(t, () => ({ type: "result", result: upgradeResult("accepted", "rollback", "rollback", "") }))
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, { ...upgradeStart, phase: "rollback", expectedCommit: "" }, new AbortController().signal, (value) => states.push(value)), /expected_release_commit/)
  assert.equal(calls.length, 1)
  assert.equal(states.at(-1).job, "job-rollback")
  assert.notEqual(states.at(-1).status, "verified")
})

test("structured unknown outcomes retain their Job and require explicit observation without replay", async (t) => {
  const calls = upgradeGateway(t, (call) => call.arguments.preflight === false
    ? { type: "error", error: "category=upgrade_job_create_unknown", data: { result: { ...upgradeResult("unknown", "apply"), isError: true } } }
    : normalUpgrade(call))
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, (value) => states.push(value)), /upgrade_job_create_unknown/)
  const saved = states.at(-1)
  assert.equal(saved.job, "job-apply")
  assert.equal(saved.status, "outcome_unknown")
  assert.equal(calls.length, 3)
  await runAgentUpgrade(session, upgradeTarget, saved, new AbortController().signal, (value) => states.push(value), true)
  assert.equal(calls.length, 5)
  assert.ok(calls.slice(3).every((call) => call.arguments.observe_job === "job-apply"))
  assert.equal(states.at(-1).status, "verified")
})

test("unknown mutation result without a receipt never becomes an ordinary failure or resubmits", async (t) => {
  const calls = upgradeGateway(t, (call) => call.arguments.preflight === false
    ? { type: "error", error: "execution_outcome_unknown", code: -32008 }
    : normalUpgrade(call))
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, (value) => states.push(value)), /execution_outcome_unknown/)
  assert.equal(states.at(-1).phase, "apply")
  assert.equal(states.at(-1).job, "doops-self-upgrade-af16785cab34225b1e15dbbe-apply")
  assert.equal(states.at(-1).status, "outcome_unknown")
  assert.equal(calls.length, 3)
})

test("lost mutation responses remain unknown for network loss, truncated streams and local abort", async (t) => {
  for (const scenario of ["network", "truncated", "abort"]) {
    const controller = new AbortController()
    const calls = []
    t.mock.method(globalThis, "fetch", async (_url, request) => {
      const call = JSON.parse(request.body)
      calls.push(call)
      if (call.arguments.preflight === false) {
        if (scenario === "network") throw new TypeError("Failed to fetch")
        if (scenario === "abort") {
          controller.abort()
          throw new DOMException("view closed", "AbortError")
        }
        return new Response(JSON.stringify({ type: "open" }) + "\n")
      }
      return new Response(JSON.stringify(normalUpgrade(call)) + "\n")
    })
    const states = []
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, controller.signal, (value) => states.push(value)))
    assert.equal(calls.length, 3, scenario)
    assert.equal(states.at(-1).status, "outcome_unknown", scenario)
    assert.equal(states.at(-1).job, "doops-self-upgrade-af16785cab34225b1e15dbbe-apply", scenario)
    t.mock.restoreAll()
  }
})

test("an unknown rollback recovers missing receipt fields only from its own Job", async (t) => {
  const restoredCommit = "c".repeat(40)
  const calls = upgradeGateway(t, (call) => {
    if (call.arguments.rollback) return { type: "error", error: "upgrade_job_create_unknown", data: { result: {
      isError: true, structuredContent: { outcome: "unknown", phase: "rollback", job: "job-rollback", namespace: "test", upgrade_operation_id: upgradeID },
    } } }
    return { type: "result", result: upgradeResult("succeeded", "observe", "rollback", restoredCommit) }
  })
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, { ...upgradeStart, phase: "rollback", sourceCommit: "", ciURL: "", expectedCommit: "" }, new AbortController().signal, (value) => states.push(value)))
  const saved = JSON.parse(JSON.stringify(states.at(-1)))
  assert.equal(saved.job, "job-rollback")
  assert.equal(saved.release, undefined)
  await runAgentUpgrade(session, upgradeTarget, saved, new AbortController().signal, (value) => states.push(value), true)
  assert.equal(calls.length, 3)
  assert.equal(calls.at(-1).expectedReleaseCommit, restoredCommit)
  assert.equal(states.at(-1).status, "verified")
})

test("malformed error evidence preserves the original RPC failure and never dispatches apply", async (t) => {
  for (const result of [null, {}, { structuredContent: { outcome: "accepted", job: 12 } }]) {
    const data = { audit_id: 42, upgrade_operation_id: upgradeID, result }
    const calls = upgradeGateway(t, () => ({ type: "error", code: -32603, error: "audit_completion_failed", data }))
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, () => {}), (error) => {
      assert.ok(error instanceof RpcError)
      assert.equal(error.message, "audit_completion_failed")
      assert.deepEqual(error.data, data)
      return true
    })
    assert.equal(calls.length, 1)
    t.mock.restoreAll()
  }
})

test("rollback observations must match release, phase and the accepted commit", async (t) => {
  for (const field of ["release", "job_phase", "expected_release_commit"]) {
    const calls = upgradeGateway(t, (call) => {
      const event = { type: "result", result: upgradeResult(call.arguments.observe_job ? "succeeded" : "accepted", call.arguments.observe_job ? "observe" : "rollback", "rollback") }
      if (call.arguments.observe_job) event.result.structuredContent[field] = "different"
      return event
    })
    const states = []
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, { ...upgradeStart, phase: "rollback", sourceCommit: "", ciURL: "", expectedCommit: "" }, new AbortController().signal, (value) => states.push(value)))
    assert.equal(calls.length, 2, field)
    assert.equal(states.some((value) => value.status === "verified"), false, field)
    t.mock.restoreAll()
  }
})

test("verification rejection, changed Job, and text-only results never pass", async (t) => {
  for (const scenario of ["protocol", "commit", "job", "text-only"]) {
    const calls = upgradeGateway(t, (call) => {
      if (call.expectedReleaseCommit && ["protocol", "commit"].includes(scenario)) return { type: "error", error: `${scenario} verification failed` }
      const event = normalUpgrade(call)
      if (scenario === "text-only") delete event.result.structuredContent
      if (scenario === "job" && call.arguments.observe_job) event.result.structuredContent.job = "other-job"
      return event
    })
    const states = []
    await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, new AbortController().signal, (value) => states.push(value)))
    assert.equal(states.some((value) => value.status === "verified"), false)
    assert.ok(calls.length <= 5)
    t.mock.restoreAll()
  }
})

test("stopped observation keeps the Job and never proceeds to apply", async (t) => {
  const controller = new AbortController()
  const calls = upgradeGateway(t, (call) => {
    if (call.arguments.observe_job) {
      controller.abort()
      return { type: "result", result: upgradeResult("running", "observe", "preflight") }
    }
    return normalUpgrade(call)
  })
  const states = []
  await assert.rejects(runAgentUpgrade(session, upgradeTarget, upgradeStart, controller.signal, (value) => states.push(value)), { name: "AbortError" })
  assert.equal(calls.length, 2)
  assert.equal(states.at(-1).job, "job-preflight")
  assert.equal(states.at(-1).status, "paused")
})

test("upgrade persistence is isolated by Gateway, user and target without credential payloads", () => {
  const base = { gateway: session.gateway, username: "ops-one", token: "never-persist-this-token" }
  const key = upgradeStorageKey(base, upgradeTarget)
  for (const [login, target] of [
    [{ ...base, username: "ops-two" }, upgradeTarget],
    [{ ...base, gateway: "https://other.test" }, upgradeTarget],
    [base, { ...upgradeTarget, instance: "other" }],
    [base, { ...upgradeTarget, cluster: "other" }],
  ]) assert.notEqual(upgradeStorageKey(login, target), key)
  assert.doesNotMatch(key, /never-persist-this-token/)
  assert.throws(() => upgradeStorageKey(session, upgradeTarget), /用户标识/)
})
