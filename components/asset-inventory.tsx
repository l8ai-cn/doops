"use client"

import { useRef, useState, type ReactNode } from "react"
import { Activity, Boxes, Layers, Server } from "lucide-react"
import { assetRequest } from "@/lib/assets"
import { fetchTargets, sessionIsAdmin, type Session, type Target } from "@/lib/client"
import { Button, PageTabs } from "./ui"
import { MachineAssetsPanel } from "./machine-assets-panel"
import { ClusterAssetsPanel } from "./cluster-assets-panel"
import { AssetPlatformPanel } from "./asset-platform-panel"
import { BusinessAssetsPanel } from "./business-assets-panel"

const sections = [
  { id: "platform" as const, label: "平台看板", icon: <Activity className="size-4" /> },
  { id: "machines" as const, label: "机器台账", icon: <Server className="size-4" /> },
  { id: "clusters" as const, label: "集群台账", icon: <Boxes className="size-4" /> },
  { id: "systems" as const, label: "业务系统", icon: <Layers className="size-4" /> },
]

type TargetTab = "overview" | "credentials" | "k8s"
export type MachinePage = {
  section: "machines"; machineId: string; query?: string; kind?: "all" | "physical" | "vm"; nodesOpen?: boolean
}
export type ClusterSourcePage = {
  section: "clusters"; source: { assetId: string; cluster: string; instance: string; namespace?: string }
  query: string; expanded: string[]
}
type TargetOrigin = (MachinePage | ClusterSourcePage) & { returnTab?: TargetTab }
export type AssetPage =
  | { section: (typeof sections)[number]["id"] }
  | { section: "clusters"; clusterId: string; from: "platform" | "clusters" }
  | TargetOrigin

export function assetReturnLabel(page: AssetPage) {
  if ("machineId" in page) return `返回机器 ${page.machineId}`
  if ("source" in page) {
    const { assetId, cluster, instance, namespace } = page.source
    return `返回集群 ${assetId} · ${cluster}/${instance}${namespace ? ` · ${namespace}` : ""}`
  }
  if ("clusterId" in page) return `返回集群 ${page.clusterId}`
  return `返回${sections.find(item => item.id === page.section)!.label}`
}

type TargetAction = { target?: Target; tab: TargetTab; origin: TargetOrigin; label: string }
export type RenderAssetTargetAction = (action: TargetAction) => ReactNode
type OpenTarget = (target: Target, tab: TargetTab, namespace: string | undefined, origin: AssetPage) => void

function actionKey(origin: TargetOrigin, tab: TargetTab) {
  return JSON.stringify(origin.section === "machines" ? [tab, "machines", origin.machineId]
    : [tab, "clusters", origin.source.assetId, origin.source.cluster, origin.source.instance, origin.source.namespace])
}

async function verifyTarget(session: Session, target: Target, origin: TargetOrigin) {
  const matchesTarget = (item: { cluster?: string; instance?: string }) => item.cluster === target.cluster && item.instance === target.instance
  const binding = origin.section === "machines"
    ? assetRequest<{ machines: { asset_id: string; cluster?: string; instance?: string }[] }>("machines")
      .then(({ machines }) => machines.some(row => row.asset_id === origin.machineId && matchesTarget(row)))
    : assetRequest<{ clusters: { asset_id: string; cluster: string; instance: string; namespaces: { name: string }[] }[] }>("clusters")
      .then(({ clusters }) => clusters.some(row => row.asset_id === origin.source.assetId && matchesTarget(row) &&
        (!origin.source.namespace || row.namespaces.some(ns => ns.name === origin.source.namespace))))
  const [visible, bound] = await Promise.all([fetchTargets(session), binding])
  const online = visible.find(item => matchesTarget(item) && item.status !== "offline")
  if (!online) throw new Error("目标已离线或当前账号无权访问。请刷新台账。")
  if (!bound) throw new Error("资产关联或可见范围已变化。请刷新台账。")
  return online
}

function TargetActionButton({ target, label, restoreFocus, enter }: {
  target?: Target; label: string; restoreFocus: (button: HTMLButtonElement) => void
  enter: (button: HTMLButtonElement) => Promise<void>
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  return <span className="inline-flex flex-col items-start gap-1">
    <Button ref={button => { if (button) restoreFocus(button) }} size="sm" disabled={!target || busy} onClick={async event => {
      setBusy(true)
      setError("")
      try { await enter(event.currentTarget) } catch (reason) { setError(reason instanceof Error ? reason.message : String(reason)) }
      finally { setBusy(false) }
    }}>{busy ? "正在核验…" : label}</Button>
    {error && <span role="alert" className="text-xs text-destructive">{error}</span>}
  </span>
}

export function AssetInventory({ session, targets, initialPage, onOpenTarget }: {
  session: Session
  targets: Target[]
  initialPage?: AssetPage
  onOpenTarget: OpenTarget
}) {
  const [page, setPage] = useState<AssetPage>(initialPage ?? { section: "platform" })
  const [initialAction] = useState(() => initialPage && "returnTab" in initialPage && initialPage.returnTab
    ? actionKey(initialPage, initialPage.returnTab) : undefined)
  const returnFocus = useRef(initialAction)
  const attempt = useRef(0)
  const renderTargetAction: RenderAssetTargetAction = ({ target, tab, origin, label }) => {
    const key = actionKey(origin, tab)
    return <TargetActionButton key={key} target={target} label={label} restoreFocus={button => {
      if (returnFocus.current === key) {
        returnFocus.current = undefined
        button.scrollIntoView({ block: "nearest" })
        button.focus({ preventScroll: true })
      }
    }} enter={async trigger => {
      if (!target) return
      const current = ++attempt.current
      const online = await verifyTarget(session, target, origin)
      if (current === attempt.current && trigger.isConnected) {
        onOpenTarget(online, tab, origin.section === "clusters" ? origin.source.namespace : undefined, { ...origin, returnTab: tab })
      }
    }} />
  }
  const detail = "clusterId" in page ? page : undefined
  // A changed online set invalidates previous reads. Gateway may still return authorized offline assets.
  const scope = sessionIsAdmin(session) ? "admin" : JSON.stringify(targets
    .filter((target) => target.status !== "offline")
    .map(({ cluster, instance }) => JSON.stringify([cluster, instance])).sort())
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <PageTabs ariaLabel="资产类型" items={sections} value={page.section} onChange={section => setPage({ section })} />
      <div key={JSON.stringify([scope, page])} className="min-h-0 flex-1 overflow-auto p-4">
        {session.demo ? <p className="card-surface p-4 text-sm">请连接 Gateway 查看和维护持久化资产台账。</p>
          : <>
            {(page.section === "platform" || detail) && <AssetPlatformPanel session={session} targets={targets}
              cluster={detail && {
                id: detail.clusterId,
                from: detail.from === "platform" ? "平台看板" : "集群台账",
                onBack: () => setPage({ section: detail.from }),
              }}
              onCluster={clusterId => setPage({ section: "clusters", clusterId, from: "platform" })}
              onMachine={machineId => setPage({ section: "machines", machineId })} />}
            {page.section === "machines" && <MachineAssetsPanel initialPage={"machineId" in page ? page : undefined} session={session} targets={targets} renderTargetAction={renderTargetAction} />}
            {page.section === "clusters" && !detail && <ClusterAssetsPanel session={session} targets={targets}
              onOpenCluster={clusterId => setPage({ section: "clusters", clusterId, from: "clusters" })} initialPage={"source" in page ? page : undefined} renderTargetAction={renderTargetAction} />}
            {page.section === "systems" && <BusinessAssetsPanel session={session} targets={targets}/>}
          </>}
      </div>
    </div>
  )
}
