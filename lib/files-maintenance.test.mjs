import assert from "node:assert/strict"
import test from "node:test"
import { callTool } from "./client.ts"
import { demoCallTool } from "./demo.ts"
import {
  checkDeploymentRequest,
  cleanWorkspaceRequest,
  EMPTY_MAINTENANCE_LOG,
  isImageReference,
  maintenanceErrorText,
  maintenanceFailure,
  maintenanceOutput,
  maintenanceResultText,
} from "./files-maintenance.ts"

const sessionId = "console-6lacl52e"
const digest = "sha256:" + "a".repeat(64)
const valid = { namespace: "apps", deployment: "api", image: "registry.example.com/doops/api:1.8.2" }

// Mirrors FilesPanel.runMaintenance: every event and the thrown error go through the same reducers.
async function runLikePanel(kind, call) {
  let log = EMPTY_MAINTENANCE_LOG
  try {
    await call((ev) => {
      if (ev.type === "output") log = maintenanceOutput(log, ev.data)
      else if (ev.type === "result") log = maintenanceOutput(log, maintenanceResultText(kind, ev.result.content[0].text))
      else if (ev.type === "error") log = maintenanceFailure(log, kind, ev.error)
    })
  } catch (err) {
    log = maintenanceFailure(log, kind, err.message)
  }
  return log
}

test("check deployment sends namespace, deployment and image with the session, never the session alone", () => {
  for (const [name, input, expected] of [
    ["trimmed fields", { namespace: " apps ", deployment: " api ", image: " nginx:1.27 " }, { namespace: "apps", deployment: "api", image: "nginx:1.27" }],
    ["dotted deployment", { namespace: "kube-system", deployment: "coredns.v2", image: "coredns" }, { namespace: "kube-system", deployment: "coredns.v2", image: "coredns" }],
    ["registry with port", { ...valid, image: "localhost:5000/team/app:v1" }, { ...valid, image: "localhost:5000/team/app:v1" }],
    ["digest", { ...valid, image: `app@${digest}` }, { ...valid, image: `app@${digest}` }],
  ]) {
    const request = checkDeploymentRequest(sessionId, input)
    assert.deepEqual(request, { session_id: sessionId, ...expected }, name)
    assert.deepEqual(Object.keys(request).sort(), ["deployment", "image", "namespace", "session_id"], name)
  }
})

test("check deployment validates every field locally and never defaults the namespace", () => {
  for (const [name, input, message] of [
    ["empty namespace", { ...valid, namespace: "" }, /^请填写命名空间$/],
    ["blank namespace", { ...valid, namespace: "   " }, /^请填写命名空间$/],
    ["uppercase namespace", { ...valid, namespace: "Apps" }, /^命名空间只能包含/],
    ["underscore namespace", { ...valid, namespace: "my_apps" }, /^命名空间只能包含/],
    ["dotted namespace", { ...valid, namespace: "a.b" }, /^命名空间只能包含/],
    ["namespace over 63", { ...valid, namespace: "a".repeat(64) }, /^命名空间只能包含/],
    ["empty deployment", { ...valid, deployment: "" }, /^请填写 Deployment 名称$/],
    ["deployment trailing dash", { ...valid, deployment: "api-" }, /^Deployment 名称只能包含/],
    ["deployment with slash", { ...valid, deployment: "deploy/api" }, /^Deployment 名称只能包含/],
    ["deployment over 253", { ...valid, deployment: "a".repeat(254) }, /^Deployment 名称只能包含/],
    ["empty image", { ...valid, image: "" }, /^请填写期望镜像$/],
    ["image with space", { ...valid, image: "nginx 1.27" }, /^期望镜像格式无效/],
    ["image with shell", { ...valid, image: "nginx;rm -rf /" }, /^期望镜像格式无效/],
    ["uppercase repository", { ...valid, image: "registry.example.com/Team/app:1" }, /^期望镜像格式无效/],
    ["empty tag", { ...valid, image: "nginx:" }, /^期望镜像格式无效/],
    ["short digest", { ...valid, image: "app@sha256:abc" }, /^期望镜像格式无效/],
  ]) {
    assert.throws(() => checkDeploymentRequest(sessionId, input), { message }, name)
  }
})

test("image references follow the registry/path:tag@digest grammar", () => {
  for (const [image, ok] of [
    ["nginx", true],
    ["library/nginx:1.27-alpine", true],
    ["registry.example.com:5000/a/b_c/d__e:TAG.1", true],
    [`registry.example.com/team/app:1.2.3@${digest}`, true],
    ["-nginx", false],
    ["nginx:-bad", false],
    ["nginx@latest", false],
    ["a".repeat(256), false],
  ]) {
    assert.equal(isImageReference(image), ok, image)
  }
})

test("clean workspace sends both session_id and workspace for the current session", () => {
  assert.deepEqual(cleanWorkspaceRequest(sessionId), { session_id: sessionId, workspace: sessionId })
  for (const bad of ["", " console-a", "../console-a", "console/a", "console\\a", ".hidden", "a..b"]) {
    assert.throws(() => cleanWorkspaceRequest(bad), { message: /^当前会话无效，无法清理工作区$/ }, JSON.stringify(bad))
  }
})

test("Agent errors and results are shown in Chinese; unknown text is kept", () => {
  for (const [kind, raw, expected] of [
    ["check", "namespace, deployment and image are required", "命名空间、Deployment 和期望镜像均为必填"],
    ["check", 'kubectl get deploy apps/api failed: exit status 1: Error from server (NotFound): deployments.apps "api" not found',
      '查询 Deployment apps/api 失败（kubectl get deploy）：exit status 1: Error from server (NotFound): deployments.apps "api" not found'],
    ["check", 'image mismatch: want "app:2" got "app:1"', "镜像不一致：期望 app:2，实际 app:1"],
    ["check", "empty image from kubectl for apps/api", "Deployment apps/api 未返回镜像"],
    ["clean", 'unsafe session id: ""', '工作区会话无效：""'],
    ["clean", "Gateway authentication rejected", "Gateway authentication rejected"],
  ]) {
    assert.equal(maintenanceErrorText(kind, raw), expected, raw)
  }
  assert.equal(maintenanceResultText("check", "check ok: apps/api -> app:1"), "检查通过：Deployment apps/api 当前镜像为 app:1")
  assert.equal(maintenanceResultText("clean", `Cleaned workspace and bare repo: ${sessionId}`), `已删除工作区和 Git 裸仓库：${sessionId}`)
})

test("the same failure is recorded once; distinct failures are all kept", () => {
  for (const [name, raws, expected] of [
    ["event then throw", ["image mismatch: want \"a:2\" got \"a:1\"", "image mismatch: want \"a:2\" got \"a:1\""], ["镜像不一致：期望 a:2，实际 a:1"]],
    ["two different", ["连接错误: reset", "连接已结束，未收到工具执行结果"], ["连接错误: reset", "连接已结束，未收到工具执行结果"]],
    ["empty message", [""], ["执行失败"]],
  ]) {
    const log = raws.reduce((acc, raw) => maintenanceFailure(acc, "check", raw), EMPTY_MAINTENANCE_LOG)
    assert.deepEqual(log.errors, expected, name)
  }
})

test("a Gateway tool error reaches the panel log once, with the clean arguments on the wire", async (t) => {
  const bodies = []
  t.mock.method(globalThis, "fetch", async (_url, request) => {
    bodies.push(JSON.parse(request.body))
    return new Response(JSON.stringify({ type: "result", result: { isError: true, content: [{ type: "text", text: 'unsafe session id: ""' }] } }) + "\n")
  })
  const session = { gateway: "https://gateway.example.test", token: "synthetic-token" }
  const log = await runLikePanel("clean", (onEvent) =>
    callTool(session, { cluster: "test", instance: "node", tool: "doops_clean_workspace", arguments: cleanWorkspaceRequest(sessionId) }, onEvent),
  )
  assert.deepEqual(bodies[0].arguments, { session_id: sessionId, workspace: sessionId })
  assert.deepEqual(log.errors, ['工作区会话无效：""'])
})

test("demo mode answers with the Agent contract for both tools", async () => {
  for (const [name, tool, args, lines, errors] of [
    ["check ok", "doops_check_deployment", checkDeploymentRequest(sessionId, valid), ["检查通过：Deployment apps/api 当前镜像为 registry.example.com/doops/api:1.8.2"], []],
    ["check repository only", "doops_check_deployment", checkDeploymentRequest(sessionId, { ...valid, image: "registry.example.com/doops/api" }), ["检查通过：Deployment apps/api 当前镜像为 registry.example.com/doops/api:1.8.2"], []],
    ["check mismatch", "doops_check_deployment", checkDeploymentRequest(sessionId, { ...valid, image: "registry.example.com/doops/api:2.0.0" }), [], ["镜像不一致：期望 registry.example.com/doops/api:2.0.0，实际 registry.example.com/doops/api:1.8.2"]],
    ["check missing", "doops_check_deployment", checkDeploymentRequest(sessionId, { ...valid, deployment: "absent" }), [], ['查询 Deployment apps/absent 失败（kubectl get deploy）：exit status 1: Error from server (NotFound): deployments.apps "absent" not found']],
    ["check session only", "doops_check_deployment", { session_id: sessionId }, [], ["命名空间、Deployment 和期望镜像均为必填"]],
    ["clean ok", "doops_clean_workspace", cleanWorkspaceRequest(sessionId), [`已删除工作区和 Git 裸仓库：${sessionId}`], []],
    ["clean session only", "doops_clean_workspace", { session_id: sessionId }, [], ['工作区会话无效：""']],
  ]) {
    const kind = tool === "doops_check_deployment" ? "check" : "clean"
    const log = await runLikePanel(kind, (onEvent) => demoCallTool({ tool, arguments: args }, onEvent))
    assert.deepEqual(log, { lines, errors }, name)
  }
})
