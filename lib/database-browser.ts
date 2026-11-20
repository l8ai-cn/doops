import { callTool, extractText, type Session, type Target, type ToolCancel } from "./client.ts"
import { TOOLS } from "./gateway.ts"

export const CATALOG_PAGE_SIZE = 40

export interface QueryResult {
  operation_id: string
  mode: "query"
  engine: string
  database: string
  credential: string
  result: { status: "verified"; statements: { columns?: string[]; rows?: string[][]; truncated?: boolean }[] }
}

interface QueryOptions {
  session: Session
  target: Target
  sessionId: string
  engine: string
  database: string
  credential: { id: string; name: string }
  sql: string
  signal?: AbortSignal
  onCancelReady?: (cancel: ToolCancel) => void
}

function requireEngine(engine: string): asserts engine is "postgres" | "mysql" | "mssql" | "oracle" {
  if (engine !== "postgres" && engine !== "mysql" && engine !== "mssql" && engine !== "oracle") {
    throw new Error(`不支持 ${engine} 的关系型 SQL 浏览`)
  }
}

function validStrings(value: unknown): value is string[] {
  return Array.isArray(value) && value.every(cell => typeof cell === "string")
}

function validStatement(statement: QueryResult["result"]["statements"][number]) {
  const columns = statement?.columns
  return validStrings(columns) && columns.length > 0 &&
    (statement.truncated === undefined || typeof statement.truncated === "boolean") &&
    (statement.rows === undefined || (Array.isArray(statement.rows) && statement.rows.every(row =>
      validStrings(row) && row.length === columns.length)))
}

export async function queryDatabase({ session, target, sessionId, engine, database, credential, sql, signal, onCancelReady }: QueryOptions): Promise<QueryResult> {
  requireEngine(engine)
  if (!credential.id.trim() || !credential.name.trim() || !sql.trim()) throw new Error("只读 SQL 需要选定凭据并填写语句")
  let text = ""
  await callTool(session, {
    cluster: target.cluster, instance: target.instance, tool: TOOLS.prompt, signal, onCancelReady,
    arguments: { session_id: sessionId, operation: "ask", skill: "dosql", credential_mode: "auto",
      sql_credential: credential.id, ...(database ? { sql_database: database } : {}), sql, instruction: "DoSql 原样只读 SQL" },
  }, event => { if (event.type === "result") text = extractText(event.result) })
  const receipt: QueryResult = JSON.parse(text)
  if (receipt?.mode !== "query" || receipt.result?.status !== "verified" || receipt.engine !== engine ||
      receipt.credential !== credential.name || typeof receipt.database !== "string" || !receipt.database.trim() ||
      (database && receipt.database !== database) || typeof receipt.operation_id !== "string" || !receipt.operation_id.trim() ||
      !Array.isArray(receipt.result.statements) || !receipt.result.statements.length || !receipt.result.statements.every(validStatement)) {
    throw new Error("只读 SQL 返回的执行回执与所选数据库不匹配")
  }
  // The Go receipt omits rows for a successful, empty result set.
  for (const statement of receipt.result.statements) statement.rows ??= []
  return receipt
}

function page(engine: string, offset: number, limit: number) {
  if (!Number.isSafeInteger(offset) || offset < 0 || !Number.isSafeInteger(limit) || limit < 1 || limit > 51) {
    throw new RangeError("分页 offset 必须是非负安全整数，limit 必须为 1 至 51 的整数")
  }
  return engine === "mssql" || engine === "oracle" ? `OFFSET ${offset} ROWS FETCH NEXT ${limit} ROWS ONLY` : `LIMIT ${limit} OFFSET ${offset}`
}

export function databaseListSql(engine: string, offset = 0): string {
  requireEngine(engine)
  const paging = page(engine, offset, CATALOG_PAGE_SIZE + 1)
  switch (engine) {
    case "postgres": return `SELECT datname AS name FROM pg_catalog.pg_database
WHERE datallowconn AND NOT datistemplate AND pg_catalog.has_database_privilege(oid, 'CONNECT') ORDER BY datname ${paging}`
    case "mysql": return `SELECT schema_name AS name FROM information_schema.schemata
WHERE schema_name NOT IN ('information_schema','mysql','performance_schema','sys','ndbinfo') ORDER BY BINARY schema_name ${paging}`
    case "mssql": return `SELECT name FROM sys.databases
WHERE database_id > 4 AND state = 0 AND HAS_DBACCESS(name) = 1 ORDER BY name ${paging}`
    default: return `SELECT SYS_CONTEXT('USERENV','SERVICE_NAME') AS "name" FROM dual ORDER BY 1 ${paging}`
  }
}

function checkName(name: string) {
  if (!name || /[\0\u0085\uFEFF]/u.test(name)) throw new Error("数据库对象名称为空或包含执行器不支持的字符")
}

export function tableListSql(engine: string, database: string, offset = 0): string {
  requireEngine(engine)
  checkName(database)
  const paging = page(engine, offset, CATALOG_PAGE_SIZE + 1)
  const literal = `'${database.replaceAll("'", "''")}'`
  switch (engine) {
    case "postgres": return `SELECT n.nspname AS schema, t.relname AS name FROM pg_catalog.pg_class t
JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
WHERE pg_catalog.current_database() = E${literal.replaceAll("\\", "\\\\")} AND t.relkind IN ('r','p')
AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
AND pg_catalog.has_schema_privilege(n.oid,'USAGE') AND pg_catalog.has_table_privilege(t.oid,'SELECT')
ORDER BY n.nspname, t.relname ${paging}`
    case "mysql": {
      // A hex string is independent of NO_BACKSLASH_ESCAPES. COLUMNS.PRIVILEGES includes active roles.
      // A SELECT on a column without a column-specific grant proves a broader grant for SELECT *.
      const hex = Array.from(new TextEncoder().encode(database), byte => byte.toString(16).padStart(2, "0")).join("")
      return `SELECT t.table_schema AS \`schema\`, t.table_name AS name FROM information_schema.tables t
WHERE BINARY t.table_schema = X'${hex}' AND BINARY t.table_schema = BINARY DATABASE() AND t.table_type = 'BASE TABLE'
AND t.table_schema NOT IN ('information_schema','mysql','performance_schema','sys','ndbinfo') AND EXISTS (
 SELECT 1 FROM information_schema.columns c WHERE BINARY c.table_schema = BINARY t.table_schema AND BINARY c.table_name = BINARY t.table_name
 AND FIND_IN_SET('select', c.privileges) > 0 AND NOT EXISTS (
  SELECT 1 FROM information_schema.column_privileges p WHERE BINARY p.table_schema = BINARY c.table_schema AND BINARY p.table_name = BINARY c.table_name
  AND p.column_name = c.column_name AND p.privilege_type = 'SELECT') AND NOT EXISTS (
  SELECT 1 FROM information_schema.role_column_grants p WHERE BINARY p.table_schema = BINARY c.table_schema AND BINARY p.table_name = BINARY c.table_name
  AND p.column_name = c.column_name AND p.privilege_type = 'SELECT'))
ORDER BY BINARY t.table_schema, BINARY t.table_name ${paging}`
    }
    case "mssql": return `SELECT s.name AS [schema], t.name AS name FROM sys.tables t JOIN sys.schemas s ON s.schema_id = t.schema_id
WHERE DB_NAME() = N${literal} AND DB_ID() > 4 AND t.is_ms_shipped = 0 AND s.name NOT IN ('sys','INFORMATION_SCHEMA')
AND HAS_PERMS_BY_NAME(QUOTENAME(s.name) + '.' + QUOTENAME(t.name),'OBJECT','SELECT') = 1
AND NOT EXISTS (SELECT 1 FROM sys.columns c WHERE c.object_id = t.object_id AND c.is_hidden = 0
AND ISNULL(HAS_PERMS_BY_NAME(QUOTENAME(s.name) + '.' + QUOTENAME(t.name),'OBJECT','SELECT',c.name,'COLUMN'),0) = 0)
ORDER BY s.name, t.name ${paging}`
    default: return `SELECT t.owner AS "schema", t.table_name AS "name" FROM all_tables t JOIN all_users u ON u.username = t.owner
WHERE SYS_CONTEXT('USERENV','SERVICE_NAME') = ${literal} AND u.oracle_maintained = 'N' AND t.nested = 'NO' AND t.secondary = 'N'
AND (t.owner = USER OR EXISTS (SELECT 1 FROM session_privs WHERE privilege IN ('SELECT ANY TABLE','READ ANY TABLE'))
OR EXISTS (SELECT 1 FROM all_tab_privs p WHERE p.table_schema = t.owner AND p.table_name = t.table_name
 AND p.privilege IN ('SELECT','READ') AND (p.grantee IN (USER,'PUBLIC') OR p.grantee IN (SELECT role FROM session_roles))))
ORDER BY t.owner, t.table_name ${paging}`
  }
}

export function tablePreviewSql(engine: string, schema: string, table: string, offset = 0, limit = 51): string {
  requireEngine(engine)
  const quote = engine === "mysql" ? "`" : '"'
  const qualified = [schema, table].map(name => {
    checkName(name)
    if (engine === "oracle" && name.includes('"')) throw new Error("Oracle 对象名称不能包含双引号")
    return quote + name.replaceAll(quote, quote + quote) + quote
  }).join(".")
  const order = { postgres: " ORDER BY tableoid, ctid", mysql: "", mssql: " ORDER BY (SELECT NULL)", oracle: " ORDER BY ROWID" }[engine]
  return `SELECT * FROM ${qualified}${order} ${page(engine, offset, limit)}`
}
