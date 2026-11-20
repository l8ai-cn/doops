"use client"

import { useEffect, useRef, useState } from "react"
import type { Session } from "@/lib/client"
import { consoleFetch } from "@/lib/network-error"
import { Button, Input, Select, Textarea } from "./ui"

type Kind = "machines" | "systems"
type Metric = { total: number; covered: number; shortfall: number; percent: number | null; passed: boolean | null }
type Version = { id: string; frozen_at: string }
type Evidence = {
  type: "host" | "hosts_vm" | "node" | "deployment"
  reference: string; source: { cluster: string; instance: string }; verified: boolean
  verified_at?: string; revision?: number; cluster_asset?: string
}
type RegisterEntry = { kind: Kind; asset_id: string; name: string; source: { cluster: string; instance: string } }
type Register = { entries: RegisterEntry[]; document_ref: string; revision: number }
type Line = { memberships?: { cluster_id: string; document_ref: string }[]; asset_id: string; name: string; kind: Kind; covered: boolean; evidence: Evidence[] }
type MachineState = { asset_id: string; name: string; active: boolean; revision: number }
type Report = {
  register?: Register; document_ref?: string; register_revision?: number
  machine_lifecycle?: MachineState[]
  scope: "platform" | "visible"; state: "missing" | "live" | "frozen" | "scope_unavailable" | "register_missing"; baselines: Version[]; baseline: Version | null
  accepted: boolean | null; metrics: Record<Kind, Metric>; rows: Line[]
}
const labels = { machines: "在役服务器", systems: "在役业务系统" }
const relationNames = { host: "宿主", hosts_vm: "承载虚拟机", node: "人工绑定 Node", deployment: "部署" }
const endpoint = "/api/assets/coverage"

async function checked(response: Response) {
  if (response.ok) return response
  const error = await response.json()
  const messages: Record<string, string> = {
    cluster_not_visible: "集群不存在或当前无权访问，请返回资产看板。",
    register_changed: "底册已变更，请刷新对账，核对最新底册后重试。",
    baseline_exists: "此编号已冻结，请选择已有基线或使用新的编号。",
    invalid_register_entry: "请核对资产类型、名称和重复编号。",
    invalid_register_source: "来源必须是登记在此集群下的精确目标。",
    coverage_register_migration_required: "集群盘点底册尚未初始化，请联系平台管理员。",
  }
  throw new Error(messages[error.code] || error.message || error.code || `对账请求失败（HTTP ${response.status}）`)
}

function MetricCard({ title, metric }: { title: string; metric: Metric }) {
  return <section className="card-surface space-y-1 p-4">
    <h3 className="text-sm font-semibold">{title}</h3>
    <p className="text-xl font-semibold tabular-nums">{metric.percent === null ? "无适用资产" : `${metric.percent}%`}</p>
    <p className="text-sm">已覆盖 {metric.covered} / {metric.total} · 未覆盖 {metric.total - metric.covered}</p>
    {metric.passed !== null && <p className={metric.passed ? "text-xs text-primary" : "text-xs text-warning"}>{metric.passed ? "达到 80%" : `距 80% 还差 ${metric.shortfall} 项`}</p>}
  </section>
}

function CoverageMetrics({ metrics }: { metrics: Report["metrics"] }) {
  return <div className="grid gap-3 sm:grid-cols-2">{(["machines", "systems"] as const).map(k => <MetricCard key={k} title={labels[k]} metric={metrics[k]} />)}</div>
}

type Summary = Pick<Report, "scope" | "state" | "baseline" | "metrics" | "accepted">
type SummaryProps = { onOpen: () => void; clusterId?: string }
export function AssetCoverageSummary(props: SummaryProps) {
  return <ScopedCoverageSummary key={JSON.stringify(props.clusterId)} {...props} />
}
function ScopedCoverageSummary({ onOpen, clusterId }: SummaryProps) {
  const [data, setData] = useState<Summary | null>(null)
  const [error, setError] = useState("")
  const [attempt, retry] = useState(0)
  useEffect(() => {
    const controller = new AbortController()
    setData(null); setError("")
    const query = new URLSearchParams({ summary: "1" })
    if (clusterId !== undefined) query.set("cluster_id", clusterId)
    consoleFetch(`${endpoint}?${query}`, { cache: "no-store", signal: controller.signal }).then(checked).then(r => r.json()).then((summary: Summary) => {
      if (!controller.signal.aborted) setData(summary)
    }).catch(err => { if (!controller.signal.aborted) setError(err.message) })
    const reload = () => retry(n => n + 1)
    window.addEventListener("focus", reload)
    return () => { controller.abort(); window.removeEventListener("focus", reload) }
  }, [attempt, clusterId])
  return <section aria-label="拓扑覆盖率摘要" className="space-y-3">
    <header className="flex flex-wrap items-center justify-between gap-2"><h2 className="font-semibold">拓扑覆盖率 · 双 80% 目标</h2><Button size="sm" onClick={onOpen}>查看覆盖率对账</Button></header>
    {error && <div role="alert" className="status-error">{error} <Button size="sm" onClick={() => retry(n => n + 1)}>重试</Button></div>}
    {!error && !data && <p role="status">正在读取最近冻结基线…</p>}
    {data && <><p className="text-sm text-muted-foreground">{clusterId && `集群 ${clusterId} · `}{data.scope === "platform" ? (clusterId ? "完整底册范围" : "平台范围") : "我的可见范围 · 仅按当前授权测算"}{data.baseline && ` · ${data.baseline.id} · ${new Date(data.baseline.frozen_at).toLocaleString()}`}</p>
      {data.state === "scope_unavailable" ? <p>此历史基线未冻结该集群的底册范围。</p> : data.state === "missing" ? <p>待建立基线。冻结盘点后显示服务器和业务系统的覆盖率。</p> : <CoverageMetrics metrics={data.metrics} />}</>}
  </section>
}

function MachineLifecycle({ machines, busy, change }: { machines: MachineState[]; busy: boolean; change: (machine: MachineState) => void }) {
  const [id, setID] = useState("")
  const machine = machines.find(item => item.asset_id === id)
  return <section className="card-surface space-y-2 p-4"><h3 className="font-semibold">机器在役状态</h3>
    <p className="text-xs text-muted-foreground">退役后从当前分母移除，恢复在役后重新计入。已冻结基线保留盘点时的状态。</p>
    {machines.length ? <div className="flex flex-wrap items-center gap-2">
      <Select aria-label="选择盘点机器" className="max-w-full" value={id} disabled={busy} onChange={e => setID(e.target.value)}>
        <option value="">选择盘点机器</option>{machines.map(m => <option key={m.asset_id} value={m.asset_id}>{m.name}（{m.asset_id}）· {m.active ? "在役" : "退役"}</option>)}
      </Select>
      {machine && <Button size="sm" disabled={busy} onClick={() => change(machine)}>{machine.active ? "确认退役" : "恢复在役"}</Button>}
    </div> : <p className="text-sm">机器台账为空。</p>}
  </section>
}

type Host = { machine_id: string; host_id: string; revision: number }
function FreezeForm({ rows, busy, freeze }: { rows: Line[]; busy: boolean; freeze: (id: string, hosts: Host[]) => void }) {
  const [id, setID] = useState("")
  const [selected, setSelected] = useState<Host[]>([])
  return <form className="card-surface space-y-3 p-4" onSubmit={e => { e.preventDefault(); freeze(id, selected) }}>
    <h3 className="font-semibold">冻结当前台账</h3>
    <p className="text-xs text-muted-foreground">勾选已人工核验的宿主关系。冻结后保留当时的资产与证据。</p>
    {rows.flatMap(row => row.evidence.filter(e => e.type === "host").map(e => <label key={row.asset_id} className="flex min-h-9 items-center gap-2 text-sm">
      <input type="checkbox" disabled={busy} onChange={event => setSelected(previous => event.target.checked
        ? [...previous, { machine_id: row.asset_id, host_id: e.reference, revision: e.revision! }]
        : previous.filter(h => h.machine_id !== row.asset_id))} />{row.asset_id} 的宿主为 {e.reference}
    </label>))}
    <div className="flex flex-wrap items-end gap-2"><label className="text-sm">基线编号<Input required maxLength={128} placeholder="例如 BL-20261119" value={id} disabled={busy} onChange={e => setID(e.target.value)} /></label>
      <Button variant="primary" disabled={busy || !id.trim()} type="submit">确认冻结基线</Button></div>
  </form>
}

function RegisterEditor({ register, clusterId, busy, save }: { register: Register; clusterId?: string; busy: boolean; save: (value: Register) => void }) {
  const [document, setDocument] = useState(register.document_ref)
  const [text, setText] = useState(register.entries.map(e => [e.kind, e.asset_id, e.name, e.source.cluster, e.source.instance].join("\t").trimEnd()).join("\n"))
  const [error, setError] = useState("")
  function submit(event: React.FormEvent) {
    event.preventDefault()
    const entries: RegisterEntry[] = []
    for (const [i, line] of text.split(/\r?\n/).entries()) {
      if (!line.trim()) continue
      const fields = line.split("\t").map(v => v.trim())
      const [kind, asset_id, name, cluster = "", instance = ""] = fields
      if ((kind !== "machines" && kind !== "systems") || !asset_id || !name || !(fields.length === 3 || (clusterId && fields.length === 5 && cluster && instance))) {
        setError(`第 ${i + 1} 行的类型、必填列或来源目标有误。`); return
      }
      entries.push({ kind, asset_id, name, source: { cluster, instance } })
    }
    setError(""); save({ document_ref: document.trim(), entries, revision: register.revision })
  }
  return <details className="card-surface p-4" open={!register.revision}><summary className="cursor-pointer font-semibold">维护{clusterId ? "集群" : "平台直属"}盘点底册 · 版本 {register.revision}</summary>
    <form onSubmit={submit} className="mt-3 space-y-3"><label className="block">盘点凭据编号<Input required maxLength={256} value={document} disabled={busy} onChange={e => setDocument(e.target.value)} placeholder="例如 CMDB-2026-11-19" /></label>
      <p className="text-sm text-muted-foreground">从表格粘贴在役资产，每行依次填写类型（machines 或 systems）、稳定编号、名称，用制表符分列。{clusterId ? "可追加来源 cluster、instance 两列，指定未发现资产对操作员的可见目标。" : "这里登记平台直属资产。集群底册和在役台账会自动并入平台统计。"}</p>
      <label className="block">底册清单<Textarea rows={5} value={text} disabled={busy} onChange={e => setText(e.target.value)} /></label>
      {error && <p role="alert" className="status-error">{error}</p>}<Button disabled={busy || !document.trim()} type="submit">确认保存整份底册（{text.split(/\r?\n/).filter(line => line.trim()).length} 项）</Button>
    </form></details>
}

function CoverageWorkspace({ clusterId }: { clusterId?: string }) {
  const [baseline, setBaseline] = useState("")
  const [kind, setKind] = useState("all")
  const [status, setStatus] = useState("all")
  const [revision, refresh] = useState(0)
  const [loaded, setLoaded] = useState<{ key: string; data: Report } | null>(null)
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const operation = useRef<AbortController | null>(null)
  const query = new URLSearchParams({ kind, status })
  if (clusterId !== undefined) query.set("cluster_id", clusterId)
  if (baseline) query.set("baseline_id", baseline)
  const address = `${endpoint}?${query}`
  const key = `${revision}:${address}`
  const report = loaded?.key === key ? loaded.data : null
  useEffect(() => {
    const controller = new AbortController()
    setError(""); setLoaded(null)
    consoleFetch(address, { cache: "no-store", signal: controller.signal }).then(checked).then(r => r.json()).then((data: Report) => {
      if (!controller.signal.aborted) setLoaded({ key, data })
    }).catch(err => { if (!controller.signal.aborted) setError(err.message) })
    return () => controller.abort()
  }, [address, key])
  useEffect(() => () => operation.current?.abort(), [])
  const changeBaseline = (id: string) => { setBaseline(id); setKind("all"); setStatus("all") }

  async function perform(request: { action: "save_register"; register: Register } | { action: "export" } | { action: "freeze"; baseline_id: string; expected_revision?: number; verified_hosts: Host[] } | { action: "set_machine_active"; machine_id: string; active: boolean; expected_revision: number }) {
    if (operation.current || !report) return
    const controller = new AbortController(); operation.current = controller
    setBusy(true); setError("")
    try {
      if (request.action !== "export") {
        await checked(await consoleFetch(endpoint, { method: "POST", headers: { "Content-Type": "application/json" }, signal: controller.signal,
          body: JSON.stringify({ ...request, cluster_id: clusterId }) }))
        if (request.action === "freeze") changeBaseline(request.baseline_id)
      } else {
        // Pin "latest" to the exact version displayed even if another admin freezes concurrently.
        if (report.baseline) query.set("baseline_id", report.baseline.id)
        query.set("format", "csv")
        const response = await checked(await consoleFetch(`${endpoint}?${query}`, { cache: "no-store", signal: controller.signal }))
        const blob = await response.blob()
        if (controller.signal.aborted) return
        const url = URL.createObjectURL(blob); const link = document.createElement("a")
        link.href = url; link.download = `coverage-${clusterId || "platform"}-${report.baseline?.id || "live"}-${kind}-${status}.csv`; link.click()
        window.setTimeout(() => URL.revokeObjectURL(url), 1000)
        if (report.baseline) setBaseline(report.baseline.id)
      }
      refresh(value => value + 1)
    } catch (err) {
      if (!controller.signal.aborted) { setLoaded(null); setError(err instanceof Error ? err.message : "操作失败") }
    } finally { if (!controller.signal.aborted) setBusy(false); operation.current = null }
  }
  return <section aria-label="资产覆盖率对账" className="space-y-4 p-4">
    <header className="flex flex-wrap items-center justify-between gap-2"><h2 className="text-xl font-semibold">拓扑覆盖率对账</h2><Button size="sm" disabled={busy} onClick={() => refresh(value => value + 1)}>刷新对账</Button></header>
    {error && <p role="alert" className="status-error">{error}</p>}
    {!report && !error && <p role="status">正在读取盘点基线…</p>}
    {report && <>
      <div className="flex flex-wrap items-center gap-2"><label>基线 <Select aria-label="基线" scale="sm" value={baseline || report.baseline?.id || ""} disabled={busy} onChange={e => changeBaseline(e.target.value)}>
        {!report.baselines.length && <option value="">待建立基线</option>}{report.baselines.map(v => <option key={v.id} value={v.id}>{v.id} · {new Date(v.frozen_at).toLocaleString()}</option>)}<option value="live">当前台账预览</option></Select></label>
        <span className="text-sm text-muted-foreground">{clusterId && `集群 ${clusterId} · `}{report.scope === "platform" ? (clusterId ? "完整底册范围" : "平台范围") : "我的可见范围"}</span></div>
      {report.register && <RegisterEditor key={report.register.revision} register={report.register} clusterId={clusterId} busy={busy} save={register => perform({action:"save_register",register})} />}
      {report.state === "scope_unavailable" ? <p>此历史基线未冻结该集群的底册范围。可选择其他基线或进入当前台账维护底册。</p> : report.state === "register_missing" ? <p>此集群尚无已确认的底册。请平台管理员录入盘点凭据和在役清单。</p> : report.state === "missing" ? <p>待建立基线。可选择「当前台账预览」核对盘点范围。</p> : <>
        {report.document_ref && <p className="text-sm">盘点凭据 {report.document_ref} · 底册版本 {report.register_revision}</p>}
        <CoverageMetrics metrics={report.metrics} />
        <p className="text-sm">{report.scope === "visible" ? "仅对当前授权范围测算。" : report.state === "live" ? "当前台账预览。冻结后生成验收结论。" : report.accepted === null ? "存在空分母，无法出具双 80% 验收结论。" : report.accepted ? `${clusterId ? "集群" : "平台"}验收通过，两项均达到 80%。` : `${clusterId ? "集群" : "平台"}验收未通过，两项尚未同时达到 80%。`}</p>
        <div className="flex flex-wrap items-center gap-2"><label>对象 <Select aria-label="对象" scale="sm" value={kind} disabled={busy} onChange={e => setKind(e.target.value)}><option value="all">全部对象</option><option value="machines">服务器</option><option value="systems">业务系统</option></Select></label>
          <label>结论 <Select aria-label="结论" scale="sm" value={status} disabled={busy} onChange={e => setStatus(e.target.value)}><option value="all">全部结论</option><option value="gap">缺口</option><option value="covered">已覆盖</option></Select></label>
          <Button size="sm" disabled={busy} onClick={() => perform({ action: "export" })}>导出当前明细 CSV</Button></div>
        {report.rows.length ? <div className="overflow-x-auto"><table className="w-full text-left text-sm"><caption className="sr-only">资产逐项对账</caption><thead><tr>{["资产 / 编号", "结论", "关系证据与访问来源"].map(s => <th key={s} className="p-2">{s}</th>)}</tr></thead>
          <tbody>{report.rows.map(row => <tr className="border-t" key={`${row.kind}:${row.asset_id}`}><td className="p-2">{row.name}<small className="block text-muted-foreground">{labels[row.kind]} · {row.asset_id}</small>{row.memberships?.map(m => <small key={m.cluster_id} className="block text-muted-foreground">{m.cluster_id || "平台直属"} · {m.document_ref}</small>)}</td><td className="p-2">{row.covered ? "已覆盖" : "缺口"}</td><td className="p-2">{row.evidence.length ? row.evidence.map((e,i) => <p key={i}>{relationNames[e.type]} {e.reference} · {e.verified ? "已核验" : "待核验"}<small className="block text-muted-foreground">{[e.cluster_asset, e.source.cluster, e.source.instance].filter(Boolean).join(" / ") || "机器台账"}{e.verified_at && ` · ${new Date(e.verified_at).toLocaleString()}`}</small></p>) : "无可见关系证据"}</td></tr>)}</tbody></table></div> : <p className="text-sm text-muted-foreground">当前范围及筛选下没有记录。</p>}
        {!clusterId && report.state === "live" && report.scope === "platform" && <MachineLifecycle machines={report.machine_lifecycle || []} busy={busy} change={m => perform({ action: "set_machine_active", machine_id: m.asset_id, active: !m.active, expected_revision: m.revision })} />}
        {report.state === "live" && report.scope === "platform" && kind === "all" && status === "all" && <FreezeForm rows={report.rows} busy={busy} freeze={(id,hosts) => perform({ action: "freeze", baseline_id: id, expected_revision: clusterId ? report.register_revision : undefined, verified_hosts: hosts })} />}
      </>}
    </>}
  </section>
}

export function AssetCoveragePanel({ session, clusterId }: { session: Session; clusterId?: string }) {
  if (session.demo) return <p className="card-surface p-4">请连接 Gateway 查看盘点基线。</p>
  return <CoverageWorkspace clusterId={clusterId} key={JSON.stringify([clusterId, session.gateway, session.username, session.tokenId, session.token, session.isAdmin])} />
}
