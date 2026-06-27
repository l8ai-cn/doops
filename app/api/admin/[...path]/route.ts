import { readAuth, toHttpBase } from "@/lib/gateway"

export const runtime = "nodejs"
export const dynamic = "force-dynamic"

// 通用代理：把 /api/admin/<path> 透传到 gateway /v1/admin/<path>
// 覆盖 users / grants / tokens / instances / operations 等 admin 接口。
async function proxy(req: Request, ctx: { params: Promise<{ path: string[] }> }) {
  const { gateway, token } = readAuth(req)
  if (!gateway || !token) {
    return Response.json({ error: "缺少 gateway 地址或 token" }, { status: 400 })
  }
  const { path } = await ctx.params
  const sub = (path || []).join("/")
  const incoming = new URL(req.url)
  const qs = incoming.search // 含前导 ? 或为空字符串
  const target = `${toHttpBase(gateway)}/v1/admin/${sub}${qs}`

  const init: RequestInit = {
    method: req.method,
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
    cache: "no-store",
  }
  if (req.method !== "GET" && req.method !== "DELETE") {
    init.body = await req.text()
  }

  try {
    const res = await fetch(target, init)
    const text = await res.text()
    if (!res.ok) {
      return Response.json(
        { error: text || `请求失败 (${res.status})`, status: res.status },
        { status: res.status },
      )
    }
    return new Response(text || "{}", {
      status: 200,
      headers: { "Content-Type": "application/json" },
    })
  } catch (err) {
    return Response.json(
      { error: `无法连接 gateway: ${(err as Error).message}` },
      { status: 502 },
    )
  }
}

export const GET = proxy
export const POST = proxy
export const DELETE = proxy
export const PUT = proxy
export const PATCH = proxy
