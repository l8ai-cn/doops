# User Confirmation Workflow

DoSql is Agent-native. The user should confirm the intended database change, not
raw SQL text.

## Why Not SQL-First Confirmation

Many users cannot safely review complex SQL. Even when they can, SQL alone does
not explain business intent, target environment, risk, version impact or
verification steps.

The agent must explain database operations in business terms. Confirmation
concerns the intended change, target, impact, risk and verification. If the
user has already authorized that scope, proceed without asking again merely
because a plan or SQL artifact was generated. Ask only when material scope,
target or risk is unresolved or changes beyond that authorization.

## Confirmation Example

Use a readable explanation like the illustrative example below when presenting
a plan or clarifying missing authorization. This is a conversation convention,
not a mandatory JSON approval artifact or a separate execution gate:

```json
{
  "format": "human_readable",
  "title": "确认变更：为订单表增加「external_id」字段",
  "summary": "新增字段用于保存外部系统订单号，便于售后和对账。",
  "target": {
    "projectId": "proj_erp",
    "environmentId": "test",
    "databaseAssetId": "db_orders_test",
    "engine": "mysql"
  },
  "version": {
    "from": 3,
    "to": 4,
    "label": "dosql_000004"
  },
  "changes": [
    "为订单表增加 external_id 字段，类型为 varchar(64)，允许为空。"
  ],
  "risks": [
    "需要确认应用代码已经兼容新增字段。"
  ],
  "verification": [
    "执行后检查订单表是否存在 external_id 字段。"
  ]
}
```

The example's version 3 to 4 is hypothetical. A plan must not claim that example
versions, caller-supplied values or in-memory fixture state are real database
state. Cite live read/journal evidence when known; otherwise say the current
version is unverified and leave the actual apply version to the execution journal.

The SQL or MongoDB command is still generated and stored as an execution artifact,
but it is not the primary confirmation UI. The former `propose-confirmation`
Node entrypoint has been removed: `agent-change-flow.mjs` remains an internal
test fixture, not a general change planner, confirmation service or executor.

## Execution Gate

There is no signed-approval artifact. A change is admitted by the Gateway:
the caller needs the grant on the selected target (one Agent = one
authorization unit) and the target must carry a database credential binding. The change then runs through
`doops sql --mode apply --sql/--file`, which submits the statements verbatim,
runs rollback-capable batches with the optional read-only `--verify` in one
transaction, and records a version in the per-database hash-chained journal
before touching the database. MySQL DDL commits implicitly; later
verification failure cannot undo it. Explain this boundary before an authorized
DDL operation, execute it separately from DML without `--dry-run`, and read back
the schema. MongoDB non-transactional operations have the same dry-run and mixing
restrictions; see [agent-cli-contract.md](agent-cli-contract.md).
The earlier signed `dosql.operation-approval.v1` path had no issuer and was
removed.
