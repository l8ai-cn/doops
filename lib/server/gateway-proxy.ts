import { readAllowedAuth } from "@/lib/gateway-policy"
import { gatewayProxyErrorResponse, gatewayUnreachableResponse } from "@/lib/server/gateway-errors"
import { rejectCrossOriginMutation } from "@/lib/server/same-origin"

export async function proxyGatewayRequest(
  req: Request,
  path: string,
  options: { errorLabel?: string; csvDownload?: boolean } = {},
): Promise<Response> {
  const rejected = rejectCrossOriginMutation(req)
  if (rejected) return rejected
  const { gateway, token } = readAllowedAuth(req)
  if (!gateway || !token) {
    return Response.json({ error: "未建立 Gateway 会话", message: "未建立 Gateway 会话，请重新登录", status: 401 }, { status: 401 })
  }

  const init: RequestInit = {
    method: req.method,
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
    cache: "no-store",
    redirect: "manual",
    signal: req.signal,
  }
  if (req.method !== "GET" && req.method !== "HEAD") {
    init.body = await req.text()
  }

  const label = options.errorLabel || "Gateway 请求"
  try {
    const response = await fetch(`${gateway}${path}`, init)
    // 错误响应体可能带 Go/SQLite 错误、HTML 错误页或英文说明，只保留类别和中文说明。
    if (!response.ok) return await gatewayProxyErrorResponse(response, label)
    if (options.csvDownload) {
      if (response.headers.get("content-type")?.split(";")[0].trim().toLowerCase() !== "text/csv") {
        return Response.json({ error: "Gateway 返回了无效的 CSV 回执" }, { status: 502 })
      }
      return new Response(response.body, { status: response.status, headers: {
        "Content-Type": "text/csv; charset=utf-8",
        "Content-Disposition": 'attachment; filename="asset-coverage.csv"',
        "Cache-Control": "no-store",
      } })
    }
    const text = await response.text()
    return new Response(response.status === 204 ? null : text || "{}", {
      status: response.status,
      headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
    })
  } catch {
    return gatewayUnreachableResponse()
  }
}
