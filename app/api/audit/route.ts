import { readAuth, toHttpBase } from "@/lib/gateway"

export const runtime = "nodejs"
export const dynamic = "force-dynamic"

// 透传到 gateway GET /v1/audit（需要 admin 权限的 token）
export async function GET(req: Request) {
  const { gateway, token } = readAuth(req)
  if (!gateway || !token) {
    return Response.json({ error: "缺少 gateway 地址或 token" }, { status: 400 })
  }

  const incoming = new URL(req.url)
  const qs = new URLSearchParams()
  for (const k of ["cluster", "instance", "session", "action", "status", "user_id", "limit"]) {
    const v = incoming.searchParams.get(k)
    if (v) qs.set(k, v)
  }

  try {
    const res = await fetch(`${toHttpBase(gateway)}/v1/audit?${qs.toString()}`, {
      headers: { Authorization: `Bearer ${token}` },
      cache: "no-store",
    })
    const text = await res.text()
    if (!res.ok) {
      return Response.json(
        { error: text || `查询审计失败 (${res.status})`, status: res.status },
        { status: res.status },
      )
    }
    return new Response(text, {
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
