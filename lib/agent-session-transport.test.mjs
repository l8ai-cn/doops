import assert from "node:assert/strict"
import { mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs"
import test, { after } from "node:test"
import { fileURLToPath, pathToFileURL } from "node:url"
import { join, resolve } from "node:path"
import TypeScript from "typescript"
import { create, toJson } from "@bufbuild/protobuf"
import { createAgentCommandEnvelope } from "@agent-cloud/agent-ui/runtime"
import { CommandReceiptSchema, CommandReceiptState } from "@agent-cloud/agent-ui/protocol"

const root = fileURLToPath(new URL("../", import.meta.url))
const fixture = mkdtempSync(join(root, ".agent-session-transport-test-"))
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
  const parsed = TypeScript.createSourceFile(filename, javascript, TypeScript.ScriptTarget.ES2022, true, TypeScript.ScriptKind.JS)
  for (const statement of parsed.statements.filter((statement) =>
    (TypeScript.isImportDeclaration(statement) || TypeScript.isExportDeclaration(statement)) && statement.moduleSpecifier).reverse()) {
    const specifier = statement.moduleSpecifier
    const name = specifier.text
    if (!name.startsWith(".") && !name.startsWith("@/")) continue
    const source = name.startsWith("@/") ? join(root, name.slice(2)) : name
    const resolved = TypeScript.resolveModuleName(source, filename, {
      moduleResolution: TypeScript.ModuleResolutionKind.Bundler,
      module: TypeScript.ModuleKind.ESNext,
    }, TypeScript.sys).resolvedModule
    assert.ok(resolved && resolved.resolvedFileName.startsWith(root) && resolved.resolvedFileName.endsWith(".ts"))
    const replacement = sourceModule(resolved.resolvedFileName)
    javascript = javascript.slice(0, specifier.getStart(parsed)) + JSON.stringify(replacement) + javascript.slice(specifier.end)
  }
  writeFileSync(output, javascript)
  return url
}

const { createDoopsAgentSessionTransport } = await import(sourceModule(join(root, "lib/agent-session-transport.ts")))

test("native agent prompt keeps text and retry identity stable without injected bindings", async (t) => {
  const transport = createDoopsAgentSessionTransport({
    session: { gateway: "https://gateway.example.test" },
    target: { cluster: "test", instance: "node" },
    sessionId: "session-one",
  })
  const issuedAt = new Date().toISOString()
  const commandInput = {
    sessionId: "session-one",
    streamEpoch: "epoch-one",
    commandId: "command-one",
    issuedAt,
    expectedRevision: 0n,
    command: { case: "sendPrompt", value: { text: "deploy synthetic", attachments: [] } },
  }
  const first = await createAgentCommandEnvelope(commandInput)
  const requests = []
  t.mock.method(globalThis, "fetch", async (_url, init) => {
    const body = JSON.parse(init.body)
    requests.push(body.command)
    const receipt = create(CommandReceiptSchema, {
      sessionId: body.command.sessionId,
      commandId: body.command.commandId,
      payloadDigest: body.command.payloadDigest,
      state: CommandReceiptState.ACCEPTED,
      receivedAt: issuedAt,
      updatedAt: issuedAt,
    })
    return new Response(JSON.stringify(toJson(CommandReceiptSchema, receipt)), {
      headers: { "Content-Type": "application/json" },
    })
  })

  await transport.execute(first, new AbortController().signal)
  await transport.execute(first, new AbortController().signal)
  const retryEnvelope = await createAgentCommandEnvelope(commandInput)
  await transport.execute(retryEnvelope, new AbortController().signal)

  assert.equal(requests.length, 3)
  assert.deepEqual(requests[0], requests[1])
  assert.deepEqual(requests[1], requests[2])
  assert.match(JSON.stringify(requests[0]), /deploy synthetic/)
  assert.doesNotMatch(JSON.stringify(requests[0]), /数据库引用|代码仓库引用/)
})

test("a target without a model stops retrying after the limit and shows the Chinese hint", { timeout: 10_000 }, async (t) => {
  const { createDoopsAgentUiRuntime } = await import(sourceModule(join(root, "lib/agent-ui-runtime.ts")))
  const requests = []
  t.mock.method(globalThis, "fetch", async (url) => {
    requests.push(String(url))
    return new Response(JSON.stringify({ error: "Gateway 还没有可下发到该节点的模型" }), {
      status: 503, headers: { "Content-Type": "application/json" },
    })
  })
  const runtime = createDoopsAgentUiRuntime({
    session: { gateway: "https://gateway.example.test" },
    target: { cluster: "test", instance: "node", key: "test/node" },
    sessionId: "session-no-model",
  })
  await assert.rejects(runtime.open("session-no-model"))
  const snapshotRequests = requests.filter((url) => url.startsWith("/api/agent-sessions?"))
  // maxConsecutiveFailures=2: the first attempt plus two retries, then the connection fails closed.
  assert.equal(snapshotRequests.length, 3)
  // The runtime publishes connection changes on its next frame; wait past it and past any retry delay.
  await new Promise((resolve) => setTimeout(resolve, 1_500))
  const snapshot = runtime.getSnapshot("session-no-model")
  assert.equal(snapshot.connection, "disconnected")
  assert.equal(snapshot.error, "该目标还没有绑定 DoAgent 模型。平台管理员可在「凭据」里为该目标绑定并激活 modelProvider 凭据")
  assert.equal(requests.length, snapshotRequests.length, "no further requests after the retry limit")
  runtime.close("session-no-model")
})

test("demo mode answers from local data and never calls the gateway", async () => {
  const { createDemoAgentSessionTransport } = await import(sourceModule(join(root, "lib/demo-agent-session.ts")))
  const { createAgentCommandEnvelope } = await import("@agent-cloud/agent-ui/runtime")
  const { create } = await import("@bufbuild/protobuf")
  const { SessionCursorSchema } = await import("@agent-cloud/agent-ui/protocol")
  const transport = createDemoAgentSessionTransport("demo-session")
  const snapshot = await transport.getSnapshot()
  assert.equal(snapshot.configuration.model, "demo/ops-assistant")
  assert.equal(snapshot.capabilities.extensions.some((item) => item.namespace === "doops.operations" && item.semanticKey === "apply"), true)
  const command = await createAgentCommandEnvelope({
    sessionId: snapshot.sessionId,
    streamEpoch: snapshot.streamEpoch,
    commandId: "demo-command",
    issuedAt: new Date().toISOString(),
    expectedRevision: snapshot.revision,
    command: { case: "sendPrompt", value: { text: "检查一下节点", attachments: [] } },
  })
  const abort = new AbortController()
  const seen = []
  const reading = (async () => {
    for await (const batch of transport.streamDeltas(create(SessionCursorSchema, {
      sessionId: snapshot.sessionId,
      streamEpoch: snapshot.streamEpoch,
      revision: snapshot.revision,
      sequence: snapshot.latestSequence,
    }), abort.signal)) {
      seen.push(JSON.stringify(batch, (_, value) => typeof value === "bigint" ? value.toString() : value))
      if (seen.join("\n").includes("巡检结果")) break
    }
  })()
  await transport.execute(command)
  await Promise.race([
    reading,
    new Promise((_, reject) => setTimeout(() => reject(new Error("演示回复没有出现")), 2000)),
  ])
  abort.abort()
  await reading.catch(() => {})
  assert.match(seen.join("\n"), /巡检结果/)
  assert.doesNotMatch(seen.join("\n"), /缺少登录 token/)
})

test("a demo console session releases each completed Helm interaction without calling the gateway", { timeout: 10_000 }, async (t) => {
  t.mock.method(globalThis, "fetch", () => { throw new Error("demo must not call the gateway") })
  const { createDoopsAgentUiRuntime, isDoopsAgentSessionBusy } = await import(sourceModule(join(root, "lib/agent-ui-runtime.ts")))
  const runtime = createDoopsAgentUiRuntime({
    session: { gateway: "demo://local", demo: true, token: "demo-local-token", username: "演示用户" },
    target: { cluster: "prod-cn", instance: "web-01", key: "prod-cn/web-01" },
    sessionId: "demo-ui",
  })
  await runtime.open("demo-ui")
  await new Promise((resolve) => setTimeout(resolve, 50))
  const snapshot = runtime.getSnapshot("demo-ui")
  assert.equal(snapshot.connection, "connected")
  assert.equal(snapshot.error, null)
  assert.equal(snapshot.capabilities.extensions.some((item) => item.namespace === "doops.operations" && item.semanticType === "apply"), true)
  for (const commandId of ["demo-first-release", "demo-second-release"]) {
    await runtime.executeExtension("demo-ui", commandId, {
      namespace: "doops.operations", semanticType: "apply", schemaVersion: "1", payload: { input: {} },
    })
    for (let attempt = 0; attempt < 100 && runtime.getSnapshot("demo-ui").interactions.find((item) => item.commandId === commandId)?.status !== "succeeded"; attempt++) {
      await new Promise((resolve) => setTimeout(resolve, 10))
    }
    const final = runtime.getSnapshot("demo-ui")
    assert.equal(final.status, "completed")
    assert.equal(final.interactions.find((item) => item.commandId === commandId)?.status, "succeeded")
    assert.equal(isDoopsAgentSessionBusy(final), false)
  }
  runtime.close("demo-ui")
})

test("the original release runtime loads read-only and refuses command submission", { timeout: 10_000 }, async (t) => {
  t.mock.method(globalThis, "fetch", () => assert.fail("read-only demo must not reach the Gateway"))
  const { createDoopsAgentUiRuntime } = await import(sourceModule(join(root, "lib/agent-ui-runtime.ts")))
  const runtime = createDoopsAgentUiRuntime({
    session: { gateway: "demo://local", demo: true, username: "synthetic-user" },
    target: { cluster: "test", instance: "node", key: "test/node" },
    sessionId: "deploy-old1",
    readOnly: true,
  })
  try {
    await runtime.open("deploy-old1")
    await new Promise((resolve) => setTimeout(resolve, 50))
    assert.equal(runtime.getSnapshot("deploy-old1").connection, "connected")
    for (const attempt of [
      () => runtime.sendMessage("deploy-old1", "blocked-message", { text: "must not execute" }),
      () => runtime.executeExtension("deploy-old1", "blocked-apply", {
        namespace: "doops.operations", semanticType: "apply", schemaVersion: "1", payload: { input: {} },
      }),
      () => runtime.interrupt("deploy-old1", "blocked-stop"),
    ]) await assert.rejects(attempt, /原发布回执只读/)
  } finally {
    runtime.close("deploy-old1")
  }
})
