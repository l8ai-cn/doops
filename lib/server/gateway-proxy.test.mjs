import assert from "node:assert/strict"
import { mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import test, { after } from "node:test"
import { fileURLToPath, pathToFileURL } from "node:url"
import TypeScript from "typescript"
import { listUsers, setProxyRoute } from "../admin.ts"
import { getCredentialBundle, listCredentials } from "../credentials.ts"
import { describeProxyRouteError } from "../proxy-routes.ts"

const root = fileURLToPath(new URL("../../", import.meta.url))
const fixture = mkdtempSync(join(tmpdir(), "doops-gateway-proxy-test-"))
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
    (TypeScript.isImportDeclaration(statement) || TypeScript.isExportDeclaration(statement)) && statement.moduleSpecifier,
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
    javascript = javascript.slice(0, specifier.getStart(parsed)) +
      JSON.stringify(sourceModule(resolved.resolvedFileName)) +
      javascript.slice(specifier.end)
  }
  writeFileSync(output, javascript)
  return url
}

const { gatewayProxyErrorBody } = await import(sourceModule(join(root, "lib/server/gateway-errors.ts")))
const adminRoute = await import(sourceModule(join(root, "app/api/admin/[...path]/route.ts")))
const credentialsRoute = await import(sourceModule(join(root, "app/api/credentials/[[...path]]/route.ts")))
const bundlesRoute = await import(sourceModule(join(root, "app/api/credential-bundles/route.ts")))
const targetsRoute = await import(sourceModule(join(root, "app/api/targets/route.ts")))
const auditRoute = await import(sourceModule(join(root, "app/api/audit/route.ts")))

test("audit proxy forwards operation, cursor, time and observation visibility filters", async (t) => {
  let upstream
  t.mock.method(globalThis, "fetch", async (url) => {
    upstream = new URL(url)
    return Response.json({ events: [] })
  })
  const query = new URLSearchParams({
    correlation_id: "operation-42", before_id: "100", limit: "50",
    since: "2026-11-10T00:00:00Z", until: "2026-11-11T00:00:00Z", hide_upgrade_observe: "false",
  })
  const response = await auditRoute.GET(cookieRequest(`http://console.test/api/audit?${query}`))
  assert.equal(response.status, 200)
  assert.equal(upstream.pathname, "/v1/audit")
  assert.deepEqual(Object.fromEntries(upstream.searchParams), Object.fromEntries(query))
})

const gateway = "http://127.0.0.1:18080"
const TOKEN = "doops-synthetic-proxy-token-3a7e"
const CANARY = "doops-synthetic-canary-k16-9b4c"
const ENGLISH_AUTH = "Gateway user token is missing, invalid, expired, or revoked"
const ENGLISH_NEXT = "Run doops login --gateway <gateway-url> for this Gateway"
const authFailed = JSON.stringify({ error: "unauthorized", code: "gateway_auth_expired", auth_domain: "gateway", message: ENGLISH_AUTH, next_action: ENGLISH_NEXT })

// 代理给浏览器的 error 是调用方要解析的类别，message 是页面直接展示的中文说明。
const BODIES = [
  {
    name: "known JSON code",
    status: 401,
    raw: authFailed,
    want: { error: JSON.stringify({ code: "gateway_auth_expired" }), message: "Gateway token 已过期，请重新登录", status: 401, code: "gateway_auth_expired" },
  },
  {
    name: "unknown JSON code keeps the code",
    status: 403,
    raw: JSON.stringify({ code: "gateway_future_reason", message: `Some new English reason ${CANARY}` }),
    want: { error: JSON.stringify({ code: "gateway_future_reason" }), message: "管理请求失败：没有访问该 Gateway 的权限", status: 403, code: "gateway_future_reason" },
  },
  {
    name: "JSON without a usable code",
    status: 401,
    raw: JSON.stringify({ code: "Not A Code <b>", message: CANARY }),
    want: { error: "身份验证失败，请检查登录信息", message: "管理请求失败：身份验证失败，请检查登录信息", status: 401 },
  },
  {
    name: "lower_snake category passes through",
    status: 403,
    raw: "credential_forbidden\n",
    want: { error: "credential_forbidden", message: "管理请求失败：没有访问该 Gateway 的权限", status: 403 },
  },
  {
    name: "category with targets keeps the category and the targets",
    status: 502,
    raw: "credential_revocation_outcome_unknown: dev/local, dev/renamed\n",
    want: {
      error: "credential_revocation_outcome_unknown",
      message: "管理请求失败：Gateway 暂时不可用，请稍后再试",
      status: 502,
      targets: ["dev/local", "dev/renamed"],
    },
  },
  {
    name: "category followed by non-identifier text is unknown text",
    status: 503,
    raw: `target_offline: dev/local, password=${CANARY}`,
    want: { error: "Gateway 暂时不可用，请稍后再试", message: "管理请求失败：Gateway 暂时不可用，请稍后再试", status: 503 },
  },
  {
    name: "category with a Chinese message",
    status: 503,
    raw: "audit_unavailable\n",
    want: { error: "audit_unavailable", message: "Gateway 审计存储不可用，本次操作未执行，请稍后重试", status: 503 },
  },
  {
    name: "known plain-text phrase",
    status: 403,
    raw: "Forbidden\n",
    want: { error: "Forbidden", message: "没有权限执行该操作", status: 403 },
  },
  {
    name: "Go mux 404",
    status: 404,
    raw: "404 page not found\n",
    want: { error: "404 page not found", message: "Gateway 没有这个接口，请确认控制台与 Gateway 版本一致", status: 404 },
  },
  {
    name: "SQLite unique constraint keeps only the constraint",
    status: 400,
    raw: `constraint failed: UNIQUE constraint failed: users.name (2067) ${CANARY}`,
    want: { error: "UNIQUE constraint failed: users.name", message: "用户名已存在", status: 400 },
  },
  {
    name: "unknown Go error text",
    status: 500,
    raw: `sql: database is locked at /var/lib/doops/gateway.db ${CANARY}`,
    want: { error: "Gateway 暂时不可用，请稍后再试", message: "管理请求失败：Gateway 暂时不可用，请稍后再试", status: 500 },
  },
  {
    name: "non-JSON HTML error page",
    status: 502,
    raw: `<html><body>Bad Gateway from nginx ${CANARY}</body></html>`,
    want: { error: "Gateway 暂时不可用，请稍后再试", message: "管理请求失败：Gateway 暂时不可用，请稍后再试", status: 502 },
  },
  {
    name: "empty response",
    status: 409,
    raw: "",
    want: { error: "请求与当前状态冲突，请刷新后重试", message: "管理请求失败：请求与当前状态冲突，请刷新后重试", status: 409 },
  },
]

for (const item of BODIES) {
  test(`proxy error body: ${item.name}`, () => {
    const body = gatewayProxyErrorBody(item.status, item.raw, "管理请求")
    assert.deepEqual(body, item.want)
    const text = JSON.stringify(body)
    for (const leaked of [CANARY, ENGLISH_AUTH, ENGLISH_NEXT, "next_action", "auth_domain", "nginx", "database is locked", "/var/lib"]) {
      assert.ok(!text.includes(leaked), `${item.name}: must not contain ${JSON.stringify(leaked)}`)
    }
  })
}

function cookieRequest(url, init = {}) {
  return new Request(url, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      Origin: new URL(url).origin,
      Cookie: `doops_session_gateway=${encodeURIComponent(gateway)}; doops_session_token=${TOKEN}`,
    },
  })
}

// 每个走 proxyGatewayRequest 的路由都不把未知文本原样交给浏览器，状态码不变。
const ROUTES = [
  { name: "/api/admin", call: () => adminRoute.GET(cookieRequest("http://console.test/api/admin/users"), { params: Promise.resolve({ path: ["users"] }) }), label: "管理请求" },
  { name: "/api/credentials", call: () => credentialsRoute.GET(cookieRequest("http://console.test/api/credentials?view=managed"), { params: Promise.resolve({}) }), label: "凭据请求" },
  { name: "/api/credential-bundles", call: () => bundlesRoute.GET(cookieRequest("http://console.test/api/credential-bundles?name=x")), label: "凭据组请求" },
  { name: "/api/targets", call: () => targetsRoute.GET(cookieRequest("http://console.test/api/targets")), label: "查询目标" },
  { name: "/api/audit", call: () => auditRoute.GET(cookieRequest("http://console.test/api/audit?limit=5")), label: "查询审计" },
]

for (const route of ROUTES) {
  test(`${route.name}: unknown Gateway text is replaced, status is kept`, async (t) => {
    t.mock.method(globalThis, "fetch", async () => new Response(`panic: runtime error ${CANARY}\n`, { status: 500 }))
    const response = await route.call()
    assert.equal(response.status, 500)
    const body = await response.json()
    assert.deepEqual(body, {
      error: "Gateway 暂时不可用，请稍后再试",
      message: `${route.label}失败：Gateway 暂时不可用，请稍后再试`,
      status: 500,
    })
  })

  test(`${route.name}: a network error does not reach the browser`, async (t) => {
    t.mock.method(globalThis, "fetch", async () => { throw new TypeError(`connect ECONNREFUSED 10.0.0.5:42222 ${CANARY}`) })
    const response = await route.call()
    assert.equal(response.status, 502)
    const text = await response.text()
    assert.ok(!text.includes(CANARY) && !text.includes("ECONNREFUSED") && !text.includes("10.0.0.5"))
    assert.match(JSON.parse(text).error, /^无法连接 gateway:/)
  })
}

test("a successful Gateway response is forwarded unchanged", async (t) => {
  t.mock.method(globalThis, "fetch", async () => Response.json({ users: [{ id: "usr_1", name: "synthetic" }] }))
  const response = await adminRoute.GET(cookieRequest("http://console.test/api/admin/users"), { params: Promise.resolve({ path: ["users"] }) })
  assert.equal(response.status, 200)
  assert.deepEqual(await response.json(), { users: [{ id: "usr_1", name: "synthetic" }] })
})

// 从 Gateway 响应经过代理，再到页面自己的错误映射：类别原文仍能识别，未知文本只剩中文。
async function throughAdminRoute(t, status, raw, subpath) {
  t.mock.method(globalThis, "fetch", async () => new Response(raw, { status }))
  const proxied = await adminRoute.POST(
    cookieRequest(`http://console.test/api/admin/${subpath}`, { method: "POST", body: "{}" }),
    { params: Promise.resolve({ path: subpath.split("/") }) },
  )
  const forwarded = await proxied.text()
  t.mock.restoreAll()
  t.mock.method(globalThis, "fetch", async () => new Response(forwarded, { status: proxied.status }))
}

const route = { host: "app.example.com", cluster: "prod", instance: "node-1", upstream: "http://127.0.0.1:8080/", enabled: true }
const PROXY_ROUTE_PAGE = [
  { name: "host already in use", status: 409, raw: "proxy host already in use\n", message: "保存代理路由失败：该域名已被其它路由使用，可能刚被其他管理员添加；列表已刷新", reload: true },
  { name: "route changed", status: 409, raw: "proxy route changed since it was read\n", message: /刚被其他管理员修改/, reload: true },
  { name: "old Gateway unique constraint", status: 400, raw: `constraint failed: UNIQUE constraint failed: proxy_routes.host (2067)`, message: /该域名已被其它路由使用/, reload: true },
  { name: "not platform admin", status: 403, raw: "Forbidden\n", kind: "forbidden" },
  { name: "expired login", status: 401, raw: authFailed, kind: "auth" },
  { name: "audit not finished", status: 503, raw: "audit_completion_failed; mutation outcome requires verification\n", message: /结果未知：Gateway 审计未写完/, reload: true },
  { name: "unknown text", status: 500, raw: `sql: database is locked ${CANARY}`, message: "保存代理路由失败：Gateway 暂时不可用，请稍后再试（HTTP 500）", reload: false },
]

for (const item of PROXY_ROUTE_PAGE) {
  test(`proxy route page still maps ${item.name}`, async (t) => {
    await throughAdminRoute(t, item.status, item.raw, "proxy-routes")
    const err = await setProxyRoute({ gateway }, route).then(() => null, (e) => e)
    assert.ok(err, "the request must fail")
    const view = describeProxyRouteError(err, "save")
    if (item.kind) assert.equal(view.kind, item.kind)
    if (typeof item.message === "string") assert.equal(view.message, item.message)
    else if (item.message) assert.match(view.message, item.message)
    if (item.reload !== undefined) assert.equal(view.reload, item.reload)
    assert.ok(!view.message.includes(CANARY) && !view.message.includes("database is locked"))
  })
}

test("admin pages show the Chinese message, not the Gateway text", async (t) => {
  for (const [raw, status, want] of [
    ["user not found\n", 404, "用户不存在，请刷新用户列表"],
    [`sql: database is locked ${CANARY}`, 500, "管理请求失败：Gateway 暂时不可用，请稍后再试"],
    [authFailed, 401, "Gateway token 已过期，请重新登录"],
  ]) {
    t.mock.method(globalThis, "fetch", async () => new Response(raw, { status }))
    const proxied = await adminRoute.GET(cookieRequest("http://console.test/api/admin/users"), { params: Promise.resolve({ path: ["users"] }) })
    const forwarded = await proxied.text()
    t.mock.restoreAll()
    t.mock.method(globalThis, "fetch", async () => new Response(forwarded, { status: proxied.status }))
    const err = await listUsers({ gateway }).then(() => null, (e) => e)
    assert.equal(err.message, want)
    assert.equal(err.status, status)
    t.mock.restoreAll()
  }
})

async function throughCredentialsRoute(t, status, raw) {
  t.mock.method(globalThis, "fetch", async () => new Response(raw, { status }))
  const proxied = await credentialsRoute.GET(cookieRequest("http://console.test/api/credentials?view=managed"), { params: Promise.resolve({}) })
  const forwarded = await proxied.text()
  t.mock.restoreAll()
  t.mock.method(globalThis, "fetch", async () => new Response(forwarded, { status: proxied.status }))
}

const target = { cluster: "prod", instance: "node-1", key: "prod/node-1" }
const CREDENTIAL_PAGE = [
  { name: "category", status: 404, raw: "credential_not_found\n", message: /凭据不存在或已删除.*（credential_not_found）/ },
  { name: "Forbidden phrase", status: 403, raw: "Forbidden\n", message: /owner 或平台管理员.*（Forbidden）/ },
  { name: "request shape phrase", status: 400, raw: "invalid credential list view\n", message: /版本一致（HTTP 400）/ },
  { name: "expired login", status: 401, raw: authFailed, message: /重新登录（gateway_auth_expired）/ },
  { name: "unknown text", status: 500, raw: `pq: payload ${CANARY}`, message: /无法识别的 Gateway 响应.*（HTTP 500）/ },
]

for (const item of CREDENTIAL_PAGE) {
  test(`credential page still maps ${item.name}`, async (t) => {
    await throughCredentialsRoute(t, item.status, item.raw)
    const err = await listCredentials({ gateway }, target, "managed").then(() => null, (e) => e)
    assert.match(err.message, item.message)
    assert.ok(!err.message.includes(CANARY))
  })
}

test("credential bundle page still maps its categories", async (t) => {
  for (const [raw, status, want] of [
    ["credential_bundle_not_found\n", 404, /未找到该凭据组.*（credential_bundle_not_found）/],
    [`sql: ${CANARY}`, 500, /Gateway 暂时不可用（HTTP 500）/],
  ]) {
    t.mock.method(globalThis, "fetch", async () => new Response(raw, { status }))
    const proxied = await bundlesRoute.GET(cookieRequest("http://console.test/api/credential-bundles?name=release"))
    const forwarded = await proxied.text()
    t.mock.restoreAll()
    t.mock.method(globalThis, "fetch", async () => new Response(forwarded, { status: proxied.status }))
    const err = await getCredentialBundle({ gateway }, "release").then(() => null, (e) => e)
    assert.match(err.message, want)
    assert.ok(!err.message.includes(CANARY))
    t.mock.restoreAll()
  }
})
