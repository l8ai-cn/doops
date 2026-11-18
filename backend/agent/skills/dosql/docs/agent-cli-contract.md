# DoSql Agent CLI Contract

The DoSql Skill should expose a CLI that doagent can call. The CLI is the only
database execution path.

## Current Implementation

The bundled Agent CLI wrapper is:

```bash
node backend/agent/skills/dosql/scripts/dosql-agent.mjs <command> \
  --input request.json \
  --output response.json
```

Users normally reach this wrapper through `doops sql`. The wrapper is an
internal deterministic executor, not a second login or target authorization
entrypoint. Gateway identity, target scope and managed credential scope are
already established before the Agent invokes it.

Implemented commands:

- `classify`
- `query`
- `discover-databases`
- `discover-resources`
- `register-database`
- `compare-databases`
- `derive-change-plan-from-comparison`
- `render-forward-change-artifacts`
- `execute-forward-artifacts`
- `render-forward-evidence-metadata-commit`
- `resolve-database`
- `scan`
- `record-execution`
- `inspect-journal`
- `migrate-journal`
- `replay-timeline`
- `resolve-timeline-at`
- `render-timepoint-state-query`
- `execute-timepoint-state-query`
- `create-timepoint-state-manifest`
- `check-head-drift`
- `import-drift`
- `project-current-head`
- `render-metadata-commit`
- `create-baseline-records`
- `derive-initial-baseline-from-structure-snapshot`
- `derive-baseline-records-from-structure-snapshots`
- `render-baseline-records-commit`
- `render-change-metadata-commit`
- `record-metadata-commit-execution`
- `execute-metadata-commit`
- `plan-rollback`
- `plan-rollback-at`
- `render-restore-plan-metadata-commit`
- `render-schema-rollback-artifacts`
- `render-data-rollback-artifacts`
- `render-snapshot-restore-artifacts`
- `render-rollback-artifacts`
- `render-timeline-artifacts-metadata-commit`
- `execute-rollback-artifacts`
- `execute-restore-checks`
- `verify-restore`
- `verify-rollback-restore`
- `render-restore-evidence-metadata-commit`
- `finalize-restore`
- `derive-restore-baseline-records`

These implemented commands are deterministic wrappers around local policy,
discovery normalization, asset-registration, natural-language database
resolution, scan-analysis, execution-journal, timeline, forward/rollback artifact
and metadata commit modules. `query` and explicit `execute-*` commands open a
database connection only through the adapter supplied in the input.
The in-memory `agent-change-flow.mjs` fixture is test-only; it is not a
confirmation service or a source of live database versions. Plan output must
identify supplied or hypothetical state and cite live evidence before claiming
an actual version. Conversation confirmation follows
[user-confirmation.md](user-confirmation.md).

## Target Command Shape

The CLI should accept JSON input and return JSON output:

```bash
dosql-agent <command> --input request.json --output response.json
```

Every command must receive an `operationId` created by DoSql Server before
execution. Use a new response output path distinct from the input, journal,
claims and evidence. Before running the command, the CLI checks for an existing
output, exclusively reserves `<outputPath>.claim`, and checks the output again.
An existing output or claim fails with `DOSQL_RESPONSE_ALREADY_EXISTS` before
dispatch or lifecycle events. Claims remain after failures and crashes.

The response is published exclusively. If reservation or final publication
fails, the CLI exits nonzero and writes one failed JSON response to stderr;
the previous output is never overwritten and must not be treated as the new
response. Final publication failure reports `DOSQL_RESPONSE_PERSISTENCE_FAILED`,
the underlying `auditErrorCode`, the command's actual `executionOutcome`, and
`retryable: false`. If the command itself failed, `commandError` preserves that
failure. Client success followed by publication failure remains `succeeded`,
not a rollback. A reservation failure reports `not_started`.

These are Node CLI response/stderr fields, not a Gateway error schema. The
current Go MCP wrapper does not parse stderr JSON: when the response file is
missing and the process fails, it returns `dosql_execution_failed` with a
sanitized combined-output reason; a missing file after process success returns
`dosql_response_missing`. Gateway callers cannot rely on structured
`commandError` or `executionOutcome` propagation. Inspect the persisted journal
and evidence to reconcile execution; do not automatically retry.

## Commands

| Command | Status | Purpose |
|---|---|---|
| `inspect-schema` | Not exposed | Live schema inspection is routed through `query` with one read-only engine-catalog/information-schema statement; supplied structure snapshots are not live inspection. |
| `classify` | Implemented | Classify a statement before execution. Server policy remains final. |
| `discover-databases` | Implemented | Convert supplied service probe output into database candidates and naming prompts. |
| `discover-resources` | Implemented | Scan authorized code, connection references, Helm values and Kubernetes manifests, merge PostgreSQL/MySQL/MongoDB assets with explicit target hints, preserve Gateway canonical asset IDs, deduplicate by database identity, and persist metadata-only inventory/display resources without secret payloads. |
| `register-database` | Implemented | Convert database probe output into assets, baseline versions, snapshots and checklist items. |
| `compare-databases` | Implemented | Compare structure snapshots against a reference database and write a difference artifact. |
| `derive-change-plan-from-comparison` | Implemented | Derive additive change descriptors and manual review items from a database comparison artifact. |
| `render-forward-change-artifacts` | Implemented | Derive forward SQL artifacts from supported comparison change descriptors. |
| `execute-forward-artifacts` | Implemented | Execute derived forward SQL artifacts through the configured adapter and record verified evidence. |
| `render-forward-evidence-metadata-commit` | Implemented | Render one metadata transaction for verified forward execution evidence. |
| `resolve-database` | Implemented | Resolve a natural-language database reference to one registered asset, or return ambiguity. |
| `query` | Implemented | Execute one audited read-only MySQL/PostgreSQL statement or structured MongoDB operation. Use this route for both business-data reads and live schema reads. |
| `scan` | Implemented | Run health, SQL log and security inspection modules from supplied evidence. |
| `record-execution` | Implemented | Append SQL/MongoDB lifecycle events plus optional timeline baseline evidence to a JSONL execution journal. |
| `replay-timeline` | Implemented | Read JSONL journal events and return latest environment/change status. |
| `resolve-timeline-at` | Implemented | Resolve the verified timeline node that was current at a timestamp. |
| `render-timepoint-state-query` | Implemented | Render metadata lookup SQL for the timepoint state node, baseline records, derived artifacts and execution evidence. |
| `execute-timepoint-state-query` | Implemented | Execute the rendered metadata lookup through the PostgreSQL `psql` adapter and write a result artifact. |
| `create-timepoint-state-manifest` | Implemented | Turn a resolved timepoint query result into an auditable state manifest with the proving baseline and execution evidence. |
| `check-head-drift` | Implemented | Compare live schema fingerprint with the current timeline head before planning. |
| `import-drift` | Implemented | Append an explicit `drift_import` timeline node for confirmed external changes. |
| `project-current-head` | Implemented | Project a verified timeline child node into the current-head cache. |
| `render-metadata-commit` | Implemented | Render a guarded SQL transaction for inserting a timeline node and advancing the current-head cache. |
| `create-baseline-records` | Implemented | Write auditable baseline records for a timeline node from explicit evidence refs. |
| `derive-initial-baseline-from-structure-snapshot` | Implemented | Derive the initial sequence-0 timeline node and initial baseline record set from one structure snapshot. |
| `derive-baseline-records-from-structure-snapshots` | Implemented | Derive baseline records for a timeline node from before/after structure snapshots. |
| `render-baseline-records-commit` | Implemented | Render a SQL transaction for persisting a baseline record set. |
| `render-change-metadata-commit` | Implemented | Render one atomic SQL transaction for timeline node, current-head cache and baseline records. |
| `record-metadata-commit-execution` | Implemented | Verify and record external execution evidence for a metadata commit transaction. |
| `execute-metadata-commit` | Implemented | Execute a supported metadata commit through the PostgreSQL `psql` adapter and record verified evidence. |
| `plan-rollback` | Implemented | Plan a rollback path from the current timeline node to an older target node. |
| `plan-rollback-at` | Implemented | Resolve a timestamp to a verified target node, then plan rollback to that timepoint. |
| `render-restore-plan-metadata-commit` | Implemented | Render one metadata transaction for a restore plan artifact. |
| `render-schema-rollback-artifacts` | Implemented | Derive schema rollback SQL artifacts from structured timeline descriptors. |
| `render-data-rollback-artifacts` | Implemented | Derive inverse data rollback SQL artifacts from affected-row before images. |
| `render-snapshot-restore-artifacts` | Implemented | Bind snapshot/PITR restore evidence to snapshot-required rollback steps. |
| `render-rollback-artifacts` | Implemented | Bind generated rollback artifacts to a restore plan manifest. |
| `render-timeline-artifacts-metadata-commit` | Implemented | Render one metadata transaction for derived timeline artifacts from a forward or rollback artifact manifest. |
| `execute-rollback-artifacts` | Implemented | Execute rollback SQL, snapshot, PITR or restore report artifacts through the configured adapter and record verified evidence. |
| `execute-restore-checks` | Implemented | Execute post-restore scalar checks through the PostgreSQL `psql` adapter and record verified evidence. |
| `verify-restore` | Implemented | Write restore verification evidence after a restore attempt passes checks. |
| `verify-rollback-restore` | Implemented | Write restore verification evidence bound to verified rollback and restore-check execution artifacts. |
| `render-restore-evidence-metadata-commit` | Implemented | Render one metadata transaction for rollback execution, restore-check execution and restore verification evidence. |
| `finalize-restore` | Implemented | Convert a restore verification artifact into a `restore` timeline node artifact. |
| `derive-restore-baseline-records` | Implemented | Derive before/after baseline records for a verified restore timeline node. |
| `extract-sql-logs` | Target | Extract SQL logs into structured events. Current implementation exposes this through `scan`. |
| `analyze-security` | Target | Analyze SQL log events for SQL injection indicators. Current implementation exposes this through `scan`. |
| `plan-change` | Target | Build migration plan from user intent and current schema. |
| `propose-confirmation` | Removed | This entrypoint exposed an in-memory add-column test fixture. Business intent is confirmed in the conversation, respecting existing authorization; no replacement confirmation system is provided. |
| `render-script` | Target | Render engine-specific migration script. |
| `dry-run` | Target | Validate script without changing target when engine supports it. |
| `execute` | Removed | The signed-approval executor had no approval issuer. Changes run through `doops sql --mode apply --sql/--file` on the Agent's direct path. |
| `verify` | Target | Run post-change verification. |

## Request Envelope

```json
{
  "operationId": "dbop_...",
  "projectId": "proj_erp",
  "databaseAssetId": "db_orders_prod",
  "environmentId": "prod",
  "engine": "mysql",
  "connectionRef": "secret://dosql/proj_erp/prod/orders",
  "statement": "alter table orders add column external_id varchar(64)",
  "version": {
    "from": 3,
    "to": 4,
    "label": "dosql_000004"
  },
  "changeRequestId": "chg_..."
}
```

## Controlled Query

`query` accepts only operations classified as `read`. MySQL and PostgreSQL
requests must contain exactly one SQL statement and execute inside a database
read-only transaction. It writes
`database.operation.planned`, `running`, `succeeded` and `verified` events to
the append-only journal and writes a `dosql.database-operation-evidence.v1`
artifact:

```json
{
  "operationId": "dbop_query_001",
  "projectId": "proj_erp",
  "databaseAssetId": "db_orders_test",
  "environmentId": "test",
  "environment": "test",
  "engine": "mysql",
  "databaseName": "orders",
  "actor": { "type": "user", "id": "u_001" },
  "statement": "select count(*) from orders",
  "journalPath": ".dosql/journals/db_orders_test.jsonl",
  "evidencePath": ".dosql/evidence/dbop_query_001.json",
  "adapter": {
    "type": "mysql-cli",
    "hostEnv": "DOSQL_MYSQL_HOST",
    "userEnv": "DOSQL_MYSQL_USER",
    "passwordEnv": "DOSQL_MYSQL_PASSWORD"
  }
}
```

Changes are not a node command. The earlier `execute` command, which required a
signed `dosql.operation-approval.v1` artifact that nothing issued, was removed.
Mutations run only through the Agent's direct `--sql/--file` path with optional
read-only verification and a per-database version journal. Rollback-capable
batches execute with verification in one transaction. MySQL DDL commits
implicitly: DDL dry-run is rejected with `dosql_dry_run_ddl_unsupported`, and
mixing DDL and DML is rejected with `dosql_mixed_ddl_dml_unsupported`. Execute
authorized DDL separately without `--dry-run`, inspect `implicit_commits` and
any `partially_committed` result, then read back the schema. Later statement or
verification failure cannot roll back already committed DDL. MongoDB
non-transactional operations (`createCollection/createIndex/dropIndex/drop`)
follow the same dry-run and mixing restrictions; transactional writes require
a replica set. Do not promise a universal rollback across engines.

Supported adapter types:

- `postgres-psql`: parses the URI from `connectionUriEnv` into libpq
  environment variables; passwords are not placed in process arguments.
- `mysql-cli`: reads host, user and password from named environment variables;
  the password is passed only through `MYSQL_PWD`.
- `mongodb-mongosh`: reads the URI from `connectionUriEnv`, starts `mongosh`
  with `--nodb` and executes only a fixed dispatcher over a structured
  `mongoOperation`; raw MongoDB JavaScript is rejected. `query` additionally
  requires the authenticated MongoDB user to have only `read`,
  `readAnyDatabase` or `clusterMonitor` roles.

Before dispatch, `query` rejects existing evidence, reserves a persistent
operation ID and evidence-path claim, and rejects IDs already in the selected
journal. Claims and evidence are published exclusively with
`writeAuditArtifact`; retries cannot overwrite prior evidence. The audit root
is `.dosql` in the working directory, or the trusted embedding caller's
`artifactRootProvider`. All cooperating callers must use the same persistent
root. Cross-journal protection covers only IDs already reserved in
`operation-claims/`, including IDs explicitly imported by legacy migration.
Pre-upgrade historical IDs without claims in another journal, including valid
hash-chained journals, are outside that protection. Keep using the original
journal and reconcile its history before changing paths; no global historical
index or directory scan is performed. The selected journal still rejects its
own historical IDs. See [journal-recovery.md](journal-recovery.md) for recovery
boundaries.

Policy rejection records `planned` and `failed` with `executionOutcome:
"not_started"`; client failures record `planned`, `running` and `failed` with
outcome `unknown`, when audit storage remains writable. A successful read is
`verified` after evidence publication and the final journal append; query does
not run a separate verification statement. Audit persistence failure after
client success reports `DOSQL_AUDIT_PERSISTENCE_FAILED` with outcome `succeeded`,
never rollback, and never replaces evidence with a failure artifact. Failure
responses include `retryable: false` and preserve underlying `auditErrorCode`
and journal `lineNumber` where available. Persisted evidence stores output
sizes and fingerprints rather than raw stdout, stderr or database error text.

MongoDB requests use structured operations:

```json
{
  "engine": "mongodb",
  "mongoOperation": {
    "collection": "orders",
    "method": "find",
    "args": [{ "status": "paid" }]
  }
}
```

`record-execution` may include timeline evidence when a verified mutating
operation should become the next state node:

```json
{
  "timeline": {
    "baselineBeforeRef": "baselines/db_orders_prod/000004.before.json",
    "baselineAfterRef": "baselines/db_orders_prod/000004.after.json",
    "schemaFingerprint": "sha256:...",
    "dataCheckpointRef": "",
    "restoreCapability": "schema_reversible"
  }
}
```

Without this evidence, the journal records the lifecycle event but does not
prove rollback capability for a timeline node.

`compare-databases` compares supplied structure snapshots against one explicit
reference database asset and writes a `dosql.database-comparison.v1` artifact:

```json
{
  "operationId": "dbop_compare_databases_...",
  "structureSnapshots": [],
  "referenceDatabaseAssetId": "db_orders_dev",
  "targetDatabaseAssetIds": ["db_orders_prod"],
  "comparedAt": "2026-07-06T10:00:00.000Z",
  "comparedBy": "u_001",
  "comparisonArtifactPath": ".dosql/comparisons/orders-dev-prod.json"
}
```

The comparison reports table, column, collection and engine differences. It
does not execute SQL. Cross-engine comparisons are report-only and produce
manual review differences.

`derive-change-plan-from-comparison` consumes that artifact and writes a
`dosql.compare-change-plan.v1` artifact:

```json
{
  "operationId": "dbop_derive_compare_plan_...",
  "comparisonArtifactPath": ".dosql/comparisons/orders-dev-prod.json",
  "changeRequestId": "chg_compare_001",
  "changePlanPath": ".dosql/comparisons/orders-dev-prod-change-plan.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T10:05:00.000Z"
}
```

Only additive suggestions that can be represented as structured
`changeDescriptor` objects are placed in `changeDescriptors`. Risky or
destructive differences remain in `manualDifferences`, so downstream SQL
rendering cannot silently hide a data-loss decision.

`render-forward-change-artifacts` consumes the comparison change plan and writes
derived forward SQL plus a `dosql.forward-artifact-manifest.v1` manifest:

```json
{
  "operationId": "dbop_render_forward_artifacts_...",
  "changePlanPath": ".dosql/comparisons/orders-dev-prod-change-plan.json",
  "targetDatabaseAssetId": "db_orders_prod",
  "timelineNode": {
    "timelineNodeId": "tln_000001",
    "databaseAssetId": "db_orders_prod",
    "nodeSequence": 1,
    "nodeLabel": "dosql_000001",
    "nodeKind": "change",
    "stateStatus": "verified",
    "baselineBeforeRef": "baselines/db_orders_prod/000001.before.json",
    "baselineAfterRef": "baselines/db_orders_prod/000001.after.json",
    "schemaFingerprint": "sha256:s1"
  },
  "scriptDir": ".dosql/changes/chg_compare_001/scripts",
  "artifactBaseRef": "changes/chg_compare_001/scripts",
  "artifactManifestPath": ".dosql/changes/chg_compare_001/forward-artifacts.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T09:05:00.000Z"
}
```

The first supported descriptor is `add_column`, which derives
`alter table <table> add column <column> <type>;`. Unsupported descriptor
actions, non-empty key declarations and unsafe SQL type text fail the command.
The generated SQL is a derived artifact; the version source remains the
timeline node and baseline records.

`execute-forward-artifacts` executes the manifest's `forward_sql` artifacts and
writes `dosql.forward-execution.v1` evidence. The command reads each artifact
under `artifactBaseDir`, verifies its `sha256:` fingerprint against the
manifest, and then sends SQL through the PostgreSQL `psql` adapter:

```json
{
  "operationId": "dbop_execute_forward_artifacts_...",
  "artifactManifestPath": ".dosql/changes/chg_compare_001/forward-artifacts.json",
  "artifactBaseDir": ".dosql/changes/chg_compare_001",
  "forwardExecutionPath": ".dosql/changes/chg_compare_001/forward-execution.json",
  "executedBy": "u_001",
  "executedAt": "2026-07-06T12:10:00.000Z",
  "connectionRef": "secret://dosql/orders",
  "forwardAdapter": {
    "type": "postgres-psql",
    "psqlPath": "psql",
    "connectionUriEnv": "DOSQL_TARGET_DATABASE_URL"
  }
}
```

Artifact refs that escape `artifactBaseDir`, unsupported artifact kinds,
non-zero `psql` exits, failed execution results or fingerprint mismatches fail
the command. DoSql does not record forward execution success unless every
artifact execution is verified.

`render-forward-evidence-metadata-commit` consumes the verified forward
execution artifact and renders a `dosql.forward-evidence-metadata-commit.v1`
SQL transaction:

```json
{
  "operationId": "dbop_render_forward_evidence_commit_...",
  "forwardExecutionPath": ".dosql/changes/chg_compare_001/forward-execution.json",
  "commitPath": ".dosql/changes/chg_compare_001/forward-evidence-commit.sql",
  "commitArtifactPath": ".dosql/changes/chg_compare_001/forward-evidence-commit.json"
}
```

The renderer writes one row into `dosql_forward_executions`. It rejects
unverified forward execution artifacts, missing artifact executions or an
`executionCount` that does not match the execution list. This command is
`plan_only`; execute the resulting commit through `execute-metadata-commit`
when the metadata store should be updated.

`check-head-drift` compares the current verified timeline head with live schema
evidence before a new mutating plan is created:

```json
{
  "operationId": "dbop_check_drift_...",
  "currentNode": {
    "timelineNodeId": "tln_current",
    "databaseAssetId": "db_orders_prod",
    "nodeSequence": 12,
    "nodeLabel": "dosql_000012",
    "schemaFingerprint": "sha256:head",
    "baselineAfterRef": "baselines/db_orders_prod/000012.after.json"
  },
  "liveSchemaFingerprint": "sha256:live",
  "checkedAt": "2026-07-06T09:05:00.000Z",
  "evidenceRef": "scans/db_orders_prod/0905.json"
}
```

When fingerprints differ, the command returns `status: "drift_detected"` and
`canPlanChange: false`. The caller must import or correct drift before
planning a new managed change.

`import-drift` appends a `drift_import` timeline node after the new live
baseline is confirmed:

```json
{
  "operationId": "dbop_import_drift_...",
  "driftNodePath": ".dosql/changes/chg_drift_001/drift-node.json",
  "currentNode": {
    "timelineNodeId": "tln_current",
    "databaseAssetId": "db_orders_prod",
    "nodeSequence": 12,
    "nodeLabel": "dosql_000012",
    "schemaFingerprint": "sha256:head",
    "baselineAfterRef": "baselines/db_orders_prod/000012.after.json"
  },
  "validFrom": "2026-07-06T09:10:00.000Z",
  "baselineAfterRef": "baselines/db_orders_prod/000013.drift.after.json",
  "schemaFingerprint": "sha256:live",
  "evidenceRef": "scans/db_orders_prod/0910-drift.json",
  "restoreCapability": "manual_mitigation"
}
```

The drift fingerprint must differ from the current head fingerprint. Drift
import records reality; it does not infer the missing change or claim automatic
rollback.

`project-current-head` writes a `dosql.database-version-projection.v1` artifact
for updating the current-head cache:

```json
{
  "operationId": "dbop_project_head_...",
  "projectionPath": ".dosql/changes/chg_001/database-version.json",
  "updatedBy": "u_001",
  "updatedAt": "2026-07-06T09:05:00.000Z",
  "currentVersion": {
    "databaseAssetId": "db_orders_prod",
    "currentVersion": 12,
    "currentLabel": "dosql_000012",
    "currentTimelineNodeId": "tln_current"
  },
  "nextNode": {
    "timelineNodeId": "tln_next",
    "databaseAssetId": "db_orders_prod",
    "nodeSequence": 13,
    "nodeLabel": "dosql_000013",
    "parentNodeId": "tln_current",
    "stateStatus": "verified"
  }
}
```

The command refuses non-verified nodes, skipped sequence numbers, database
mismatches and parent IDs that do not match `currentTimelineNodeId`.

`render-metadata-commit` writes a `dosql.timeline-metadata-commit.v1` artifact
and, when `commitPath` is provided, writes the SQL transaction that persists the
verified node:

```json
{
  "operationId": "dbop_render_metadata_commit_...",
  "commitPath": ".dosql/changes/chg_001/metadata-commit.sql",
  "updatedBy": "u_001",
  "updatedAt": "2026-07-06T09:05:00.000Z",
  "currentVersion": {
    "databaseAssetId": "db_orders_prod",
    "currentVersion": 12,
    "currentLabel": "dosql_000012",
    "currentTimelineNodeId": "tln_current"
  },
  "node": {
    "timelineNodeId": "tln_next",
    "databaseAssetId": "db_orders_prod",
    "nodeSequence": 13,
    "nodeLabel": "dosql_000013",
    "parentNodeId": "tln_current",
    "operationId": "dbop_add_external_id",
    "nodeKind": "change",
    "stateStatus": "verified",
    "validFrom": "2026-07-06T09:00:00.000Z",
    "baselineBeforeRef": "baselines/db_orders_prod/000013.before.json",
    "baselineAfterRef": "baselines/db_orders_prod/000013.after.json",
    "schemaFingerprint": "sha256:...",
    "restoreCapability": "schema_reversible",
    "createdAt": "2026-07-06T09:00:00.000Z"
  }
}
```

The generated SQL inserts `dosql_timeline_nodes` and updates
`dosql_database_versions` in one transaction. The update keeps the previous
`current_version` and `current_timeline_node_id` in the `where` clause and uses
a guard query to fail if the cached head was already moved by another writer.

`create-baseline-records` writes a `dosql.baseline-record-set.v1` artifact for
the baseline evidence associated with one timeline node:

```json
{
  "operationId": "dbop_create_baselines_...",
  "baselineRecordsPath": ".dosql/changes/chg_001/baseline-records.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T09:05:00.000Z",
  "currentNode": {
    "timelineNodeId": "tln_current",
    "databaseAssetId": "db_orders_prod",
    "nodeSequence": 0,
    "nodeLabel": "dosql_000000",
    "nodeKind": "baseline",
    "stateStatus": "verified",
    "baselineAfterRef": "baselines/db_orders_prod/000000.after.json",
    "schemaFingerprint": "sha256:current"
  },
  "timelineNode": {
    "timelineNodeId": "tln_next",
    "databaseAssetId": "db_orders_prod",
    "nodeSequence": 1,
    "nodeLabel": "dosql_000001",
    "nodeKind": "change",
    "parentNodeId": "tln_current",
    "baselineBeforeRef": "baselines/db_orders_prod/000001.before.json",
    "baselineAfterRef": "baselines/db_orders_prod/000001.after.json",
    "schemaFingerprint": "sha256:next"
  },
  "records": [
    {
      "baselineKind": "before",
      "capturedAt": "2026-07-06T08:59:00.000Z",
      "schemaSnapshotRef": "baselines/db_orders_prod/000001.before.json",
      "schemaFingerprint": "sha256:...",
      "dataScope": "before_image",
      "dataEvidenceRef": "baselines/db_orders_prod/000001.before-image.json",
      "artifactFingerprint": "sha256:..."
    }
  ]
}
```

The command validates the record refs against the timeline node's
`baselineBeforeRef` and `baselineAfterRef`. Mismatched evidence fails the
command instead of producing a partial baseline record.

`derive-initial-baseline-from-structure-snapshot` creates the first durable
state anchor for a newly registered database asset:

```json
{
  "operationId": "dbop_initial_baseline_...",
  "databaseAssetId": "db_orders_prod",
  "baselineAfterRef": "baselines/db_orders_prod/000000.after.json",
  "structureSnapshotPath": ".dosql/snapshots/db_orders_prod-0800.json",
  "initialBaselinePath": ".dosql/baselines/db_orders_prod/initial-baseline.json",
  "timelineNodePath": ".dosql/baselines/db_orders_prod/initial-node.json",
  "baselineRecordsPath": ".dosql/baselines/db_orders_prod/baseline-records.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T08:05:00.000Z"
}
```

The command finds `databaseAssetId` inside the structure snapshot, uses the
asset `structureFingerprint` as the sequence-0 timeline node
`schemaFingerprint`, writes a `dosql.initial-baseline.v1` artifact, and can
also write the embedded timeline node and `dosql.baseline-record-set.v1`
artifact separately. Missing database assets or missing structure fingerprints
fail the command; no version SQL is accepted as the source of truth for this
initial state.

`derive-baseline-records-from-structure-snapshots` derives the same
`dosql.baseline-record-set.v1` artifact from before/after structure snapshots:

```json
{
  "operationId": "dbop_derive_baselines_...",
  "baselineRecordsPath": ".dosql/changes/chg_001/baseline-records.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T09:05:00.000Z",
  "timelineNode": {
    "timelineNodeId": "tln_current",
    "databaseAssetId": "db_orders_prod",
    "nodeSequence": 1,
    "nodeLabel": "dosql_000001",
    "nodeKind": "change",
    "baselineBeforeRef": "baselines/db_orders_prod/000001.before.json",
    "baselineAfterRef": "baselines/db_orders_prod/000001.after.json",
    "schemaFingerprint": "sha256:..."
  },
  "beforeSnapshotPath": ".dosql/snapshots/db_orders_prod-0859.json",
  "afterSnapshotPath": ".dosql/snapshots/db_orders_prod-0904.json"
}
```

The command finds the timeline node's database asset in both snapshots, uses
the asset `structureFingerprint` as each baseline record's schema fingerprint,
and derives stable record artifact fingerprints from the source snapshot
content. The before snapshot fingerprint must match `currentNode.schemaFingerprint`,
`timelineNode.parentNodeId` must match `currentNode.timelineNodeId`, and the
after snapshot fingerprint must match `timelineNode.schemaFingerprint`;
otherwise the command fails.

`render-baseline-records-commit` consumes the record-set artifact and writes the
SQL transaction for `dosql_baseline_records`:

```json
{
  "operationId": "dbop_render_baseline_records_commit_...",
  "baselineRecordsPath": ".dosql/changes/chg_001/baseline-records.json",
  "commitPath": ".dosql/changes/chg_001/baseline-records-commit.sql"
}
```

The generated SQL inserts every baseline record in one transaction. The command
rejects record sets whose rows do not all belong to the same database asset and
timeline node declared by the record-set artifact.

`render-change-metadata-commit` is the preferred metadata commit renderer for a
verified change because it keeps the timeline node, current-head cache and
baseline records in one transaction:

```json
{
  "operationId": "dbop_render_change_metadata_commit_...",
  "baselineRecordsPath": ".dosql/changes/chg_001/baseline-records.json",
  "commitPath": ".dosql/changes/chg_001/change-metadata-commit.sql",
  "commitArtifactPath": ".dosql/changes/chg_001/change-metadata-commit.json",
  "updatedBy": "u_001",
  "updatedAt": "2026-07-06T09:05:00.000Z",
  "currentVersion": {
    "databaseAssetId": "db_orders_prod",
    "currentVersion": 12,
    "currentLabel": "dosql_000012",
    "currentTimelineNodeId": "tln_current"
  },
  "node": {
    "timelineNodeId": "tln_next",
    "databaseAssetId": "db_orders_prod",
    "nodeSequence": 13,
    "nodeLabel": "dosql_000013",
    "parentNodeId": "tln_current",
    "operationId": "dbop_add_external_id",
    "nodeKind": "change",
    "stateStatus": "verified",
    "validFrom": "2026-07-06T09:00:00.000Z",
    "baselineBeforeRef": "baselines/db_orders_prod/000013.before.json",
    "baselineAfterRef": "baselines/db_orders_prod/000013.after.json",
    "schemaFingerprint": "sha256:...",
    "restoreCapability": "schema_reversible",
    "createdAt": "2026-07-06T09:00:00.000Z"
  }
}
```

When `commitArtifactPath` is provided, the command writes the full
`dosql.change-metadata-commit.v1` artifact alongside the SQL file. That JSON
artifact is the input for `record-metadata-commit-execution`. The baseline
record set must contain a state baseline for the new timeline node: an `after`,
`initial` or `drift` record whose snapshot ref and schema fingerprint match the
node's `baselineAfterRef` and `schemaFingerprint`. The state baseline kind must
also match the node kind: `initial` for `baseline`, `drift` for
`drift_import`, and `after` for `change` or `restore`. The command refuses a
combined commit that would advance the current head without a provable state
baseline.

The lower-level `render-metadata-commit` and
`render-baseline-records-commit` commands remain available when an operator
needs to inspect or regenerate one side of the metadata transaction separately.

`record-metadata-commit-execution` records the result returned by the external
metadata database adapter after executing a `dosql.change-metadata-commit.v1`
transaction:

```json
{
  "operationId": "dbop_record_metadata_execution_...",
  "commitArtifactPath": ".dosql/changes/chg_001/change-metadata-commit.json",
  "executionArtifactPath": ".dosql/changes/chg_001/metadata-execution.json",
  "executedBy": "u_001",
  "executedAt": "2026-07-06T09:06:00.000Z",
  "connectionRef": "secret://dosql/metadata",
  "executionResult": {
    "status": "succeeded",
    "transactionId": "tx_metadata_001",
    "statementCount": 4,
    "timelineNodeInsertCount": 1,
    "currentHeadUpdateCount": 1,
    "currentHeadGuardPassed": true,
    "baselineRecordInsertCount": 2
  }
}
```

The command fails if the adapter reports failure, a stale current-head guard,
zero or multiple current-head updates, or a baseline insert count that differs
from the commit's `recordCount`. It records evidence; it does not fabricate a
database execution result.

`execute-metadata-commit` executes a supported commit artifact with the
PostgreSQL `psql` adapter and writes verified execution evidence:

```json
{
  "operationId": "dbop_execute_metadata_commit_...",
  "commitArtifactPath": ".dosql/changes/chg_001/change-metadata-commit.json",
  "executionArtifactPath": ".dosql/changes/chg_001/metadata-execution.json",
  "executedBy": "u_001",
  "executedAt": "2026-07-06T09:06:00.000Z",
  "connectionRef": "secret://dosql/metadata",
  "metadataAdapter": {
    "type": "postgres-psql",
    "psqlPath": "psql",
    "connectionUriEnv": "DOSQL_METADATA_DATABASE_URL"
  }
}
```

The adapter obtains the connection URI from `connectionUriEnv`, injects it as
`PGDATABASE`, sends SQL through `psql` stdin and sets `ON_ERROR_STOP=1`.
For `dosql.change-metadata-commit.v1`, it parses the timeline insert count,
current-head guard result and baseline insert count. For
`dosql.timeline-artifacts-metadata-commit.v1`, it parses the timeline artifact
insert count and requires it to match the commit `recordCount`. For
`dosql.restore-plan-metadata-commit.v1`, it parses the restore-plan insert
count and requires exactly one inserted row. For
`dosql.forward-evidence-metadata-commit.v1`, it parses the forward execution
insert count and requires exactly one inserted row. For
`dosql.restore-evidence-metadata-commit.v1`, it parses rollback execution,
restore-check execution and restore-verification insert counts; each must be
exactly one for the execution artifact to be accepted. Non-zero `psql` exits
fail the command.

`plan-rollback` returns the computed reverse path. When the request also
includes `changeRequestId`, `createdBy`, `createdAt` and `restorePlanPath`, it
writes a `dosql.restore-plan.v1` artifact:

```json
{
  "operationId": "dbop_plan_rollback_...",
  "changeRequestId": "chg_restore_...",
  "databaseAssetId": "db_orders_prod",
  "currentNodeId": "tln_current",
  "targetNodeId": "tln_target",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T12:00:00.000Z",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "nodes": []
}
```

If `restorePlanPath` is supplied but the metadata required to create an
auditable artifact is missing, the command fails instead of writing a partial
plan.

`plan-rollback-at` accepts a timestamp instead of a target node ID. It resolves
the latest verified timeline node at or before that timestamp, then creates the
same rollback plan and optional `dosql.restore-plan.v1` artifact:

```json
{
  "operationId": "dbop_plan_rollback_at_...",
  "changeRequestId": "chg_restore_...",
  "databaseAssetId": "db_orders_prod",
  "currentNodeId": "tln_current",
  "timestamp": "2026-07-06T09:30:00.000Z",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T12:00:00.000Z",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "nodes": []
}
```

The command fails when no verified timeline node exists at the timestamp, or
when the resolved node is not an ancestor of the requested current head.

`render-timepoint-state-query` writes a `dosql.timepoint-state-query.v1`
artifact containing metadata lookup SQL:

```json
{
  "operationId": "dbop_render_timepoint_query_...",
  "databaseAssetId": "db_orders_prod",
  "timestamp": "2026-07-06T09:30:00.000Z",
  "queryArtifactPath": ".dosql/queries/db_orders_prod-0930.json"
}
```

The query resolves the verified `dosql_timeline_nodes` row current at that
timestamp and aggregates the node's `dosql_baseline_records` and
`dosql_timeline_artifacts`, plus execution evidence from
`dosql_forward_executions` and `dosql_restore_verifications`. This keeps
timestamp lookup anchored in persisted timeline facts rather than a
caller-supplied in-memory node list.

`execute-timepoint-state-query` runs the rendered query through the PostgreSQL
`psql` adapter and writes a `dosql.timepoint-state-query-result.v1` artifact:

```json
{
  "operationId": "dbop_execute_timepoint_query_...",
  "queryArtifactPath": ".dosql/queries/db_orders_prod-0930.json",
  "queryResultPath": ".dosql/queries/db_orders_prod-0930-result.json",
  "queriedBy": "u_001",
  "queriedAt": "2026-07-06T10:00:00.000Z",
  "connectionRef": "secret://dosql/metadata",
  "metadataAdapter": {
    "type": "postgres-psql",
    "psqlPath": "psql",
    "connectionUriEnv": "DOSQL_METADATA_DATABASE_URL"
  }
}
```

The result artifact stores the resolved timeline node, baseline records,
timeline artifacts, execution evidence, execution result and source query
fingerprint. It is read-only evidence for a historical database state; it does
not mutate the metadata store or target database.

`create-timepoint-state-manifest` consumes that query-result artifact and writes
a stable `dosql.timepoint-state-manifest.v1` artifact:

```json
{
  "operationId": "dbop_create_timepoint_manifest_...",
  "queryResultPath": ".dosql/queries/db_orders_prod-0930-result.json",
  "stateManifestPath": ".dosql/queries/db_orders_prod-0930-state.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T10:05:00.000Z"
}
```

The manifest names the resolved timeline node, schema fingerprint, state
baseline ref, proving baseline record, timeline artifacts, execution evidence
and restore capability boundary. The command fails when the query result does
not contain an `after`, `initial` or `drift` baseline record whose snapshot ref and schema
fingerprint match the resolved node.

`render-restore-plan-metadata-commit` consumes the restore plan artifact and
renders a `dosql.restore-plan-metadata-commit.v1` SQL transaction:

```json
{
  "operationId": "dbop_render_restore_plan_commit_...",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "commitPath": ".dosql/changes/chg_restore_001/restore-plan-commit.sql",
  "commitArtifactPath": ".dosql/changes/chg_restore_001/restore-plan-commit.json"
}
```

The renderer writes `dosql_restore_plans` in one transaction and rejects a
restore plan whose `plan.currentNodeId` or `plan.targetNodeId` does not match
the embedded current/target node summaries. This command is `plan_only`; use
`execute-metadata-commit` to apply the generated commit to the metadata store.

`render-rollback-artifacts` consumes a restore plan and generated artifact refs:

```json
{
  "operationId": "dbop_render_rollback_artifacts_...",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "artifactManifestPath": ".dosql/changes/chg_restore_001/rollback-artifacts.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T12:05:00.000Z",
  "artifacts": [
    {
      "timelineNodeId": "tln_current",
      "artifactKind": "rollback_sql",
      "artifactRef": "changes/chg_restore_001/scripts/rollback-dosql_000001.sql",
      "artifactFingerprint": "sha256:..."
    }
  ]
}
```

The command validates artifact kinds against restore-plan methods. Missing
artifacts fail the command; DoSql does not invent rollback SQL or silently
downgrade to a weaker restore path.

`render-timeline-artifacts-metadata-commit` consumes that manifest and renders
a `dosql.timeline-artifacts-metadata-commit.v1` SQL transaction:

```json
{
  "operationId": "dbop_render_timeline_artifacts_commit_...",
  "artifactManifestPath": ".dosql/changes/chg_restore_001/rollback-artifacts.json",
  "commitPath": ".dosql/changes/chg_restore_001/timeline-artifacts-commit.sql",
  "commitArtifactPath": ".dosql/changes/chg_restore_001/timeline-artifacts-commit.json"
}
```

The renderer writes `dosql_timeline_artifacts` rows for generated rollback SQL,
snapshot and PITR artifacts. Each row keeps the source timeline node ID and
artifact fingerprint, so these files remain derived, auditable artifacts rather
than the version source. This command is `plan_only`; use
`execute-metadata-commit` to apply the generated commit to the metadata store.

`execute-rollback-artifacts` executes the manifest's rollback artifacts and
writes `dosql.rollback-execution.v1` evidence. For `rollback_sql`, use the
PostgreSQL `psql` adapter:

```json
{
  "operationId": "dbop_execute_rollback_artifacts_...",
  "artifactManifestPath": ".dosql/changes/chg_restore_001/rollback-artifacts.json",
  "artifactBaseDir": ".dosql/changes/chg_restore_001",
  "rollbackExecutionPath": ".dosql/changes/chg_restore_001/rollback-execution.json",
  "executedBy": "u_001",
  "executedAt": "2026-07-06T12:10:00.000Z",
  "connectionRef": "secret://dosql/orders",
  "rollbackAdapter": {
    "type": "postgres-psql",
    "psqlPath": "psql",
    "connectionUriEnv": "DOSQL_TARGET_DATABASE_URL"
  }
}
```

For `snapshot_manifest`, `pitr_marker` or `restore_report`, use a JSON command
adapter. DoSql sends one JSON request on stdin for each artifact and expects a
JSON result on stdout with `status`, `transactionId`, `statementCount` and
`affectedRows`:

```json
{
  "operationId": "dbop_execute_snapshot_rollback_...",
  "artifactManifestPath": ".dosql/changes/chg_restore_001/rollback-artifacts.json",
  "artifactBaseDir": ".dosql/changes/chg_restore_001",
  "rollbackExecutionPath": ".dosql/changes/chg_restore_001/rollback-execution.json",
  "executedBy": "u_001",
  "executedAt": "2026-07-06T12:20:00.000Z",
  "connectionRef": "secret://dosql/orders",
  "rollbackAdapter": {
    "type": "snapshot-json-command",
    "command": ["/opt/dosql/bin/restore-snapshot"]
  }
}
```

Before execution, the command reads each artifact under `artifactBaseDir` and
verifies its `sha256:` fingerprint against the manifest. Artifact refs that
escape `artifactBaseDir` fail. Non-zero `psql` exits, non-zero JSON command
exits, failed artifact execution results or fingerprint mismatches fail the
command; DoSql does not record rollback success unless the adapter execution
result is verified. A manifest that mixes SQL and JSON restore artifacts needs
an adapter object that implements the required execution method for every
artifact kind.

`render-schema-rollback-artifacts` is the built-in renderer for simple
schema-reversible changes. It consumes a restore plan whose step includes a
structured `changeDescriptor` and writes rollback SQL plus a manifest:

```json
{
  "operationId": "dbop_render_schema_rollback_...",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "scriptDir": ".dosql/changes/chg_restore_001/scripts",
  "artifactBaseRef": "changes/chg_restore_001/scripts",
  "artifactManifestPath": ".dosql/changes/chg_restore_001/rollback-artifacts.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T12:05:00.000Z"
}
```

The first supported descriptor is `add_column`, which derives
`alter table <table> drop column <column>;`. Unsupported or missing descriptors
fail the command.

`render-data-rollback-artifacts` is the built-in renderer for
data-patch-reversible changes. It consumes a restore plan whose
`inverse_data_patch` step has matching affected-row before images and writes
rollback SQL plus a manifest:

```json
{
  "operationId": "dbop_render_data_rollback_...",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "scriptDir": ".dosql/changes/chg_restore_001/scripts",
  "artifactBaseRef": "changes/chg_restore_001/scripts",
  "artifactManifestPath": ".dosql/changes/chg_restore_001/rollback-artifacts.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T12:05:00.000Z",
  "beforeImages": [
    {
      "timelineNodeId": "tln_current",
      "table": "orders",
      "primaryKey": { "id": 101 },
      "before": { "status": "pending", "external_id": null }
    }
  ]
}
```

This derives statements such as
`update orders set status = 'pending', external_id = null where id = 101;`.
Missing before images fail the command; the command does not infer old values
from summaries or current data.

`render-snapshot-restore-artifacts` is the built-in renderer for
snapshot-required changes. It consumes a restore plan whose
`snapshot_or_pitr_restore` step has matching snapshot or PITR evidence and
writes a restore evidence artifact plus a manifest:

```json
{
  "operationId": "dbop_render_snapshot_rollback_...",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "artifactDir": ".dosql/changes/chg_restore_001/artifacts",
  "artifactBaseRef": "changes/chg_restore_001/artifacts",
  "artifactManifestPath": ".dosql/changes/chg_restore_001/rollback-artifacts.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T12:05:00.000Z",
  "restoreEvidence": [
    {
      "timelineNodeId": "tln_current",
      "artifactKind": "snapshot_manifest",
      "evidenceRef": "snapshots/db_orders_prod/000001.snapshot.json",
      "evidenceFingerprint": "sha256:...",
      "restoreTargetRef": "baselines/db_orders_prod/000001.before.json",
      "capturedAt": "2026-07-06T08:59:00.000Z"
    }
  ]
}
```

`artifactKind` must be `snapshot_manifest` or `pitr_marker`.
`restoreTargetRef` must match the restore-plan step's `baselineBeforeRef`.
Missing or mismatched evidence fails the command; this command records restore
evidence and does not execute a live database restore.

`verify-restore` consumes a restore plan artifact and writes
`dosql.restore-verification.v1` after post-restore checks pass:

```json
{
  "operationId": "dbop_verify_restore_...",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "restoreVerificationPath": ".dosql/changes/chg_restore_001/restore-verification.json",
  "verifiedBy": "u_001",
  "verifiedAt": "2026-07-06T12:30:00.000Z",
  "baselineBeforeRef": "baselines/db_orders_prod/000002.before.json",
  "baselineAfterRef": "baselines/db_orders_prod/000002.after.json",
  "schemaFingerprint": "sha256:...",
  "evidenceRef": "changes/chg_restore_001/evidence/restore-report.json",
  "checks": [
    {
      "checkName": "schema_fingerprint",
      "checkStatus": "passed",
      "expected": "sha256:...",
      "actual": "sha256:..."
    }
  ]
}
```

The command refuses to write the verification artifact if the restored schema
fingerprint does not match the restore plan target node or any check fails.

`execute-restore-checks` runs live post-restore scalar checks and writes
`dosql.restore-check-execution.v1`:

```json
{
  "operationId": "dbop_execute_restore_checks_...",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "checkExecutionPath": ".dosql/changes/chg_restore_001/restore-check-execution.json",
  "executedBy": "u_001",
  "executedAt": "2026-07-06T12:25:00.000Z",
  "connectionRef": "secret://dosql/orders",
  "checkAdapter": {
    "type": "postgres-psql",
    "psqlPath": "psql",
    "connectionUriEnv": "DOSQL_TARGET_DATABASE_URL"
  },
  "checks": [
    {
      "checkName": "schema_fingerprint",
      "expected": "sha256:...",
      "sqlText": "select dosql_schema_fingerprint();"
    }
  ]
}
```

The command fails if any adapter result differs from its expected value. For
`schema_fingerprint`, the expected value must match the restore plan target
node's schema fingerprint.

`verify-rollback-restore` is the artifact-backed rollback verification path. It
requires both the rollback execution artifact and the restore-check execution
artifact, then verifies that both match the restore plan before writing
`dosql.restore-verification.v1`:

```json
{
  "operationId": "dbop_verify_rollback_restore_...",
  "restorePlanPath": ".dosql/changes/chg_restore_001/restore-plan.json",
  "rollbackExecutionPath": ".dosql/changes/chg_restore_001/rollback-execution.json",
  "rollbackExecutionRef": "changes/chg_restore_001/rollback-execution.json",
  "restoreCheckExecutionPath": ".dosql/changes/chg_restore_001/restore-check-execution.json",
  "restoreCheckExecutionRef": "changes/chg_restore_001/restore-check-execution.json",
  "restoreVerificationPath": ".dosql/changes/chg_restore_001/restore-verification.json",
  "verifiedBy": "u_001",
  "verifiedAt": "2026-07-06T12:30:00.000Z",
  "baselineBeforeRef": "baselines/db_orders_prod/000002.before.json",
  "baselineAfterRef": "baselines/db_orders_prod/000002.after.json",
  "evidenceRef": "changes/chg_restore_001/evidence/post-restore-scan.json"
}
```

The command fails if the rollback execution is not verified, belongs to a
different restore plan, has a different database or change request, or does not
cover every restore-plan step with matching timeline node, method and restore
capability, or uses an artifact kind that is not valid for that step's restore
method. It also fails if the restore-check execution is not verified, does not
target the restore plan's target node, or contains any failed check.

`render-restore-evidence-metadata-commit` consumes the three verified restore
evidence artifacts and renders a `dosql.restore-evidence-metadata-commit.v1`
SQL transaction:

```json
{
  "operationId": "dbop_render_restore_evidence_commit_...",
  "rollbackExecutionPath": ".dosql/changes/chg_restore_001/rollback-execution.json",
  "restoreCheckExecutionPath": ".dosql/changes/chg_restore_001/restore-check-execution.json",
  "restoreVerificationPath": ".dosql/changes/chg_restore_001/restore-verification.json",
  "commitPath": ".dosql/changes/chg_restore_001/restore-evidence-commit.sql",
  "commitArtifactPath": ".dosql/changes/chg_restore_001/restore-evidence-commit.json"
}
```

The renderer writes `dosql_rollback_executions`,
`dosql_restore_check_executions` and `dosql_restore_verifications` in one
transaction. It rejects mismatched restore plan IDs, change request IDs,
database asset IDs, source execution fingerprints, restored schema fingerprint
or check evidence. This command is `plan_only`; execute the resulting commit
through `execute-metadata-commit` when the metadata store should be updated.

`finalize-restore` consumes the restore verification artifact and writes the
verified restore node:

```json
{
  "operationId": "dbop_finalize_restore_...",
  "restoreVerificationPath": ".dosql/changes/chg_restore_001/restore-verification.json",
  "restoreVerificationRef": "changes/chg_restore_001/restore-verification.json",
  "restoreNodePath": ".dosql/changes/chg_restore_001/restore-node.json"
}
```

The restore node's `dataCheckpointRef` points to the verification artifact, so
the timeline can trace the restore state boundary back to concrete evidence.

`derive-restore-baseline-records` consumes the verified restore node and
restore verification artifact, then writes the restore node's own
`dosql.baseline-record-set.v1`:

```json
{
  "operationId": "dbop_derive_restore_baselines_...",
  "restoreVerificationPath": ".dosql/changes/chg_restore_001/restore-verification.json",
  "restoreNodePath": ".dosql/changes/chg_restore_001/restore-node.json",
  "baselineRecordsPath": ".dosql/changes/chg_restore_001/restore-baseline-records.json",
  "beforeDataScope": "none",
  "afterDataScope": "snapshot",
  "afterDataEvidenceRef": "changes/chg_restore_001/evidence/post-restore-scan.json",
  "createdBy": "u_001",
  "createdAt": "2026-07-06T12:31:00.000Z"
}
```

The command validates that the restore node is verified, has `nodeKind:
"restore"`, belongs to the same database asset as the verification artifact,
points back to the verification's current node, points forward to the
verification's target node and uses the same before/after baseline refs and
restored schema fingerprint. The restore node's `validFrom` must match the
verification's `verifiedAt`, and its `dataCheckpointRef` must match
`restoreVerificationRef`, so baseline derivation cannot attach restore evidence
to a different timeline timestamp or artifact. The resulting record set
contains `before` and `after` records for the restore node, so a later
timepoint lookup can prove the post-rollback version from records tied to that
node. The caller should then pass the restore node and this record set to
`render-change-metadata-commit`; that combined commit inserts the restore
timeline node, advances the current-head cache and persists the restore
baselines in one transaction.

## Response Envelope

```json
{
  "operationId": "dbop_...",
  "status": "succeeded",
  "engine": "mysql",
  "summary": {
    "affectedRows": 0,
    "schemaChanged": true
  },
  "evidence": {
    "stdout": "...",
    "stderr": "",
    "artifacts": [
      {
        "kind": "schema_snapshot",
        "ref": "artifact://...",
        "sha256": "..."
      }
    ]
  }
}
```

## Hard Requirements

- Do not execute without `operationId`.
- Do not execute mutating operations outside the journaled `doops sql --mode apply --sql/--file` path.
- Confirm business intent, target and impact in user-readable language. Honor
  existing authorization for that scope without asking again; clarify only
  missing scope, new risks or material changes. Keep SQL as an execution artifact.
- Do not present fixture or caller-supplied version numbers as verified live state.
- Do not print database passwords, tokens or secret values.
- Do not hide driver errors. Return the real error class and message.
- Return evidence in machine-readable form.
