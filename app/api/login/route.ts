import { toHttpBase } from "@/lib/gateway"

export const runtime = "nodejs"
export const dynamic = "force-dynamic"

// 透传到 gateway POST /v1/auth/login，返回 user token
export async function POST(req: Request) {
  let body: { gateway?: string; username?: string; password?: string; name?: string }
  try {
    body = await req.json()
  } catch {
    return Response.json({ error: "请求体不是合法 JSON" }, { status: 400 })
  }

  const { gateway, username, password, name } = body
  if (!gateway || !username || !password) {
    return Response.json({ error: "缺少 gateway / username / password" }, { status: 400 })
  }

  try {
    const res = await fetch(`${toHttpBase(gateway)}/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password, name: name || "doops-console" }),
    })
    const text = await res.text()
    if (!res.ok) {
      return Response.json(
        { error: text || `登录失败 (${res.status})` },
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
