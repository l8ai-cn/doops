"use client"

import { Boxes, RefreshCw } from "lucide-react"
import { useEffect, useState } from "react"
import { fetchK8sOverview, K8S_OVERVIEW_QUERIES, type K8sOverview, type K8sOverviewKey, type K8sResource } from "@/lib/k8s-overview"
import type { Session, Target } from "@/lib/client"
import { Button, Input } from "./ui"

export function K8sResourceOverview({ session, target, sessionId, initialNamespace = "", running, onOpenDetails }: {
  session: Session
  target: Target
  sessionId: string
  initialNamespace?: string
  running: boolean
  onOpenDetails: (kind: K8sOverviewKey, resource: K8sResource) => void
}) {
  const [overview, setOverview] = useState<K8sOverview | null>(null)
  const [refreshKey, setRefreshKey] = useState(0)
  const [loading, setLoading] = useState(true)
  const [kind, setKind] = useState<K8sOverviewKey>("pods")
  const [filter, setFilter] = useState("")
  const [namespace, setNamespace] = useState(initialNamespace)
  const [namespaceInput, setNamespaceInput] = useState(initialNamespace)
  const { cluster, instance } = target
  function changeScope(value: string) {
    setNamespaceInput(value)
    if (value === namespace) return
    setOverview(null)
    setLoading(true)
    setFilter("")
    setKind("pods")
    setNamespace(value)
  }

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    void fetchK8sOverview(session, { cluster, instance }, sessionId, controller.signal, namespace).then((result) => {
      if (controller.signal.aborted) return
      setOverview(result)
      setLoading(false)
    })
    return () => controller.abort()
  }, [session, cluster, instance, sessionId, refreshKey, namespace])

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
            <p className="break-all font-mono text-xs text-muted-foreground">{cluster}/{instance}{namespace && ` · ${namespace}`}</p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <span role="status" className="text-xs text-muted-foreground">{loading ? "读取中…" : overview && `${new Date(overview.fetchedAt).toLocaleTimeString()} 更新`}</span>
          <Button size="sm" aria-label="刷新 Kubernetes 资源" onClick={() => setRefreshKey((value) => value + 1)} disabled={loading || running}>
            <RefreshCw aria-hidden="true" className={`size-3.5 ${loading ? "motion-safe:animate-spin" : ""}`} />刷新
          </Button>
        </div>
      </header>
      <form className="mb-3 flex flex-wrap items-center gap-2" onSubmit={(event) => { event.preventDefault(); changeScope(namespaceInput.trim()) }}>
        <label className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          查询命名空间
          <Input scale="sm" className="w-48" value={namespaceInput} maxLength={63} pattern="[a-z0-9]([a-z0-9\-]*[a-z0-9])?"
            placeholder="留空查询整个集群" title="小写字母、数字和短横线，最多 63 个字符"
            onChange={(event) => setNamespaceInput(event.target.value)} />
        </label>
        <Button type="submit" size="sm" disabled={running || namespaceInput === namespace}>应用范围</Button>
        {namespace && <Button type="button" size="sm" variant="ghost" disabled={running} onClick={() => changeScope("")}>返回整个集群</Button>}
      </form>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <div role="group" aria-label="资源类别" className="flex flex-wrap gap-1">
          {K8S_OVERVIEW_QUERIES.filter((query) => !namespace || query.allNamespaces).map(({ key, label }) => {
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
        <p role="status" className="py-8 text-center text-sm text-muted-foreground">{group.items.length ? "没有匹配的资源" : namespace ? `命名空间 ${namespace} 的查询返回 0 项` : "当前目标没有此类资源"}</p>
      ) : (
        <div className="max-h-[55dvh] overflow-auto rounded-lg border border-border/60">
          <table className="w-full text-left text-sm">
            <thead className="sticky top-0 bg-card text-xs text-muted-foreground">
              <tr><th scope="col" className="px-3 py-2">名称</th>{namespaced && <th scope="col" className="px-3 py-2">命名空间</th>}<th scope="col" className="px-3 py-2">状态</th>{namespaced && <th scope="col" className="px-3 py-2">就绪</th>}
                {kind === "nodes" && <><th scope="col" className="px-3 py-2">CPU 容量</th><th scope="col" className="px-3 py-2">内存容量</th></>}
                {kind === "namespaces" && <th scope="col" className="px-3 py-2">工作负载</th>}
              </tr>
            </thead>
            <tbody className="divide-y divide-border/50">
              {items.map((item) => (
                <tr key={`${item.namespace}/${item.name}`} className="hover:bg-muted/30">
                  <td className="px-3 py-2"><button type="button" disabled={running} onClick={() => onOpenDetails(kind, item)} className="break-all text-left font-mono text-primary hover:underline disabled:opacity-50">{item.name}</button></td>
                  {namespaced && <td className="break-all px-3 py-2 text-muted-foreground">{item.namespace}</td>}
                  <td className="px-3 py-2">{item.status}</td>
                  {namespaced && <td className="px-3 py-2 font-mono tabular-nums">{item.ready ?? "—"}</td>}
                  {kind === "nodes" && <><td className="px-3 py-2">{item.cpu ?? "未上报"}</td><td className="px-3 py-2">{item.memory ?? "未上报"}</td></>}
                  {kind === "namespaces" && <td className="px-3 py-2"><Button size="sm" variant="ghost" disabled={running} aria-label={`查看 ${item.name} 工作负载`} onClick={() => changeScope(item.name)}>查看工作负载</Button></td>}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  )
}
