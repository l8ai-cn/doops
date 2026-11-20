"use client"

import { useCallback, useEffect, useState } from "react"
import { assetRequest } from "@/lib/assets"
import { sessionIsAdmin, type Session, type Target } from "@/lib/client"
import { Button, Input, Select } from "./ui"
import type { ClusterSourcePage, RenderAssetTargetAction } from "./asset-inventory"
import { AssetNodeLinks } from "./asset-node-links"

type NamespaceRecord = { name: string; verified_at?: string; scope: "list_pods" | "list_namespaces" }
type ClusterRecord = {
  asset_id: string
  name: string
  cluster: string
  instance: string
  verified_at: string
  namespaces: NamespaceRecord[]
}
type ImportInput = { asset_id: string; name: string; cluster: string; instance: string; namespace?: string }
const emptyForm = { id: "", name: "", target: "", namespace: "" }

function observedTime(value?: string) {
  if (!value) return "—"
  return new Date(value).toLocaleString("zh-CN", { hour12: false })
}

export function ClusterAssetsPanel({ session, targets, initialPage, onOpenCluster, renderTargetAction }: {
  session: Session
  targets: Target[]
  onOpenCluster: (id: string) => void
  initialPage?: ClusterSourcePage
  renderTargetAction: RenderAssetTargetAction
}) {
  const [records, setRecords] = useState<ClusterRecord[]>([])
  const [reading, setReading] = useState(true)
  const [readError, setReadError] = useState("")
  const [query, setQuery] = useState(initialPage?.query ?? "")
  const [form, setForm] = useState({ ...emptyForm })
  const [open, setOpen] = useState(false)
  const [expanded, setExpanded] = useState(() => new Set(initialPage?.expanded))
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const [notice, setNotice] = useState("")
  const [preview, setPreview] = useState<{ input: ImportInput; namespaces: NamespaceRecord[] } | null>(null)
  const online = targets.filter(target => target.status !== "offline")
  const admin = sessionIsAdmin(session)
  const read = useCallback(async () => {
    setReading(true)
    setReadError("")
    try {
      const { clusters } = await assetRequest<{clusters: ClusterRecord[]}>("clusters")
      setRecords(clusters)
    } catch (reason) {
      setRecords([])
      setReadError((reason as Error).message)
    } finally {
      setReading(false)
    }
  }, [])
  useEffect(() => { void read() }, [read])

  function toggleDetail(key: string, show: boolean) {
    setExpanded((previous) => {
      const next = new Set(previous)
      if (show) next.add(key)
      else next.delete(key)
      return next
    })
    if (show && !admin) void read()
  }

  function start(record?: ClusterRecord) {
    const target = record && online.find((item) => item.cluster === record.cluster && item.instance === record.instance)
    setForm(record ? { id: record.asset_id, name: record.name, target: target?.key || "", namespace: "" } : { ...emptyForm })
    setError("")
    setNotice("")
    setPreview(null)
    setOpen(true)
  }
  function change(field: keyof typeof form, value: string) {
    setForm((old) => ({ ...old, [field]: value }))
    setPreview(null)
    setError("")
  }
  async function discover() {
    const target = online.find((item) => item.key === form.target)
    if (!target) { setError("请选择一个在线 Agent"); return }
    const input: ImportInput = {
      asset_id: form.id.trim(), name: form.name.trim(), cluster: target.cluster, instance: target.instance,
      ...(form.namespace.trim() ? { namespace: form.namespace.trim() } : {}),
    }
    setPreview(null)
    setError("")
    setBusy(true)
    try {
      const result = await assetRequest<{namespaces: NamespaceRecord[]}>("clusters", { action: "discover", ...input })
      setPreview({ input, namespaces: result.namespaces })
    } catch (reason) { setError((reason as Error).message) }
    finally { setBusy(false) }
  }
  async function save() {
    if (!preview) return
    setBusy(true)
    setError("")
    try {
      const result = await assetRequest<{namespaces: NamespaceRecord[]}>("clusters", { action: "import", ...preview.input })
      setNotice(`已保存 ${preview.input.asset_id}，本次验证 ${result.namespaces.length} 个命名空间。`)
      setOpen(false)
      setPreview(null)
      await read()
    } catch (reason) { setError((reason as Error).message) }
    finally { setBusy(false) }
  }
  const term = query.toLowerCase().trim()
  const visible = records.filter((row) => [row.asset_id, row.name, row.cluster, row.instance, ...row.namespaces.map((space) => space.name)]
    .some((value) => value.toLowerCase().includes(term)))
  const byAsset = new Map<string, ClusterRecord[]>()
  for (const source of visible) {
    const entries = byAsset.get(source.asset_id)
    if (entries) entries.push(source)
    else byAsset.set(source.asset_id, [source])
  }

  return <section aria-label="K8s 集群资产" className="space-y-4">
    <header className="flex flex-wrap items-center justify-between gap-3">
      <h2 className="text-lg font-semibold">K8s 集群资产</h2>
      <div className="flex flex-wrap gap-2">
        <Button size="sm" disabled={reading || busy} onClick={() => void read()}>刷新列表</Button>
        <Button size="sm" variant="primary" disabled={busy || !online.length} onClick={() => start()}>{admin ? "从 Agent 导入" : "从 Agent 发现"}</Button>
      </div>
    </header>
    {notice && <p role="status" className="text-sm text-primary">{notice}</p>}
    {open && <form className="card-surface space-y-3 p-4" onSubmit={(e) => { e.preventDefault(); void discover() }}>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {admin && <>
          <label className="text-sm">集群资产编号<Input required value={form.id} disabled={busy} onChange={(e) => change("id", e.target.value)} placeholder="如 ops-east" /></label>
          <label className="text-sm">集群名称<Input required value={form.name} disabled={busy} onChange={(e) => change("name", e.target.value)} /></label>
        </>}
        <label className="text-sm">来源 Agent<Select required value={form.target} disabled={busy} onChange={(e) => change("target", e.target.value)}>
          <option value="">请选择目标</option>
          {online.map((t) => <option key={t.key} value={t.key}>{t.cluster}/{t.instance}</option>)}
        </Select></label>
        <label className="text-sm">指定命名空间<Input maxLength={63} value={form.namespace} disabled={busy} placeholder="留空发现整个集群" onChange={(e) => change("namespace", e.target.value)} /></label>
      </div>
      <p className="text-xs text-muted-foreground">{form.namespace.trim() ? "将验证该命名空间的 Pod 列举权限。" : "将读取来源 Agent 有权列举的命名空间。"}</p>
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      {preview && <div aria-label="发现结果" className="rounded-md border p-3 text-sm">
        <p className="font-medium">发现结果 · {preview.namespaces.length} 个命名空间</p>
        <ul className="mt-1 max-h-44 overflow-auto">{preview.namespaces.map((ns) => <li key={ns.name} className="font-mono">{ns.name}</li>)}</ul>
      </div>}
      <div className="flex flex-wrap gap-2">
        <Button disabled={busy} size="sm" type="submit">验证并预览</Button>
        {admin && preview && <Button disabled={busy} size="sm" type="button" variant="primary" onClick={() => void save()}>确认导入</Button>}
        <Button disabled={busy} type="button" size="sm" variant="ghost" onClick={() => setOpen(false)}>关闭</Button>
        {busy && <span role="status" className="self-center text-xs text-muted-foreground">正在处理…</span>}
      </div>
    </form>}
    <Input type="search" scale="sm" className="max-w-sm" aria-label="搜索集群资产" placeholder="搜索资产编号、集群或命名空间" value={query} onChange={(e) => setQuery(e.target.value)} />
    {readError ? <p role="alert" className="text-sm text-destructive">{readError}</p> : reading ? <p role="status">正在读取台账…</p>
      : !visible.length ? <p className="py-4 text-sm text-muted-foreground">{records.length ? "无匹配集群" : "当前没有可见的集群资产"}</p>
      : [...byAsset].map(([id, sources]) => <article key={id} className="card-surface space-y-3 p-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h3 className="font-semibold">{sources[0].name} <span className="text-xs font-normal text-muted-foreground">{id}</span></h3>
          <Button size="sm" onClick={() => onOpenCluster(id)}>打开集群看板</Button>
        </div>
        {sources.map((row) => {
        const target = online.find((t) => t.cluster === row.cluster && t.instance === row.instance)
        const source = JSON.stringify([id, row.cluster, row.instance])
        const origin: ClusterSourcePage = {
          section: "clusters", source: { assetId: id, cluster: row.cluster, instance: row.instance }, query, expanded: [...expanded],
        }
        const namespacesKey = `namespaces:${source}`
        const nodesKey = `nodes:${source}`
        return <section key={JSON.stringify([row.cluster, row.instance])} className="border-t pt-3">
          <div className="flex flex-wrap justify-between gap-2">
            <p className="mt-1 break-all text-xs text-muted-foreground">{row.cluster}/{row.instance} · {target ? "Agent 在线" : "Agent 离线"} · 最近验证 {observedTime(row.verified_at)}</p>
            <div className="flex flex-wrap gap-2">
              {admin && <Button size="sm" disabled={!target || busy} onClick={() => start(row)}>更新纳管范围</Button>}
              {renderTargetAction({ target, origin, tab: "credentials", label: "查看目标凭据" })}
              {renderTargetAction({ target, origin, tab: "k8s", label: "进入 K8s 实时资源" })}
            </div>
          </div>
          <details open={expanded.has(namespacesKey)} className="mt-3 border-t pt-2 text-sm">
            <summary className="cursor-pointer" onClick={(event) => {
              event.preventDefault()
              toggleDetail(namespacesKey, !expanded.has(namespacesKey))
            }}>已纳管命名空间（{row.namespaces.length}）</summary>
            {expanded.has(namespacesKey) && <ul className="mt-2 max-h-60 space-y-1 overflow-auto">{row.namespaces.map((ns) => <li key={ns.name} className="flex flex-wrap items-center justify-between gap-2">
              <span className="font-mono">{ns.name}</span>
              <span className="ml-auto text-xs text-muted-foreground">{ns.scope === "list_pods" ? "已验证 Pod 列举" : "已读取命名空间对象"} · {observedTime(ns.verified_at)}</span>
              {renderTargetAction({ target, tab: "k8s", label: `查看 ${ns.name} 工作负载`,
                origin: { ...origin, source: { ...origin.source, namespace: ns.name } } })}
            </li>)}</ul>}
          </details>
          <AssetNodeLinks assetId={row.asset_id} cluster={row.cluster} instance={row.instance} session={session}
            open={expanded.has(nodesKey)} onOpenChange={(show) => toggleDetail(nodesKey, show)} />
        </section>
      })}</article>)}
  </section>
}
