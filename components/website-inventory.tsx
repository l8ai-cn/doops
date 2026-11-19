"use client"

import { Globe2, RefreshCw } from "lucide-react"
import { useEffect, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { Button, Input } from "./ui"

type Site = { site_id: string; domain: string; current_release: string; previous_release: string | null; updated_at: string }

function isSite(value: unknown): value is Site {
  if (!value || typeof value !== "object") return false
  const row = value as Record<string, unknown>
  return [row.site_id, row.domain, row.current_release].every((field) => typeof field === "string" && field.length > 0)
    && (row.previous_release === null || typeof row.previous_release === "string")
    && typeof row.updated_at === "string" && Number.isFinite(Date.parse(row.updated_at))
}

export function WebsiteInventory({ session, target, sessionId, revision, running, onRefresh, onInspect }: {
  session: Session
  target: Target
  sessionId: string
  revision: number
  running: boolean
  onRefresh: () => void
  onInspect: (siteId: string, domain: string) => void
}) {
  const [sites, setSites] = useState<Site[]>([])
  const [error, setError] = useState("")
  const [loading, setLoading] = useState(true)
  const [filter, setFilter] = useState("")
  const { cluster, instance } = target

  useEffect(() => {
    const controller = new AbortController()
    async function load() {
      setLoading(true)
      setError("")
      try {
        let text = ""
        await callTool(session, { cluster, instance, tool: "doops_web", arguments: { session_id: sessionId, operation: "site-list" }, signal: controller.signal }, (event) => {
          if (event.type === "result") text = extractText(event.result)
        })
        if (controller.signal.aborted) return
        let result
        try { result = JSON.parse(text) } catch { throw new Error("站点列表响应不是有效的 JSON") }
        if (!Array.isArray(result?.sites) || !result.sites.every(isSite)) throw new Error("站点列表响应格式无效")
        setSites(result.sites)
      } catch (reason) {
        if (controller.signal.aborted) return
        setSites([])
        setError(reason instanceof Error ? reason.message : String(reason))
      } finally {
        if (!controller.signal.aborted) setLoading(false)
      }
    }
    void load()
    return () => controller.abort()
  }, [session, cluster, instance, sessionId, revision])

  const term = filter.trim().toLowerCase()
  const visible = sites.filter((site) => site.site_id.includes(term) || site.domain.includes(term))
  return (
    <section aria-label="已发布站点" className="card-surface min-w-0 p-4">
      <header className="mb-4 flex items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <Globe2 aria-hidden="true" className="size-5 shrink-0 text-primary" />
          <div className="min-w-0"><h2 className="text-base font-semibold">已发布站点</h2><p className="break-all text-xs text-muted-foreground">DoWeb · {cluster}/{instance}</p></div>
        </div>
        <Button size="sm" aria-label="刷新站点" onClick={onRefresh} disabled={loading || running}><RefreshCw aria-hidden="true" className={`size-3.5 ${loading ? "motion-safe:animate-spin" : ""}`} />刷新</Button>
      </header>
      {loading ? <p role="status" className="py-6 text-center text-sm text-muted-foreground">正在读取站点发布记录…</p> : error ? (
        <p role="alert" className="rounded-lg bg-warning/10 px-3 py-3 text-sm text-warning [overflow-wrap:anywhere]">读取站点失败 · {error}</p>
      ) : sites.length === 0 ? <p role="status" className="py-6 text-center text-sm text-muted-foreground">当前目标没有 DoWeb 发布记录</p> : (
        <>
          <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
            <span className="text-xs text-muted-foreground">{visible.length} 个站点</span>
            <Input type="search" scale="sm" aria-label="筛选站点" placeholder="按站点 ID 或域名筛选" className="w-full sm:w-64" value={filter} onChange={(event) => setFilter(event.target.value)} />
          </div>
          {visible.length === 0 ? <p role="status" className="py-6 text-center text-sm text-muted-foreground">没有匹配的站点</p> : (
            <div className="max-h-[55dvh] overflow-auto rounded-lg border border-border/60">
              <table className="min-w-[42rem] w-full text-left text-sm">
                <thead className="sticky top-0 bg-card text-xs text-muted-foreground"><tr>{["站点 ID", "域名", "当前版本", "上一版本", "更新时间"].map((label) => <th key={label} scope="col" className="px-3 py-2">{label}</th>)}</tr></thead>
                <tbody className="divide-y divide-border/50">{visible.map((site) => (
                  <tr key={site.site_id} className="hover:bg-muted/30">
                    <td className="px-3 py-2"><button type="button" title="带入站点巡检" disabled={running} onClick={() => onInspect(site.site_id, site.domain)} className="text-left font-mono text-primary hover:underline disabled:opacity-50">{site.site_id}</button></td>
                    <td className="px-3 py-2">{site.domain}</td><td className="px-3 py-2 font-mono">{site.current_release}</td><td className="px-3 py-2 font-mono">{site.previous_release ?? "—"}</td>
                    <td className="px-3 py-2 text-muted-foreground">{new Date(site.updated_at).toLocaleString()}</td>
                  </tr>
                ))}</tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  )
}
