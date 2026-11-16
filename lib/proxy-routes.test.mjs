import assert from "node:assert/strict"
import test from "node:test"
import {
  EMPTY_PROXY_ROUTE_FORM,
  describeProxyRouteError,
  normalizeProxyHost,
  planProxyRouteDelete,
  planProxyRouteSave,
  proxyRouteBody,
  proxyRouteForm,
  proxyRouteListState,
  proxyRouteSavedNotice,
  proxyRouteTargetStatus,
  proxyRouteTargetWarning,
  validateProxyUpstream,
} from "./proxy-routes.ts"

const route = (over = {}) => ({
  id: "route_a",
  host: "app.example.com",
  cluster: "prod",
  instance: "node-1",
  upstream: "http://127.0.0.1:8080/",
  enabled: true,
  ...over,
})
const other = route({ id: "route_b", host: "other.example.com", cluster: "staging", instance: "node-2" })
const httpError = (status, message) => Object.assign(new Error(message), { status })

test("host normalization matches the Gateway rules", () => {
  const cases = [
    { in: "App.Example.COM", out: "app.example.com" },
    { in: " app.example.com. ", out: "app.example.com" },
    { in: "app.example.com:443", out: "app.example.com" },
    { in: "10.0.0.1", out: "10.0.0.1" },
    { in: "[::1]:8080", out: "::1" },
    { in: "", err: "请填写域名" },
    { in: "https://app.example.com", err: "域名只填主机名，不要带协议或路径" },
    { in: "app.example.com/path", err: "域名只填主机名，不要带协议或路径" },
    { in: "app.example.com:", err: "域名中的端口无效" },
    { in: "::1", err: "域名不能带端口" },
    { in: "-bad.example.com", err: "域名格式无效" },
    { in: "bad-.example.com", err: "域名格式无效" },
    { in: "a..b", err: "域名格式无效" },
    { in: "under_score.example", err: "域名格式无效" },
    { in: `${"a".repeat(64)}.example`, err: "域名格式无效" },
  ]
  for (const c of cases) {
    const got = normalizeProxyHost(c.in)
    if (c.err) assert.deepEqual(got, { ok: false, error: c.err }, c.in)
    else assert.deepEqual(got, { ok: true, value: c.out }, c.in)
  }
})

test("upstream is validated and normalized like the Gateway", () => {
  const invalid = "上游必须是 http(s) 地址，且不能包含账号密码、查询参数或锚点"
  const cases = [
    { in: "http://127.0.0.1:8080", out: "http://127.0.0.1:8080/" },
    { in: " https://internal.example.com/app/ ", out: "https://internal.example.com/app" },
    { in: "HTTP://Internal.example.com//", out: "http://Internal.example.com/" },
    { in: "http://[::1]:8080/api", out: "http://[::1]:8080/api" },
    // 浏览器 URL 会把下面两种补成 http://host/，Gateway 的 Go url.Parse 不会。
    { in: "http:/127.0.0.1:8080", err: invalid },
    { in: "http:127.0.0.1:8080", err: invalid },
    { in: "http://@127.0.0.1", err: invalid },
    { in: "http://user:pass@127.0.0.1", err: invalid },
    { in: "ftp://127.0.0.1", err: invalid },
    { in: "127.0.0.1:8080", err: invalid },
    { in: "http://127.0.0.1/?a=1", err: invalid },
    { in: "http://127.0.0.1/#frag", err: invalid },
    { in: "http://", err: invalid },
    { in: "", err: invalid },
  ]
  for (const c of cases) {
    const got = validateProxyUpstream(c.in)
    if (c.err) assert.deepEqual(got, { ok: false, error: c.err }, c.in)
    else assert.deepEqual(got, { ok: true, value: c.out }, c.in)
  }
})

test("form becomes an upsert body with every field", () => {
  const cases = [
    {
      name: "new route omits id",
      form: { ...EMPTY_PROXY_ROUTE_FORM, host: "App.Example.com", cluster: " prod ", instance: " node-1 ", upstream: "http://127.0.0.1:8080" },
      want: { ok: true, value: { host: "app.example.com", cluster: "prod", instance: "node-1", upstream: "http://127.0.0.1:8080/", enabled: true } },
    },
    {
      name: "edit keeps id and disabled flag",
      form: proxyRouteForm({ id: "route_1", host: "a.example", cluster: "c", instance: "i", upstream: "http://h", enabled: false }),
      want: { ok: true, value: { id: "route_1", host: "a.example", cluster: "c", instance: "i", upstream: "http://h/", enabled: false } },
    },
    {
      name: "missing scope",
      form: { ...EMPTY_PROXY_ROUTE_FORM, host: "a.example", upstream: "http://h" },
      want: { ok: false, error: "请填写目标集群和实例" },
    },
    {
      name: "bad host reported first",
      form: { ...EMPTY_PROXY_ROUTE_FORM, host: "http://a", cluster: "c", instance: "i", upstream: "bad" },
      want: { ok: false, error: "域名只填主机名，不要带协议或路径" },
    },
  ]
  for (const c of cases) assert.deepEqual(proxyRouteBody(c.form), c.want, c.name)
})

test("save plan checks duplicates and concurrent changes against the latest list", () => {
  const newForm = (over) => ({ ...EMPTY_PROXY_ROUTE_FORM, cluster: "prod", instance: "node-1", upstream: "http://127.0.0.1:9000", ...over })
  const editForm = (over) => ({ ...proxyRouteForm(route()), ...over })
  const cases = [
    {
      name: "new route",
      form: newForm({ host: "new.example.com" }), base: null, latest: [route(), other],
      want: { ok: true, unchanged: false, body: { host: "new.example.com", cluster: "prod", instance: "node-1", upstream: "http://127.0.0.1:9000/", enabled: true } },
    },
    {
      name: "new route reuses a host",
      form: newForm({ host: "app.example.com" }), base: null, latest: [route(), other],
      want: { ok: false, reason: "duplicate", error: "域名 app.example.com 已被路由 route_a（prod/node-1）使用" },
    },
    {
      name: "new route reuses a host with other case and trailing dot",
      form: newForm({ host: "APP.Example.com." }), base: null, latest: [route(), other],
      want: { ok: false, reason: "duplicate", error: "域名 app.example.com 已被路由 route_a（prod/node-1）使用" },
    },
    {
      name: "edit keeps its own host",
      form: editForm({ upstream: "http://127.0.0.1:9000" }), base: route(), latest: [route(), other],
      want: { ok: true, unchanged: false, body: { ...route(), upstream: "http://127.0.0.1:9000/" } },
    },
    {
      name: "edit renames onto another route's host",
      form: editForm({ host: "Other.Example.com" }), base: route(), latest: [route(), other],
      want: { ok: false, reason: "duplicate", error: "域名 other.example.com 已被路由 route_b（staging/node-2）使用" },
    },
    {
      name: "edit renames the host",
      form: editForm({ host: "renamed.example.com" }), base: route(), latest: [route(), other],
      want: { ok: true, unchanged: false, body: { ...route(), host: "renamed.example.com" } },
    },
    {
      name: "edit without changes",
      form: editForm({ host: "APP.example.com.", upstream: "http://127.0.0.1:8080" }), base: route(), latest: [route()],
      want: { ok: true, unchanged: true, body: route() },
    },
    {
      name: "edit after the route was deleted",
      form: editForm({ enabled: false }), base: route(), latest: [other],
      want: { ok: false, reason: "deleted", error: "路由 route_a（app.example.com）已被其他管理员删除，未保存；如需恢复请新建路由" },
    },
    {
      name: "edit keeps another admin's change to a field it did not touch",
      form: editForm({ enabled: false }), base: route(), latest: [route({ upstream: "http://10.0.0.5:8080/" })],
      want: { ok: true, unchanged: false, body: route({ upstream: "http://10.0.0.5:8080/", enabled: false }) },
    },
    {
      name: "edit and another admin changed the same field differently",
      form: editForm({ upstream: "http://127.0.0.1:9000" }), base: route(), latest: [route({ upstream: "http://10.0.0.5:8080/" })],
      want: {
        ok: false, reason: "conflict", latest: route({ upstream: "http://10.0.0.5:8080/" }),
        form: { ...proxyRouteForm(route()), upstream: "http://127.0.0.1:9000" },
        error: "上游已被其他管理员改为 http://10.0.0.5:8080/，未保存。已按最新数据更新，你的输入保留；确认后再次保存会覆盖对方的修改",
      },
    },
    {
      name: "conflict refreshes the fields this edit did not touch",
      form: editForm({ upstream: "http://127.0.0.1:9000", enabled: false }), base: route(),
      latest: [route({ upstream: "http://10.0.0.5:8080/", cluster: "prod-b", instance: "node-7", enabled: false })],
      want: {
        ok: false, reason: "conflict",
        latest: route({ upstream: "http://10.0.0.5:8080/", cluster: "prod-b", instance: "node-7", enabled: false }),
        form: { ...proxyRouteForm(route()), cluster: "prod-b", instance: "node-7", upstream: "http://127.0.0.1:9000", enabled: false },
        error: "上游已被其他管理员改为 http://10.0.0.5:8080/，未保存。已按最新数据更新，你的输入保留；确认后再次保存会覆盖对方的修改",
      },
    },
    {
      name: "cluster and instance conflict as one target",
      form: editForm({ host: "mine.example.com", instance: "node-3" }), base: route(),
      latest: [route({ host: "theirs.example.com", cluster: "prod-b" })],
      want: {
        ok: false, reason: "conflict", latest: route({ host: "theirs.example.com", cluster: "prod-b" }),
        form: { ...proxyRouteForm(route()), host: "mine.example.com", instance: "node-3" },
        error: "域名已被其他管理员改为 theirs.example.com；目标已被其他管理员改为 prod-b/node-1，未保存。已按最新数据更新，你的输入保留；确认后再次保存会覆盖对方的修改",
      },
    },
    {
      name: "edit and another admin made the same change",
      form: editForm({ upstream: "http://10.0.0.5:8080" }), base: route(), latest: [route({ upstream: "http://10.0.0.5:8080/" })],
      want: { ok: true, unchanged: true, body: route({ upstream: "http://10.0.0.5:8080/" }) },
    },
    {
      name: "toggle on a stale row keeps the latest upstream",
      form: { ...proxyRouteForm(route()), enabled: false }, base: route(), latest: [route({ upstream: "http://10.0.0.5:8080/" })],
      want: { ok: true, unchanged: false, body: route({ upstream: "http://10.0.0.5:8080/", enabled: false }) },
    },
    {
      name: "toggle after another admin already toggled",
      form: { ...proxyRouteForm(route()), enabled: false }, base: route(), latest: [route({ enabled: false })],
      want: { ok: true, unchanged: true, body: route({ enabled: false }) },
    },
    {
      name: "invalid form",
      form: newForm({ host: "" }), base: null, latest: [],
      want: { ok: false, reason: "invalid", error: "请填写域名" },
    },
  ]
  for (const c of cases) assert.deepEqual(planProxyRouteSave(c.form, c.base, c.latest), c.want, c.name)
})

test("Gateway errors map to an operator message and a next step", () => {
  const unauthorized = JSON.stringify({ error: "unauthorized", code: "gateway_auth_expired", message: "Gateway user token is missing, invalid, expired, or revoked" })
  const auth = { kind: "auth", message: "登录已失效或令牌已被撤销，请重新登录后再管理代理路由。", reload: false }
  const forbidden = { kind: "forbidden", message: "当前账号不是平台管理员，无权查看或修改代理路由。", reload: false }
  const cases = [
    { name: "gateway 401", op: "load", err: httpError(401, unauthorized), want: { kind: "auth", message: "Gateway token 已过期，请重新登录", reload: false } },
    { name: "console without session", op: "save", err: httpError(401, "未建立 Gateway 会话"), want: auth },
    { name: "not platform admin", op: "load", err: httpError(403, "Forbidden"), want: forbidden },
    { name: "lost admin while saving", op: "save", err: httpError(403, "Forbidden"), want: forbidden },
    {
      name: "console gateway policy", op: "load", err: httpError(403, "该 gateway 未获控制台授权"),
      want: { kind: "error", message: "加载代理路由失败：该 gateway 未获控制台授权（HTTP 403）", reload: false },
    },
    {
      name: "old gateway without the endpoint", op: "load", err: httpError(404, "404 page not found"),
      want: { kind: "error", message: "加载代理路由失败：当前 Gateway 没有代理路由管理接口（返回 404），请先升级 Gateway", reload: false },
    },
    {
      name: "delete an already deleted route", op: "delete", err: httpError(404, "proxy route not found"),
      want: { kind: "error", message: "删除代理路由失败：该路由已不存在，可能已被其他管理员删除；列表已刷新", reload: true },
    },
    {
      name: "save after the route was deleted", op: "save", err: httpError(404, "proxy route not found"),
      want: { kind: "error", message: "保存代理路由失败：该路由已被其他管理员删除，未保存；如需恢复请新建路由；列表已刷新", reload: true },
    },
    {
      name: "host already in use", op: "save", err: httpError(409, "proxy host already in use"),
      want: { kind: "error", message: "保存代理路由失败：该域名已被其它路由使用，可能刚被其他管理员添加；列表已刷新", reload: true },
    },
    {
      name: "stale updated_at", op: "save", err: httpError(409, "proxy route changed since it was read"),
      want: { kind: "error", message: "保存代理路由失败：该路由刚被其他管理员修改，本次修改未保存；列表已刷新，再次保存会先与最新数据合并", reload: true },
    },
    {
      name: "malformed updated_at", op: "save", err: httpError(400, "invalid proxy route updated_at"),
      want: { kind: "error", message: "保存代理路由失败：路由版本（updated_at）格式无效，请刷新列表后重试", reload: false },
    },
    {
      name: "old gateway host unique constraint", op: "save",
      err: httpError(400, "constraint failed: UNIQUE constraint failed: proxy_routes.host (2067)"),
      want: { kind: "error", message: "保存代理路由失败：该域名已被其它路由使用，可能刚被其他管理员添加；列表已刷新", reload: true },
    },
    {
      name: "gateway host validation", op: "save", err: httpError(400, "proxy host is invalid"),
      want: { kind: "error", message: "保存代理路由失败：域名格式无效", reload: false },
    },
    {
      name: "gateway upstream validation", op: "save",
      err: httpError(400, "proxy upstream must be an http(s) URL without credentials, query, or fragment"),
      want: { kind: "error", message: "保存代理路由失败：上游必须是 http(s) 地址，且不能包含账号密码、查询参数或锚点", reload: false },
    },
    {
      name: "gateway scope validation", op: "save", err: httpError(400, "proxy route cluster and instance are required"),
      want: { kind: "error", message: "保存代理路由失败：请填写目标集群和实例", reload: false },
    },
    {
      name: "unexpected 409", op: "save", err: httpError(409, "conflict"),
      want: { kind: "error", message: "保存代理路由失败：conflict（HTTP 409）", reload: false },
    },
    {
      name: "audit store down blocks the mutation", op: "delete", err: httpError(503, "audit_unavailable"),
      want: { kind: "error", message: "删除代理路由失败：Gateway 审计存储不可用，本次修改未执行，请稍后重试", reload: false },
    },
    {
      name: "audit completion failed after the mutation", op: "save",
      err: httpError(503, "audit_completion_failed; mutation outcome requires verification"),
      want: { kind: "error", message: "保存代理路由结果未知：Gateway 审计未写完，修改可能已经生效；列表已刷新，请核对路由当前状态", reload: true },
    },
    {
      name: "gateway unreachable", op: "load", err: httpError(502, "无法连接 gateway: fetch failed"),
      want: { kind: "error", message: "加载代理路由失败：无法连接 gateway: fetch failed（HTTP 502）", reload: false },
    },
    {
      name: "network error without status", op: "load", err: new Error("Failed to fetch"),
      want: { kind: "error", message: "加载代理路由失败：Failed to fetch", reload: false },
    },
    {
      name: "empty error", op: "delete", err: null,
      want: { kind: "error", message: "删除代理路由失败：未知错误", reload: false },
    },
    {
      name: "save interrupted by the network", op: "save", err: new TypeError("Failed to fetch"),
      want: { kind: "error", message: "保存代理路由结果未知：请求中断（Failed to fetch），修改可能已经生效；列表已刷新，请核对路由当前状态", reload: true },
    },
    {
      name: "load interrupted by the network", op: "load", err: new TypeError("Failed to fetch"),
      want: { kind: "error", message: "加载代理路由失败：Failed to fetch", reload: false },
    },
    {
      name: "reverse proxy error page on delete", op: "delete", err: new SyntaxError("Unexpected token '<'"),
      want: { kind: "error", message: "删除代理路由结果未知：服务端返回了无法识别的响应（可能是反向代理错误页），修改可能已经生效；列表已刷新，请核对路由当前状态", reload: true },
    },
    {
      name: "reverse proxy error page on load", op: "load", err: new SyntaxError("Unexpected token '<'"),
      want: { kind: "error", message: "加载代理路由失败：服务端返回了无法识别的响应（可能是反向代理错误页），请稍后重试", reload: false },
    },
    {
      name: "gateway timeout while saving", op: "save", err: httpError(504, "Gateway Timeout"),
      want: { kind: "error", message: "保存代理路由结果未知：Gateway Timeout（HTTP 504），修改可能已经生效；列表已刷新，请核对路由当前状态", reload: true },
    },
    {
      name: "unrecognized 400 keeps the raw text", op: "save", err: httpError(400, "something new"),
      want: { kind: "error", message: "保存代理路由失败：something new（HTTP 400）", reload: false },
    },
    {
      name: "store error on delete", op: "delete", err: httpError(500, "database is locked"),
      want: { kind: "error", message: "删除代理路由失败：database is locked（HTTP 500）", reload: false },
    },
  ]
  for (const c of cases) assert.deepEqual(describeProxyRouteError(c.err, c.op), c.want, c.name)
})

test("target status warns about offline and unregistered targets", () => {
  const instances = [
    { cluster: "prod", instance: "node-1", status: "online" },
    { cluster: "staging", instance: "node-2", status: "offline" },
  ]
  const cases = [
    { name: "online", target: { cluster: "prod", instance: "node-1" }, instances, status: "online", warning: "" },
    {
      name: "offline", target: { cluster: "staging", instance: "node-2" }, instances, status: "offline",
      warning: "目标 staging/node-2 当前离线：可以保存，但 Agent 重新连上之前访问该域名会返回 502",
    },
    {
      name: "unregistered", target: { cluster: " prod ", instance: "node-9" }, instances, status: "unregistered",
      warning: "目标 prod/node-9 未注册到 Gateway，请核对集群和实例名；该 Agent 连上之前访问该域名会返回 502",
    },
    { name: "instances unavailable", target: { cluster: "prod", instance: "node-1" }, instances: null, status: "unknown", warning: "" },
    { name: "incomplete target", target: { cluster: "prod", instance: "" }, instances, status: "unknown", warning: "" },
  ]
  for (const c of cases) {
    const status = proxyRouteTargetStatus(c.target, c.instances)
    assert.equal(status, c.status, c.name)
    assert.equal(proxyRouteTargetWarning(c.target, status), c.warning, c.name)
  }
})

test("saved notice reminds that the Agent allowlist is not checked on save", () => {
  const cases = [
    {
      route: route(),
      want: "已保存 app.example.com → prod/node-1 http://127.0.0.1:8080/。Gateway 保存时不检查上游，请确认 127.0.0.1 在该 Agent 的 proxy.allowedHosts 中，否则访问该域名会返回 502",
    },
    {
      route: route({ upstream: "http://[::1]:8080/" }),
      want: "已保存 app.example.com → prod/node-1 http://[::1]:8080/。Gateway 保存时不检查上游，请确认 ::1 在该 Agent 的 proxy.allowedHosts 中，否则访问该域名会返回 502",
    },
    {
      route: route({ upstream: "http://Internal.Example.com/app" }),
      want: "已保存 app.example.com → prod/node-1 http://Internal.Example.com/app。Gateway 保存时不检查上游，请确认 internal.example.com 在该 Agent 的 proxy.allowedHosts 中，否则访问该域名会返回 502",
    },
    {
      route: route({ enabled: false }),
      want: "已保存 app.example.com → prod/node-1 http://127.0.0.1:8080/（已停用，该域名的请求不会转发）",
    },
  ]
  for (const c of cases) assert.equal(proxyRouteSavedNotice(c.route), c.want, c.route.upstream)
})

test("delete uses the displayed route and rejects concurrent changes without a confirmation prompt", () => {
  const cases = [
    {
      name: "unchanged", latest: [route(), other],
      want: { ok: true, value: route() },
    },
    {
      name: "renamed by another admin", latest: [route({ host: "renamed.example.com" })],
      want: {
        ok: false,
        error: "路由 route_a（renamed.example.com）已被其他管理员修改，未删除；列表已刷新",
      },
    },
    {
      name: "already deleted", latest: [other],
      want: { ok: false, error: "路由 route_a（app.example.com）已不存在，可能已被其他管理员删除；列表已刷新" },
    },
  ]
  for (const c of cases) assert.deepEqual(planProxyRouteDelete(route(), c.latest), c.want, c.name)
  for (const patch of [{ cluster: "another" }, { instance: "node-2" }, { upstream: "http://10.0.0.5/" }, { enabled: false }]) {
    assert.equal(planProxyRouteDelete(route(), [route(patch)]).ok, false, JSON.stringify(patch))
  }
  const refreshed = route({ host: "renamed.example.com" })
  assert.deepEqual(planProxyRouteDelete(refreshed, [refreshed]), { ok: true, value: refreshed })
})

test("list state never reports an empty list it failed to load", () => {
  const cases = [
    { name: "before first load", in: { loading: false, loaded: false, failed: false, count: 0 }, want: "loading" },
    { name: "first load", in: { loading: true, loaded: false, failed: false, count: 0 }, want: "loading" },
    { name: "loaded empty", in: { loading: false, loaded: true, failed: false, count: 0 }, want: "empty" },
    { name: "loaded rows", in: { loading: false, loaded: true, failed: false, count: 2 }, want: "rows" },
    { name: "refreshing rows", in: { loading: true, loaded: true, failed: false, count: 2 }, want: "rows" },
    { name: "first load failed", in: { loading: false, loaded: false, failed: true, count: 0 }, want: "error" },
    { name: "refresh failed after an empty load", in: { loading: false, loaded: true, failed: true, count: 0 }, want: "error" },
    { name: "refresh failed with rows", in: { loading: false, loaded: true, failed: true, count: 2 }, want: "rows" },
    { name: "retrying after a failure", in: { loading: true, loaded: true, failed: true, count: 0 }, want: "loading" },
  ]
  for (const c of cases) assert.equal(proxyRouteListState(c.in), c.want, c.name)
})
