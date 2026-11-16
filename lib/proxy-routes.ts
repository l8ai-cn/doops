import { gatewayAuthMessage } from "./gateway-auth.ts"
// Gateway 网站代理路由（/v1/admin/proxy-routes）的类型、表单校验和运维提示。
// 校验规则与 backend/agent/internal/server/proxy_route_store.go 保持一致（上游地址只会更严），
// 只用于在提交前给出中文提示；最终以 Gateway 返回为准。

export interface ProxyRoute {
  id: string
  host: string
  cluster: string
  instance: string
  upstream: string
  enabled: boolean
  created_at?: string
  updated_at?: string
}

export interface ProxyRouteBody {
  id?: string
  host: string
  cluster: string
  instance: string
  upstream: string
  enabled: boolean
}

export interface ProxyRouteForm {
  id: string
  host: string
  cluster: string
  instance: string
  upstream: string
  enabled: boolean
}

export const EMPTY_PROXY_ROUTE_FORM: ProxyRouteForm = {
  id: "",
  host: "",
  cluster: "",
  instance: "",
  upstream: "",
  enabled: true,
}

export function proxyRouteForm(route: ProxyRoute): ProxyRouteForm {
  return {
    id: route.id,
    host: route.host,
    cluster: route.cluster,
    instance: route.instance,
    upstream: route.upstream,
    enabled: route.enabled,
  }
}

type Result<T> = { ok: true; value: T } | { ok: false; error: string }

const HOST_INVALID = "域名格式无效"
const UPSTREAM_INVALID = "上游必须是 http(s) 地址，且不能包含账号密码、查询参数或锚点"
const SCOPE_REQUIRED = "请填写目标集群和实例"

// 对应 Go net.SplitHostPort：成功返回 [host, port]，失败返回 null。
function splitHostPort(raw: string): [string, string] | null {
  if (raw.startsWith("[")) {
    const end = raw.indexOf("]")
    if (end < 0 || raw[end + 1] !== ":") return null
    const port = raw.slice(end + 2)
    if (port.includes(":")) return null
    return [raw.slice(1, end), port]
  }
  const first = raw.indexOf(":")
  if (first < 0 || first !== raw.lastIndexOf(":")) return null
  return [raw.slice(0, first), raw.slice(first + 1)]
}

function isIPv4(raw: string): boolean {
  const parts = raw.split(".")
  return parts.length === 4 && parts.every((p) => /^(0|[1-9][0-9]{0,2})$/.test(p) && Number(p) <= 255)
}

function isIPv6(raw: string): boolean {
  return raw.includes(":") && /^[0-9a-f:.]+$/.test(raw) && (raw.match(/::/g) || []).length <= 1
}

export function normalizeProxyHost(input: string): Result<string> {
  let raw = input.trim()
  if (!raw) return { ok: false, error: "请填写域名" }
  if (raw.includes("://") || /[/?#]/.test(raw)) {
    return { ok: false, error: "域名只填主机名，不要带协议或路径" }
  }
  const split = splitHostPort(raw)
  if (split) {
    if (!split[1]) return { ok: false, error: "域名中的端口无效" }
    raw = split[0]
  } else if (raw.includes(":")) {
    return { ok: false, error: "域名不能带端口" }
  }
  raw = raw.trim().toLowerCase().replace(/\.$/, "")
  if (!raw || raw.length > 253 || /[ \t\r\n]/.test(raw)) return { ok: false, error: HOST_INVALID }
  if (isIPv4(raw) || isIPv6(raw)) return { ok: true, value: raw }
  for (const label of raw.split(".")) {
    if (!label || label.length > 63 || label.startsWith("-") || label.endsWith("-") || !/^[a-z0-9-]+$/.test(label)) {
      return { ok: false, error: HOST_INVALID }
    }
  }
  return { ok: true, value: raw }
}

interface UpstreamParts {
  normalized: string
  hostname: string
}

// 按 Go url.Parse + normalizeProxyUpstream 的结果解析上游。不用浏览器 URL：
// 它会把 http:/host 补成 http://host/，还会把主机名转小写，与 Gateway 保存的值不一致。
function parseUpstream(input: string): UpstreamParts | null {
  const m = /^([A-Za-z][A-Za-z0-9+.-]*):\/\/([^/?#]*)([^?#]*)$/.exec(input.trim())
  if (!m) return null
  const scheme = m[1].toLowerCase()
  if (scheme !== "http" && scheme !== "https") return null
  const authority = m[2]
  let hostname: string
  let port: string
  if (authority.startsWith("[")) {
    const end = authority.indexOf("]")
    if (end < 0) return null
    hostname = authority.slice(1, end).toLowerCase()
    port = authority.slice(end + 1)
    if (!isIPv6(hostname)) return null
  } else {
    const colon = authority.lastIndexOf(":")
    hostname = (colon < 0 ? authority : authority.slice(0, colon)).toLowerCase()
    port = colon < 0 ? "" : authority.slice(colon)
    // 不允许 @：Go 会把它解析成账号信息。
    if (!/^[a-z0-9._~-]+$/.test(hostname)) return null
  }
  if (port && !/^:[0-9]+$/.test(port)) return null
  if (/[\s\x00-\x1f\x7f]/.test(m[3])) return null
  const path = m[3].replace(/\/+$/, "") || "/"
  return { normalized: `${scheme}://${authority}${path}`, hostname }
}

export function validateProxyUpstream(input: string): Result<string> {
  const parsed = parseUpstream(input)
  return parsed ? { ok: true, value: parsed.normalized } : { ok: false, error: UPSTREAM_INVALID }
}

// 表单 → Gateway upsert 请求体。Gateway 的 upsert 会整体替换字段，
// 所以编辑时表单里必须带着全部现值（由 proxyRouteForm 预填）。
export function proxyRouteBody(form: ProxyRouteForm): Result<ProxyRouteBody> {
  const host = normalizeProxyHost(form.host)
  if (!host.ok) return host
  const cluster = form.cluster.trim()
  const instance = form.instance.trim()
  if (!cluster || !instance) return { ok: false, error: SCOPE_REQUIRED }
  const upstream = validateProxyUpstream(form.upstream)
  if (!upstream.ok) return upstream
  const body: ProxyRouteBody = { host: host.value, cluster, instance, upstream: upstream.value, enabled: form.enabled }
  const id = form.id.trim()
  if (id) body.id = id
  return { ok: true, value: body }
}

type RouteFields = Omit<ProxyRouteBody, "id">
type FieldKey = keyof RouteFields

// 合并的最小单位；集群和实例一起决定转发目标，按一个字段处理。
const MERGE_FIELDS: { label: string; keys: FieldKey[]; show: (r: RouteFields) => string }[] = [
  { label: "域名", keys: ["host"], show: (r) => r.host },
  { label: "目标", keys: ["cluster", "instance"], show: (r) => `${r.cluster}/${r.instance}` },
  { label: "上游", keys: ["upstream"], show: (r) => r.upstream },
  { label: "状态", keys: ["enabled"], show: (r) => (r.enabled ? "启用" : "停用") },
]

function sameFields(a: RouteFields, b: RouteFields, keys: FieldKey[]): boolean {
  return keys.every((k) => a[k] === b[k])
}

function copyFields(dst: RouteFields, src: RouteFields, keys: FieldKey[]): void {
  for (const k of keys) (dst as unknown as Record<FieldKey, unknown>)[k] = src[k]
}

// Gateway 返回的值已规范化；这里再规范化一次，只为和表单输入可比。
function canonRoute(route: ProxyRoute): ProxyRouteBody & { id: string } {
  const host = normalizeProxyHost(route.host)
  const upstream = validateProxyUpstream(route.upstream)
  return {
    id: route.id,
    host: host.ok ? host.value : route.host,
    cluster: route.cluster.trim(),
    instance: route.instance.trim(),
    upstream: upstream.ok ? upstream.value : route.upstream,
    enabled: route.enabled,
  }
}

function hostOwner(routes: ProxyRoute[], host: string, exceptId: string): ProxyRoute | undefined {
  return routes.find((r) => r.id !== exceptId && canonRoute(r).host === host)
}

export type ProxyRouteSavePlan =
  | { ok: true; unchanged: boolean; body: ProxyRouteBody }
  | { ok: false; reason: "invalid" | "duplicate" | "deleted"; error: string }
  | { ok: false; reason: "conflict"; error: string; latest: ProxyRoute; form: ProxyRouteForm }

// Gateway 只在请求带 updated_at 时做版本校验（本控制台未带），否则后写的整体覆盖先写的；
// 旧版 Gateway 在 id 已被删除时还会按原 id 重新插入。所以保存前要重读列表，
// 把本次表单（相对打开时的 base）与最新数据（latest）做三方合并：
// 只有本次改过的字段才覆盖最新值；同一字段双方都改且不同，就停下来让操作者确认。
export function planProxyRouteSave(
  form: ProxyRouteForm,
  base: ProxyRoute | null,
  latest: ProxyRoute[],
): ProxyRouteSavePlan {
  const parsed = proxyRouteBody(form)
  if (!parsed.ok) return { ok: false, reason: "invalid", error: parsed.error }
  const mine = parsed.value
  const duplicate = (owner: ProxyRoute): ProxyRouteSavePlan => ({
    ok: false,
    reason: "duplicate",
    error: `域名 ${mine.host} 已被路由 ${owner.id}（${owner.cluster}/${owner.instance}）使用`,
  })

  if (!base) {
    const owner = hostOwner(latest, mine.host, "")
    return owner ? duplicate(owner) : { ok: true, unchanged: false, body: mine }
  }

  const current = latest.find((r) => r.id === base.id)
  if (!current) {
    return {
      ok: false,
      reason: "deleted",
      error: `路由 ${base.id}（${base.host}）已被其他管理员删除，未保存；如需恢复请新建路由`,
    }
  }
  const was = canonRoute(base)
  const now = canonRoute(current)
  const result = { ...now }
  const nextForm = proxyRouteForm(current)
  const conflicts: string[] = []
  for (const field of MERGE_FIELDS) {
    if (sameFields(mine, was, field.keys)) continue
    // 本次改过的字段在表单里保留操作者的原始输入。
    copyFields(nextForm, form, field.keys)
    if (sameFields(now, was, field.keys) || sameFields(mine, now, field.keys)) copyFields(result, mine, field.keys)
    else conflicts.push(`${field.label}已被其他管理员改为 ${field.show(now)}`)
  }
  if (conflicts.length) {
    return {
      ok: false,
      reason: "conflict",
      latest: current,
      form: nextForm,
      error: `${conflicts.join("；")}，未保存。已按最新数据更新，你的输入保留；确认后再次保存会覆盖对方的修改`,
    }
  }

  const owner = hostOwner(latest, result.host, current.id)
  if (owner) return duplicate({ ...owner })
  if (MERGE_FIELDS.every((field) => sameFields(result, now, field.keys))) {
    return { ok: true, unchanged: true, body: result }
  }
  return { ok: true, unchanged: false, body: result }
}

// 删除只接受点击时显示的路由，已变化的路由留在刷新后的列表中。
export function planProxyRouteDelete(
  route: ProxyRoute,
  latest: ProxyRoute[],
): Result<ProxyRoute> {
  const current = latest.find((r) => r.id === route.id)
  if (!current) {
    return { ok: false, error: `路由 ${route.id}（${route.host}）已不存在，可能已被其他管理员删除；列表已刷新` }
  }
  const was = canonRoute(route)
  const now = canonRoute(current)
  if (!MERGE_FIELDS.every((field) => sameFields(was, now, field.keys))) {
    return { ok: false, error: `路由 ${route.id}（${current.host}）已被其他管理员修改，未删除；列表已刷新` }
  }
  return { ok: true, value: current }
}

export function proxyRouteSavedNotice(route: ProxyRoute): string {
  const head = `已保存 ${route.host} → ${route.cluster}/${route.instance} ${route.upstream}`
  if (!route.enabled) return `${head}（已停用，该域名的请求不会转发）`
  const hostname = parseUpstream(route.upstream)?.hostname || route.upstream
  return `${head}。Gateway 保存时不检查上游，请确认 ${hostname} 在该 Agent 的 proxy.allowedHosts 中，否则访问该域名会返回 502`
}

export type ProxyRouteTargetStatus = "online" | "offline" | "unregistered" | "unknown"

// instances 为 null 表示实例列表不可用，此时不做判断。
export function proxyRouteTargetStatus(
  target: { cluster: string; instance: string },
  instances: readonly { cluster: string; instance: string; status: string }[] | null,
): ProxyRouteTargetStatus {
  const cluster = target.cluster.trim()
  const instance = target.instance.trim()
  if (!instances || !cluster || !instance) return "unknown"
  const match = instances.find((i) => i.cluster === cluster && i.instance === instance)
  if (!match) return "unregistered"
  return match.status === "online" ? "online" : "offline"
}

export function proxyRouteTargetWarning(
  target: { cluster: string; instance: string },
  status: ProxyRouteTargetStatus,
): string {
  const key = `${target.cluster.trim()}/${target.instance.trim()}`
  if (status === "offline") return `目标 ${key} 当前离线：可以保存，但 Agent 重新连上之前访问该域名会返回 502`
  if (status === "unregistered") {
    return `目标 ${key} 未注册到 Gateway，请核对集群和实例名；该 Agent 连上之前访问该域名会返回 502`
  }
  return ""
}

export type ProxyRouteListState = "loading" | "empty" | "rows" | "error"

// 加载失败时不能显示「暂无代理路由」，否则操作者会以为线上没有路由。
export function proxyRouteListState(state: {
  loading: boolean
  loaded: boolean
  failed: boolean
  count: number
}): ProxyRouteListState {
  if (state.count > 0) return "rows"
  if (state.loading) return "loading"
  if (state.failed) return "error"
  return state.loaded ? "empty" : "loading"
}

export type ProxyRouteOp = "load" | "save" | "delete"

export interface ProxyRouteErrorView {
  kind: "auth" | "forbidden" | "error"
  message: string
  // 为 true 时界面要重读列表：失败本身说明列表已过期，或修改可能已经生效。
  reload: boolean
}

const OP_LABEL: Record<ProxyRouteOp, string> = {
  load: "加载代理路由",
  save: "保存代理路由",
  delete: "删除代理路由",
}

// Gateway 对代理路由返回的 400 文案（proxy_route_store.go / HandleAdminProxyRoutes）。
const GATEWAY_MESSAGES: Record<string, string> = {
  "proxy host is required": "请填写域名",
  "proxy host must be a hostname without scheme or path": "域名只填主机名，不要带协议或路径",
  "proxy host port is invalid": "域名中的端口无效",
  "proxy host must not contain a port": "域名不能带端口",
  "proxy host is invalid": HOST_INVALID,
  "proxy upstream must be an http(s) URL without credentials, query, or fragment": UPSTREAM_INVALID,
  "proxy route cluster and instance are required": SCOPE_REQUIRED,
  "proxy route id is required": "缺少路由 ID",
  "invalid proxy route request": "请求格式无效",
  "invalid proxy route updated_at": "路由版本（updated_at）格式无效，请刷新列表后重试",
  "missing id": "缺少路由 ID",
}

export function describeProxyRouteError(error: unknown, op: ProxyRouteOp): ProxyRouteErrorView {
  const label = OP_LABEL[op]
  const rawStatus = (error as { status?: unknown } | null)?.status
  const status = typeof rawStatus === "number" ? rawStatus : undefined
  // lib/admin.ts 把 Gateway 类别原文放在 category，message 是给页面看的中文说明。
  const rawCategory = (error as { category?: unknown } | null)?.category
  const raw = typeof rawCategory === "string" ? rawCategory.trim() : error instanceof Error ? error.message.trim() : ""
  const fail = (message: string, reload = false): ProxyRouteErrorView => ({
    kind: "error",
    message: `${label}失败：${message}`,
    reload,
  })
  const unknown = (detail: string): ProxyRouteErrorView => ({
    kind: "error",
    message: `${label}结果未知：${detail}，修改可能已经生效；列表已刷新，请核对路由当前状态`,
    reload: true,
  })
  const mutation = op !== "load"
  const authMessage = gatewayAuthMessage(raw)
  if (authMessage) return { kind: "auth", message: authMessage, reload: false }

  if (status === 401) {
    return { kind: "auth", message: "登录已失效或令牌已被撤销，请重新登录后再管理代理路由。", reload: false }
  }
  // Gateway 的平台管理员检查只回 "Forbidden"；控制台 gateway 策略的 403 带具体原因，按普通错误展示。
  if (status === 403 && raw === "Forbidden") {
    return { kind: "forbidden", message: "当前账号不是平台管理员，无权查看或修改代理路由。", reload: false }
  }
  if (status === undefined) {
    if (!(error instanceof Error)) return fail("未知错误")
    // 响应不是 JSON：通常是反向代理的错误页，请求可能已经到了 Gateway。
    if (error instanceof SyntaxError) {
      const detail = "服务端返回了无法识别的响应（可能是反向代理错误页）"
      return mutation ? unknown(detail) : fail(`${detail}，请稍后重试`)
    }
    // fetch 的网络错误是 TypeError，无法判断请求是否已送达。
    if (error instanceof TypeError && mutation) return unknown(`请求中断（${raw}）`)
    return fail(raw || "未知错误")
  }
  // 带 id 保存时 Gateway 不会按旧 id 重建已删除的路由，而是回 404。
  if (status === 404 && raw === "proxy route not found") {
    if (op === "save") return fail("该路由已被其他管理员删除，未保存；如需恢复请新建路由；列表已刷新", true)
    return fail("该路由已不存在，可能已被其他管理员删除；列表已刷新", true)
  }
  if (status === 404 && raw === "404 page not found") {
    return fail("当前 Gateway 没有代理路由管理接口（返回 404），请先升级 Gateway")
  }
  const hostInUse = "该域名已被其它路由使用，可能刚被其他管理员添加；列表已刷新"
  if (status === 409 && raw === "proxy host already in use") return fail(hostInUse, true)
  // 请求带了 updated_at，而路由在读取后又被改过。
  if (status === 409 && raw === "proxy route changed since it was read") {
    return fail("该路由刚被其他管理员修改，本次修改未保存；列表已刷新，再次保存会先与最新数据合并", true)
  }
  // 旧版 Gateway 域名重复时不回 409，而是把 SQLite 唯一约束错误作为 400 返回。
  if (status === 400 && raw.includes("UNIQUE constraint failed: proxy_routes.host")) return fail(hostInUse, true)
  if (status === 400 && GATEWAY_MESSAGES[raw]) return fail(GATEWAY_MESSAGES[raw])
  if (status === 503 && raw === "audit_unavailable") {
    return fail("Gateway 审计存储不可用，本次修改未执行，请稍后重试")
  }
  if (status === 503 && raw.startsWith("audit_completion_failed")) return unknown("Gateway 审计未写完")
  if ((status === 502 || status === 504) && mutation) return unknown(`${raw || "Gateway 无响应"}（HTTP ${status}）`)
  return fail(`${raw || "请求失败"}（HTTP ${status}）`)
}
