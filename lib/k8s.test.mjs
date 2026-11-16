import assert from "node:assert/strict"
import test from "node:test"
import * as k8s from "./k8s.ts"
import { buttonNamed, componentHarness, componentStubs, elementText, findElements } from "./component-test-harness.mjs"
import {
  EMPTY_K8S_INPUT,
  EMPTY_K8S_LOG,
  K8S_OPERATIONS,
  K8S_OUTPUT_HINTS,
  K8S_TOOL,
  appendK8sLog,
  createK8sFormStore,
  describeK8sFailure,
  isK8sMutation,
  k8sFields,
  k8sFormFor,
  k8sRequest,
  k8sResultHint,
  runK8s,
} from "./k8s.ts"

const session = "web-k8s"
const form = (patch) => ({ ...EMPTY_K8S_INPUT, ...patch })

// Each row mirrors a `doops k8s` invocation and the payload buildK8SRequest produces for it.
const mappings = [
  ["get pods -n apps -l app=api -o wide", { operation: "get", kind: "Pods", namespace: "apps", selector: "app=api", output: "wide" },
    { operation: "get", kind: "pods", namespace: "apps", selector: "app=api", output: "wide" }],
  ["get deploy -A -l app=api", { operation: "get", kind: "deploy", selector: "app=api", allNamespaces: true },
    { operation: "get", kind: "deploy", selector: "app=api", all_namespaces: true }],
  ["describe deployment api -n apps", { operation: "describe", kind: "deployment", name: "api", namespace: "apps" },
    { operation: "describe", kind: "deployment", name: "api", namespace: "apps" }],
  ["logs pod/api-0 -c app --tail 200 -p --since 10m", { operation: "logs", resource: "Pod/api-0", namespace: "apps", container: "app", tail: "200", previous: true, since: "10m" },
    { operation: "logs", resource: "pod/api-0", namespace: "apps", container: "app", tail: 200, previous: true, since: "10m" }],
  ["events -n apps", { operation: "events", namespace: "apps" },
    { operation: "events", namespace: "apps" }],
  ["events -n apps --for deployment/api", { operation: "events", namespace: "apps", resource: "deployment/api" },
    { operation: "events", namespace: "apps", resource: "deployment/api" }],
  ["top nodes", { operation: "top", kind: "nodes" },
    { operation: "top", kind: "nodes" }],
  ["rollout status deployment/api --timeout 120s", { operation: "rollout-status", resource: "deployment/api", namespace: "apps", timeout: "120s" },
    { operation: "rollout-status", resource: "deployment/api", namespace: "apps", timeout: "120s" }],
  ["helm list -A", { operation: "helm-list", allNamespaces: true },
    { operation: "helm-list", all_namespaces: true }],
  ["helm history app -n apps", { operation: "helm-history", name: "app", namespace: "apps" },
    { operation: "helm-history", name: "app", namespace: "apps" }],
  ["helm status app -n apps", { operation: "helm-status", name: "app", namespace: "apps" },
    { operation: "helm-status", name: "app", namespace: "apps" }],
  ["helm values app -n apps", { operation: "helm-values", name: "app", namespace: "apps" },
    { operation: "helm-values", name: "app", namespace: "apps" }],
  ["rollout restart deployment/api", { operation: "rollout-restart", resource: "deployment/api", namespace: "apps" },
    { operation: "rollout-restart", resource: "deployment/api", namespace: "apps" }],
  ["scale deployment/api --replicas 0", { operation: "scale", resource: "deployment/api", namespace: "apps", replicas: "0" },
    { operation: "scale", resource: "deployment/api", namespace: "apps", replicas: 0 }],
  ["node cordon node-1", { operation: "cordon", name: "node-1" },
    { operation: "cordon", name: "node-1" }],
  ["node uncordon node-1", { operation: "uncordon", name: "node-1" },
    { operation: "uncordon", name: "node-1" }],
  ["delete pod api-0", { operation: "delete-pod", name: "api-0", namespace: "apps" },
    { operation: "delete-pod", name: "api-0", namespace: "apps" }],
]

for (const [cli, input, payload] of mappings) {
  test(`form maps to doops_k8s payload: ${cli}`, () => {
    assert.deepEqual(k8sRequest(session, form(input)), { session_id: session, ...payload })
  })
}

// Agent operations without a `doops k8s` subcommand. Rows mirror the buildK8SCommands
// cases in backend/agent/internal/server/k8s_configmap_keys_test.go.
const agentOnly = [
  ["configmap-keys in one namespace", { operation: "configmap-keys", namespace: "apps" },
    { operation: "configmap-keys", namespace: "apps" }],
  ["configmap-keys across all namespaces", { operation: "configmap-keys", allNamespaces: true },
    { operation: "configmap-keys", all_namespaces: true }],
  ["configmap-keys of one object", { operation: "configmap-keys", namespace: "apps", name: "orders-config" },
    { operation: "configmap-keys", namespace: "apps", name: "orders-config" }],
  ["configmap-keys by selector", { operation: "configmap-keys", namespace: "apps", selector: "app=orders" },
    { operation: "configmap-keys", namespace: "apps", selector: "app=orders" }],
  ["configmap-keys in the agent default namespace", { operation: "configmap-keys" },
    { operation: "configmap-keys" }],
  ["configmap-keys never sends fields it does not show", { operation: "configmap-keys", namespace: "apps", kind: "secrets", output: "json", resource: "pod/x", tail: "9" },
    { operation: "configmap-keys", namespace: "apps" }],
]

for (const [name, input, payload] of agentOnly) {
  test(`form maps to doops_k8s payload: ${name}`, () => {
    assert.deepEqual(k8sRequest(session, form(input)), { session_id: session, ...payload })
  })
}

const configMapKeysRejects = [
  ["named across all namespaces", { operation: "configmap-keys", name: "orders-config", allNamespaces: true }, /全部命名空间/],
  ["named with a selector the agent would drop", { operation: "configmap-keys", namespace: "apps", name: "orders-config", selector: "app=orders" }, /选择器/],
  ["namespace and all namespaces", { operation: "configmap-keys", namespace: "apps", allNamespaces: true }, /只能选一个/],
  ["invalid name", { operation: "configmap-keys", namespace: "apps", name: "x --kubeconfig=/y" }, /名称/],
  ["invalid selector", { operation: "configmap-keys", namespace: "apps", selector: "app=$(id)" }, /选择器/],
]

for (const [name, input, message] of configMapKeysRejects) {
  test(`configmap-keys input is rejected before sending: ${name}`, async () => {
    let sent = 0
    await assert.rejects(runK8s(async () => { sent += 1 }, { cluster: "test", instance: "node" }, session, form(input)), message)
    assert.equal(sent, 0)
  })
}

const readOnly = [
  ["configmap-keys", "ConfigMap 键名", ["name", "namespace", "allNamespaces", "selector"]],
  ["helm-status", "Helm 状态", ["name", "namespace"]],
  ["helm-values", "Helm values 键路径", ["name", "namespace"]],
]

for (const [operation, label, fields] of readOnly) {
  test(`read operation is not a mutation and needs no confirm: ${operation}`, () => {
    assert.equal(isK8sMutation(operation), false)
    assert.equal(K8S_OPERATIONS[operation].label, label)
    assert.deepEqual(k8sFields(form({ operation })), fields)
    const payload = k8sRequest(session, form({ operation, name: "app", namespace: "apps" }))
    assert.equal("confirm" in payload, false)
  })
}

// Agents before v1.3.119 (3c53dbd) return the whole helm status -o json, so the
// hint must not promise that values and the manifest are left out.
const outputHints = [
  ["helm-status", "v1.3.119 及以上的 Agent 只返回发布名、修订、状态和时间；更早的 Agent 会返回完整 release，其中可能有 values 里的敏感值。"],
  ["helm-values", "只返回 values 的键路径，不返回值。"],
  ["configmap-keys", "只返回键名，不返回值。"],
  ["get", undefined],
]

for (const [operation, hint] of outputHints) {
  test(`output hint says what the Agent returns: ${operation}`, () => {
    assert.equal(K8S_OUTPUT_HINTS[operation], hint)
  })
}

test("helm-status hint makes no absolute promise about values and manifest", () => {
  assert.equal(K8S_OUTPUT_HINTS["helm-status"].includes("不返回 values 和 manifest"), false)
})

test("every operation is covered and mutations retain their state-change classification", () => {
  assert.deepEqual(new Set([...mappings, ...agentOnly].map(([, input]) => input.operation)), new Set(Object.keys(K8S_OPERATIONS)))
  const mutations = Object.keys(K8S_OPERATIONS).filter(isK8sMutation).sort()
  assert.deepEqual(mutations, ["cordon", "delete-pod", "rollout-restart", "scale", "uncordon"])
})

test("fields hidden for an operation are not sent", () => {
  const payload = k8sRequest(session, form({ operation: "describe", kind: "pod", name: "api-0", selector: "app=api", tail: "9", replicas: "3" }))
  assert.deepEqual(payload, { session_id: session, operation: "describe", kind: "pod", name: "api-0" })
})

const invalid = [
  ["secret read", { operation: "get", kind: "secrets" }, /Secret/],
  ["serviceaccount describe", { operation: "describe", kind: "sa", name: "default" }, /Secret/],
  ["secret logs resource", { operation: "logs", resource: "secret/x" }, /Secret|不支持/],
  ["describe without name", { operation: "describe", kind: "pod" }, /名称/],
  ["logs without resource", { operation: "logs" }, /资源/],
  ["logs bad resource", { operation: "logs", resource: "api" }, /kind\/name/],
  ["logs non-loggable kind", { operation: "logs", resource: "service/api" }, /不支持/],
  ["rollout status replicaset", { operation: "rollout-status", resource: "rs/api" }, /不支持/],
  ["scale daemonset", { operation: "scale", resource: "ds/agent", replicas: "1" }, /不支持/],
  ["scale without replicas", { operation: "scale", resource: "deployment/api" }, /副本数/],
  ["scale negative replicas", { operation: "scale", resource: "deployment/api", replicas: "-1" }, /整数/],
  ["scale too many replicas", { operation: "scale", resource: "deployment/api", replicas: "10001" }, /10000/],
  ["tail not integer", { operation: "logs", resource: "pod/api", tail: "1e3" }, /整数/],
  ["top services", { operation: "top", kind: "services" }, /pods 或 nodes/],
  ["namespace and all namespaces", { operation: "get", kind: "pods", namespace: "apps", allNamespaces: true }, /只能选一个/],
  ["helm status without namespace", { operation: "helm-status", name: "app" }, /命名空间/],
  ["helm history without release", { operation: "helm-history", namespace: "apps" }, /Release/],
  ["invalid namespace", { operation: "get", kind: "pods", namespace: "Apps_1" }, /命名空间/],
  ["invalid since", { operation: "logs", resource: "pod/api", since: "10 minutes" }, /10m/],
  ["invalid selector", { operation: "get", kind: "pods", selector: "app=$(id)" }, /选择器/],
]

for (const [name, input, message] of invalid) {
  test(`invalid input is rejected before sending: ${name}`, async () => {
    assert.throws(() => k8sRequest(session, form(input)), message)
    let sent = 0
    await assert.rejects(runK8s(async () => { sent += 1 }, { cluster: "test", instance: "node" }, session, form(input)), message)
    assert.equal(sent, 0)
  })
}

test("explicit writes send once to the selected target without a confirmation field", async () => {
  for (const [, input, payload] of mappings.filter(([, input]) => isK8sMutation(input.operation))) {
    const calls = []
    await runK8s(async (opts) => { calls.push(opts) }, { cluster: "test", instance: "node" }, session, form(input))
    assert.deepEqual(calls, [{
      cluster: "test", instance: "node", tool: K8S_TOOL,
      arguments: { session_id: session, ...payload },
    }])
    assert.equal("confirm" in calls[0].arguments, false)
  }
  assert.equal(K8S_TOOL, "doops_k8s")
})

test("unknown write outcomes and authority failures never replay the request", async () => {
  for (const message of ["execution_outcome_unknown", "Forbidden", "target offline", "doagent_prompt_busy"]) {
    let sent = 0
    const failure = new Error(message)
    await assert.rejects(runK8s(async () => { sent += 1; throw failure }, { cluster: "test", instance: "node" }, session,
      form({ operation: "delete-pod", name: "api-0", namespace: "apps" })), failure)
    assert.equal(sent, 1, message)
  }
})

// ---- Hardening: behaviour seen against live doops_k8s on doops-zyj/zyj-test and doops-zyj/zyj ----

const target = { cluster: "test", instance: "node" }
const rbacOutput = 'Error from server (Forbidden): namespaces is forbidden: User "system:serviceaccount:agents-test:doops-agent-test" cannot list resource "namespaces" in API group "" at the cluster scope\n'

const failures = [
  ["agent RBAC forbids a cluster-scoped list", "kubectl get namespaces failed: exit status 1", { output: rbacOutput }, "rbac", /agents-test\/doops-agent-test.*list namespaces.*集群级.*DoOps 授权没有问题.*ServiceAccount 的 RBAC/],
  ["agent RBAC forbids a namespaced read", "kubectl get pods --namespace kube-system failed: exit status 1",
    { output: 'Error from server (Forbidden): pods is forbidden: User "system:serviceaccount:agents-test:doops-agent-test" cannot list resource "pods" in API group "" in the namespace "kube-system"\n' }, "rbac", /list pods（命名空间 kube-system）/],
  ["helm reads are denied by agent RBAC", 'helm status failed: Error: query: failed to query with labels: secrets is forbidden: User "system:serviceaccount:agents-test:doops-agent-test" cannot list resource "secrets" in API group "" in the namespace "default"', {}, "rbac", /Agent 在集群里的 Kubernetes 身份无权执行此操作/],
  ["describe of a missing object", "kubectl describe pod g5 --namespace agents-test failed: exit status 1", { output: 'Error from server (NotFound): pods "g5" not found\n' }, "notfound", /资源不存在/],
  ["logs of a missing pod", "kubectl logs pod/g5 --namespace agents-test --tail 5 failed: exit status 1", { output: 'error: error from server (NotFound): pods "g5" not found in namespace "agents-test"\n' }, "notfound", /资源不存在/],
  ["missing helm release", "helm status failed: Error: release: not found", {}, "notfound", /Release 不存在/],
  ["top without metrics-server", "kubectl top nodes failed: exit status 1", { output: "error: Metrics API not available\n" }, "metrics", /metrics-server/],
  ["rollout status timeout", "kubectl rollout status deployment/api --namespace apps --timeout 60s failed: exit status 1", { output: "error: timed out waiting for the condition\n" }, "timeout", /超时/],
  ["gateway operation timeout on a read", "operation timed out", {}, "timeout", /超时/],
  ["user has no grant on the target", "forbidden: exec on test/node", {}, "forbidden", /test\/node.*授权/],
  ["grant revoked before the websocket upgrade", "连接错误: Unexpected server response: 403", {}, "forbidden", /授权/],
  ["expired login", "连接错误: Unexpected server response: 401", {}, "auth", /重新登录/],
  ["console route: Gateway refuses the upgrade without a grant", "无权访问该目标，请联系管理员授权（HTTP 403）", {}, "forbidden", /test\/node.*授权/],
  ["console route: Gateway refuses an expired login", "登录已失效，请重新登录（HTTP 401）", {}, "auth", /重新登录/],
  ["console route: Gateway does not know the target", "目标不存在或已下线（HTTP 404）", {}, "offline", /test\/node.*离线/],
  ["gateway rejects the token", "gateway_auth_expired", {}, "auth", /重新登录/],
  ["target offline", "target offline: test/node", {}, "offline", /离线/],
  ["session already running an operation", "session has an active operation; wait for terminal evidence: op-7", {}, "busy", /忙/],
  ["target queue full", "target queue full: test/node", {}, "busy", /忙/],
  ["cancellation barrier", "target busy: cancellation pending", {}, "busy", /忙/],
  ["user pressed stop", "operation canceled", { stopRequested: true }, "canceled", /已停止/],
  ["canceled without a stop", "operation canceled", {}, "canceled", /中止/],
  ["write stopped before dispatch", "请求派发前已停止", { mutation: true, stopRequested: true }, "canceled", /未派发/],
  ["write with unknown outcome", "execution_outcome_unknown: remote error: execution_outcome_unknown",
    { mutation: true, output: 'error: no objects passed to scale deployments.apps "g5" not found\n' }, "unknown", /test\/node.*核对.*不要直接重试.*资源不存在/],
  ["read stream lost before its result", "execution_outcome_unknown：远端终态未确认，请核对实际状态，不要直接重放原任务", {}, "unknown", /可以重新查询/],
  ["other kubectl failure", "kubectl get pods failed: exit status 1", { output: "" }, "exit", /退出码 1/],
  ["unclassified error is kept verbatim", "invalid kubernetes label selector", {}, "other", /^invalid kubernetes label selector$/],
  ["configmap listing above the agent limit", "configmap listing exceeds 67108864 bytes; narrow it to one namespace or a selector", {}, "toolarge", /^ConfigMap 列表超过 64 MiB.*单个命名空间.*标签选择器/],
  ["configmap listing limit relayed by the gateway", "tool error: configmap listing exceeds 67108864 bytes; narrow it to one namespace or a selector", {}, "toolarge", /64 MiB/],
  ["configmap listing limit with a forbidden-looking stream", "configmap listing exceeds 67108864 bytes; narrow it to one namespace or a selector", { output: rbacOutput }, "toolarge", /64 MiB/],
  ["agent older than configmap-keys", 'unsupported k8s operation "configmap-keys"', {}, "unsupported", /^目标 Agent 版本过旧，不支持「ConfigMap 键名」，请联系平台管理员升级该 Agent。$/],
  ["old agent error relayed by the gateway", 'tool error: unsupported k8s operation "configmap-keys"', {}, "unsupported", /不支持「ConfigMap 键名」/],
  ["unsupported operation the console does not offer stays verbatim", 'unsupported k8s operation "exec"', {}, "other", /^unsupported k8s operation "exec"$/],
]

for (const [name, message, opts, kind, text] of failures) {
  test(`failure is explained for operators: ${name}`, () => {
    const failure = describeK8sFailure(message, { target, output: "", mutation: false, stopRequested: false, ...opts })
    assert.equal(failure.kind, kind)
    assert.match(failure.text, text)
  })
}

test("agent RBAC denial does not ask for a second DoOps grant", () => {
  const failure = describeK8sFailure("kubectl get namespaces failed: exit status 1", { target, output: rbacOutput, mutation: false, stopRequested: false })
  assert.equal(failure.kind, "rbac")
  assert.equal(failure.text.includes("为该身份授权"), false)
})

const outputs = [
  ["ANSI colours are removed", ["\u001b[32mINFO\u001b[0m ready\n"], "INFO ready\n"],
  ["OSC titles and stray control bytes are removed", ["\u001b]0;title\u0007a\u0000b\u0008c\n"], "abc\n"],
  ["CRLF becomes LF", ["a\r\nb\r\n"], "a\nb\n"],
  ["carriage-return progress keeps the final state", ["10%\r50%\r100%\n"], "100%\n"],
  ["each chunk ends on its own line", ['{"name":"api"}', "next\n"], '{"name":"api"}\nnext\n'],
]

for (const [name, chunks, expected] of outputs) {
  test(`output is cleaned for display: ${name}`, () => {
    let log = EMPTY_K8S_LOG
    for (const chunk of chunks) log = appendK8sLog(log, "out", chunk)
    assert.deepEqual(log.entries, [{ kind: "out", text: expected }])
    assert.equal(log.dropped, 0)
  })
}

const caps = [
  ["many one-line chunks", Array.from({ length: 60 }, (_, i) => `line-${i}\n`)],
  ["one large chunk", [Array.from({ length: 60 }, (_, i) => `line-${i}\n`).join("")]],
]

for (const [name, chunks] of caps) {
  test(`long output keeps only the newest lines: ${name}`, () => {
    let log = appendK8sLog(EMPTY_K8S_LOG, "info", "目标 test/node · 日志 logs")
    for (const chunk of chunks) log = appendK8sLog(log, "out", chunk, 50)
    log = appendK8sLog(log, "err", "kubectl failed", 50)
    assert.equal(log.dropped, 10)
    assert.deepEqual(log.entries.map((entry) => entry.kind), ["info", "out", "err"])
    const out = log.entries[1].text.split("\n").filter(Boolean)
    assert.equal(out.length, 50)
    assert.equal(out[0], "line-10")
    assert.equal(out[49], "line-59")
  })
}

const scoping = [
  ["top nodes ignores a namespace left over from top pods", { operation: "top", kind: "nodes", namespace: "apps", allNamespaces: false }, { operation: "top", kind: "nodes" }],
  ["top nodes ignores -A left over from top pods", { operation: "top", kind: "nodes", allNamespaces: true }, { operation: "top", kind: "nodes" }],
  ["logs default to the newest 500 lines", { operation: "logs", resource: "pod/api-0" }, { operation: "logs", resource: "pod/api-0", tail: 500 }],
  ["clearing the line count asks for the full log", { operation: "logs", resource: "pod/api-0", tail: "" }, { operation: "logs", resource: "pod/api-0" }],
]

for (const [name, input, payload] of scoping) {
  test(`payload follows what the form shows: ${name}`, () => {
    assert.deepEqual(k8sRequest(session, form(input)), { session_id: session, ...payload })
  })
}

test("fields shown for top depend on pods or nodes", () => {
  assert.equal(k8sFields(form({ operation: "top", kind: "nodes" })).includes("namespace"), false)
  assert.equal(k8sFields(form({ operation: "top", kind: "nodes" })).includes("allNamespaces"), false)
  assert.equal(k8sFields(form({ operation: "top", kind: "pods" })).includes("namespace"), true)
})

const kubectlRejects = [
  ["get by name across all namespaces", { operation: "get", kind: "pods", name: "api-0", allNamespaces: true }, /全部命名空间/],
  ["get by name with a selector the agent would drop", { operation: "get", kind: "pods", name: "api-0", selector: "app=api" }, /选择器/],
  ["tail 0 would return the whole log", { operation: "logs", resource: "pod/api-0", tail: "0" }, /1 到 10000/],
]

for (const [name, input, message] of kubectlRejects) {
  test(`input kubectl would reject or misread is blocked: ${name}`, async () => {
    let sent = 0
    await assert.rejects(runK8s(async () => { sent += 1 }, target, session, form(input)), message)
    assert.equal(sent, 0)
  })
}

const switching = [
  ["same target keeps the form", "test/node", "test/node", "apps"],
  ["another target starts from an empty form", "test/node", "prod/web", ""],
]

for (const [name, from, to, namespace] of switching) {
  test(`form is scoped to its target: ${name}`, () => {
    const scoped = { targetKey: from, input: form({ namespace: "apps" }) }
    assert.equal(k8sFormFor(scoped, to).namespace, namespace)
  })
}

const alice = { gateway: "https://gw", username: "alice" }
const bob = { gateway: "https://gw", username: "bob" }
const relogin = [
  ["same login and target keeps the form", alice, "test/node", "apps"],
  ["same login on another target starts empty", alice, "prod/web", ""],
  ["another login on the same target starts empty", bob, "test/node", ""],
  ["a new login by the same account starts empty", { ...alice }, "test/node", ""],
]

for (const [name, owner, targetKey, namespace] of relogin) {
  test(`form is scoped to its login: ${name}`, () => {
    const store = createK8sFormStore()
    store.save(alice, "test/node", form({ namespace: "apps", resource: "deployment/api" }))
    const restored = store.load(owner, targetKey)
    assert.equal(restored.namespace, namespace)
    assert.equal("confirm" in restored, false)
  })
}

const hints = [
  ["empty namespace result", "No resources found in g5-ns namespace.\n", /g5-ns.*get namespaces/],
  ["rows returned", "NAME READY\napi-0 1/1\n", null],
  ["cluster-wide empty result", "No resources found\n", null],
]

for (const [name, output, hint] of hints) {
  test(`successful result hint: ${name}`, () => {
    const value = k8sResultHint(output)
    if (hint) assert.match(value, hint)
    else assert.equal(value, null)
  })
}

test("K8s panel keeps execute in place while running and only the separate stop button cancels", async () => {
  const requests = []
  const runningStates = []
  let cancels = 0
  let fail
  const view = componentHarness("../components/k8s-panel.tsx", "K8sPanel", {
    session: { gateway: "https://gateway.example.test", username: "synthetic-user" },
    target: { ...target, key: "test/node" },
    sessionId: session,
    onRunningChange: (running) => runningStates.push(running),
  }, {
    "lucide-react": componentStubs,
    "@/lib/k8s": k8s,
    "@/lib/client": {
      callTool: (_session, opts) => {
        requests.push(opts)
        opts.onCancelReady(async () => { cancels += 1 })
        return new Promise((_resolve, reject) => { fail = reject })
      },
    },
  })
  await view.flush()
  view.one((node) => node.props?.title === "写操作").props.onSelect("scale")
  await view.flush()
  for (const [label, value] of [["资源 kind/name", "deployment/api"], ["命名空间", "apps"], ["副本数", "2"]]) {
    view.one((node) => node.props?.["aria-label"] === label).props.onChange({ target: { value } })
  }
  await view.flush()
  let execute = view.one(buttonNamed("执行"))
  assert.equal(execute.props.disabled, false)
  assert.equal(view.nodes(buttonNamed("停止")).length, 0)
  execute.props.onClick({ detail: 1 })
  await view.flush()

  execute = view.one(buttonNamed("执行"))
  assert.equal(execute.props.disabled, true)
  const stop = view.one(buttonNamed("停止"))
  const controls = view.one((node) => node.type === "div" && node.props.children?.[0] === execute)
  assert.equal(controls.props.children[1], stop, "stop occupies a separate slot after execute")
  assert.equal(stop.props.disabled, false)
  execute.props.onClick({ detail: 2 })
  await view.flush()
  assert.equal(requests.length, 1, "another click must not send a second scale")
  assert.equal(cancels, 0, "another click in the execute slot must not cancel")
  assert.deepEqual(requests[0].arguments, {
    session_id: session, operation: "scale", resource: "deployment/api", namespace: "apps", replicas: 2,
  })
  assert.deepEqual(runningStates, [true])

  stop.props.onClick()
  await view.flush()
  assert.equal(cancels, 1, "an explicit stop still reaches the transport")
  assert.equal(view.one(buttonNamed("执行")).props.disabled, true)
  assert.equal(view.one(buttonNamed("正在停止")).props.disabled, true)
  assert.deepEqual(runningStates, [true], "cancel acknowledgement is not a terminal result")
  fail(new Error("execution_outcome_unknown: remote result unavailable"))
  await view.flush()
  execute = view.one(buttonNamed("执行"))
  assert.equal(execute.props.disabled, false)
  assert.equal(view.nodes(buttonNamed("停止")).length, 0)
  assert.equal(view.nodes(buttonNamed("正在停止")).length, 0)
  assert.deepEqual(runningStates, [true, false])
  const alert = view.one((node) => node.props?.role === "alert")
  assert.match(elementText(alert), /不要直接重试/)
  const idleControls = view.one((node) => node.type === "div" && node.props.children?.[0] === execute)
  assert.equal(findElements(idleControls, (node) => node === alert).length, 0, "error text must not share the button row")
  assert.equal(requests.length, 1, "an unknown result must not replay the scale")
})

for (const outcome of ["success", "failure"]) {
  test(`K8s double-click submits once after a fast ${outcome} and permits a later action`, async () => {
    const requests = []
    const view = componentHarness("../components/k8s-panel.tsx", "K8sPanel", {
      session: { gateway: "https://gateway.example.test", username: "synthetic-user" },
      target: { ...target, key: "test/node" },
      sessionId: session,
      onRunningChange: () => {},
    }, {
      "lucide-react": componentStubs,
      "@/lib/k8s": k8s,
      "@/lib/client": {
        callTool: async (_session, opts) => {
          requests.push(opts)
          if (outcome === "failure") throw new Error("synthetic operation failure")
        },
      },
    })
    await view.flush()
    view.one((node) => node.props?.title === "写操作").props.onSelect("scale")
    await view.flush()
    for (const [label, value] of [["资源 kind/name", "deployment/api"], ["命名空间", "apps"], ["副本数", "2"]]) {
      view.one((node) => node.props?.["aria-label"] === label).props.onChange({ target: { value } })
    }
    await view.flush()
    view.one(buttonNamed("执行")).props.onClick({ detail: 1 })
    await view.flush()
    assert.equal(view.one(buttonNamed("执行")).props.disabled, false)
    view.one(buttonNamed("执行")).props.onClick({ detail: 2 })
    await view.flush()
    assert.equal(requests.length, 1, "the second click in one gesture must not resubmit")
    view.one(buttonNamed("执行")).props.onClick({ detail: 1 })
    await view.flush()
    assert.equal(requests.length, 2, "a later deliberate click remains available")
    view.one(buttonNamed("执行")).props.onClick({ detail: 0 })
    await view.flush()
    assert.equal(requests.length, 3, "keyboard activation remains available")
  })
}
