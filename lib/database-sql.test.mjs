import assert from "node:assert/strict"
import test from "node:test"
import { buttonNamed, componentHarness, componentStubs, elementText } from "./component-test-harness.mjs"

const source = "../components/database-sql.tsx"
const props = {
  session: { gateway: "qa" }, target: { cluster: "local", instance: "desktop" }, sessionId: "browser",
  asset: { namespace: "qa", engine: "postgres", service: "pg", database_asset_id: "pg-qa", database_name: "postgres" },
  database: "qa_db", credential: { id: "cred-a", name: "pg-reader" }, disabled: false, onRunningChange() {},
}
const statement = { columns: ["marker", "note"], rows: [["from-db", "NULL"], ["second", ""]], truncated: true }
const receipt = { operation_id: "dbop-test", database: props.database, credential: props.credential.name, result: { statements: [statement] } }

function mount(name, options, queryDatabase) {
  return componentHarness(source, name, options, { "./ui": componentStubs, "@/lib/database-browser": { queryDatabase } })
}

test("editor executes the current SQL against the catalog selection and clears the receipt on failure", async () => {
  const requests = []
  const options = { ...props, initialSql: "SELECT 1" }
  const view = mount("DatabaseSql", options, async (request) => {
    requests.push(request)
    if (requests.length === 2) throw new Error("dosql_query_requires_read_only")
    return receipt
  })
  await view.flush()
  const run = () => view.one(buttonNamed("执行只读 SQL"))
  const input = () => view.one((n) => n.type === "textarea")
  assert.equal(input().props.value, options.initialSql)
  assert.equal(view.nodes((n) => ["Input", "input", "Select", "select"].includes(n.type)).length, 0)
  input().props.onChange({ target: { value: "  " } })
  await view.flush()
  assert.ok(run().props.disabled)
  const sql = "SELECT marker FROM qa_probe\n"
  input().props.onChange({ target: { value: sql } })
  options.disabled = true
  await view.flush()
  assert.ok(input().props.disabled && run().props.disabled)
  await run().props.onClick()
  assert.equal(requests.length, 0)
  options.disabled = false
  await view.flush()
  await run().props.onClick()
  await view.flush()
  const { signal, onCancelReady, ...context } = requests[0]
  assert.equal(signal.aborted, false)
  assert.equal(typeof onCancelReady, "function")
  assert.deepEqual(context, { session: props.session, target: props.target, sessionId: props.sessionId, engine: props.asset.engine, database: props.database, credential: props.credential, sql })
  const region = (n) => n.props["aria-label"] === "SQL 查询结果"
  assert.match(elementText(view.one(region)), /qa_db · pg-reader · dbop-test/)
  assert.equal(view.one((n) => n.type.name === "DatabaseRows").props.statement, statement)
  await run().props.onClick()
  await view.flush()
  assert.equal(view.nodes(region).length, 0)
  assert.match(elementText(view.one((n) => n.props.role === "alert")), /requires_read_only/)
})

test("stop failure permits retry and an accepted stop holds the running state until the query finishes", async () => {
  const states = []
  let attempts = 0
  let finishQuery
  const view = mount("DatabaseSql", { ...props, initialSql: "SELECT 1", onRunningChange: (value) => states.push(value) }, ({ onCancelReady }) => {
    onCancelReady(async () => { if (++attempts === 1) throw new Error("未收到确认") })
    return new Promise((_resolve, reject) => { finishQuery = reject })
  })
  await view.flush()
  const run = view.one(buttonNamed("执行只读 SQL")).props.onClick()
  await view.flush()
  assert.deepEqual(states, [true])
  assert.ok(view.one((n) => n.type === "textarea").props.disabled)
  await view.one(buttonNamed("停止 SQL")).props.onClick()
  await view.flush()
  assert.match(elementText(view.one((n) => n.props.role === "alert")), /未收到确认/)
  await view.one(buttonNamed("停止 SQL")).props.onClick()
  await view.flush()
  assert.ok(view.one(buttonNamed("等待停止终态…")).props.disabled)
  assert.deepEqual(states, [true])
  finishQuery(new Error("查询已停止"))
  await run
  await view.flush()
  assert.deepEqual(states, [true, false])
  assert.match(elementText(view.one((n) => n.props.role === "alert")), /查询已停止/)
  assert.equal(view.one(buttonNamed("执行只读 SQL")).props.disabled, false)
})

test("row viewer preserves driver values, column order, empty-result headers and truncation", async () => {
  const options = { statement }
  const view = mount("DatabaseRows", options)
  await view.flush()
  const contents = (tag) => view.nodes((n) => n.type === tag).map(elementText)
  assert.deepEqual(contents("th"), statement.columns)
  assert.deepEqual(contents("td"), ["from-db", "NULL", "second", ""])
  assert.deepEqual(contents("p"), ["2 行 · 结果已截断"])
  options.statement = { columns: ["marker"] }
  await view.flush()
  assert.deepEqual(contents("th"), ["marker"])
  assert.deepEqual(contents("td"), [])
  assert.deepEqual(contents("p"), ["0 行"])
})
