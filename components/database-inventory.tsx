"use client"

import { Database, RefreshCw } from "lucide-react"
import { useEffect, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { Button, Input, Select } from "./ui"

export type DatabaseCandidate = {
  database_asset_id: string
  namespace: string
  service: string
  engine: string
  host: string
  port?: number
  database_name?: string
  status: string
  source: string
}

const ENGINES: Record<string, string> = { postgres: "PostgreSQL", mysql: "MySQL", mongodb: "MongoDB", mssql: "SQL Server" }

function isCandidate(value: unknown): value is DatabaseCandidate {
  if (!value || typeof value !== "object") return false
  const row = value as Record<string, unknown>
  return ["database_asset_id", "namespace", "service", "engine", "host", "status", "source"].every((key) => typeof row[key] === "string" && row[key] !== "")
    && (row.database_name === undefined || typeof row.database_name === "string")
    && (row.port === undefined || typeof row.port === "number")
}

async function readInventory(session: Session, cluster: string, instance: string, sessionId: string, signal: AbortSignal): Promise<DatabaseCandidate[]> {
  let text = ""
  await callTool(session, { cluster, instance, tool: TOOLS.databaseInventory, arguments: { session_id: sessionId }, signal }, (event) => {
    if (event.type === "result") text = extractText(event.result)
  })
  let payload
  try {
    payload = JSON.parse(text)
  } catch {
    throw new Error("数据库发现接口返回了无效 JSON")
  }
  if (payload?.status !== "discovered" || !Array.isArray(payload.candidates) || !payload.candidates.every(isCandidate)) {
    throw new Error("数据库发现结果格式无效")
  }
  return payload.candidates
}

export function DatabaseInventory({ session, target, sessionId, running, onQuery }: {
  session: Session
  target: Target
  sessionId: string
  running: boolean
  onQuery: (database: DatabaseCandidate) => void
}) {
  const [rows, setRows] = useState<DatabaseCandidate[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState("")
  const [revision, setRevision] = useState(0)
  const [engine, setEngine] = useState("")
  const [filter, setFilter] = useState("")
  const [selectedId, setSelectedId] = useState("")
  const { cluster, instance } = target

  useEffect(() => {
    const controller = new AbortController()
    setLoading(true)
    setError("")
    void readInventory(session, cluster, instance, sessionId, controller.signal).then((next) => {
      if (controller.signal.aborted) return
      setRows(next)
      setEngine((current) => next.some((row) => row.engine === current) ? current : "")
      setSelectedId((current) => next.some((row) => row.database_asset_id === current) ? current : "")
    }).catch((reason) => {
      if (controller.signal.aborted) return
      setRows([])
      setSelectedId("")
      setError(reason instanceof Error ? reason.message : String(reason))
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false)
    })
    return () => controller.abort()
  }, [session, cluster, instance, sessionId, revision])

  const term = filter.trim().toLowerCase()
  const visible = rows.filter((row) => (!engine || row.engine === engine) &&
    [row.service, row.namespace, row.database_name].some((value) => value?.toLowerCase().includes(term)))
  const selected = visible.find((row) => row.database_asset_id === selectedId)

  return (
    <section aria-label="数据库资源" className="card-surface min-w-0 p-4">
      <header className="mb-4 flex items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-2.5">
          <Database aria-hidden="true" className="size-5 shrink-0 text-primary" />
          <div className="min-w-0"><h2 className="text-base font-semibold">数据库资源</h2><p className="break-all font-mono text-xs text-muted-foreground">{cluster}/{instance}</p></div>
        </div>
        <Button size="sm" disabled={loading || running} onClick={() => setRevision((n) => n + 1)} aria-label="刷新数据库资源">
          <RefreshCw aria-hidden="true" className={`size-3.5 ${loading ? "motion-safe:animate-spin" : ""}`} />刷新
        </Button>
      </header>
      {loading ? <p role="status" className="py-6 text-center text-sm text-muted-foreground">正在发现数据库实例…</p> : error ? (
        <p role="alert" className="break-words rounded-lg bg-warning/10 px-3 py-3 text-sm text-warning">数据库发现失败 · {error}</p>
      ) : rows.length === 0 ? <p role="status" className="py-6 text-center text-sm text-muted-foreground">当前目标未发现数据库实例</p> : (
        <>
          <div className="mb-3 flex flex-wrap items-center gap-2">
            <Select scale="sm" aria-label="数据库引擎" value={engine} onChange={(e) => setEngine(e.target.value)}>
              <option value="">全部引擎</option>
              {[...new Set(rows.map((row) => row.engine))].sort().map((key) => <option key={key} value={key}>{ENGINES[key] ?? key}</option>)}
            </Select>
            <span className="text-xs text-muted-foreground">{visible.length} 个实例</span>
            <Input type="search" scale="sm" aria-label="筛选数据库" className="w-full sm:ml-auto sm:w-64" placeholder="按实例、库名或命名空间筛选" value={filter} onChange={(e) => setFilter(e.target.value)} />
          </div>
          {!visible.length ? <p role="status" className="py-6 text-center text-sm text-muted-foreground">没有匹配的数据库实例</p> : (
            <div className="max-h-[55dvh] overflow-auto rounded-lg border border-border/60">
              <table className="w-full text-left text-sm">
                <thead className="sticky top-0 bg-card text-xs text-muted-foreground"><tr>{["实例", "引擎", "数据库名", "命名空间"].map((label) => <th key={label} scope="col" className="px-3 py-2">{label}</th>)}</tr></thead>
                <tbody className="divide-y divide-border/50">{visible.map((row) => (
                  <tr key={row.database_asset_id} className={selected === row ? "bg-muted/50" : "hover:bg-muted/30"}>
                    <td className="px-3 py-2"><button type="button" aria-expanded={selected === row} onClick={() => setSelectedId(selected === row ? "" : row.database_asset_id)} className="break-all text-left font-mono text-primary hover:underline">{row.service}</button></td>
                    <td className="px-3 py-2">{ENGINES[row.engine] ?? row.engine}</td>
                    <td className="break-all px-3 py-2">{row.database_name || "待补充"}</td>
                    <td className="break-all px-3 py-2 text-muted-foreground">{row.namespace}</td>
                  </tr>
                ))}</tbody>
              </table>
            </div>
          )}
          {selected && (
            <div aria-label="实例详情" role="region" className="mt-3 rounded-lg border border-border/60 p-3">
              <div className="mb-3 flex flex-wrap items-center justify-between gap-2"><h3 className="break-all text-sm font-semibold">{selected.service}</h3><Button size="sm" disabled={running} onClick={() => onQuery(selected)}>带入只读查询</Button></div>
              <dl className="grid gap-3 text-xs sm:grid-cols-2">
                {Object.entries({ 地址: `${selected.host}${selected.port ? `:${selected.port}` : ""}`, 发现来源: selected.source,
                  "资产 ID": selected.database_asset_id, 发现状态: selected.status === "candidate" ? "已识别" : selected.status === "needs_database_name_or_credential" ? "待补充库名或凭据" : selected.status }).map(([label, value]) => (
                    <div key={label}><dt className="text-muted-foreground">{label}</dt><dd className="mt-1 break-all">{value}</dd></div>
                  ))}
              </dl>
            </div>
          )}
        </>
      )}
    </section>
  )
}
