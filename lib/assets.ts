import { consoleFetch } from "./network-error.ts"

export type AssetEndpoint = "machines" | "clusters" | "node-links" | "dashboard" | "systems" | "topology" | "coverage" | "monitor" | "inspections" | "machine-diagnostics"

export async function assetRequest<T>(kind: AssetEndpoint, body?: object, query?: URLSearchParams): Promise<T> {
  const response = await consoleFetch(`/api/assets/${kind}${query?.size ? `?${query}` : ""}`, body ? {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  } : { cache: "no-store" })
  const data = await response.json()
  if (!response.ok) {
    throw new Error(data.message || data.error || `读取资产失败（HTTP ${response.status}）`)
  }
  return data as T
}
