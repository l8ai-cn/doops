import { gatewayPolicyResponse } from "@/lib/gateway-policy"
import { proxyGatewayRequest } from "@/lib/server/gateway-proxy"

export const runtime = "nodejs"
export const dynamic = "force-dynamic"

const endpoints = new Set(["machines", "clusters", "node-links", "dashboard", "systems", "topology", "coverage", "monitor", "inspections", "machine-diagnostics"])

async function inventory(req: Request, ctx: { params: Promise<{ kind: string }> }) {
  const { kind } = await ctx.params
  if (!endpoints.has(kind)) {
    return Response.json({ error: "资产类型无效" }, { status: 404 })
  }
  try {
    const query = req.method === "GET" ? new URL(req.url).search : ""
    const path = `/v1/assets/${kind}${query}`
    return await proxyGatewayRequest(req, path, {
      errorLabel: "资产台账",
      csvDownload: kind === "coverage" && req.method === "GET" && new URL(req.url).searchParams.get("format") === "csv",
    })
  } catch (error) {
    return gatewayPolicyResponse(error) || Response.json({ error: "Gateway 策略检查失败" }, { status: 500 })
  }
}

export const GET = inventory
export const POST = inventory
