import { gatewayAuthMessage } from "./gateway-auth.ts"
import { classifyAgentAdmission } from "./agent-error.ts"
// Web form for the Agent doops_k8s tool. The payload mirrors buildK8SRequest in
// backend/cli/cli/doops/k8s.go so the console and `doops k8s` call the same contract.

export const K8S_TOOL = "doops_k8s"

export type K8sOperation =
  | "get"
  | "describe"
  | "logs"
  | "events"
  | "top"
  | "rollout-status"
  | "configmap-keys"
  | "helm-list"
  | "helm-history"
  | "helm-status"
  | "helm-values"
  | "rollout-restart"
  | "scale"
  | "cordon"
  | "uncordon"
  | "delete-pod"

export type K8sField =
  | "kind"
  | "name"
  | "resource"
  | "namespace"
  | "allNamespaces"
  | "selector"
  | "output"
  | "container"
  | "tail"
  | "previous"
  | "since"
  | "timeout"
  | "replicas"

export interface K8sInput {
  operation: K8sOperation
  kind: string
  name: string
  resource: string
  namespace: string
  allNamespaces: boolean
  selector: string
  output: string
  container: string
  tail: string
  previous: boolean
  since: string
  timeout: string
  replicas: string
}

const BLANK_K8S_INPUT: K8sInput = {
  operation: "get",
  kind: "pods",
  name: "",
  resource: "",
  namespace: "",
  allNamespaces: false,
  selector: "",
  output: "",
  container: "",
  tail: "",
  previous: false,
  since: "",
  timeout: "",
  replicas: "",
}

// Logs default to the newest lines; an empty value asks for the whole log.
export const K8S_DEFAULT_TAIL = "500"

export const EMPTY_K8S_INPUT: K8sInput = { ...BLANK_K8S_INPUT, tail: K8S_DEFAULT_TAIL }

export interface K8sOperationSpec {
  label: string
  // Empty when `doops k8s` has no subcommand for the Agent operation.
  cli: string
  mutation: boolean
  fields: K8sField[]
}

export const K8S_OPERATIONS: Record<K8sOperation, K8sOperationSpec> = {
  get: { label: "查看资源 get", cli: "get <kind> [name]", mutation: false, fields: ["kind", "name", "namespace", "allNamespaces", "selector", "output"] },
  describe: { label: "详情 describe", cli: "describe <kind> <name>", mutation: false, fields: ["kind", "name", "namespace"] },
  logs: { label: "日志 logs", cli: "logs <resource>", mutation: false, fields: ["resource", "namespace", "container", "tail", "previous", "since"] },
  events: { label: "事件 events", cli: "events [--for kind/name]", mutation: false, fields: ["resource", "namespace", "allNamespaces"] },
  top: { label: "资源用量 top", cli: "top pods|nodes", mutation: false, fields: ["kind", "namespace", "allNamespaces", "selector"] },
  "rollout-status": { label: "发布状态 rollout status", cli: "rollout status <resource>", mutation: false, fields: ["resource", "namespace", "timeout"] },
  // Fields follow the doops_k8s schema in k8s_mcp_stdio.go; the output is fixed by the Agent.
  "configmap-keys": { label: "ConfigMap 键名", cli: "", mutation: false, fields: ["name", "namespace", "allNamespaces", "selector"] },
  "helm-list": { label: "Helm 列表", cli: "helm list", mutation: false, fields: ["namespace", "allNamespaces"] },
  "helm-history": { label: "Helm 历史", cli: "helm history <release>", mutation: false, fields: ["name", "namespace"] },
  "helm-status": { label: "Helm 状态", cli: "helm status <release>", mutation: false, fields: ["name", "namespace"] },
  "helm-values": { label: "Helm values 键路径", cli: "helm values <release>", mutation: false, fields: ["name", "namespace"] },
  "rollout-restart": { label: "滚动重启 rollout restart", cli: "rollout restart <resource>", mutation: true, fields: ["resource", "namespace"] },
  scale: { label: "扩缩容 scale", cli: "scale <resource> --replicas N", mutation: true, fields: ["resource", "namespace", "replicas"] },
  cordon: { label: "节点停止调度 cordon", cli: "node cordon <node>", mutation: true, fields: ["name"] },
  uncordon: { label: "节点恢复调度 uncordon", cli: "node uncordon <node>", mutation: true, fields: ["name"] },
  "delete-pod": { label: "删除 Pod", cli: "delete pod <name>", mutation: true, fields: ["name", "namespace"] },
}

// What the Agent leaves out of the output. Helm status was trimmed in Agent
// v1.3.119 (3c53dbd); older Agents still return the whole release.
export const K8S_OUTPUT_HINTS: Partial<Record<K8sOperation, string>> = {
  "helm-values": "只返回 values 的键路径，不返回值。",
  "helm-status": "v1.3.119 及以上的 Agent 只返回发布名、修订、状态和时间；更早的 Agent 会返回完整 release，其中可能有 values 里的敏感值。",
  "configmap-keys": "只返回键名，不返回值。",
}

const NAME = /^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$/
const KIND = /^[a-z][a-z0-9.-]*$/
const CONTAINER = /^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$/
const DURATION = /^[1-9][0-9]*(s|m|h)$/
const SELECTOR = /^[A-Za-z0-9_./!=(), -]{1,512}$/
const CREDENTIAL_KINDS = new Set(["secret", "secrets", "serviceaccount", "serviceaccounts", "sa", "token", "tokenrequest", "tokenrequests"])
const LOGGABLE = new Set(["pod", "pods", "deployment", "deployments", "deploy", "statefulset", "statefulsets", "sts", "daemonset", "daemonsets", "ds", "job", "jobs"])
const ROLLOUT = new Set(["deployment", "deployments", "deploy", "statefulset", "statefulsets", "sts", "daemonset", "daemonsets", "ds"])
const SCALABLE = new Set(["deployment", "deployments", "deploy", "statefulset", "statefulsets", "sts", "replicaset", "replicasets", "rs"])

export function isK8sMutation(operation: K8sOperation): boolean {
  return K8S_OPERATIONS[operation]?.mutation === true
}

function resourceKind(resource: string, allowed: Set<string> | null, label: string): void {
  const [kind, name, extra] = resource.split("/")
  if (extra !== undefined || !kind || !name || !KIND.test(kind) || !NAME.test(name)) {
    throw new Error(`${label}必须为 kind/name，例如 deployment/api`)
  }
  if (CREDENTIAL_KINDS.has(kind)) throw new Error("Secret 与 ServiceAccount 不开放读取")
  if (allowed && !allowed.has(kind)) throw new Error(`${label}不支持资源类型 ${kind}`)
}

function integer(value: string, label: string, min = 0): number {
  if (!/^\d+$/.test(value)) throw new Error(`${label}必须为整数`)
  const n = Number(value)
  if (n < min || n > 10000) throw new Error(`${label}必须在 ${min} 到 10000 之间`)
  return n
}

// Fields the form shows for this input. Nodes are cluster-scoped: kubectl top nodes
// ignores --namespace and rejects --all-namespaces, so neither is shown or sent.
export function k8sFields(input: K8sInput): K8sField[] {
  const fields = K8S_OPERATIONS[input.operation]?.fields || []
  if (input.operation === "top" && input.kind.trim().toLowerCase() === "nodes") {
    return fields.filter((field) => field !== "namespace" && field !== "allNamespaces")
  }
  return fields
}

function normalizeResource(value: string): string {
  const parts = value.trim().split("/")
  if (parts.length !== 2) return value.trim()
  return parts[0].trim().toLowerCase() + "/" + parts[1].trim()
}

// Builds the doops_k8s arguments. Throws a user-facing error on invalid input.
export function k8sRequest(sessionId: string, input: K8sInput): Record<string, unknown> {
  const spec = K8S_OPERATIONS[input.operation]
  if (!spec) throw new Error("不支持的 Kubernetes 操作")
  const fields = k8sFields(input)
  const uses = (field: K8sField) => fields.includes(field)
  const kind = input.kind.trim().toLowerCase()
  const name = input.name.trim()
  const resource = normalizeResource(input.resource)
  const namespace = input.namespace.trim()
  const payload: Record<string, unknown> = { session_id: sessionId, operation: input.operation }

  if (uses("kind")) {
    if (!kind) throw new Error(input.operation === "top" ? "请选择 pods 或 nodes" : "请填写资源类型")
    if (!KIND.test(kind)) throw new Error("资源类型格式无效")
    if (CREDENTIAL_KINDS.has(kind)) throw new Error("Secret 与 ServiceAccount 不开放读取")
    if (input.operation === "top" && kind !== "pods" && kind !== "nodes") throw new Error("top 只支持 pods 或 nodes")
    payload.kind = kind
  }
  if (uses("name")) {
    const required = input.operation !== "get" && input.operation !== "configmap-keys"
    if (!name && required) {
      const label = input.operation.startsWith("helm-") ? "Release 名称" : input.operation === "delete-pod" ? "Pod 名称" : input.operation === "describe" ? "资源名称" : "节点名称"
      throw new Error(`请填写${label}`)
    }
    if (name) {
      if (!NAME.test(name)) throw new Error("名称必须为有效 Kubernetes 名称")
      payload.name = name
    }
  }
  if (uses("resource")) {
    if (resource) {
      const allowed = input.operation === "logs" ? LOGGABLE
        : input.operation === "rollout-status" || input.operation === "rollout-restart" ? ROLLOUT
        : input.operation === "scale" ? SCALABLE
        : null
      resourceKind(resource, allowed, input.operation === "events" ? "事件对象" : "资源")
      payload.resource = resource
    } else if (input.operation !== "events") {
      throw new Error("请填写资源，例如 deployment/api")
    }
  }
  if (uses("namespace") && namespace) {
    if (!NAME.test(namespace)) throw new Error("命名空间必须为有效 Kubernetes 名称")
    payload.namespace = namespace
  }
  if (uses("allNamespaces") && input.allNamespaces) {
    if (namespace) throw new Error("命名空间与「全部命名空间」只能选一个")
    payload.all_namespaces = true
  }
  if (input.operation.startsWith("helm-") && input.operation !== "helm-list" && !namespace) {
    throw new Error("请填写 Release 所在命名空间")
  }
  if ((input.operation === "get" || input.operation === "configmap-keys") && name) {
    // kubectl refuses a named get across all namespaces, and the Agent drops the
    // selector when a name is given, so neither combination means what it shows.
    // configmap-keys runs the same kubectl get.
    if (input.allNamespaces) throw new Error("按名称查看时不能勾选「全部命名空间」，请填写命名空间")
    if (input.selector.trim()) throw new Error("按名称查看时标签选择器不会生效，请清空名称或选择器")
  }
  if (uses("selector") && input.selector.trim()) {
    if (!SELECTOR.test(input.selector.trim())) throw new Error("标签选择器格式无效")
    payload.selector = input.selector.trim()
  }
  if (uses("output") && input.output.trim()) payload.output = input.output.trim()
  if (uses("container") && input.container.trim()) {
    if (!CONTAINER.test(input.container.trim())) throw new Error("容器名格式无效")
    payload.container = input.container.trim()
  }
  // The Agent treats tail 0 as "no limit", so 0 is not offered; an empty value asks for the whole log.
  if (uses("tail") && input.tail.trim()) payload.tail = integer(input.tail.trim(), "日志行数", 1)
  if (uses("previous") && input.previous) payload.previous = true
  if (uses("since") && input.since.trim()) {
    if (!DURATION.test(input.since.trim())) throw new Error("时间范围格式如 10m、2h")
    payload.since = input.since.trim()
  }
  if (uses("timeout") && input.timeout.trim()) {
    if (!DURATION.test(input.timeout.trim())) throw new Error("超时格式如 120s、5m")
    payload.timeout = input.timeout.trim()
  }
  if (uses("replicas")) {
    if (!input.replicas.trim()) throw new Error("请填写副本数")
    payload.replicas = integer(input.replicas.trim(), "副本数")
  }
  return payload
}

export interface K8sCallOptions {
  cluster: string
  instance: string
  tool: string
  arguments: Record<string, unknown>
}

// Validates first; nothing is sent when the request is invalid.
export async function runK8s(
  send: (opts: K8sCallOptions) => Promise<void>,
  target: { cluster: string; instance: string },
  sessionId: string,
  input: K8sInput,
): Promise<void> {
  const args = k8sRequest(sessionId, input)
  await send({ cluster: target.cluster, instance: target.instance, tool: K8S_TOOL, arguments: args })
}

export interface K8sScopedForm {
  targetKey: string
  input: K8sInput
}

// The form survives leaving and reopening the tab on the same target. Another
// target starts empty, so no namespace or object name carries over.
export function k8sFormFor(scoped: K8sScopedForm | null, targetKey: string): K8sInput {
  if (!scoped || scoped.targetKey !== targetKey) return EMPTY_K8S_INPUT
  return { ...scoped.input }
}

// Saved forms belong to one login session object. Logging out drops the session,
// so the next login in the same browser, by any account, starts from an empty form.
export function createK8sFormStore() {
  const forms = new WeakMap<object, K8sScopedForm>()
  return {
    load: (owner: object, targetKey: string): K8sInput => k8sFormFor(forms.get(owner) ?? null, targetKey),
    save: (owner: object, targetKey: string, input: K8sInput) => { forms.set(owner, { targetKey, input }) },
  }
}

// ---- Output ----

export type K8sLogKind = "out" | "err" | "info"

export interface K8sLogEntry {
  kind: K8sLogKind
  text: string
}

export interface K8sLog {
  entries: K8sLogEntry[]
  // Output lines dropped from the start to keep the page responsive.
  dropped: number
}

export const EMPTY_K8S_LOG: K8sLog = { entries: [], dropped: 0 }

export const K8S_LOG_MAX_LINES = 2000

const ANSI_OSC = /\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)/g
const ANSI_CSI = /\u001b\[[0-9;?]*[ -/]*[@-~]/g
const ANSI_ESCAPE = /\u001b[@-Z\\-_]/g
// Keeps \t, \n and \r; \r is resolved per line below.
const CONTROL = /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/g

function cleanOutput(chunk: string): string {
  const text = chunk.replace(ANSI_OSC, "").replace(ANSI_CSI, "").replace(ANSI_ESCAPE, "").replace(CONTROL, "")
  const joined = text
    .replace(/\r\n/g, "\n")
    .split("\n")
    .map((line) => line.slice(line.lastIndexOf("\r") + 1))
    .join("\n")
  if (!joined) return ""
  return joined.endsWith("\n") ? joined : joined + "\n"
}

function lineCount(text: string): number {
  let count = 0
  for (let i = text.indexOf("\n"); i >= 0; i = text.indexOf("\n", i + 1)) count += 1
  return count
}

// Appends one streamed chunk with terminal control sequences removed. Only command
// output is capped: the run header and error lines are always kept.
export function appendK8sLog(log: K8sLog, kind: K8sLogKind, chunk: string, maxLines = K8S_LOG_MAX_LINES): K8sLog {
  const text = cleanOutput(chunk)
  if (!text) return log
  const entries = log.entries.slice()
  const last = entries[entries.length - 1]
  if (last && last.kind === kind) entries[entries.length - 1] = { kind, text: last.text + text }
  else entries.push({ kind, text })

  let dropped = log.dropped
  let excess = entries.reduce((total, entry) => total + (entry.kind === "out" ? lineCount(entry.text) : 0), 0) - maxLines
  for (let index = 0; excess > 0 && index < entries.length;) {
    const entry = entries[index]
    if (entry.kind !== "out") {
      index += 1
      continue
    }
    const lines = entry.text.split("\n")
    const count = lines.length - 1
    if (count <= excess) {
      entries.splice(index, 1)
      excess -= count
      dropped += count
      continue
    }
    entries[index] = { kind: "out", text: lines.slice(excess).join("\n") }
    dropped += excess
    excess = 0
  }
  return { entries, dropped }
}

// kubectl prints the same message whether a namespace is empty or does not exist.
export function k8sResultHint(output: string): string | null {
  const empty = /^No resources found in (\S+) namespace\.?\s*$/m.exec(output)
  if (!empty) return null
  return `命名空间 ${empty[1]} 下没有匹配的资源。kubectl 对不存在的命名空间也这样返回，不确定时可用 get namespaces 核对。`
}

// ---- Failures ----

export type K8sFailureKind =
  | "rbac"
  | "notfound"
  | "metrics"
  | "timeout"
  | "forbidden"
  | "auth"
  | "offline"
  | "busy"
  | "canceled"
  | "toolarge"
  | "unsupported"
  | "unknown"
  | "exit"
  | "other"

export interface K8sFailure {
  kind: K8sFailureKind
  text: string
}

export interface K8sFailureContext {
  target: { cluster: string; instance: string }
  // Streamed output of the failed run; kubectl writes its error there.
  output: string
  mutation: boolean
  stopRequested: boolean
}

const RBAC_DENIAL = /User "([^"]+)" cannot ([\w-]+) resource "([^"]+)"(?: in API group "[^"]*")?(?: (at the cluster scope)| in the namespace "([^"]+)")?/
const NOT_FOUND = /([\w.-]+) "([^"]+)" not found(?: in namespace "([^"]+)")?/i

function rbacDenial(text: string): string | null {
  const match = RBAC_DENIAL.exec(text)
  if (match) {
    const [, user, verb, resource, clusterScope, namespace] = match
    const account = "system:serviceaccount:"
    const who = user.startsWith(account) ? `ServiceAccount ${user.slice(account.length).replace(":", "/")}` : `用户 ${user}`
    const scope = namespace ? `（命名空间 ${namespace}）` : clusterScope ? "（集群级）" : ""
    return `${who} 不能 ${verb} ${resource}${scope}`
  }
  if (/\(Forbidden\)|\bis forbidden\b/.test(text)) return firstErrorLine(text)
  return null
}

function notFound(text: string): string | null {
  if (/release: not found/i.test(text)) return "Release 不存在，请确认 Release 名称和命名空间"
  if (!/\(NotFound\)|\bnot found\b/i.test(text)) return null
  const match = NOT_FOUND.exec(text)
  if (!match) return `资源不存在：${firstErrorLine(text)}`
  const [, kind, name, namespace] = match
  if (/^namespaces?$/.test(kind)) return `资源不存在：命名空间 ${name} 不存在`
  return `资源不存在：${kind} ${name}${namespace ? `（命名空间 ${namespace}）` : ""}，请确认名称和命名空间`
}

function firstErrorLine(text: string): string {
  const lines = text.split("\n").map((line) => line.trim()).filter(Boolean)
  return lines.find((line) => /error|forbidden|not found/i.test(line)) || lines[0] || ""
}

function lastErrorLine(text: string): string {
  const lines = text.split("\n").map((line) => line.trim()).filter(Boolean)
  return [...lines].reverse().find((line) => /^error\b/i.test(line)) || ""
}

// Explains a failed doops_k8s call in terms an operator can act on. Agent RBAC
// denials are told apart from the user's own Gateway grant.
// /api/rpc reports a refused Gateway upgrade as "…（HTTP n）". Same format as
// gatewayUpgradeStatus in lib/shell-blocks.ts; kept local because this file has
// no imports so its tests can load it directly. shell-blocks.test.mjs checks both agree.
function gatewayUpgradeStatus(raw: string): string | null {
  return /（HTTP (\d{3})）$/.exec(raw.trim())?.[1] ?? null
}

export function describeK8sFailure(message: string, context: K8sFailureContext): K8sFailure {
  const raw = message.trim()
  const where = `${context.target.cluster}/${context.target.instance}`
  const evidence = `${raw}\n${context.output}`

  if (classifyAgentAdmission(raw).kind === "unknown") {
    if (!context.mutation) {
      return { kind: "unknown", text: "查询结果未确认：连接在收到终态前中断。只读操作不改变集群，可以重新查询。" }
    }
    const denial = rbacDenial(context.output)
    const missing = notFound(context.output)
    const hint = denial ? `Agent 输出提示权限不足：${denial}。` : missing ? `Agent 输出提示${missing}。` : ""
    return {
      kind: "unknown",
      text: `写操作结果未确认：请先在 ${where} 上用 get、describe 或 rollout status 核对实际状态，不要直接重试。${hint}`,
    }
  }
  const upgrade = gatewayUpgradeStatus(raw)
  const authMessage = gatewayAuthMessage(raw)
  if (authMessage) return { kind: "auth", text: authMessage }
  if (upgrade === "401" || /Unexpected server response: 401\b/.test(raw) ||
      raw.includes("未建立 Gateway 会话") || raw.includes("缺少 gateway 地址或 token") || raw.includes("Gateway 会话已失效")) {
    return { kind: "auth", text: "登录已失效，请重新登录后再执行" }
  }
  if (/^forbidden\b/i.test(raw) || upgrade === "403" || /Unexpected server response: 403\b/.test(raw)) {
    return { kind: "forbidden", text: `无权操作 ${where}：你的账号没有该目标的授权，请联系管理员授权` }
  }
  if (/^target offline\b/i.test(raw) || upgrade === "404" || raw === "agent disconnected") {
    return { kind: "offline", text: `目标 ${where} 已离线或断开：请确认 Agent 在线后重试` }
  }
  if (/session has an active operation|target queue (full|timeout)|target busy/i.test(raw)) {
    return { kind: "busy", text: `目标 ${where} 正忙：当前会话或目标还有操作没有结束，请等它完成后再试` }
  }
  if (raw.includes("请求派发前已停止") || raw.includes("operation canceled before dispatch")) {
    return { kind: "canceled", text: `已停止：请求未派发到 Agent${context.mutation ? "，集群没有变化" : ""}` }
  }
  if (raw.includes("operation canceled")) {
    return { kind: "canceled", text: context.stopRequested ? "已停止" : "操作被中止：可能超过执行时限，或会话被取消" }
  }
  // runConfigMapKeysCommand refuses a kubectl listing above its in-memory limit.
  const tooLarge = /configmap listing exceeds (\d+) bytes/i.exec(raw)
  if (tooLarge) {
    const mib = Math.round(Number(tooLarge[1]) / (1 << 20))
    return { kind: "toolarge", text: `ConfigMap 列表超过 ${mib} MiB，Agent 不返回这么大的结果：请改为单个命名空间，或填写名称、标签选择器缩小范围` }
  }
  // buildK8SCommands on an Agent older than the operation.
  const unsupported = /unsupported k8s operation "([^"]+)"/.exec(raw)?.[1]
  if (unsupported && unsupported in K8S_OPERATIONS) {
    const label = K8S_OPERATIONS[unsupported as K8sOperation].label
    return { kind: "unsupported", text: `目标 Agent 版本过旧，不支持「${label}」，请联系平台管理员升级该 Agent。` }
  }

  const denial = rbacDenial(evidence)
  if (denial) {
    return {
      kind: "rbac",
      text: `目标 Agent 在集群里的 Kubernetes 身份无权执行此操作：${denial}。你对该目标的 DoOps 授权没有问题；这台 Agent 能操作哪些资源由它的部署配置（ServiceAccount 的 RBAC）决定。如需执行，请联系平台管理员调整该 Agent 的 RBAC，或换用能执行该操作的目标。`,
    }
  }
  const missing = notFound(evidence)
  if (missing) return { kind: "notfound", text: missing }
  if (/Metrics API not available|metrics\.k8s\.io/i.test(evidence)) {
    return { kind: "metrics", text: "集群没有可用的 Metrics API：top 依赖 metrics-server，请先在集群中安装或修复 metrics-server" }
  }
  if (/timed out waiting|operation timed out|context deadline exceeded/i.test(evidence)) {
    return { kind: "timeout", text: "执行超时：操作没有在时限内完成。可调大超时，或用 describe、events 查看卡在哪一步。" }
  }
  const exit = /exit status (\d+)/.exec(raw)
  if (exit) {
    const detail = lastErrorLine(context.output)
    return { kind: "exit", text: `命令失败，退出码 ${exit[1]}${detail ? `：${detail}` : ""}` }
  }
  return { kind: "other", text: raw }
}
