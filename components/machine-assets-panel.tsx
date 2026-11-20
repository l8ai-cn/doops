"use client"

import { useEffect, useState, type FormEvent } from "react"
import { sessionIsAdmin, type Session, type Target } from "@/lib/client"
import { assetRequest } from "@/lib/assets"
import { Button, Input, SegmentedControl, Select } from "./ui"
import { MachineDiagnosticPanel } from "./machine-diagnostic-panel"
import { MachineCsvImport } from "./machine-csv-import"
import type { MachinePage, RenderAssetTargetAction } from "./asset-inventory"
import { AssetNodeLinks } from "./asset-node-links"

type Machine = {
  asset_id: string
  name: string
  kind: "physical" | "vm"
  address: string
  cpu_count?: number | null
  memory_gib?: number | null
  disk_gib?: number | null
  host_asset_id?: string
  cluster?: string
  instance?: string
  last_seen?: string
  revision: number
  login_accounts?: { username: string; credential_id: string }[]
}
type PanelProps = { session: Session; targets: Target[]; initialPage?: MachinePage; renderTargetAction: RenderAssetTargetAction }
type Screen = { page: "list" | "create" | "csv" } | { page: "detail" | "edit" | "login"; id: string }
const kindLabel = { physical: "物理机", vm: "虚拟机" }
const kindOptions = [{ value: "all", label: "全部" }, { value: "physical", label: "物理机" }, { value: "vm", label: "虚拟机" }] as const
const capacities = [
  { key: "cpu_count", label: "CPU 数", unit: "核" },
  { key: "memory_gib", label: "内存", unit: "GiB" },
  { key: "disk_gib", label: "磁盘", unit: "GiB" },
] as const

function bindingKey({ cluster, instance }: Pick<Machine, "cluster" | "instance">) {
  return cluster && instance ? JSON.stringify([cluster, instance]) : ""
}

function connection(machine: Machine, target?: Target) {
  if (target) return "Agent 在线"
  return bindingKey(machine) ? "Agent 离线" : "未接入"
}

export function MachineAssetsPanel({ session, targets, initialPage, renderTargetAction }: PanelProps) {
  const identity = JSON.stringify([session.gateway, session.username, session.tokenId, session.issuedAt, session.isAdmin])
  return <Inventory key={identity} session={session} targets={targets} initialPage={initialPage} renderTargetAction={renderTargetAction} />
}

function Inventory({ session, targets, initialPage, renderTargetAction }: PanelProps) {
  const admin = sessionIsAdmin(session)
  const [machines, setMachines] = useState<Machine[] | null>(null)
  const [readError, setReadError] = useState("")
  const [version, setVersion] = useState(0)
  const [screen, setScreen] = useState<Screen>(initialPage ? { page: "detail", id: initialPage.machineId } : { page: "list" })
  const [nodesOpen, setNodesOpen] = useState(initialPage?.nodesOpen ?? false)
  const [kind, setKind] = useState<Machine["kind"] | "all">(initialPage?.kind ?? "all")
  const [query, setQuery] = useState(initialPage?.query ?? "")

  function refresh() {
    setMachines(null)
    setReadError("")
    setVersion((value) => value + 1)
  }
  useEffect(() => {
    let active = true
    async function load() {
      try {
        const result = await assetRequest<{ machines: Machine[] }>("machines")
        if (!Array.isArray(result.machines)) throw new Error("服务器返回的机器列表无效")
        if (active) setMachines(result.machines)
      } catch (error) {
        if (active) setReadError(error instanceof Error ? error.message : String(error))
      }
    }
    void load()
    return () => { active = false }
  }, [version])

  const online = new Map(targets.filter((t) => t.cluster && t.instance && t.status !== "offline").map((t) => [bindingKey(t), t]))
  const selected = "id" in screen ? machines?.find((m) => m.asset_id === screen.id) : undefined
  function detail(id: string) {
    setNodesOpen(false)
    setScreen({ page: "detail", id })
    if (!admin) refresh()
  }
  const back = () => setScreen({ page: "list" })
  const term = query.trim().toLowerCase()
  const filtered = screen.page === "list" ? machines?.filter((m) => (kind === "all" || m.kind === kind) &&
    [m.asset_id, m.name, m.address, m.cluster && `${m.cluster}/${m.instance}`].some((field) => field?.toLowerCase().includes(term))) ?? [] : []

  return (
    <section aria-label="机器台账" className="card-surface min-w-0 space-y-4 p-4">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-xl font-semibold">机器台账</h2>
        {screen.page === "list" && <div className="flex flex-wrap gap-2">
          <Button size="sm" onClick={refresh} disabled={!machines && !readError}>刷新</Button>
          {admin && <>
            <Button size="sm" disabled={!machines} onClick={() => setScreen({ page: "csv" })}>CSV 导入</Button>
            <Button size="sm" variant="primary" disabled={!machines} onClick={() => setScreen({ page: "create" })}>手工新增</Button>
          </>}
        </div>}
      </header>
      {readError ? <div role="alert" className="status-error flex flex-wrap items-center justify-between gap-2">
        <span className="break-words">读取机器台账失败。{readError}</span><Button size="sm" onClick={refresh}>重新读取台账</Button>
      </div> : !machines && <p role="status" className="text-sm text-muted-foreground">正在读取机器台账…</p>}
      {admin && screen.page === "csv" && <MachineCsvImport machines={machines ?? []} onClose={back} onReadback={refresh} />}
      {admin && screen.page === "login" && <MachineCsvImport machines={[]} machine={{asset_id: screen.id, name: selected?.name ?? screen.id}} onClose={() => detail(screen.id)} onReadback={refresh} />}
      {machines && screen.page === "list" && <>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <SegmentedControl ariaLabel="机器类型" value={kind} options={kindOptions} onChange={setKind} />
          <Input type="search" scale="sm" className="w-full sm:w-72" aria-label="搜索机器" placeholder="搜索编号、名称、地址或目标" value={query} onChange={(e) => setQuery(e.target.value)} />
        </div>
        {filtered.length ? <div className="overflow-x-auto rounded-lg border">
          <table className="w-full text-left text-sm">
            <thead className="bg-muted/40 text-xs text-muted-foreground"><tr>
              <th scope="col" className="px-3 py-2">机器 / 资产编号</th><th scope="col" className="px-3 py-2">类型</th>
              <th scope="col" className="hidden px-3 py-2 sm:table-cell">管理地址</th><th scope="col" className="px-3 py-2">Agent</th>
            </tr></thead>
            <tbody className="divide-y">{filtered.map((m) => {
              const target = online.get(bindingKey(m))
              return <tr key={m.asset_id} className="hover:bg-muted/30">
                <td className="px-3 py-2"><button type="button" onClick={() => detail(m.asset_id)} className="block break-all text-left font-medium text-primary hover:underline">{m.name}<span className="block font-mono text-xs text-muted-foreground">{m.asset_id}</span></button></td>
                <td className="whitespace-nowrap px-3 py-2">{kindLabel[m.kind]}</td>
                <td className="hidden break-all px-3 py-2 font-mono text-xs sm:table-cell">{m.address || "未登记"}</td>
                <td className="px-3 py-2 text-xs"><span className={target ? "text-success" : "text-muted-foreground"}>{connection(m, target)}</span>
                  {bindingKey(m) && <span className="hidden break-all font-mono text-muted-foreground md:block">{m.cluster}/{m.instance}</span>}</td>
              </tr>
            })}</tbody>
          </table>
        </div> : <div className="space-y-2 py-8 text-center text-sm text-muted-foreground" role="status">
          <p>{machines.length ? "没有匹配的机器" : admin ? "尚未登记机器，可手工新增或从 CSV 导入。" : "当前账号没有可查看的机器。"}</p>
          {machines.length > 0 && <Button size="sm" onClick={() => { setQuery(""); setKind("all") }}>清除筛选</Button>}
        </div>}
      </>}
      {machines && "id" in screen && !selected && <div role="status" className="space-y-2 text-sm"><p>这台机器已不在可见台账中。</p><Button size="sm" onClick={back}>返回台账</Button></div>}
      {screen.page === "detail" && selected && machines && <>
        <MachineDetail machine={selected} machines={machines} target={online.get(bindingKey(selected))}
          onBack={back} onSelect={detail} onEdit={admin ? () => setScreen({ page: "edit", id: selected.asset_id }) : undefined}
          origin={{ section: "machines", machineId: selected.asset_id, kind, query, nodesOpen }} renderTargetAction={renderTargetAction} />
        {admin && <section aria-label="机器登录凭据" className="space-y-2 border-t pt-3">
          <div className="flex flex-wrap items-center justify-between gap-2"><h4 className="font-medium">机器登录凭据</h4>
            <Button size="sm" onClick={() => setScreen({ page: "login", id: selected.asset_id })}>添加登录凭据</Button></div>
          {selected.login_accounts?.length ? <ul>{selected.login_accounts.map(login => <li className="text-sm" key={login.credential_id}>{login.username} · 已保存</li>)}</ul>
            : <p className="text-sm text-muted-foreground">{Array.isArray(selected.login_accounts) ? "尚无已保存的账号" : "未取得登录凭据状态"}</p>}
        </section>}
        <MachineDiagnosticPanel isAdmin={admin} key={JSON.stringify([selected.asset_id, selected.cluster, selected.instance])} assetId={selected.asset_id} cluster={selected.cluster} instance={selected.instance} />
        <AssetNodeLinks key={selected.asset_id} machineId={selected.asset_id} session={session} open={nodesOpen} onOpenChange={(open) => {
          setNodesOpen(open)
          if (open && !admin) refresh()
        }} />
      </>}
      {admin && machines && (screen.page === "create" || (screen.page === "edit" && selected)) && <MachineForm key={selected?.asset_id ?? ""}
        machine={selected} machines={machines} online={online} onCancel={() => selected ? detail(selected.asset_id) : back()}
        onReload={refresh} onSaved={(id) => { detail(id); refresh() }} />}
    </section>
  )
}

function MachineDetail({ machine, machines, target, onBack, onSelect, onEdit, origin, renderTargetAction }: {
  machine: Machine; machines: Machine[]; target?: Target; onBack: () => void; onSelect: (id: string) => void
  onEdit?: () => void; origin: MachinePage; renderTargetAction: RenderAssetTargetAction
}) {
  const host = machines.find((m) => m.kind === "physical" && m.asset_id === machine.host_asset_id)
  const children = machines.filter((m) => m.kind === "vm" && m.host_asset_id === machine.asset_id)
  const lastSeen = target?.last_seen || machine.last_seen
  const facts = [
    ["资产编号", machine.asset_id], ["类型", kindLabel[machine.kind]], ["管理地址", machine.address || "未登记"],
    ...capacities.map(({ key, label, unit }) => [label, machine[key] == null ? "未登记" : `${machine[key]} ${unit}`]),
    ["Agent 状态", connection(machine, target)], ["关联目标", bindingKey(machine) ? `${machine.cluster}/${machine.instance}` : "未关联"],
    ...(lastSeen ? [["最后在线", lastSeen]] : []),
  ]
  const relation = (m: Machine) => <Button key={m.asset_id} size="sm" className="max-w-full whitespace-normal break-all text-left" onClick={() => onSelect(m.asset_id)}>{m.name} · {m.asset_id}</Button>
  return <div className="space-y-4">
    <div className="flex flex-wrap items-center justify-between gap-2"><h3 className="break-all font-semibold">{machine.name}</h3>
      <div className="flex flex-wrap gap-2"><Button size="sm" onClick={onBack}>返回台账</Button>{onEdit && <Button variant="primary" size="sm" onClick={onEdit}>编辑机器</Button>}</div></div>
    <dl className="grid grid-cols-2 gap-4 rounded-lg border p-4 sm:grid-cols-3 lg:grid-cols-4">{facts.map(([label, value]) => <div key={label} className="min-w-0">
      <dt className="text-xs text-muted-foreground">{label}</dt><dd className="mt-1 break-all text-sm">{value}</dd></div>)}</dl>
    <div className="space-y-2"><h4 className="text-sm font-medium">{machine.kind === "vm" ? "宿主物理机" : "名下虚拟机"}</h4>
      <div className="flex flex-wrap gap-2 text-sm text-muted-foreground">{machine.kind === "vm" ? host ? relation(host) : "暂无可查看的宿主" : children.length ? children.map(relation) : "暂无可见虚拟机"}</div></div>
    {bindingKey(machine) ? <div className="flex flex-wrap gap-2">
      {renderTargetAction({ target, origin, tab: "overview", label: "进入目标运维" })}
      {renderTargetAction({ target, origin, tab: "credentials", label: "查看当前目标可用凭据" })}
    </div> : <p className="text-xs text-muted-foreground">关联在线 Agent 后可进入目标运维和查看凭据。</p>}
  </div>
}

function MachineForm({ machine, machines, online, onCancel, onSaved, onReload }: {
  machine?: Machine; machines: Machine[]; online: Map<string, Target>; onCancel: () => void; onSaved: (id: string) => void; onReload: () => void
}) {
  const [kind, setKind] = useState(machine?.kind ?? "physical")
  const savedBinding = machine ? bindingKey(machine) : ""
  const [binding, setBinding] = useState(savedBinding)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const unavailable = Boolean(binding && !online.has(binding) && binding !== savedBinding)
  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy || unavailable) return
    const data = new FormData(event.currentTarget)
    const field = (key: string) => String(data.get(key) ?? "").trim()
    const asset_id = field("asset_id"), name = field("name")
    if (!asset_id || !name) { setError("请填写资产编号和显示名称。"); return }
    const capacity: Partial<Pick<Machine, (typeof capacities)[number]["key"]>> = {}
    for (const { key, label } of capacities) {
      if (!field(key)) continue
      const value = Number(field(key))
      if (!Number.isFinite(value) || value <= 0 || (key === "cpu_count" && !Number.isInteger(value))) {
        setError(`${label}须为${key === "cpu_count" ? "正整数" : "正数"}。`); return
      }
      capacity[key] = value
    }
    const target = online.get(binding) ?? (binding && binding === savedBinding ? machine : undefined)
    const next = { asset_id, name, kind, address: field("address"), ...capacity,
      host_asset_id: kind === "vm" ? field("host_asset_id") || undefined : undefined, cluster: target?.cluster, instance: target?.instance }
    setBusy(true)
    setError("")
    try {
      await assetRequest("machines", machine ? { action: "update", machine: next, expected_revision: machine.revision } : { action: "create", machine: next })
      onSaved(asset_id)
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause))
    } finally { setBusy(false) }
  }
  return <form onSubmit={save} className="space-y-4">
    <h3 className="font-semibold">{machine ? "编辑机器" : "手工新增机器"}</h3>
    <fieldset disabled={busy} className="grid min-w-0 gap-3 text-xs text-muted-foreground sm:grid-cols-2 lg:grid-cols-3 [&_label]:grid [&_label]:min-w-0 [&_label]:gap-1.5">
      <label>资产编号（必填）<Input name="asset_id" required readOnly={!!machine} defaultValue={machine?.asset_id} /></label>
      <label>显示名称（必填）<Input name="name" required defaultValue={machine?.name} /></label>
      <label>机器类型<Select aria-label="机器类型" value={kind} onChange={(e) => setKind(e.target.value as Machine["kind"])}><option value="physical">物理机</option><option value="vm">虚拟机</option></Select></label>
      <label>管理地址<Input name="address" placeholder="IP 或主机名" defaultValue={machine?.address} /></label>
      {capacities.map(({ key, label, unit }) => <label key={key}>{label}（{unit}）<Input name={key} type="number" min={key === "cpu_count" ? 1 : 0} step={key === "cpu_count" ? 1 : "any"} defaultValue={machine?.[key] ?? ""} /></label>)}
      {kind === "vm" && <label>宿主物理机<Select aria-label="宿主物理机" name="host_asset_id" defaultValue={machine?.host_asset_id ?? ""}><option value="">未登记宿主</option>
        {machines.filter((m) => m.kind === "physical" && m.asset_id !== machine?.asset_id).map((m) => <option key={m.asset_id} value={m.asset_id}>{m.name} · {m.asset_id}</option>)}
      </Select></label>}
      <label>关联 Agent 目标<Select aria-label="关联 Agent 目标" value={binding} onChange={(e) => setBinding(e.target.value)}><option value="">不关联目标</option>
        {savedBinding && !online.has(savedBinding) && <option value={savedBinding}>{machine?.cluster}/{machine?.instance}（已保存，离线）</option>}
        {unavailable && <option value={binding} disabled>所选目标已离线，请重新选择</option>}
        {Array.from(online, ([key, t]) => <option key={key} value={key}>{t.cluster}/{t.instance}</option>)}
      </Select></label>
    </fieldset>
    {error && <div role="alert" className="status-error space-y-2"><p className="break-words">{error}</p><Button type="button" size="sm" disabled={busy} onClick={onReload}>放弃草稿并重新读取台账</Button></div>}
    <div className="flex flex-wrap gap-2"><Button type="submit" size="sm" variant="primary" disabled={busy || unavailable}>{busy ? "正在保存…" : "保存机器"}</Button><Button type="button" size="sm" disabled={busy} onClick={onCancel}>取消</Button></div>
  </form>
}
