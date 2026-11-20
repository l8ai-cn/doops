"use client"

import { useEffect, useState, type FormEvent } from "react"
import { assetRequest } from "@/lib/assets"
import { sessionIsAdmin, type Session } from "@/lib/client"
import { Button, Input, Select } from "./ui"

type Props = { assetId?: string; machineId?: string; session: Session; cluster?: string; instance?: string }
type Disclosure = { open: boolean; onOpenChange: (open: boolean) => void }
type ClusterSource = { asset_id: string; cluster: string; instance: string }
type LinkKey = ClusterSource & { machine_id: string; node_name: string }
type Link = LinkKey & { source: "manual" }
type Machine = { asset_id: string; name: string }

export function AssetNodeLinks({ session, ...scope }: Props & Disclosure) {
  if (Boolean(scope.assetId) === Boolean(scope.machineId) || Boolean(scope.cluster) !== Boolean(scope.instance)) return null
  const admin = sessionIsAdmin(session)
  const identity = [session.gateway, session.username, session.tokenId, session.issuedAt, admin, scope.assetId, scope.machineId, scope.cluster, scope.instance]
  return <NodeLinks key={JSON.stringify(identity)} {...scope} admin={admin} />
}

function NodeLinks({ assetId, machineId, cluster, instance, admin, open, onOpenChange }: Omit<Props, "session"> & Disclosure & { admin: boolean }) {
  const [version, setVersion] = useState(0)
  const [links, setLinks] = useState<Link[] | null>(null)
  const [editing, setEditing] = useState(false)
  const [removing, setRemoving] = useState<Link | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const [notice, setNotice] = useState("")

  useEffect(() => {
    if (!open) return
    let active = true
    assetRequest<{ links: Link[] }>("node-links").then(({ links }) => {
      if (active) setLinks(links.filter((link) => machineId ? link.machine_id === machineId : link.asset_id === assetId &&
        (!cluster || (link.cluster === cluster && link.instance === instance))))
    }).catch((reason: Error) => { if (active) setError(reason.message) })
    return () => { active = false }
  }, [open, version, assetId, machineId, cluster, instance])

  function refresh() {
    setLinks(null)
    setError("")
    setRemoving(null)
    setEditing(false)
    setVersion((v) => v + 1)
  }

  async function change(action: "bind" | "unbind", { asset_id, cluster, instance, node_name, machine_id }: LinkKey) {
    setBusy(true)
    setError("")
    setNotice("")
    try {
      await assetRequest("node-links", { action, asset_id, cluster, instance, node_name, machine_id })
      setNotice(action === "bind" ? "已保存手工关联。" : "已解除手工关联。")
      refresh()
    } catch (reason) {
      setError((reason as Error).message)
    } finally { setBusy(false) }
  }

  return <section className="mt-3 min-w-0 border-t pt-2 text-sm">
    <button type="button" aria-expanded={open} disabled={busy} className="text-left font-medium" onClick={() => {
      onOpenChange(!open); setLinks(null); setError(""); setNotice(""); setRemoving(null); setEditing(false)
    }}><span aria-hidden="true">{open ? "▾" : "▸"} </span>{machineId ? "关联的 K8s Node" : "Node 与机器关联"}</button>
    {open && <div className="mt-2 space-y-2">
      <p className="text-xs text-muted-foreground">手工登记关系，Node 名称未经集群验证。</p>
      {error && <p role="alert" className="break-words text-destructive">{error}</p>}
      {notice && <p role="status" className="text-primary">{notice}</p>}
      {links === null ? !error && <p role="status">正在读取关联…</p> : links.length ? <ul className="divide-y">
        {links.map((link) => <li key={JSON.stringify([link.cluster, link.instance, link.node_name])} className="flex flex-wrap items-center justify-between gap-2 py-1.5">
          <div className="min-w-0 break-all"><span className="font-mono">{link.node_name}</span><span className="ml-2 text-xs text-muted-foreground">手工关联</span>
            <div className="text-xs text-muted-foreground">{machineId ? `集群 ${link.asset_id}` : `机器 ${link.machine_id}`} · {link.cluster}/{link.instance}</div>
          </div>
          {admin && (removing === link ? <div className="flex gap-2">
            <Button type="button" size="sm" variant="danger-outline" disabled={busy} onClick={() => void change("unbind", link)}>确认解除</Button>
            <Button type="button" size="sm" disabled={busy} onClick={() => setRemoving(null)}>取消</Button>
          </div> : <Button type="button" size="sm" variant="ghost" disabled={busy} onClick={() => setRemoving(link)}>解除关联</Button>)}
        </li>)}
      </ul> : <p className="text-muted-foreground">暂无可见的手工关联。</p>}
      <div className="flex flex-wrap gap-2">
        {admin && links !== null && !editing && <Button type="button" size="sm" disabled={busy} onClick={() => { setEditing(true); setRemoving(null); setError(""); setNotice("") }}>添加手工关联</Button>}
        <Button type="button" size="sm" disabled={busy || (links === null && !error)} onClick={refresh}>刷新关联</Button>
      </div>
      {editing && <BindNodeForm assetId={assetId} machineId={machineId} cluster={cluster} instance={instance} busy={busy}
        onCancel={() => setEditing(false)} onBind={(key) => void change("bind", key)} />}
    </div>}
  </section>
}

function BindNodeForm({ assetId, machineId, cluster, instance, busy, onCancel, onBind }: Omit<Props, "session"> & {
  busy: boolean; onCancel: () => void; onBind: (key: LinkKey) => void
}) {
  const fixedSource = Boolean(assetId && cluster && instance)
  const [choices, setChoices] = useState<{ machines: Machine[]; sources: ClusterSource[] } | null>(null)
  const [error, setError] = useState("")
  useEffect(() => {
    let active = true
    Promise.all([
      machineId ? Promise.resolve([]) : assetRequest<{ machines: Machine[] }>("machines").then((r) => r.machines),
      fixedSource ? Promise.resolve([]) : assetRequest<{ clusters: ClusterSource[] }>("clusters").then((r) => r.clusters.filter((s) => !assetId || s.asset_id === assetId)),
    ]).then(([machines, sources]) => { if (active) setChoices({ machines, sources }) })
      .catch((reason: Error) => { if (active) setError(reason.message) })
    return () => { active = false }
  }, [machineId, assetId, fixedSource])

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const data = new FormData(event.currentTarget)
    const selected = fixedSource ? { asset_id: assetId!, cluster: cluster!, instance: instance! } : choices?.sources.find((s) => JSON.stringify([s.cluster, s.instance]) === data.get("target"))
    if (selected) onBind({ ...selected, node_name: String(data.get("node")), machine_id: machineId || String(data.get("machine")) })
  }
  return <form className="space-y-2" onSubmit={submit}>
    {error ? <p role="alert" className="text-destructive">{error}</p> : !choices ? <p role="status">正在读取可选资产…</p> : <fieldset disabled={busy} className="grid min-w-0 gap-2 sm:grid-cols-2">
      {!machineId && <label>机器资产<Select name="machine" required defaultValue=""><option value="">{choices.machines.length ? "请选择机器" : "暂无可见机器"}</option>
        {choices.machines.map((m) => <option key={m.asset_id} value={m.asset_id}>{m.name} · {m.asset_id}</option>)}</Select></label>}
      {!fixedSource && <label>已导入的集群来源<Select name="target" required defaultValue=""><option value="">{choices.sources.length ? "请选择来源" : "暂无可见来源"}</option>
        {choices.sources.map((s) => <option key={JSON.stringify([s.cluster, s.instance])} value={JSON.stringify([s.cluster, s.instance])}>{s.asset_id} · {s.cluster}/{s.instance}</option>)}</Select></label>}
      <label>Node 完整名称<Input name="node" required maxLength={253} autoCapitalize="none" spellCheck={false} placeholder="如 worker-01.example" /></label>
    </fieldset>}
    <div className="flex gap-2"><Button type="submit" size="sm" variant="primary" disabled={busy || !choices || (!machineId && !choices.machines.length) || (!fixedSource && !choices.sources.length)}>保存关联</Button>
      <Button type="button" size="sm" disabled={busy} onClick={onCancel}>取消</Button></div>
  </form>
}
