import assert from "node:assert/strict"
import test from "node:test"
import { consoleViewEntries, credentialsDestination } from "./console-views.ts"
import { readFileSync } from "node:fs"
import TypeScript from "typescript"
import { workspaceRootPath } from "./workspace-path.ts"
import { componentHarness, componentStubs } from "./component-test-harness.mjs"

test("console session drafts do not remount Files or construct invalid workspaces", async (t) => {
  const session = { gateway: "http://gateway.test" }
  const target = { cluster: "test", instance: "node", key: "test/node" }
  const workspaces = []
  const runtimes = []
  const paths = { workspace_root: "/home/ops/ws", kb_root: "/home/ops/kb" }
  let pathState = null
  const noop = () => {}
  t.mock.method(globalThis, "setInterval", () => 0)
  const dependencies = new Proxy({
    "next/link": "a",
    "next/dynamic": () => "AgentUiPanel",
    "@/lib/client": { restoreServerSession: async () => session, fetchTargets: async () => [target], sessionIsAdmin: () => false },
    "@/lib/gateway": { randomSession: () => "initial", resolveDefaultGateway: () => "" },
    "@/lib/demo-policy": { isDemoAllowed: () => false },
    "@/lib/console-refresh": { errorAfterRefresh: () => "", reconcileTargets: (_, next) => next, reconcileSelected: (current) => current },
    "@/lib/console-persistence": {
      browserSessionStorage: () => null, consoleScopeKey: () => "scope", loadSelection: () => null,
      restoreSelection: () => ({ selected: target }), saveSelection: noop,
      commandJournal: { takeInterrupted: () => [] },
    },
    "@/lib/network-error": { subscribeNetworkStatus: () => noop },
    "@/lib/unload-journal": { installJournalUnloadHandlers: () => noop },
    "@/lib/interrupted-banner": { interruptedBannerView: () => null },
    "@/lib/console-views": { consoleViewEntries, credentialsDestination },
    "@/lib/session-display": { displayUserName: () => "test" },
    "@/lib/workspace-path": { workspaceRootPath },
    "./agent-paths": { useAgentPaths: () => pathState },
    "@/lib/agent-code-workspace": { createDoopsCodeWorkspace: (options) => {
      assert.ok(workspaceRootPath(options.sessionId), "invalid drafts must not reach workspace construction")
      workspaces.push(options)
      return { workspaceRoot: workspaceRootPath(options.sessionId, options.workspaceRoot) }
    } },
    "@/lib/agent-ui-runtime": { createDoopsAgentUiRuntime: (options) => { runtimes.push(options); return {} } },
  }, { has: () => true, get: (object, key) => object[key] ?? componentStubs })
  const view = componentHarness("../components/console-shell.tsx", "ConsoleShell", {}, dependencies, {
    location: { origin: "http://console.test", search: "" },
    localStorage: { getItem: () => null, setItem: noop, removeItem: noop },
  })
  await view.flush()
  view.one((node) => node.type === "TargetToolsNav").props.onChange("ask")
  await view.flush()
  assert.equal(view.one((node) => node.props?.role === "status").props.children, "正在读取 Agent 目录配置…")
  assert.equal(workspaces.length, 0)
  pathState = { error: "synthetic path lookup failure", retry: () => { pathState = { paths } } }
  await view.flush()
  assert.equal(view.one((node) => node.props?.role === "alert").props.children, pathState.error)
  view.one((node) => node.type === "Button" && node.props.children === "重试").props.onClick()
  await view.flush()
  assert.equal(workspaces.length, 1, "retry restores the AI workspace")
  assert.equal(view.nodes((node) => node.props?.role === "alert").length, 0)
  view.one((node) => node.type === "TargetToolsNav").props.onChange("files")
  await view.flush()
  const files = () => view.one((node) => node.type === "FilesPanel")
  const key = files().key
  for (const value of ["", "../bad", "has spaces", "c", "custom-roots-e2e"]) {
    const before = workspaces.length
    files().props.onSessionChange(value)
    await view.flush()
    assert.equal(files().key, key, "Files must not remount on session edits")
    assert.equal(files().props.sessionDraft, value)
    assert.equal(workspaces.length, before, "typing does not replace the committed session")
    files().props.onSessionCommit(value)
    await view.flush()
    if (!workspaceRootPath(value)) assert.equal(workspaces.length, before)
    else assert.equal(files().props.sessionId, value)
  }
  assert.ok(workspaces.every((options) => options.workspaceRoot === "/home/ops/ws"))
  assert.equal(runtimes.at(-1).codeWorkspace.workspaceRoot, "/home/ops/ws/custom-roots-e2e")
})

test("code workspace uses the reported Agent workspace root", () => {
  const code = TypeScript.transpile(readFileSync(new URL("./agent-code-workspace.ts", import.meta.url), "utf8"), {
    module: TypeScript.ModuleKind.CommonJS, target: TypeScript.ScriptTarget.ES2022,
  })
  const create = new Function("require", "exports", `${code}\nreturn createDoopsCodeWorkspace`)(
    (name) => name === "./workspace-path" ? { workspaceRootPath } : {}, {},
  )
  const source = create({ session: {}, target: { cluster: "test", instance: "node" }, sessionId: "custom", workspaceRoot: "/home/ops/ws" })
  assert.equal(source.workspaceRoot, "/home/ops/ws/custom")
  assert.equal(source.workspaceRootForSession("custom"), "/home/ops/ws/custom")
})

test("Given the account role, When the header lists views, Then admin appears only for admins and the order is fixed", () => {
  for (const [name, isAdmin, want] of [
    ["admin", true, ["console", "assets", "deploy", "admin"]],
    ["ordinary user", false, ["console", "assets", "deploy"]],
  ]) {
    assert.deepEqual(consoleViewEntries(isAdmin).map((entry) => entry.id), want, name)
  }
  assert.deepEqual(consoleViewEntries(true).map((entry) => entry.label), ["目标运维", "资产", "发布部署", "平台管理"])
})

// The AI assistant's and the release page's "打开凭据" both lead here. DoAgent
// models come only from modelProvider credentials, so it never leads to the
// admin config center.
test("Given the account role, When a page asks to open credentials, Then admins land on the console credentials tab", () => {
  for (const [name, isAdmin, want] of [
    ["admin", true, { view: "console", tab: "credentials" }],
    ["ordinary user", false, null],
  ]) {
    assert.deepEqual(credentialsDestination(isAdmin), want, name)
  }
  assert.notEqual(credentialsDestination(true)?.view, "admin")
})
