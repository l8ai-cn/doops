"use client"

import { useEffect, useRef, useState } from "react"
import { assetRequest } from "@/lib/assets"
import { Button, Select } from "./ui"

const labels = {
  planned: "应检",
  completed: "已检",
  missed: "漏检",
  partial: "部分 / 证据不足",
  pending: "待执行",
}
type State = Exclude<keyof typeof labels, "planned">
type Duty = {
  asset_id: string
  plan_id: number
  name: string
  timezone: string
  due: number
  run_id?: number
  trigger_kind?: string
  status: State
  reconcile_due?: number
  items: number
  checked: number
  missing: number
  insufficient: number
}
type Weekly = Record<keyof typeof labels, number> & { rows: Duty[]; as_of: string }
type Props = { clusterId?: string; onOpen: (runId: number) => void }
const at = (unix: number, zone: string) =>
  new Date(unix * 1000).toLocaleString("zh-CN", { timeZone: zone, hour12: false })

export function AssetWeeklySummary(props: Props) {
  return <WeeklyScope key={props.clusterId || ""} {...props} />
}
function WeeklyScope({ clusterId, onOpen }: Props) {
  const [summary, setSummary] = useState<Weekly | null>(null),
    [error, setError] = useState(""),
    [revision, refresh] = useState(0)
  const [filter, setFilter] = useState<State | "all">("all"),
    [busy, setBusy] = useState(false)
  const live = useRef(true)
  useEffect(() => {
    live.current = true
    return () => {
      live.current = false
    }
  }, [])
  useEffect(() => {
    let current = true
    setSummary(null)
    setError("")
    const query = new URLSearchParams({ view: "weekly", ...(clusterId ? { cluster_id: clusterId } : {}) })
    assetRequest<Weekly>("inspections", undefined, query)
      .then((value) => {
        if (current) setSummary(value)
      })
      .catch((e) => {
        if (current) setError(e.message)
      })
    return () => {
      current = false
    }
  }, [clusterId, revision])
  async function reconcile(row: Duty) {
    setBusy(true)
    setError("")
    try {
      const result = await assetRequest<{ run_id: number }>("inspections", {
        action: "reconcile",
        plan_id: row.plan_id,
        due: row.reconcile_due,
      })
      if (!live.current) return
      if (row.reconcile_due === row.due) onOpen(result.run_id)
      refresh((v) => v + 1)
    } catch (e) {
      if (live.current) setError((e as Error).message)
    } finally {
      if (live.current) setBusy(false)
    }
  }
  const rows = summary?.rows.filter((r) => filter === "all" || r.status === filter) || []
  return (
    <section aria-label="本周巡检" className="card-surface min-w-0 space-y-3 p-4">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="font-semibold">本周巡检</h2>
          <p className="text-sm text-muted-foreground">
            {clusterId || "平台可见范围"} · 各计划本地时间周一至周日
          </p>
        </div>
        <Button size="sm" disabled={busy} onClick={() => refresh((v) => v + 1)}>
          刷新周检
        </Button>
      </header>
      {error && (
        <p role="alert" className="status-error">
          {error}
        </p>
      )}
      {!summary && !error && <p role="status">正在读取本周巡检…</p>}
      {summary && (
        <>
          <dl className="grid grid-cols-2 gap-3 sm:grid-cols-5">
            {(Object.keys(labels) as (keyof typeof labels)[]).map((k) => (
              <div key={k}>
                <dt className="text-sm text-muted-foreground">{labels[k]}</dt>
                <dd className="text-xl font-semibold tabular-nums">{summary[k]}</dd>
              </div>
            ))}
          </dl>
          <p className="text-xs text-muted-foreground">
            按可见资产编号去重。全部周检项取得有效证据计已检，含异常发现。存在漏项计漏检，其余已取部分证据或证据不足计部分。
          </p>
          {summary.rows.length > 0 && (
            <label className="flex flex-wrap items-center gap-2 text-sm">
              计划明细
              <Select
                scale="sm"
                className="w-auto"
                value={filter}
                onChange={(e) => setFilter(e.target.value as State | "all")}
              >
                <option value="all">全部状态</option>
                {(["missed", "partial", "pending", "completed"] as State[]).map((s) => (
                  <option key={s} value={s}>
                    {labels[s]}
                  </option>
                ))}
              </Select>
            </label>
          )}
          {!rows.length ? (
            <p className="text-sm">
              {summary.rows.length ? "该状态下暂无明细。" : "当前范围本周无应检资产。"}
            </p>
          ) : (
            <ul className="divide-y">
              {rows.map((r) => (
                <li
                  key={`${r.plan_id}/${r.asset_id}/${r.due}`}
                  className="flex flex-wrap items-center justify-between gap-3 py-2 text-sm"
                >
                  <div className="min-w-0 break-words">
                    <strong>
                      {r.asset_id} · {r.name}
                    </strong>
                    <p>
                      {labels[r.status]} · 有效取证 {r.checked}/{r.items} 项
                      {r.missing > 0 ? ` · 漏 ${r.missing} 项` : ""}
                      {r.insufficient > 0 ? ` · 证据不足 ${r.insufficient} 项` : ""}
                    </p>
                    <p className="text-xs text-muted-foreground">
                      {at(r.due, r.timezone)} {r.timezone} ·{" "}
                      {r.run_id
                        ? r.trigger_kind === "reconciled"
                          ? "漏期对账"
                          : "定时运行"
                        : r.status === "missed"
                          ? "逾期未对账"
                          : "等待定时运行"}
                    </p>
                  </div>
                  {r.run_id ? (
                    <Button size="sm" onClick={() => onOpen(r.run_id!)}>
                      报告与整改 #{r.run_id}
                    </Button>
                  ) : r.reconcile_due ? (
                    <Button size="sm" disabled={busy} onClick={() => void reconcile(r)}>
                      {r.reconcile_due === r.due
                        ? "对账整期并打开报告"
                        : `先对账前序周期 ${at(r.reconcile_due, r.timezone)}`}
                    </Button>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  )
}
