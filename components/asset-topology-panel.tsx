"use client"

import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import { assetRequest } from "@/lib/assets"
import { Button, Input, SegmentedControl, Select } from "./ui"

const kinds = { cluster: "集群", workload: "工作负载", pod: "Pod", source: "访问来源", system: "业务系统", application: "应用", database: "数据库", node: "K8s Node", machine: "机器" }
const verbs = { scheduled_on: "调度至", controls: "控制", bound_workload: "绑定工作负载", observes: "已发现", member_of: "属于", maps_to: "映射到", hosted_on: "由其承载", bound_to: "绑定来源", contains: "包含应用", deployed_to: "部署于", uses: "依赖数据库" }
const layers = { business: "业务依赖", deployment: "运行部署", physical: "物理承载" }
const records = { pod_node_name: "Pod.spec.nodeName", controller_uid: "控制器 UID 引用", runtime_binding: "管理员确认的工作负载 UID", cluster_inventory_sources: "集群发现", inventory_node_links: "人工绑定 Node", "inventory_machines.host_asset_id": "机器台账宿主记录", "inventory_machines.target": "机器台账来源绑定", business_deployment: "人工核验业务部署" }

type AssetNode = { id: string; kind: keyof typeof kinds; ref: string; name: string; namespace?: string }
type Evidence = { record: keyof typeof records; cluster?: string; instance?: string; namespace?: string; status?: string; verified_at?: string }
type Edge = { from: string; to: string; kind: keyof typeof verbs; layer: keyof typeof layers; evidence: Evidence[] }
type RuntimeScope = {
  cluster_id: string; cluster: string; instance: string; namespace: string; status: string; revision: string; observed_at: string
  applications: string[]; workloads: { uid: string; kind: string; name: string }[]; bindings: { application: string; uid: string; at: string }[]
}
type Graph = { nodes: AssetNode[]; edges: Edge[]; runtime: RuntimeScope[]; can_bind: boolean }
type NodeSelection = (id: string) => void
const edgeKey = (edge: Edge) => JSON.stringify([edge.from, edge.kind, edge.to])

function NodeButton({ node, choose, active = false }: { node: AssetNode; choose: NodeSelection; active?: boolean }) {
  return <button type="button" onClick={() => choose(node.id)} aria-pressed={active} className="min-w-0 break-all rounded-md border bg-card px-3 py-2 text-left hover:border-primary aria-pressed:border-primary">
    <span className="block text-xs text-muted-foreground">{kinds[node.kind]}{node.namespace && ` · ${node.namespace}`}</span>
    <span className="block font-medium">{node.name}</span>
    {node.ref !== node.name && <code className="text-xs text-muted-foreground">{node.ref}</code>}
  </button>
}

function Proof({ evidence }: { evidence: Evidence[] }) {
  return <ul className="space-y-1 break-words text-xs text-muted-foreground">{evidence.map(p => <li key={JSON.stringify(p)}>
    <span className="font-medium text-foreground">{records[p.record]}</span>
    {p.cluster && <code className="ml-2 break-all">{p.cluster}/{p.instance}</code>}
    {p.namespace && <span> · {p.namespace}</span>}{p.status === "offline" && <span> · 来源离线</span>}
    <span className="block">{p.verified_at ? `核验于 ${new Date(p.verified_at).toLocaleString("zh-CN", { hour12: false })}` : "台账未记录关系核验时间"}</span>
  </li>)}</ul>
}

function RelationView({ edges, byID, selected, mode, choose }: { edges: Edge[]; byID: Map<string, AssetNode>; selected?: AssetNode; mode: "graph" | "list"; choose: NodeSelection }) {
  const node = (id: string) => <NodeButton node={byID.get(id)!} active={selected?.id === id} choose={choose} />
  if (mode === "list") return <ul className="divide-y rounded-lg border">{edges.map(e => <li key={edgeKey(e)} className="grid gap-2 p-3 sm:grid-cols-2">
    <div className="flex flex-wrap items-center gap-2">{node(e.from)}<span className="text-xs">{verbs[e.kind]} →</span>{node(e.to)}</div>
    <Proof evidence={e.evidence} />
  </li>)}</ul>
  const groups = new Map<string, Edge[]>()
  for (const e of edges) {
    const key = JSON.stringify([e.to, e.kind])
    const group = groups.get(key)
    if (group) group.push(e)
    else groups.set(key, [e])
  }
  return <div className="space-y-3">{[...groups].map(([key, group]) => <div key={key} className="grid grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] items-center rounded-lg border bg-muted/30 p-3">
    <div>{group.map(e => <div key={e.from} className="relative flex items-center py-1"><div className="grid min-w-0 flex-1">{node(e.from)}</div><span aria-hidden="true" className="w-3 border-t border-muted-foreground" />{group.length > 1 && <span aria-hidden="true" className={`absolute right-0 border-r border-muted-foreground ${e === group[0] ? "bottom-0 top-1/2" : e === group[group.length-1] ? "bottom-1/2 top-0" : "inset-y-0"}`} />}</div>)}</div>
    <div className="flex items-center text-xs text-muted-foreground"><span aria-hidden="true" className="w-2 border-t" /><span className="px-1">{verbs[group[0].kind]} →</span></div>{node(group[0].to)}
  </div>)}</div>
}

// Follow only attested runtime and hosting edges. Carry the observing target across the
// workload chain: independently visible observations from two Agents do not form a path.
function businessImpact(graph: Graph, start: string) {
  const kinds = new Set<Edge["kind"]>(["hosted_on", "maps_to", "scheduled_on", "controls", "bound_workload", "contains"])
  const incoming = new Map<string, Edge[]>()
  for (const e of graph.edges) {
    if (!kinds.has(e.kind)) continue
    const group = incoming.get(e.to)
    if (group) group.push(e)
    else incoming.set(e.to, [e])
  }
  // Keep every branch that converges at a controller or app. Expand each (node, source)
  // once, then walk forward from the systems reached to retain only complete paths.
  const queue = [{ node: start, target: "" }]
  const stateKey = (node: string, target: string) => JSON.stringify([node, target])
  const visited = new Set([stateKey(start, "")])
  const towardStart = new Map<string, { to: string; edge: Edge; proof: Evidence }[]>()
  const systems = new Set<string>()
  const complete = new Set<string>()
  for (const { node, target } of queue) {
    for (const edge of incoming.get(node) ?? []) {
      for (const proof of edge.evidence) {
        const source = edge.kind === "hosted_on" ? "" : JSON.stringify([proof.cluster, proof.instance])
        if (target && source !== target) continue
        const key = stateKey(edge.from, source)
        const hops = towardStart.get(key) ?? []
        hops.push({ to: stateKey(node, target), edge, proof })
        towardStart.set(key, hops)
        if (edge.kind === "contains") {
          systems.add(edge.from)
          complete.add(key)
        } else if (!visited.has(key)) {
          visited.add(key)
          queue.push({ node: edge.from, target: source })
        }
      }
    }
  }
  const paths = new Map<string, Edge>()
  for (const key of complete) {
    for (const { to, edge, proof } of towardStart.get(key) ?? []) {
      complete.add(to)
      const id = edgeKey(edge)
      const part = paths.get(id)
      if (!part) paths.set(id, { ...edge, evidence: [proof] })
      else if (!part.evidence.includes(proof)) part.evidence.push(proof)
    }
  }
  return { systems: systems.size, edges: [...paths.values()] }
}

function RuntimeControls({ scopes, canBind, read }: { scopes: RuntimeScope[]; canBind: boolean; read: () => Promise<void> }) {
  const [key, setKey] = useState("")
  const [app, setApp] = useState("")
  const [uid, setUID] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const scopeKey = (s: RuntimeScope) => JSON.stringify([s.cluster_id, s.cluster, s.instance, s.namespace])
  const scope = scopes.find(s => scopeKey(s) === key)
  async function act(action: "collect" | "bind" | "unbind", application = app, workload_uid = uid) {
    if (!scope || busy) return
    setBusy(true); setError("")
    try {
      const { cluster_id, cluster, instance, namespace, revision } = scope
      await assetRequest("topology", { action, cluster_id, cluster, instance, namespace, ...(action === "collect" ? {} : { application, workload_uid, revision }) })
      await read()
    } catch (reason) { setError((reason as Error).message); await read() }
    finally { setBusy(false) }
  }
  return <section className="card-surface space-y-2 p-3" aria-label="运行快照"><h3 className="font-semibold">运行快照</h3>
    <label className="text-sm">来源与纳管命名空间<Select value={scope ? key : ""} disabled={busy} onChange={e => {setKey(e.target.value); setApp(""); setUID(""); setError("")}}><option value="">选择采集范围</option>{scopes.map(s => <option value={scopeKey(s)} key={scopeKey(s)}>{s.cluster}/{s.instance} · {s.namespace} · {s.status === "online" ? "在线" : "离线"}</option>)}</Select></label>
    {!scopes.length && <p className="text-sm text-muted-foreground">暂无可见且已纳管的命名空间。</p>}
    {scope && <><p className="text-sm">{scope.observed_at ? `最近成功快照 ${new Date(scope.observed_at).toLocaleString()}` : "尚无成功快照"} · {scope.workloads.length} 个工作负载</p>
      <Button size="sm" disabled={busy || scope.status !== "online"} onClick={() => void act("collect")}>{busy ? "处理中…" : "采集运行快照"}</Button>
      {canBind && scope.revision && <div className="flex flex-wrap items-end gap-2"><label className="text-sm">台账应用<Select value={app} onChange={e => setApp(e.target.value)}><option value="">选择已核验应用</option>{scope.applications.map(a => <option key={a}>{a}</option>)}</Select></label>
        <label className="min-w-0 max-w-full text-sm">观测工作负载<Select value={uid} onChange={e => setUID(e.target.value)}><option value="">选择工作负载及 UID</option>{scope.workloads.map(w => <option key={w.uid} value={w.uid}>{w.kind}/{w.name} · {w.uid}</option>)}</Select></label>
        <Button size="sm" disabled={busy || scope.status !== "online" || !scope.applications.includes(app) || !scope.workloads.some(w => w.uid === uid) || scope.bindings.some(b => b.application === app && b.uid === uid)} onClick={() => void act("bind")}>确认绑定</Button></div>}
      {scope.bindings.map(b => <p className="break-all text-sm" key={JSON.stringify([b.application,b.uid])}>{b.application} → {b.uid} {canBind && <Button size="sm" disabled={busy} onClick={() => void act("unbind",b.application,b.uid)}>解除绑定</Button>}</p>)}
    </>}{error && <p role="alert" className="text-sm text-destructive">{error}</p>}</section>
}

export function AssetTopologyPanel({ clusterId }: { clusterId?: string }) {
  return <TopologyScope key={clusterId ?? ""} clusterId={clusterId} />
}

function TopologyScope({ clusterId }: { clusterId?: string }) {
  const [graph, setGraph] = useState<Graph | null>(null)
  const [error, setError] = useState("")
  const [mode, setMode] = useState<"graph" | "list">("graph")
  const [layer, setLayer] = useState("")
  const [ns, setNS] = useState("")
  const [query, setQuery] = useState("")
  const [id, setID] = useState("")
  const [direction, setDirection] = useState<"out" | "in" | "impact">("out")
  const generation = useRef(0)
  const read = useCallback(async (clear = true) => {
    const request = ++generation.current
    if (clear) setGraph(null)
    setError("")
    const params = new URLSearchParams()
    if (clusterId !== undefined) params.set("cluster_id", clusterId)
    try {
      const next = await assetRequest<Graph>("topology", undefined, params)
      if (request !== generation.current) return
      setGraph(next)
      setID(old => next.nodes.some(n => n.id === old) ? old : "")
      setNS(old => next.edges.some(e => e.evidence.some(p => p.namespace === old)) ? old : "")
    } catch (reason) {
      if (request === generation.current) { setGraph(null); setID(""); setNS(""); setError((reason as Error).message) }
    }
  }, [clusterId])
  useEffect(() => {
    void read()
    const focus = () => { void read() }
    window.addEventListener("focus", focus)
    return () => { ++generation.current; window.removeEventListener("focus", focus) }
  }, [read])
  const byID = useMemo(() => new Map(graph?.nodes.map(n => [n.id, n])), [graph])
  const selected = byID.get(id)
  const namespaces = [...new Set(graph?.edges.flatMap(e => e.evidence.flatMap(p => p.namespace ? [p.namespace] : [])))].sort()
  const term = query.trim().toLowerCase()
  const matches = (n: AssetNode) => !term || [n.name, n.ref, n.id, n.namespace ?? ""].some(v => v.toLowerCase().includes(term))
  const scoped = (graph?.edges ?? []).filter(e => !layer || e.layer === layer)
  // A selected node expands its directly connected, authorized endpoints across namespaces.
  // The search and namespace selection remain intact when returning to the result set.
  const impact = graph && selected && direction === "impact" ? businessImpact(graph, selected.id) : null
  const edges = impact ? impact.edges : selected ? scoped.filter(e => (direction === "out" ? e.from : e.to) === id) : scoped.flatMap(e => {
    const evidence = ns ? e.evidence.filter(p => p.namespace === ns) : e.evidence
    return evidence.length && (matches(byID.get(e.from)!) || matches(byID.get(e.to)!)) ? [{ ...e, evidence }] : []
  })
  const linked = new Set(edges.flatMap(e => [e.from, e.to]))
  const isolated = selected || layer ? [] : graph?.nodes.filter(n => !linked.has(n.id) && (!ns || n.namespace === ns) && matches(n)) ?? []
  function choose(next: string) {
    setID(next); setDirection("out"); void read()
  }
  return <section className="min-w-0 space-y-4" aria-label="资产拓扑">
    <header className="flex flex-wrap items-center justify-between gap-3"><div><h2 className="text-lg font-semibold">{clusterId ? "集群拓扑" : "平台拓扑"}</h2>{clusterId && <code className="text-xs text-muted-foreground">{clusterId}</code>}</div><Button size="sm" onClick={() => void read()}>刷新拓扑</Button></header>
    {error ? <p role="alert" className="rounded-lg border p-4 text-sm text-destructive">{error}</p> : !graph ? <p role="status" className="text-sm text-muted-foreground">正在读取授权范围内的拓扑…</p> : <>
      <RuntimeControls scopes={graph.runtime} canBind={graph.can_bind} read={() => read(false)} />
      <div className="flex flex-wrap items-end gap-3">
        <label className="min-w-0 text-sm">搜索节点<Input type="search" value={query} placeholder="名称、编号或命名空间" onChange={e => { setQuery(e.target.value); setID("") }} /></label>
        <label className="text-sm">关系层<Select value={layer} onChange={e => { setLayer(e.target.value); setID("") }}><option value="">全部关系</option>{Object.entries(layers).map(([k,v]) => <option key={k} value={k}>{v}</option>)}</Select></label>
        {namespaces.length > 0 && <label className="text-sm">命名空间<Select value={ns} onChange={e => { setNS(e.target.value); setID("") }}><option value="">全部可见命名空间</option>{namespaces.map(n => <option key={n}>{n}</option>)}</Select></label>}
        <SegmentedControl ariaLabel="拓扑展示" value={mode} onChange={setMode} options={[{ value: "graph", label: "拓扑图" }, { value: "list", label: "关系列表" }]} />
      </div>
      {selected && <aside className="card-surface space-y-3 p-4"><div className="flex flex-wrap items-center justify-between gap-2"><div><p className="text-xs text-muted-foreground">{kinds[selected.kind]} · {impact ? "多跳影响" : "一跳关系"}</p><h3 className="break-all font-semibold">{selected.name}</h3><code className="break-all text-xs">{selected.id}</code></div><Button size="sm" onClick={() => setID("")}>返回筛选结果</Button></div>
        <SegmentedControl ariaLabel="展开方向" value={direction} onChange={d => { setDirection(d); void read() }} options={[{ value: "out", label: "依赖谁" }, { value: "in", label: "影响谁（反查）" }, ...(["machine", "node", "pod", "workload"].includes(selected.kind) ? [{ value: "impact" as const, label: "受影响业务（多跳）" }] : [])]} />
        {impact && <p role="status" className="text-sm">{impact.systems} 个受影响业务系统 · 按最近成功快照及当前台账跨关系层追溯</p>}
        {ns && <p className="text-xs text-muted-foreground">从 {ns} 展开，包含当前授权范围内的跨命名空间关系。</p>}
      </aside>}
      <div className="space-y-3"><p className="text-xs text-muted-foreground" role="status">{linked.size + isolated.length} 个结果节点 · {edges.length} 条{impact ? "路径" : selected ? "一跳" : ""}关系</p>
        {edges.length > 0 && <RelationView edges={edges} byID={byID} selected={selected} mode={mode} choose={choose} />}
        {isolated.length > 0 && <div className="flex flex-wrap gap-2">{isolated.map(n => <NodeButton key={n.id} node={n} choose={choose} />)}</div>}
        {!edges.length && !isolated.length && <p className="rounded-lg border border-dashed p-4 text-sm text-muted-foreground">{impact ? "没有可追溯到业务系统的完整证据链。" : selected ? "当前方向没有可见的一跳关系。" : graph.nodes.length ? "没有匹配的资产或关系。可调整筛选条件。" : "当前范围暂无可见资产或已登记关系。"}</p>}
        {selected && mode === "graph" && edges.length > 0 && <details className="rounded-lg border p-3"><summary className="cursor-pointer text-sm">查看关系证据</summary><ul className="mt-2 space-y-3">{edges.map(e => <li key={edgeKey(e)}><p className="text-sm">{byID.get(e.from)?.name} → {byID.get(e.to)?.name}</p><Proof evidence={e.evidence} /></li>)}</ul></details>}
      </div>
    </>}
  </section>
}
