import assert from "node:assert/strict"
import test from "node:test"
import * as adminViews from "./admin-views.ts"
import * as grantForm from "./grant-form.ts"
import * as proxyRoutes from "./proxy-routes.ts"
import { mapWithConcurrency } from "./console-refresh.ts"
import { buttonNamed, componentHarness, componentStubs, elementText, findElements } from "./component-test-harness.mjs"
import {
  ACTIVE_INSTANCE_WINDOW_DAYS,
  TOKEN_GROUP_COLLAPSE_AT,
  commandPreview,
  compareInstances,
  gatewayTime,
  groupInstances,
  groupTokensByUser,
  instanceActivity,
  instanceClusterOptions,
  isTokenGroupCollapsed,
  offlineLabel,
  sortUsersForDisplay,
  summarizeInstanceScope,
  summarizeUsers,
  tokenState,
} from "./admin-views.ts"

const DAY = 24 * 60 * 60 * 1000
const NOW = Date.parse("2026-11-01T12:00:00Z")
const ago = (ms) => new Date(NOW - ms).toISOString()
const ZERO_TIME = "0001-01-01T00:00:00Z"

function pendingMutation() {
  const requests = []
  return {
    requests,
    run: (...args) => new Promise((resolve, reject) => requests.push({ args, resolve, reject })),
  }
}

async function checkMutationLock(view, action, mutation) {
  for (const failure of [true, false]) {
    const before = mutation.requests.length
    const button = action()
    const first = button.props.onClick()
    await button.props.onClick()
    await view.flush()
    assert.equal(mutation.requests.length, before + 1, "same-render double click sends once")
    view.render()
    assert.equal(action().props.disabled, true, "pending action is visibly disabled")
    await action().props.onClick()
    assert.equal(mutation.requests.length, before + 1, "re-render does not release the lock")
    if (failure) mutation.requests.at(-1).reject(new Error("synthetic mutation failure"))
    else mutation.requests.at(-1).resolve()
    await first
    await view.flush()
    assert.equal(action().props.disabled, false, "success and failure both release the lock")
  }
}

for (const [name, action, operation] of [
  ["GrantsView", "删除", "deleteGrant"],
  ["TokensView", "撤销", "revokeToken"],
  ["InstancesView", "归档", "archiveInstance"],
  ["InstancesView", "取消归档", "unarchiveInstance"],
]) {
  test(`${name} ${action}: pending double clicks send one mutation and failure unlocks`, async () => {
    const mutation = pendingMutation()
    const view = componentHarness("../components/admin-console.tsx", name, { session: {} }, {
      "./ui": componentStubs,
      "./icons": componentStubs,
      "./admin-config-dispatch": componentStubs,
      "./proxy-routes-panel": componentStubs,
      "./app-dialog": { useAppDialog: () => ({}) },
      "@/lib/admin-views": adminViews,
      "@/lib/grant-form": grantForm,
      "@/lib/admin": {
        listInstances: async () => [{ ...instance("test", "node", "offline", new Date().toISOString()), archived: action === "取消归档" }],
        listUsers: async () => [{ id: "user-1", name: "synthetic-user", disabled: false }],
        listGrants: async () => [{ id: 1, user_id: "user-1", cluster: "test", instance: "node" }],
        listTokens: async () => [{ id: "token-1", user_id: "user-1", revoked: false }],
        [operation]: mutation.run,
      },
    })
    await view.flush()
    if (action === "取消归档") {
      view.one((node) => node.type === "button" && elementText(node).includes("显示已归档实例")).props.onClick()
      await view.flush()
    }
    await checkMutationLock(view, () => view.one((node) => buttonNamed(action)(node) || buttonNamed("处理中…")(node)), mutation)
  })
}

for (const action of ["激活", "激活版本", "吊销凭据", "解除绑定"]) {
  test(`CredentialPanel ${action}: pending double clicks send once and failure unlocks`, async () => {
    const mutation = pendingMutation()
    const resource = { id: "credential-1", name: "synthetic-registry", type: "registry", scope: "personal", state: "active" }
    const binding = { id: "binding-1", state: "active", scope: { cluster: "test", instance: "node", use: "imagePull" } }
    const view = componentHarness("../components/credential-panel.tsx", "CredentialPanel", {
      session: {}, target: { cluster: "test", instance: "node", key: "test/node" },
    }, {
      "./ui": componentStubs,
      "./icons": componentStubs,
      "./credential-secret-import": componentStubs,
      "./credential-bundle-dialog": componentStubs,
      "@/lib/console-refresh": { mapWithConcurrency },
      "@/lib/gateway": {},
      "@/lib/credentials": {
        listCredentials: async () => [resource],
        listCredentialVersions: async () => [{ id: "version-1", state: "staged" }],
        listCredentialBindings: async () => [binding],
        canImportFromSecret: () => false,
        promoteCredentialVersion: mutation.run,
        revokeCredential: mutation.run,
        revokeCredentialBinding: mutation.run,
      },
    })
    await view.flush()
    view.one((node) => node.type === "SegmentedControl").props.onChange("managed")
    await view.flush()
    const locate = () => {
      if (action === "解除绑定") {
        const summary = view.one((node) => node.type.name === "BindingSummary")
        return findElements(summary.type(summary.props), buttonNamed(action))[0]
      }
      if (action === "激活版本") {
        // A successful promote closes the version dialog.
        if (!view.nodes((node) => node.type.name === "VersionDialog").length) {
          view.one(buttonNamed("查看版本")).props.onClick()
          view.render()
        }
        const dialog = view.one((node) => node.type.name === "VersionDialog")
        return findElements(dialog.type(dialog.props), buttonNamed(action))[0]
      }
      return view.one(buttonNamed(action))
    }
    await checkMutationLock(view, locate, mutation)
  })
}

for (const action of ["删除", "停用"]) {
  test(`ProxyRoutesPanel ${action}: route checks and mutation stay under one synchronous lock`, async () => {
    const mutation = pendingMutation()
    const route = { id: "route-1", host: "app.example.test", cluster: "test", instance: "node", upstream: "http://127.0.0.1:8080/", enabled: true }
    const view = componentHarness("../components/proxy-routes-panel.tsx", "ProxyRoutesPanel", { session: {} }, {
      "./ui": componentStubs,
      "./icons": componentStubs,
      "@/lib/proxy-routes": proxyRoutes,
      "@/lib/admin": {
        listInstances: async () => [],
        listProxyRoutes: async () => [route],
        deleteProxyRoute: mutation.run,
        setProxyRoute: async (...args) => { await mutation.run(...args); return route },
      },
    })
    await view.flush()
    await checkMutationLock(view, () => view.one(buttonNamed(action)), mutation)
  })
}

test("AdminConfigDispatch publishes once while pending and releases the lock on node failure", async () => {
  const mutation = pendingMutation()
  const cfg = { id: "config-1", kind: "file", name: "synthetic", path: "/tmp/synthetic.txt", content: "synthetic" }
  const view = componentHarness("../components/admin-config-dispatch.tsx", "AdminConfigDispatch", { session: {} }, {
    "./ui": componentStubs,
    "./icons": componentStubs,
    "@/lib/admin-views": adminViews,
    "@/lib/admin": { listInstances: async () => [instance("test", "node", "online", new Date().toISOString())] },
    "@/lib/gateway": { randomSession: () => "admin-cfg-test" },
    "@/lib/model-configs": {
      loadConfigs: () => [cfg],
      markPublished: () => [cfg],
      configKind: (config) => config.kind,
      configTargetPath: (config) => config.path,
      hasUnpublishedChanges: () => true,
      applyConfigToNode: mutation.run,
    },
  })
  await view.flush()
  view.one((node) => node.type === "Select").props.onChange({ target: { value: cfg.id } })
  view.one((node) => node.type === "input" && node.props.type === "checkbox").props.onChange()
  await view.flush()
  await checkMutationLock(view, () => view.one((node) => buttonNamed("发布上线 (1)")(node) || buttonNamed("发布中…")(node)), mutation)
})

test("Agent path loader gates directory access on target configuration", async () => {
  const { parseAgentPaths } = await import("./agent-paths.ts")
  for (const scenario of [
    { text: '{"workspace_root":"/home/ops/ws","kb_root":"/home/ops/kb"}', expected: "/home/ops/kb" },
    { text: "{}", expected: "/root/kb" },
    { text: "", error: true },
    { error: true, failure: "Agent offline" },
    { failure: "unsupported node info section", code: -32602, expected: "/root/kb" },
    { error: true, failure: "unsupported node info section", code: -32603 },
    { error: true, failure: "unsupported node info section: unavailable", code: -32602 },
  ]) {
    const calls = []
    const view = componentHarness("../components/agent-paths.tsx", "AgentPathsLoader", {
      session: {}, target: { cluster: "test", instance: "node" }, sessionId: "test-paths", children: (paths) => ({ type: "paths", props: paths }),
    }, {
      "@/lib/gateway": { TOOLS: { nodeInfo: "doops_node_info" } },
      "@/lib/agent-paths": { parseAgentPaths },
      "@/lib/client": {
        extractText: (result) => result,
        callTool: async (_session, request, emit) => {
          calls.push(request)
          if (scenario.failure) throw Object.assign(new Error(scenario.failure), { code: scenario.code })
          emit({ type: "result", result: scenario.text })
        },
      },
    })
    assert.match(elementText(view.render()), /正在读取/)
    await view.flush()
    const result = view.render()
    assert.equal(calls.length, 1)
    assert.equal(calls[0].arguments.section, "paths")
    if (scenario.error) assert.equal(result.props.role, "alert")
    else {
      assert.equal(result.props.kb_root, scenario.expected)
      assert.equal(result.props.workspace_root, scenario.expected === "/root/kb" ? "/root/ws" : "/home/ops/ws")
    }
  }
})

test("KnowledgeBase deletes once while pending and switches unsaved drafts without confirmation", async () => {
  const mutation = pendingMutation()
  const writes = []
  const reads = []
  const view = componentHarness("../components/knowledge-base.tsx", "KnowledgeBaseContent", {
    session: {}, target: { cluster: "test", instance: "node" }, sessionId: "test-kb", KB_ROOT: "/home/ops/knowledge",
  }, {
    "./agent-paths": componentStubs,
    "./ui": componentStubs,
    "./icons": componentStubs,
    "./markdown": componentStubs,
    "@/lib/gateway": { TOOLS: { shell: "shell", fileRead: "read", fileWrite: "write" } },
    "@/lib/client": {
      extractText: (result) => result,
      callTool: async (_session, request, emit) => {
        if (request.arguments.command?.startsWith("rm -f")) return mutation.run()
        if (request.tool === "write") {
          writes.push(request.arguments)
          emit({ type: "result", result: "saved" })
        } else if (request.tool === "read") {
          reads.push(request.arguments.path)
          emit({ type: "result", result: `stored ${request.arguments.path}` })
        } else emit({ type: "output", data: "a.md\nb.md\n" })
      },
    },
  })
  await view.flush()
  await checkMutationLock(view, () => view.nodes(buttonNamed("删除"))[0], mutation)
  await view.one(buttonNamed("a")).props.onClick()
  await view.flush()
  view.one((node) => node.type === "textarea").props.onChange({ target: { value: "unsaved synthetic draft" } })
  await view.flush()
  await view.one(buttonNamed("b")).props.onClick()
  await view.flush()
  await view.one(buttonNamed("a")).props.onClick()
  await view.flush()
  assert.equal(view.one((node) => node.type === "textarea").props.value, "unsaved synthetic draft")
  assert.equal(writes.length, 0, "document switching never auto-saves")
  assert.equal(reads.length, 2, "restoring a draft does not overwrite it with a remote read")
  assert.ok(reads.every((path) => path.startsWith("/home/ops/knowledge/")))
  await view.one(buttonNamed("保存")).props.onClick()
  await view.flush()
  assert.equal(writes.length, 1)
  assert.equal(writes[0].content, "unsaved synthetic draft")
  assert.equal(writes[0].path, "/home/ops/knowledge/a.md")
  await view.one(buttonNamed("b")).props.onClick()
  await view.flush()
  await view.one(buttonNamed("a")).props.onClick()
  await view.flush()
  assert.match(view.one((node) => node.type === "textarea").props.value, /^stored /, "successful save clears the draft")
})

function instance(cluster, name, status, lastSeen) {
  return { cluster, instance: name, status, busy: false, active_ops: 0, queued_ops: 0, last_seen: lastSeen }
}

test("gateway times treat empty, zero and malformed values as missing", () => {
  for (const [value, expected] of [
    [undefined, null],
    ["", null],
    [ZERO_TIME, null],
    ["not-a-time", null],
    ["2026-11-01T12:00:00Z", NOW],
  ]) {
    assert.equal(gatewayTime(value), expected, String(value))
  }
})

test("instance activity uses a 7-day offline window, inclusive", () => {
  assert.equal(ACTIVE_INSTANCE_WINDOW_DAYS, 7)
  for (const [name, row, activity, label] of [
    ["online without heartbeat", instance("c", "a", "online", ZERO_TIME), "online", ""],
    ["offline 1 hour", instance("c", "a", "offline", ago(3600_000)), "recent", "离线不到 1 天"],
    ["offline 6 days", instance("c", "a", "offline", ago(6 * DAY)), "recent", "离线 6 天"],
    ["offline exactly 7 days", instance("c", "a", "offline", ago(7 * DAY)), "recent", "离线 7 天"],
    ["offline 7 days + 1 ms", instance("c", "a", "offline", ago(7 * DAY + 1)), "history", "离线 7 天"],
    ["offline 100 days", instance("c", "a", "offline", ago(100 * DAY)), "history", "离线 100 天"],
    ["offline without last_seen", instance("c", "a", "offline", undefined), "history", "离线时间未知"],
    ["offline with zero last_seen", instance("c", "a", "offline", ZERO_TIME), "history", "离线时间未知"],
    ["heartbeat slightly in the future", instance("c", "a", "offline", ago(-5000)), "recent", "离线不到 1 天"],
  ]) {
    assert.equal(instanceActivity(row, NOW), activity, name)
    assert.equal(offlineLabel(row, NOW), label, name)
  }
})

test("instances sort online first, then by last_seen descending, missing heartbeats last", () => {
  const rows = [
    instance("b", "missing", "offline", undefined),
    instance("a", "old", "offline", ago(40 * DAY)),
    instance("z", "online-old", "online", ago(2 * DAY)),
    instance("a", "fresh", "offline", ago(1 * DAY)),
    instance("a", "online-new", "online", ago(0)),
    instance("a", "missing", "offline", ZERO_TIME),
  ]
  assert.deepEqual(
    [...rows].sort(compareInstances).map((row) => `${row.cluster}/${row.instance}`),
    ["a/online-new", "z/online-old", "a/fresh", "a/old", "a/missing", "b/missing"],
  )
})

test("instances split into active and history groups, optionally filtered by cluster", () => {
  const rows = [
    instance("prod", "web-01", "online", ago(0)),
    instance("prod", "web-02", "offline", ago(3 * DAY)),
    instance("prod", "web-03", "offline", ago(30 * DAY)),
    instance("proxy-a-20260913", "p1", "offline", ago(50 * DAY)),
    instance("probe", "x", "offline", undefined),
  ]
  for (const [cluster, active, history] of [
    ["", ["web-01", "web-02"], ["web-03", "p1", "x"]],
    ["prod", ["web-01", "web-02"], ["web-03"]],
    ["proxy-a-20260913", [], ["p1"]],
    ["missing", [], []],
  ]) {
    const groups = groupInstances(rows, NOW, cluster)
    assert.deepEqual(groups.active.map((row) => row.instance), active, `cluster=${cluster}`)
    assert.deepEqual(groups.history.map((row) => row.instance), history, `cluster=${cluster}`)
  }
  assert.deepEqual(summarizeInstanceScope(rows, NOW), { active: 2, online: 1, history: 3, archived: 0 })
  assert.deepEqual(summarizeInstanceScope([], NOW), { active: 0, online: 0, history: 0, archived: 0 })
  assert.deepEqual(instanceClusterOptions(rows), [
    { name: "probe", online: 0, total: 1 },
    { name: "prod", online: 1, total: 3 },
    { name: "proxy-a-20260913", online: 0, total: 1 },
  ])
})

const USERS = [
  { id: "u_ops", name: "ops", disabled: false, has_password: true, grant_count: 1, is_admin: false },
  { id: "u_off", name: "former", disabled: true, has_password: true, grant_count: 0, is_admin: false },
  { id: "u_admin", name: "admin", disabled: false, has_password: true, grant_count: 0, is_admin: true },
]

function token(id, userId, extra = {}) {
  return { id, kind: "user", user_id: userId, name: id, prefix: `dgw_${id}`, revoked: false, created_at: ago(DAY), ...extra }
}

test("token state matches the Gateway token check", () => {
  const ops = USERS[0]
  const former = USERS[1]
  for (const [name, row, user, state] of [
    ["usable, no expiry", token("t1", "u_ops"), ops, "active"],
    ["usable, zero expiry means no expiry", token("t1", "u_ops", { expires_at: ZERO_TIME }), ops, "active"],
    ["expires later", token("t1", "u_ops", { expires_at: ago(-DAY) }), ops, "active"],
    ["expires exactly now", token("t1", "u_ops", { expires_at: ago(0) }), ops, "expired"],
    ["expired", token("t1", "u_ops", { expires_at: ago(DAY) }), ops, "expired"],
    ["revoked", token("t1", "u_ops", { revoked: true }), ops, "revoked"],
    ["disabled user", token("t1", "u_off"), former, "user_disabled"],
    ["disabled user and revoked", token("t1", "u_off", { revoked: true }), former, "revoked"],
    ["disabled user and expired", token("t1", "u_off", { expires_at: ago(DAY) }), former, "user_disabled"],
    ["deleted user", token("t1", "u_gone"), undefined, "user_missing"],
  ]) {
    assert.equal(tokenState(row, user, NOW), state, name)
  }
})

test("tokens group by user with counts, hidden revoked tokens and disabled users last", () => {
  const tokens = [
    token("a1", "u_ops", { created_at: ago(3 * DAY) }),
    token("a2", "u_ops", { created_at: ago(1 * DAY) }),
    token("a3", "u_ops", { revoked: true, created_at: ago(5 * DAY) }),
    token("b1", "u_off"),
    token("c1", "u_admin", { revoked: true }),
    token("d1", "u_gone", { user_name: "ghost" }),
  ]
  for (const [showRevoked, expected] of [
    [false, [
      { userName: "ghost", disabled: false, total: 1, usable: 0, revoked: 0, rows: ["d1:user_missing"] },
      { userName: "ops", disabled: false, total: 3, usable: 2, revoked: 1, rows: ["a2:active", "a1:active"] },
      { userName: "former", disabled: true, total: 1, usable: 0, revoked: 0, rows: ["b1:user_disabled"] },
    ]],
    [true, [
      { userName: "admin", disabled: false, total: 1, usable: 0, revoked: 1, rows: ["c1:revoked"] },
      { userName: "ghost", disabled: false, total: 1, usable: 0, revoked: 0, rows: ["d1:user_missing"] },
      { userName: "ops", disabled: false, total: 3, usable: 2, revoked: 1, rows: ["a2:active", "a1:active", "a3:revoked"] },
      { userName: "former", disabled: true, total: 1, usable: 0, revoked: 0, rows: ["b1:user_disabled"] },
    ]],
  ]) {
    const groups = groupTokensByUser(tokens, USERS, { now: NOW, showRevoked })
    assert.deepEqual(groups.map((group) => ({
      userName: group.userName,
      disabled: group.userDisabled,
      total: group.total,
      usable: group.usable,
      revoked: group.revoked,
      rows: group.rows.map((row) => `${row.token.id}:${row.state}`),
    })), expected, `showRevoked=${showRevoked}`)
  }
  assert.deepEqual(groupTokensByUser([], USERS, { now: NOW, showRevoked: false }), [])
})

test("large token groups start collapsed and a toggle flips the default", () => {
  const many = Array.from({ length: TOKEN_GROUP_COLLAPSE_AT + 1 }, (_, i) => token(`m${i}`, "u_ops"))
  const few = Array.from({ length: TOKEN_GROUP_COLLAPSE_AT }, (_, i) => token(`f${i}`, "u_admin"))
  const groups = groupTokensByUser([...many, ...few], USERS, { now: NOW, showRevoked: false })
  const ops = groups.find((group) => group.key === "u_ops")
  const admin = groups.find((group) => group.key === "u_admin")
  for (const [group, toggled, collapsed] of [
    [ops, new Set(), true],
    [ops, new Set(["u_ops"]), false],
    [admin, new Set(), false],
    [admin, new Set(["u_admin"]), true],
    [admin, new Set(["u_ops"]), false],
  ]) {
    assert.equal(isTokenGroupCollapsed(group, toggled), collapsed, `${group.key} ${[...toggled]}`)
  }
})

test("disabled users sort last without reordering the rest, and admins exclude disabled users", () => {
  const users = [
    { id: "1", name: "first", disabled: true, is_admin: false },
    { id: "2", name: "second", disabled: false, is_admin: true },
    { id: "3", name: "third", disabled: true, is_admin: true },
    { id: "4", name: "fourth", disabled: false, is_admin: false },
  ]
  assert.deepEqual(sortUsersForDisplay(users).map((user) => user.name), ["second", "fourth", "first", "third"])
  assert.deepEqual(users.map((user) => user.name), ["first", "second", "third", "fourth"])
  assert.deepEqual(summarizeUsers(users), { total: 4, admins: 1, disabled: 2 })
  assert.deepEqual(summarizeUsers([]), { total: 0, admins: 0, disabled: 0 })
})

test("command previews stay on one line and never spill long or base64 content", () => {
  const base64 = "QWxhZGRpbjpvcGVuIHNlc2FtZQ0KZXhhbXBsZS1jb25maWctcGF5bG9hZA=="
  const hex = "sha256:" + "a1b2c3d4".repeat(8)
  for (const [name, value, text, shortened] of [
    ["empty", undefined, "", false],
    ["short", "systemctl status doops-app", "systemctl status doops-app", false],
    ["multi-line", "kubectl get pods\n  -n apps\t--watch", "kubectl get pods -n apps --watch", true],
    ["base64", `echo ${base64} | base64 -d`, `echo ‹base64 ${base64.length} 字符› | base64 -d`, true],
    ["hex digest", hex, hex, false],
    ["long", "x".repeat(200), `${"x".repeat(119)}…`, true],
    ["long chinese", "部署".repeat(100), `${"部署".repeat(59)}部…`, true],
  ]) {
    const preview = commandPreview(value)
    assert.equal(preview.text, text, name)
    assert.equal(preview.shortened, shortened, name)
    assert.equal(preview.text.includes("\n"), false, name)
    assert.equal(preview.full, (value || "").trim(), name)
  }
  const huge = commandPreview("y".repeat(5000))
  assert.equal(huge.title, `${"y".repeat(1000)}…（点击展开完整内容）`)
  assert.equal(commandPreview("ls -la").title, "ls -la")
})
