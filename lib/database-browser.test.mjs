import assert from "node:assert/strict"
import test from "node:test"
import { CATALOG_PAGE_SIZE, databaseListSql, tableListSql, tablePreviewSql, queryDatabase } from "./database-browser.ts"

const options = {
  session: { gateway: "https://gateway.example.test" }, target: { cluster: "test", instance: "node" },
  sessionId: "catalog-test", engine: "postgres", database: "app", credential: { id: "cred-01", name: "reader" }, sql: "SELECT 1\n",
}
const receipt = { operation_id: "dbop_01", mode: "query", engine: "postgres", database: "app", credential: "reader",
  result: { status: "verified", statements: [{ columns: ["name"], rows: [["orders"]] }] } }

function resultEvent(report) {
  return new Response(JSON.stringify({ type: "result", result: { content: [{ type: "text", text: JSON.stringify(report) }] } }) + "\n")
}

test("catalogs fetch one lookahead row, including after the executor's 200-row cap", () => {
  assert.equal(CATALOG_PAGE_SIZE, 40)
  for (const engine of ["postgres", "mysql", "mssql", "oracle"]) {
    for (const sql of [databaseListSql(engine, 200), tableListSql(engine, "app", 200)]) {
      assert.match(sql, /LIMIT 41 OFFSET 200$|OFFSET 200 ROWS FETCH NEXT 41 ROWS ONLY$/)
    }
    assert.match(tablePreviewSql(engine, "one", "orders", 50), /LIMIT 51 OFFSET 50$|OFFSET 50 ROWS FETCH NEXT 51 ROWS ONLY$/)
    assert.match(tablePreviewSql(engine, "one", "orders", 0, 50), /LIMIT 50 OFFSET 0$|OFFSET 0 ROWS FETCH NEXT 50 ROWS ONLY$/)
  }
})

test("literal dots, delimiters and semicolons stay inside their own identifier", () => {
  for (const engine of ["postgres", "mssql"]) {
    assert.ok(tablePreviewSql(engine, 'team."a', 'log];old"').includes('FROM "team.""a"."log];old""" '))
  }
  assert.ok(tablePreviewSql("mysql", 'team.`a', 'log`;old').includes('FROM `team.``a`.`log``;old` '))
  assert.throws(() => tablePreviewSql("oracle", "OWNER", 'LOG"S'), /双引号/)
  assert.ok(tableListSql("postgres", "a\\b'c").includes("E'a\\\\b''c'"))
  assert.ok(tableListSql("mysql", "a\\b'c").includes("X'615c622763'"))
  assert.ok(tableListSql("mssql", "a\\b'c").includes("N'a\\b''c'"))
})

test("unsupported engine, unusable names and invalid page bounds fail explicitly", async () => {
  for (const engine of ["mongo", "mongodb", "unknown"]) {
    assert.throws(() => databaseListSql(engine), /不支持/)
    assert.throws(() => tableListSql(engine, "app"), /不支持/)
    assert.throws(() => tablePreviewSql(engine, "a", "b"), /不支持/)
    await assert.rejects(queryDatabase({ ...options, engine }), /不支持/)
  }
  for (const name of ["", "bad\0name", "x\ufeff"]) assert.throws(() => tablePreviewSql("postgres", name, "t"))
  for (const offset of [-1, 0.5, Infinity, Number.MAX_SAFE_INTEGER + 1, "10"]) assert.throws(() => databaseListSql("mssql", offset), RangeError)
  for (const limit of [0, 52, NaN]) assert.throws(() => tablePreviewSql("mysql", "sample", "t", 0, limit), RangeError)
})

test("first lookup binds the credential by ID and returns the verified entry database", async t => {
  const requests = []
  t.mock.method(globalThis, "fetch", async (_url, init) => {
    requests.push(JSON.parse(init.body))
    return resultEvent({ ...receipt, result: { status: "verified", statements: [{ columns: ["name"] }] } })
  })
  let cancel
  const result = await queryDatabase({ ...options, database: "", onCancelReady: fn => { cancel = fn } })
  assert.equal(result.database, "app")
  assert.deepEqual(result.result.statements[0].rows, [])
  assert.equal(typeof cancel, "function")
  await queryDatabase(options)
  for (const [i, request] of requests.entries()) {
    assert.deepEqual([request.cluster, request.instance, request.tool], ["test", "node", "doops_agent_prompt"])
    assert.deepEqual(request.arguments, { session_id: "catalog-test", operation: "ask", skill: "dosql", credential_mode: "auto",
      sql_credential: "cred-01", ...(i ? { sql_database: "app" } : {}), sql: options.sql, instruction: "DoSql 原样只读 SQL" })
  }
})

test("reject mismatched identity, non-query outcomes and malformed result rows", async t => {
  let report
  t.mock.method(globalThis, "fetch", async () => resultEvent(report))
  for (const difference of [{ mode: "apply" }, { database: "other" }, { credential: "cred-01" }, { engine: "mysql" }, { operation_id: " " },
    { result: { ...receipt.result, status: "failed" } }, { result: { ...receipt.result, statements: [] } },
    { result: { ...receipt.result, statements: [{ columns: ["name"], rows: [["x", "y"]] }] } },
    { result: { ...receipt.result, statements: [{ columns: ["name"], rows: [[42]] }] } }]) {
    report = { ...receipt, ...difference }
    await assert.rejects(queryDatabase(options), /执行回执/)
  }
  report = { ...receipt, database: "" }
  await assert.rejects(queryDatabase({ ...options, database: "" }), /执行回执/)
})
