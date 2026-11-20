"use client"

import { ChevronRight, Database, Table2 } from "lucide-react"
import { useEffect, useState } from "react"
import type { Session, Target } from "@/lib/client"
import { listCredentials, type CredentialResource } from "@/lib/credentials"
import { CATALOG_PAGE_SIZE, databaseListSql, queryDatabase, tableListSql, tablePreviewSql } from "@/lib/database-browser"
import type { DatabaseCandidate } from "./database-inventory"
import { DatabaseRows, DatabaseSql } from "./database-sql"
import { Button, Select } from "./ui"

type Connection = Pick<CredentialResource, "id" | "name">
type Statement = Awaited<ReturnType<typeof queryDatabase>>["result"]["statements"][number]
type Table = { schema: string; name: string }
type Page = { database: string; table?: Table; editor?: boolean }

export function DatabaseExplorer({ session, target, sessionId, asset, busy, onRunningChange, onBack, onQuery }: {
  session: Session; target: Target; sessionId: string; asset: DatabaseCandidate; busy: boolean
  onRunningChange: (value: boolean) => void; onBack: () => void; onQuery: (asset: DatabaseCandidate) => void
}) {
  const [connections, setConnections] = useState<Connection[] | null>(null)
  const [credentialId, setCredentialId] = useState("")
  const [page, setPage] = useState<Page>({ database: "" })
  const [offset, setOffset] = useState(0)
  const [revision, setRevision] = useState(0)
  const [credentialRevision, setCredentialRevision] = useState(0)
  const [credentialError, setCredentialError] = useState("")
  const [statement, setStatement] = useState<Statement | null>(null)
  const [error, setError] = useState("")
  const credential = connections?.find((item) => item.id === credentialId)
  const { database, table, editor } = page
  const supported = ["postgres", "mysql", "mssql", "oracle"].includes(asset.engine)
  const pageSize = table ? 50 : CATALOG_PAGE_SIZE
  const rows = statement?.rows ?? []
  const visible = rows.slice(0, pageSize)

  useEffect(() => {
    if (!supported) return
    const controller = new AbortController()
    setCredentialError("")
    setConnections(null)
    void listCredentials(session, target, "target", controller.signal).then((items) => {
      if (controller.signal.aborted) return
      const eligible = items.filter((item) => item.type === "opaque" && item.state === "active" && item.bindings?.some(({ scope, state }) =>
        state === "active" && scope.cluster === target.cluster && scope.instance === target.instance && scope.use === "database" &&
        scope.namespace === asset.namespace && (!scope.database_asset_id || scope.database_asset_id === asset.database_asset_id) &&
        (!scope.database_engine || scope.database_engine === asset.engine)))
      setConnections(eligible)
    }).catch((cause) => { if (!controller.signal.aborted) setCredentialError(String(cause instanceof Error ? cause.message : cause)) })
    return () => controller.abort()
  }, [session, target, asset, supported, credentialRevision])

  useEffect(() => {
    if (!credential || editor) return
    const controller = new AbortController()
    async function read() {
      if (!credential) return
      try {
        const sql = table ? tablePreviewSql(asset.engine, table.schema, table.name, offset)
          : database ? tableListSql(asset.engine, database, offset) : databaseListSql(asset.engine, offset)
        const report = await queryDatabase({ session, target, sessionId, engine: asset.engine, database, credential, sql, signal: controller.signal })
        if (controller.signal.aborted) return
        const data = report.result.statements[0]
        if (!data || (data.rows !== undefined && !Array.isArray(data.rows)) || (!table && data.rows?.some((row) => !row[0] || (database && !row[1])))) throw new Error("目录或数据响应缺少所需列")
        setStatement(data)
      } catch (cause) {
        if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : String(cause))
      }
    }
    void read()
    return () => controller.abort()
  }, [session, target, sessionId, asset, credential, database, table, editor, offset, revision])

  function navigate(next: Page, nextOffset = 0) {
    setStatement(null)
    setError("")
    setPage(next)
    setOffset(nextOffset)
    setRevision((n) => n + 1)
  }

  const title = editor ? "SQL 编辑器" : table ? table.name : database || asset.service
  const subject = table ? "表数据" : database ? "表列表" : "数据库列表"
  return (
    <section className="card-surface min-w-0 p-4" aria-label="数据库浏览器">
      <nav aria-label="数据库路径" className="mb-4 flex flex-wrap items-center gap-1 text-sm">
        <button type="button" disabled={busy} onClick={onBack} className="text-primary hover:underline">实例列表</button>
        <ChevronRight size={14} /><button type="button" disabled={busy || !database} onClick={() => navigate({ database: "" })} className="break-all text-primary hover:underline disabled:text-muted-foreground">{asset.service}</button>
        {database && <><ChevronRight size={14} /><button type="button" disabled={busy || (!table && !editor)} onClick={() => navigate({ database })} className="break-all text-primary hover:underline disabled:text-muted-foreground">{database}</button></>}
        {table && <><ChevronRight size={14} /><button type="button" disabled={busy || !editor} onClick={() => navigate({ database, table })} className="break-all text-primary disabled:text-muted-foreground">{table.schema}.{table.name}</button></>}
        {editor && <><ChevronRight size={14} /><span>SQL</span></>}
      </nav>
      <div className="mb-3 flex flex-wrap items-start justify-between gap-2">
        <div className="min-w-0"><h2 className="break-all text-lg font-semibold">{title}</h2><p className="break-all text-xs text-muted-foreground">{target.cluster}/{target.instance} · {asset.engine} · {asset.namespace}/{asset.service}</p></div>
        <div className="flex flex-wrap gap-2">
          {database && !editor && credential && <Button size="sm" variant="primary" disabled={busy || (!statement && !error)} onClick={() => navigate({ database, table, editor: true })}>执行 SQL</Button>}
          {!database && <Button size="sm" disabled={busy} onClick={() => onQuery(asset)}>带入 DoSql</Button>}
        </div>
      </div>
      {!database && <details className="mb-3 text-xs text-muted-foreground"><summary className="cursor-pointer">实例信息</summary><dl className="mt-2 grid gap-1 break-all"><div><dt>资产 ID</dt><dd>{asset.database_asset_id}</dd></div><div><dt>服务地址</dt><dd>{asset.host ? `${asset.host}${asset.port ? `:${asset.port}` : ""}` : "未发现"}</dd></div><div><dt>发现来源</dt><dd>{asset.source}</dd></div></dl></details>}
      {!supported ? <p className="py-4 text-sm">此引擎暂不支持关系数据库目录和 SQL 编辑器。</p> : <>
        {credentialError ? <div role="alert" className="text-sm text-warning">{credentialError} <Button size="sm" onClick={() => setCredentialRevision((n) => n + 1)}>重新读取凭据</Button></div> : !connections ? <p role="status">正在读取凭据…</p> : connections.length === 0 ? <p role="status" className="py-3 text-sm text-muted-foreground">请先在「凭据」中为当前目标和命名空间绑定数据库凭据。</p> : (
          <label className="mb-4 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">数据库凭据
            <Select scale="sm" value={credentialId} disabled={busy} onChange={(event) => { setCredentialId(event.target.value); navigate({ database }) }}>
              <option value="">选择此实例的连接凭据</option>{connections.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}
            </Select>
          </label>
        )}
        {credential && (editor ? <DatabaseSql session={session} target={target} sessionId={sessionId} asset={asset} database={database} credential={credential}
          initialSql={table ? tablePreviewSql(asset.engine, table.schema, table.name, 0, 50) : ""} disabled={busy} onRunningChange={onRunningChange} /> : <>
          <div className="mb-2 flex items-center justify-between gap-2"><h3 className="text-sm font-medium">{subject}</h3><Button size="sm" onClick={() => navigate(page, offset)} disabled={busy || (!statement && !error)}>刷新</Button></div>
          {error ? <p role="alert" className="break-words rounded-md bg-warning/10 p-3 text-sm text-warning">{error.startsWith("dosql_database_not_bound") ? `当前凭据未绑定数据库 ${database}。请选择该库的凭据。` : error}</p> : !statement ? <p role="status" className="py-6 text-sm text-muted-foreground">正在读取{subject}…</p> : <>
            {table ? <DatabaseRows statement={{ ...statement, rows: visible, truncated: false }} /> : visible.length === 0 ? <p role="status" className="py-5 text-sm text-muted-foreground">{database ? "当前库中没有可见的表" : "当前实例没有可见的数据库"}</p> : <div className="overflow-auto rounded-lg border border-border/60"><table className="w-full text-left text-sm"><thead className="bg-muted/40 text-xs text-muted-foreground"><tr><th scope="col" className="px-3 py-2">{database ? "表名" : "数据库名"}</th>{database && <th scope="col" className="px-3 py-2">Schema</th>}</tr></thead><tbody className="divide-y divide-border/50">{visible.map((row) => {
              const Icon = database ? Table2 : Database
              const label = database ? row[1] : row[0]
              return <tr key={JSON.stringify(row)} className="hover:bg-muted/30"><td className="px-3 py-2"><button type="button" disabled={busy} onClick={() => navigate(database ? { database, table: { schema: row[0], name: row[1] } } : { database: row[0] })} className="inline-flex items-center gap-2 break-all text-left font-mono text-primary hover:underline"><Icon size={14} aria-hidden="true" />{label}</button></td>{database && <td className="px-3 py-2 text-muted-foreground">{row[0]}</td>}</tr>
            })}</tbody></table></div>}
            {(offset > 0 || rows.length > pageSize) && <div className="mt-3 flex items-center justify-end gap-2 text-xs"><span>{visible.length ? `${offset + 1}–${offset + visible.length}` : "0 项"}</span><Button size="sm" disabled={busy || offset === 0} onClick={() => navigate(page, Math.max(0, offset - pageSize))}>上一页</Button><Button size="sm" disabled={busy || rows.length <= pageSize} onClick={() => navigate(page, offset + pageSize)}>下一页</Button></div>}
          </>}
        </>)}
      </>}
    </section>
  )
}
