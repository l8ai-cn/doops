"use client"

import { useEffect, useRef, useState } from "react"
import type { Session, Target, ToolCancel } from "@/lib/client"
import { queryDatabase } from "@/lib/database-browser"
import type { DatabaseCandidate } from "./database-inventory"
import { Button } from "./ui"

type QueryResult = Awaited<ReturnType<typeof queryDatabase>>
type Statement = QueryResult["result"]["statements"][number]

export function DatabaseRows({ statement }: { statement: Statement }) {
  const { columns, rows, truncated } = statement
  return <div className="min-w-0">
    {!!columns?.length && <div className="max-h-80 max-w-full overflow-auto">
      <table className="w-max min-w-full border-collapse text-left font-mono text-xs">
        <thead><tr>{columns.map((column, i) => <th key={i} scope="col" className="whitespace-pre border p-2">{column}</th>)}</tr></thead>
        <tbody>{rows?.map((row, i) => <tr key={i}>{row.map((cell, j) => <td key={j} className="whitespace-pre border p-2">{cell}</td>)}</tr>)}</tbody>
      </table>
    </div>}
    <p className="mt-1 text-xs text-muted-foreground">{rows?.length ?? 0} 行{truncated && " · 结果已截断"}</p>
  </div>
}

export function DatabaseSql({ session, target, sessionId, asset, database, credential, initialSql = "", disabled, onRunningChange }: {
  session: Session; target: Target; sessionId: string; asset: DatabaseCandidate
  database: string; credential: { id: string; name: string }; initialSql?: string
  disabled: boolean; onRunningChange: (value: boolean) => void
}) {
  const [sql, setSql] = useState(initialSql)
  const [running, setRunning] = useState(false)
  const [stopping, setStopping] = useState(false)
  const [cancel, setCancel] = useState<ToolCancel | null>(null)
  const [error, setError] = useState("")
  const [report, setReport] = useState<QueryResult | null>(null)
  const active = useRef<AbortController | null>(null)
  const region = useRef<HTMLElement>(null)

  useEffect(() => {
    region.current?.scrollIntoView({ block: "nearest" })
    setSql(initialSql)
    setReport(null)
    setError("")
    setRunning(false)
    setStopping(false)
    setCancel(null)
    return () => { active.current?.abort(); active.current = null }
  }, [session, target.cluster, target.instance, sessionId, asset.database_asset_id, asset.engine, database, credential.id, credential.name, initialSql])

  const canRun = !disabled && !!database.trim() && !!credential.id && !!credential.name && !!sql.trim()
  async function run() {
    if (!canRun || active.current) return
    const controller = new AbortController()
    active.current = controller
    controller.signal.addEventListener("abort", () => onRunningChange(false), { once: true })
    setRunning(true)
    onRunningChange(true)
    setError("")
    setReport(null)
    try {
      const receipt = await queryDatabase({ session, target, sessionId, engine: asset.engine, database, credential, sql,
        signal: controller.signal, onCancelReady: (stop) => { if (!controller.signal.aborted) setCancel(() => stop) },
      })
      if (!controller.signal.aborted) setReport(receipt)
    } catch (err) {
      if (!controller.signal.aborted) setError(err instanceof Error ? err.message : String(err))
    } finally {
      if (!controller.signal.aborted) {
        active.current = null
        setCancel(null)
        setRunning(false)
        setStopping(false)
        onRunningChange(false)
      }
    }
  }

  async function stop() {
    const request = active.current
    if (!request || !cancel || stopping) return
    setStopping(true)
    setError("")
    try { await cancel() } catch (err) {
      if (active.current === request) {
        setError(`停止请求失败：${String(err)}`)
        setStopping(false)
      }
    }
  }

  return <section className="card-surface mt-4 min-w-0 space-y-3 p-4" aria-label="只读 SQL 操作" ref={region}>
    <h3 className="text-sm font-semibold">只读 SQL</h3>
    <p className="break-all font-mono text-xs text-muted-foreground">{target.cluster}/{target.instance} · {asset.namespace}/{asset.service} · {asset.engine} · {database} · {credential.name}</p>
    <label className="grid gap-1 text-xs">SQL 语句<textarea className="min-h-24 min-w-0 rounded-md border bg-background p-2 font-mono text-sm" disabled={disabled || running} value={sql} onChange={(e) => setSql(e.target.value)} spellCheck={false} /></label>
    <div className="flex justify-end">{running ? <Button onClick={stop} disabled={!cancel || stopping}>{stopping ? "等待停止终态…" : "停止 SQL"}</Button> : <Button onClick={run} disabled={!canRun}>执行只读 SQL</Button>}</div>
    {error && <p role="alert" className="break-words text-sm text-destructive">{error}</p>}
    {report && <div aria-label="SQL 查询结果" role="region" className="min-w-0 space-y-2">
      <p className="break-all font-mono text-xs text-muted-foreground">{report.database} · {report.credential} · {report.operation_id}</p>
      {report.result.statements.map((statement, i) => <DatabaseRows key={i} statement={statement} />)}
    </div>}
  </section>
}
