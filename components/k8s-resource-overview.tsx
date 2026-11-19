"use client"

import { Boxes, RefreshCw } from "lucide-react"
import { useEffect, useState } from "react"
import { fetchK8sOverview, K8S_OVERVIEW_QUERIES, type K8sOverview, type K8sOverviewKey, type K8sResource } from "@/lib/k8s-overview"
import type { Session, Target } from "@/lib/client"
import { Button, Input } from "./ui"

export function K8sResourceOverview({ session, target, sessionId, running, onOpenDetails }: {
  session: Session
  target: Target
  sessionId: string
  running: boolean
  onOpenDetails: (kind: K8sOverviewKey, resource: K8sResource) => void
}) {
  const [overview, setOverview] = useState<K8sOverview | null>(null)
  const [refreshKey, setRefreshKey] = useState(0)
  const [loading, setLoading] = useState(true)
  const [kind, setKind] = useState<K8sOverviewKey>("pods")
  const [filter, setFilter] = useState("")
  const { cluster, instance } = target

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    void fetchK8sOverview(session, { cluster, instance }, sessionId, controller.signal).then((result) => {
      if (controller.signal.aborted) return
      setOverview(result)
      setLoading(false)
    })
    return () => controller.abort()
  }, [session, cluster, instance, sessionId, refreshKey])

  const group = overview?.[kind]
  const namespaced = kind === "deployments" || kind === "pods"
  const term = filter.trim().toLowerCase()
  const items = group?.items.filter((item) => item.name.includes(term) || item.namespace?.includes(term)) ?? []

  return (
    <section aria-label="Kubernetes 资源" className="card-surface min-w-0 p-4">
      <header className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <Boxes aria-hidden="true" className="size-5 shrink-0 text-primary" />
          <div className="min-w-0">
            <h2 className="text-base font-semibold">Kubernetes 资源</h2>
            <p className="break-all font-mono text-xs text-muted-foreground">{cluster}/{instance}</p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <span role="status" className="text-xs text-muted-foreground">{loading ? "读取中…" : overview && `${new Date(overview.fetchedAt).toLocaleTimeString()} 更新`}</span>
          <Button size="sm" aria-label="刷新 Kubernetes 资源" onClick={() => setRefreshKey((value) => value + 1)} disabled={loading || running}>
            <RefreshCw aria-hidden="true" className={`size-3.5 ${loading ? "motion-safe:animate-spin" : ""}`} />刷新
          </Button>
        </div>
      </header>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <div role="group" aria-label="资源类别" className="flex flex-wrap gap-1">
          {K8S_OVERVIEW_QUERIES.map(({ key, label }) => {
            const result = overview?.[key]
            return (
              <Button key={key} size="sm" aria-pressed={kind === key} variant={kind === key ? "primary" : "ghost"}
                onClick={() => { setKind(key); setFilter("") }}>
                {label}<span className="tabular-nums">{result?.error ? "读取失败" : result?.items.length ?? "—"}</span>
              </Button>
            )
          })}
        </div>
        <Input type="search" scale="sm" className="w-full sm:w-60" aria-label="筛选资源" value={filter} onChange={(event) => setFilter(event.target.value)} placeholder={namespaced ? "按名称或命名空间筛选" : "按名称筛选"} />
      </div>
      {!group ? <p role="status" className="py-8 text-center text-sm text-muted-foreground">正在读取资源…</p> : group.error ? (
        <p role="alert" className="rounded-lg bg-warning/10 px-3 py-3 text-sm text-warning [overflow-wrap:anywhere]">{group.error}</p>
      ) : items.length === 0 ? (
        <p role="status" className="py-8 text-center text-sm text-muted-foreground">{group.items.length ? "没有匹配的资源" : "当前目标没有此类资源"}</p>
      ) : (
        <div className="max-h-[55dvh] overflow-auto rounded-lg border border-border/60">
          <table className="w-full text-left text-sm">
            <thead className="sticky top-0 bg-card text-xs text-muted-foreground">
              <tr><th scope="col" className="px-3 py-2">名称</th>{namespaced && <th scope="col" className="px-3 py-2">命名空间</th>}<th scope="col" className="px-3 py-2">状态</th>{namespaced && <th scope="col" className="px-3 py-2">就绪</th>}
              </tr>
            </thead>
            <tbody className="divide-y divide-border/50">
              {items.map((item) => (
                <tr key={`${item.namespace}/${item.name}`} className="hover:bg-muted/30">
                  <td className="px-3 py-2"><button type="button" disabled={running} onClick={() => onOpenDetails(kind, item)} className="break-all text-left font-mono text-primary hover:underline disabled:opacity-50">{item.name}</button></td>
                  {namespaced && <td className="break-all px-3 py-2 text-muted-foreground">{item.namespace}</td>}
                  <td className="px-3 py-2">{item.status}</td>
                  {namespaced && <td className="px-3 py-2 font-mono tabular-nums">{item.ready ?? "—"}</td>}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
