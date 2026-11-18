import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { realpathSync } from "node:fs";
import { chmod, lstat, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { execFile, spawnSync } from "node:child_process";
import test from "node:test";
import {
  connectTestDatabase,
  proposeAgentSchemaChange,
} from "../Server/lib/agent-change-flow.mjs";

const CLI = new URL("./dosql-agent.mjs", import.meta.url).pathname;

test("--help lists every dispatched command", async () => {
  const source = await readFile(CLI, "utf8");
  const implemented = [...source.matchAll(/case "([a-z-]+)":/g)].map((match) => match[1]);
  const help = spawnSync(process.execPath, [CLI, "--help"], { encoding: "utf8" });
  assert.equal(help.status, 0, help.stderr);
  for (const name of implemented) {
    assert.ok(
      help.stdout.includes(`    ${name}\n`),
      `${name} is dispatched but missing from --help`,
    );
  }
  const listed = [...help.stdout.matchAll(/^    ([a-z-]+)$/gm)].map((match) => match[1]);
  assert.deepEqual(
    listed.slice().sort(),
    implemented.slice().sort(),
    "--help and the dispatch switch drifted apart",
  );
});

test("classify command returns operation policy as JSON", async () => {
  const result = await runCli("classify", {
    operationId: "dbop_classify_001",
    engine: "mysql",
    environment: "prod",
    statement: "alter table users add column external_id varchar(64)",
  });

  assert.equal(result.status, "succeeded");
  assert.equal(result.operationId, "dbop_classify_001");
  assert.equal(result.command, "classify");
  assert.equal(result.result.operationKind, "schema_change");
  assert.equal(result.result.includeInChangeDocument, true);
  assert.equal(result.result.approvalRequired, true);
});

test("classify command covers every supported SQL DDL and DML family", async () => {
  const cases = [
    ["alter", "alter table items add column extra text", "schema_change"],
    ["comment", "comment on table items is 'test table'", "schema_change"],
    ["create", "create table items (id integer)", "schema_change"],
    ["drop", "drop table items", "schema_change"],
    ["rename", "rename table items to archived_items", "schema_change"],
    ["truncate", "truncate table items", "schema_change"],
    ["delete", "delete from items where id = 1", "data_change"],
    ["insert", "insert into items (id) values (1)", "data_change"],
    ["merge", "merge into items using source_items on items.id = source_items.id when matched then update set id = source_items.id", "data_change"],
    ["replace", "replace into items (id) values (1)", "data_change"],
    ["update", "update items set id = 2 where id = 1", "data_change"],
    ["with-update", "with changed as (select 1) update items set id = 2 where id = 1", "data_change"],
  ];

  for (const [name, statement, operationKind] of cases) {
    const result = await runCli("classify", {
      operationId: `dbop_classify_${name}`,
      engine: "postgres",
      environment: "test",
      statement,
    });
    assert.equal(result.status, "succeeded", name);
    assert.equal(result.result.operationKind, operationKind, name);
    assert.equal(result.result.includeInChangeDocument, true, name);
    assert.equal(result.result.approvalRequired, true, name);
  }
});

for (const engine of ["mysql", "postgres", "mongodb"]) {
  test(`query command rejects an untrusted ${engine} client executable`, async () => {
    const dir = await mkdtemp(join(tmpdir(), `dosql-cli-query-${engine}-`));
    try {
      const clientPath = await createFakeDatabaseClient(dir);
      const { proc, result } = await runCliResult(
        "query",
        databaseOperationPayload({
          dir,
          engine,
          operationId: `dbop_cli_query_${engine}`,
          operation:
            engine === "mongodb"
              ? {
                  mongoOperation: {
                    collection: "items",
                    method: "findOne",
                    args: [{ _id: 1 }],
                  },
                }
              : { statement: "select 1" },
          adapter: adapterForEngine(engine, clientPath),
        }),
        { env: databaseTestEnv(dir), cwd: dir },
      );

      assert.notEqual(proc.status, 0);
      assert.equal(result.status, "failed");
      assert.match(result.error.message, /trusted database client executable/);
      assert.equal(result.error.executionOutcome, "not_started");
      assert.equal(result.error.retryable, false);
      assert.deepEqual(await journalStatuses(join(dir, ".dosql", "journal.jsonl")), [
        "planned",
        "failed",
      ]);
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
}
test("query command rejects multiple SQL statements as a machine-readable failure", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-cli-query-multi-statement-"));
  try {
    const clientPath = await createFakeDatabaseClient(dir);
    const { proc, result } = await runCliResult(
      "query",
      databaseOperationPayload({
        dir,
        engine: "postgres",
        operationId: "dbop_cli_query_multi_statement",
        operation: { statement: "select 1; update items set value = 2" },
        adapter: adapterForEngine("postgres", clientPath),
      }),
      { env: databaseTestEnv(dir), cwd: dir },
    );

    assert.notEqual(proc.status, 0);
    assert.equal(result.status, "failed");
    assert.match(result.error.message, /exactly one SQL statement/);
    assert.deepEqual(await journalStatuses(join(dir, ".dosql", "journal.jsonl")), [
      "planned",
      "failed",
    ]);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("query CLI reports journal error codes and line numbers before dispatch", async (t) => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-cli-audit-error-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const clientPath = await createFakeDatabaseClient(dir);
  const payload = databaseOperationPayload({
    dir, engine: "postgres", operationId: "invalid-journal",
    operation: { statement: "select 1" },
    adapter: adapterForEngine("postgres", clientPath),
  });
  await mkdir(dirname(payload.journalPath), { recursive: true });
  await writeFile(payload.journalPath, "{invalid}\n");
  const { proc, result } = await runCliResult("query", payload, {
    env: databaseTestEnv(dir), cwd: dir,
  });
  assert.notEqual(proc.status, 0);
  assert.equal(result.error.code, "DOSQL_AUDIT_PERSISTENCE_FAILED");
  assert.equal(result.error.auditErrorCode, "DOSQL_JOURNAL_INVALID_JSON");
  assert.equal(result.error.lineNumber, 1);
  assert.equal(result.error.executionOutcome, "not_started");
  assert.equal(result.error.retryable, false);
  assert.equal(await readFile(payload.journalPath, "utf8"), "{invalid}\n");
});

test("query CLI response cannot overwrite an existing evidence file", async (t) => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-cli-output-evidence-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const clientPath = await createFakeDatabaseClient(dir);
  const payload = databaseOperationPayload({
    dir, engine: "postgres", operationId: "protected-evidence",
    operation: { statement: "select 1" },
    adapter: adapterForEngine("postgres", clientPath),
  });
  const inputPath = join(dir, "input.json");
  await mkdir(dirname(payload.evidencePath), { recursive: true });
  await writeFile(payload.evidencePath, "original evidence");
  await writeFile(inputPath, JSON.stringify(payload));
  const proc = spawnSync(process.execPath, [
    CLI, "query", "--input", inputPath, "--output", payload.evidencePath,
  ], { encoding: "utf8", cwd: dir, env: { ...process.env, ...databaseTestEnv(dir) } });
  assert.notEqual(proc.status, 0);
  assert.match(proc.stderr, /EEXIST/);
  assert.equal(await readFile(payload.evidencePath, "utf8"), "original evidence");
  await assert.rejects(readFile(payload.journalPath), { code: "ENOENT" });
});

test("reusing a response for a different operation never dispatches or changes lifecycle records", async (t) => {
  const fixture = await responseQueryFixture(t);
  const first = await fixture.run("first");
  assert.equal(first.status, 0, first.stderr);
  const originalResponse = await readFile(fixture.outputPath);
  const originalJournal = await readFile(fixture.payload("first").journalPath);
  const originalDispatches = await readFile(fixture.dispatchPath);
  const second = await fixture.run("second");
  assert.notEqual(second.status, 0);
  const failure = JSON.parse(second.stderr);
  assert.equal(failure.operationId, "second");
  assert.equal(failure.status, "failed");
  assert.equal(failure.error.code, "DOSQL_RESPONSE_ALREADY_EXISTS");
  assert.equal(failure.error.executionOutcome, "not_started");
  assert.equal(failure.error.retryable, false);
  assert.deepEqual(await readFile(fixture.outputPath), originalResponse);
  assert.deepEqual(await readFile(fixture.payload("first").journalPath), originalJournal);
  assert.deepEqual(await readFile(fixture.dispatchPath), originalDispatches);
  await assert.rejects(lstat(fixture.payload("second").journalPath), { code: "ENOENT" });
  await assert.rejects(lstat(fixture.payload("second").evidencePath), { code: "ENOENT" });
});

test("concurrent CLI processes sharing a response dispatch only the reservation winner", async (t) => {
  const fixture = await responseQueryFixture(t);
  const ids = Array.from({ length: 6 }, (_, index) => `concurrent-${index}`);
  const results = await Promise.all(ids.map((id) => fixture.run(id)));
  assert.equal(results.filter((result) => result.status === 0).length, 1);
  const response = JSON.parse(await readFile(fixture.outputPath));
  const claim = JSON.parse(await readFile(`${fixture.outputPath}.claim`));
  assert.equal(claim.operationId, response.operationId);
  for (const [index, result] of results.entries()) {
    if (result.status === 0) continue;
    const failure = JSON.parse(result.stderr);
    assert.equal(failure.operationId, ids[index]);
    assert.equal(failure.error.code, "DOSQL_RESPONSE_ALREADY_EXISTS");
    assert.equal(failure.error.executionOutcome, "not_started");
    assert.equal(failure.error.retryable, false);
    await assert.rejects(lstat(fixture.payload(ids[index]).journalPath), { code: "ENOENT" });
    await assert.rejects(lstat(fixture.payload(ids[index]).evidencePath), { code: "ENOENT" });
  }
  const dispatches = (await readFile(fixture.dispatchPath, "utf8")).trim().split("\n");
  assert.equal(dispatches.length, 1);
  assert.equal(JSON.parse(dispatches[0]).operation_id, response.operationId);
});

for (const kind of ["file", "directory", "dangling-symlink", "claim"]) {
  test(`pre-existing response ${kind} fails before query lifecycle starts`, async (t) => {
    const fixture = await responseQueryFixture(t);
    if (kind === "file") await writeFile(fixture.outputPath, "old response");
    if (kind === "directory") await mkdir(fixture.outputPath);
    if (kind === "dangling-symlink") await symlink(join(fixture.dir, "absent"), fixture.outputPath);
    if (kind === "claim") await writeFile(`${fixture.outputPath}.claim`, "prior claim");
    const result = await fixture.run("blocked");
    assert.notEqual(result.status, 0);
    const failure = JSON.parse(result.stderr);
    assert.equal(failure.error.code, "DOSQL_RESPONSE_ALREADY_EXISTS");
    assert.equal(failure.error.executionOutcome, "not_started");
    assert.equal(failure.error.retryable, false);
    await assert.rejects(lstat(fixture.dispatchPath), { code: "ENOENT" });
    await assert.rejects(lstat(join(fixture.dir, ".dosql")), { code: "ENOENT" });
    if (kind === "file") assert.equal(await readFile(fixture.outputPath, "utf8"), "old response");
    if (kind === "directory") assert.equal((await lstat(fixture.outputPath)).isDirectory(), true);
    if (kind === "dangling-symlink") {
      assert.equal((await lstat(fixture.outputPath)).isSymbolicLink(), true);
      await assert.rejects(lstat(join(fixture.dir, "absent")), { code: "ENOENT" });
    }
    if (kind === "claim") {
      assert.equal(await readFile(`${fixture.outputPath}.claim`, "utf8"), "prior claim");
      await assert.rejects(lstat(fixture.outputPath), { code: "ENOENT" });
    } else {
      await assert.rejects(lstat(`${fixture.outputPath}.claim`), { code: "ENOENT" });
    }
  });
}

for (const [mode, outcome, states] of [
  ["publish-response", "succeeded", ["planned", "running", "succeeded", "verified"]],
  ["publish-response-driver-failure", "unknown", ["planned", "running", "failed"]],
  ["publish-response-audit-failure", "succeeded", ["planned", "running", "succeeded"]],
]) {
  test(`response publication failure reports the true outcome for ${mode}`, async (t) => {
    const fixture = await responseQueryFixture(t);
    const result = await fixture.run("publication-failure", mode);
    assert.notEqual(result.status, 0);
    const failure = JSON.parse(result.stderr);
    assert.equal(failure.operationId, "publication-failure");
    assert.equal(failure.status, "failed");
    assert.equal(failure.error.code, "DOSQL_RESPONSE_PERSISTENCE_FAILED");
    assert.equal(failure.error.auditErrorCode, "EEXIST");
    assert.equal(failure.error.executionOutcome, outcome);
    assert.equal(failure.error.retryable, false);
    if (mode === "publish-response-audit-failure") {
      assert.equal(failure.commandError.code, "DOSQL_AUDIT_PERSISTENCE_FAILED");
      assert.equal(failure.commandError.executionOutcome, "succeeded");
    }
    if (mode === "publish-response-driver-failure") {
      assert.match(failure.commandError.message, /synthetic driver failure/);
    }
    assert.equal(await readFile(fixture.outputPath, "utf8"), "competing response");
    assert.deepEqual(await journalStatuses(fixture.payload("publication-failure").journalPath), states);
    assert.equal((await readFile(fixture.dispatchPath, "utf8")).trim().split("\n").length, 1);
    const replay = await fixture.run("another-operation");
    assert.equal(JSON.parse(replay.stderr).error.executionOutcome, "not_started");
    assert.equal((await readFile(fixture.dispatchPath, "utf8")).trim().split("\n").length, 1);
    assert.equal(await readFile(fixture.outputPath, "utf8"), "competing response");
  });
}

test("register-database turns probe output into assets, snapshot and checklist", async () => {
  const result = await runCli("register-database", {
    operationId: "dbop_register_001",
    projectId: "proj_olap",
    environmentId: "test",
    naming: {
      mysql: {
        displayName: "订单库",
        aliases: ["orders"],
      },
    },
    probe: sampleProbe(),
  });

  assert.equal(result.status, "succeeded");
  assert.equal(result.command, "register-database");
  assert.deepEqual(
    result.result.inventory.assets.map((asset) => `${asset.engine}:${asset.version.label}`),
    ["mysql:dosql_000000", "mongodb:dosql_000000"],
  );
  assert.equal(result.result.inventory.assets[0].displayName, "订单库");
  assert.deepEqual(result.result.inventory.assets[0].aliases, ["orders", "mysql", "test mysql"]);
  assert.equal(result.result.structureSnapshot.assets.length, 2);
  assert.equal(result.result.maintenanceChecklist.items.length, 8);
});

test("discover-databases command returns candidates that need user naming confirmation", async () => {
  const result = await runCli("discover-databases", {
    operationId: "dbop_discover_001",
    projectId: "proj_olap",
    environmentId: "test",
    probe: sampleProbe(),
    naming: {
      mysql: {
        displayName: "订单库",
        aliases: ["orders"],
      },
    },
  });

  assert.equal(result.status, "succeeded");
  assert.equal(result.command, "discover-databases");
  assert.equal(result.result.namingPrompt.status, "needs_user_confirmation");
  assert.deepEqual(
    result.result.candidates.map((candidate) => `${candidate.engine}:${candidate.displayName}`),
    ["mysql:订单库", "mongodb:mongodb"],
  );
});

test("compare-databases command writes structure comparison artifact", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-compare-databases-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const comparisonArtifactPath = join(dir, ".dosql", "comparisons", "orders-dev-prod.json");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_compare_databases_001",
          structureSnapshots: sampleComparisonSnapshots(),
          referenceDatabaseAssetId: "db_orders_dev",
          targetDatabaseAssetIds: ["db_orders_prod"],
          comparedAt: "2026-07-06T10:00:00.000Z",
          comparedBy: "u_001",
          comparisonArtifactPath,
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "compare-databases", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.command, "compare-databases");
    assert.equal(result.result.comparisonArtifactPath, comparisonArtifactPath);
    const artifact = JSON.parse(await readFile(comparisonArtifactPath, "utf8"));
    assert.equal(artifact.schema, "dosql.database-comparison.v1");
    assert.equal(artifact.differenceCount, 3);
    assert.deepEqual(
      artifact.differences.map((difference) => difference.differenceKind),
      ["missing_column", "column_type_changed", "nullable_changed"],
    );
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("derive-change-plan-from-comparison command writes additive change plan artifact", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-compare-change-plan-"));
  try {
    const compareInput = join(dir, "compare-input.json");
    const compareOutput = join(dir, "compare-output.json");
    const deriveInput = join(dir, "derive-input.json");
    const deriveOutput = join(dir, "derive-output.json");
    const comparisonArtifactPath = join(dir, ".dosql", "comparisons", "orders-dev-prod.json");
    const changePlanPath = join(dir, ".dosql", "comparisons", "orders-dev-prod-change-plan.json");
    await writeFile(
      compareInput,
      JSON.stringify(
        {
          operationId: "dbop_compare_databases_002",
          structureSnapshots: sampleComparisonSnapshots(),
          referenceDatabaseAssetId: "db_orders_dev",
          targetDatabaseAssetIds: ["db_orders_prod"],
          comparedAt: "2026-07-06T10:00:00.000Z",
          comparedBy: "u_001",
          comparisonArtifactPath,
        },
        null,
        2,
      ),
    );
    const compareProc = spawnSync(process.execPath, [CLI, "compare-databases", "--input", compareInput, "--output", compareOutput], {
      encoding: "utf8",
    });
    assert.equal(compareProc.status, 0, compareProc.stderr || (await readFile(compareOutput, "utf8")));
    await writeFile(
      deriveInput,
      JSON.stringify(
        {
          operationId: "dbop_derive_compare_plan_001",
          comparisonArtifactPath,
          changeRequestId: "chg_compare_001",
          changePlanPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T10:05:00.000Z",
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "derive-change-plan-from-comparison",
      "--input",
      deriveInput,
      "--output",
      deriveOutput,
    ], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(deriveOutput, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.command, "derive-change-plan-from-comparison");
    assert.equal(result.result.changePlanPath, changePlanPath);
    const artifact = JSON.parse(await readFile(changePlanPath, "utf8"));
    assert.equal(artifact.schema, "dosql.compare-change-plan.v1");
    assert.deepEqual(artifact.targetPlans[0].changeDescriptors.map((change) => change.action), ["add_column"]);
    assert.deepEqual(
      artifact.targetPlans[0].manualDifferences.map((difference) => difference.differenceKind),
      ["column_type_changed", "nullable_changed"],
    );
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-forward-change-artifacts command derives forward SQL and manifest", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-forward-artifacts-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const changePlanPath = join(dir, ".dosql", "comparisons", "orders-dev-prod-change-plan.json");
    const scriptDir = join(dir, ".dosql", "changes", "chg_compare_001", "scripts");
    const artifactManifestPath = join(dir, ".dosql", "changes", "chg_compare_001", "forward-artifacts.json");
    await mkdir(dirname(changePlanPath), { recursive: true });
    await writeFile(
      changePlanPath,
      `${JSON.stringify(sampleCompareChangePlan(), null, 2)}\n`,
      "utf8",
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_forward_artifacts_001",
          changePlanPath,
          targetDatabaseAssetId: "db_orders_prod",
          timelineNode: {
            timelineNodeId: "tln_000001",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 1,
            nodeLabel: "dosql_000001",
            nodeKind: "change",
            stateStatus: "verified",
            validFrom: "2026-07-06T09:00:00.000Z",
            baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
            baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
            schemaFingerprint: "sha256:s1",
            restoreCapability: "schema_reversible",
          },
          scriptDir,
          artifactBaseRef: "changes/chg_compare_001/scripts",
          artifactManifestPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T09:05:00.000Z",
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "render-forward-change-artifacts",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.artifactManifestPath, artifactManifestPath);
    const manifest = JSON.parse(await readFile(artifactManifestPath, "utf8"));
    const forwardSql = await readFile(
      join(scriptDir, "forward-dosql_000001-001-orders-external_id.sql"),
      "utf8",
    );
    assert.equal(forwardSql.trim(), "alter table orders add column external_id varchar(64);");
    assert.equal(manifest.schema, "dosql.forward-artifact-manifest.v1");
    assert.equal(manifest.artifacts[0].artifactKind, "forward_sql");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("resolve-database command maps natural language to database asset", async () => {
  const result = await runCli("resolve-database", {
    operationId: "dbop_resolve_001",
    utterance: "帮我看一下订单库 users 表",
    assets: [
      {
        databaseAssetId: "db_proj_olap_test_mysql",
        projectId: "proj_olap",
        environmentId: "test",
        engine: "mysql",
        displayName: "订单库",
        aliases: ["orders", "mysql"],
        logicalName: "mysql",
        databaseName: "test",
        serviceName: "mysql",
      },
    ],
  });

  assert.equal(result.status, "succeeded");
  assert.equal(result.command, "resolve-database");
  assert.equal(result.result.status, "resolved");
  assert.equal(result.result.asset.databaseAssetId, "db_proj_olap_test_mysql");
});

test("scan command returns health, SQL logs and SQL injection findings", async () => {
  const result = await runCli("scan", {
    operationId: "dbop_scan_001",
    asset: {
      databaseAssetId: "db_proj_olap_test_mysql",
      projectId: "proj_olap",
      environmentId: "test",
      engine: "mysql",
      versionText: "8.0.45",
    },
    samples: {
      mysql: {
        status: {
          Threads_connected: "5",
          Slow_queries: "2",
          Uptime: "7200",
        },
        variables: {
          max_connections: "100",
          slow_query_log: "ON",
          long_query_time: "2.000000",
        },
      },
      sqlLogs: {
        source: "slow_log",
        lines: [
          "# Time: 2026-07-05T07:40:01.000000Z",
          "# User@Host: app[app] @ 10.42.0.200 []  Id: 123",
          "# Query_time: 2.531 Lock_time: 0.000 Rows_sent: 1 Rows_examined: 1000",
          "SELECT * FROM users WHERE username='admin' OR '1'='1';",
        ],
      },
    },
  });

  assert.equal(result.status, "succeeded");
  assert.equal(result.result.sqlLogEvents.length, 1);
  assert.equal(result.result.securityFindings[0].pattern, "boolean_tautology");
  assert.equal(result.result.audit.mode, "read_only");
});

test("record-execution appends SQL lifecycle events to JSONL journal", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-agent-journal-"));
  try {
    const journalPath = join(dir, "proj_erp", "test", "db_orders.jsonl");
    const result = await runCli("record-execution", {
      operationId: "dbop_record_001",
      journalPath,
      execution: {
        operationId: "dbop_record_001",
        changeRequestId: "chg_record_001",
        projectId: "proj_erp",
        environmentId: "test",
        databaseAssetId: "db_orders_test",
        engine: "mysql",
        actor: { type: "user", id: "u_001" },
        statementKind: "alter_table",
        statementText: "alter table orders add column external_id varchar(64)",
        version: { from: 1, to: 2, label: "dosql_000002" },
        evidenceRef: "changes/chg_record_001/evidence/dbop_record_001.result.json",
        timestamps: {
          plannedAt: "2026-07-05T13:00:00.000Z",
          runningAt: "2026-07-05T13:01:00.000Z",
          finishedAt: "2026-07-05T13:02:00.000Z",
          verifiedAt: "2026-07-05T13:03:00.000Z",
        },
        execution: {
          status: "succeeded",
          affectedRows: 0,
        },
        verification: {
          status: "verified",
          querySummary: "orders.external_id exists",
        },
      },
    });

    const lines = (await readFile(journalPath, "utf8")).trim().split("\n");
    assert.equal(result.status, "succeeded");
    assert.equal(result.command, "record-execution");
    assert.equal(result.result.appendedEvents, 4);
    assert.equal(lines.length, 4);
    assert.equal(JSON.parse(lines[3]).eventType, "sql.execution.verified");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("replay-timeline returns latest environment status from JSONL journal", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-agent-journal-"));
  try {
    const journalPath = join(dir, "timeline.jsonl");
    await runCli("record-execution", {
      operationId: "dbop_replay_001",
      journalPath,
      execution: {
        operationId: "dbop_replay_001",
        changeRequestId: "chg_replay_001",
        projectId: "proj_erp",
        environmentId: "prod",
        databaseAssetId: "db_orders_prod",
        engine: "mysql",
        actor: { type: "agent", id: "dosql-agent" },
        statementKind: "update",
        statementText: "update orders set status='paid' where id=1",
        version: { from: 7, to: 8, label: "dosql_000008" },
        evidenceRef: "changes/chg_replay_001/evidence/dbop_replay_001.result.json",
        timestamps: {
          plannedAt: "2026-07-05T14:00:00.000Z",
          runningAt: "2026-07-05T14:01:00.000Z",
          finishedAt: "2026-07-05T14:02:00.000Z",
          verifiedAt: "2026-07-05T14:03:00.000Z",
        },
        execution: { status: "succeeded", affectedRows: 1 },
        verification: { status: "verified", querySummary: "order status is paid" },
      },
    });

    const result = await runCli("replay-timeline", {
      operationId: "dbop_replay_read_001",
      journalPath,
    });

    assert.equal(result.status, "succeeded");
    assert.equal(result.result.timeline.environments.prod.status, "verified");
    assert.equal(result.result.timeline.environments.prod.currentLabel, "dosql_000008");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

for (const command of ["execute", "propose-confirmation"]) {
  test(`${command} is not a production CLI entrypoint`, async () => {
    const { proc, result } = await runCliResult(command, { operationId: "removed-command" });
    assert.notEqual(proc.status, 0);
    assert.equal(result.status, "failed");
    assert.equal(result.error.message, `Unsupported command: ${command}`);
  });
}

test("internal schema-change fixture keeps readable confirmation without claiming live database state", () => {
  const database = connectTestDatabase({
    projectId: "proj_erp",
    environmentId: "test",
    databaseAssetId: "db_orders_test",
    engine: "mysql",
    schemaVersion: 2,
  });
  const proposal = proposeAgentSchemaChange({
    database,
    actor: {
      type: "user",
      id: "u_001",
    },
    userMessage: "给订单表增加 external_id 字段",
    agentAnalysis: {
      action: "add_column",
      table: "orders",
      column: "external_id",
      dataType: "varchar(64)",
      nullable: true,
      businessReason: "保存外部订单号。",
    },
  });

  assert.equal(database.kind, "test_database_fixture");
  assert.equal(database.schemaVersion, 2);
  assert.equal(proposal.version.from, 2);
  assert.equal(proposal.version.to, 3);
  assert.equal(proposal.status, "awaiting_user_confirmation");
  assert.equal(proposal.userConfirmation.format, "human_readable");
  assert.doesNotMatch(proposal.userConfirmation.confirmationText, /alter\s+table/i);
  assert.match(proposal.internalScript, /alter table orders/i);
});

test("resolve-timeline-at command returns verified node for timestamp", async () => {
  const result = await runCli("resolve-timeline-at", {
    operationId: "dbop_resolve_timeline_001",
    databaseAssetId: "db_orders_test",
    timestamp: "2026-07-06T09:45:00.000Z",
    nodes: [
      {
        timelineNodeId: "tln_000000",
        databaseAssetId: "db_orders_test",
        nodeSequence: 0,
        nodeLabel: "dosql_000000",
        stateStatus: "verified",
        validFrom: "2026-07-06T09:00:00.000Z",
      },
      {
        timelineNodeId: "tln_000001",
        databaseAssetId: "db_orders_test",
        nodeSequence: 1,
        nodeLabel: "dosql_000001",
        stateStatus: "verified",
        validFrom: "2026-07-06T09:30:00.000Z",
      },
    ],
  });

  assert.equal(result.status, "succeeded");
  assert.equal(result.command, "resolve-timeline-at");
  assert.equal(result.result.node.timelineNodeId, "tln_000001");
});

test("render-timepoint-state-query command writes metadata lookup SQL artifact", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-timepoint-query-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const queryArtifactPath = join(dir, ".dosql", "queries", "db_orders_prod-0930.json");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_timepoint_query_001",
          databaseAssetId: "db_orders_prod",
          timestamp: "2026-07-06T09:30:00.000Z",
          queryArtifactPath,
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "render-timepoint-state-query",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.command, "render-timepoint-state-query");
    assert.equal(result.result.queryArtifactPath, queryArtifactPath);
    const artifact = JSON.parse(await readFile(queryArtifactPath, "utf8"));
    assert.equal(artifact.schema, "dosql.timepoint-state-query.v1");
    assert.equal(artifact.databaseAssetId, "db_orders_prod");
    assert.match(artifact.sqlText, /from dosql_timeline_nodes/);
    assert.match(artifact.sqlText, /from dosql_baseline_records br/);
    assert.match(artifact.sqlText, /from dosql_timeline_artifacts ta/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("execute-timepoint-state-query command runs metadata lookup and writes result artifact", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-execute-timepoint-query-"));
  try {
    const renderInput = join(dir, "render-input.json");
    const renderOutput = join(dir, "render-output.json");
    const executeInput = join(dir, "execute-input.json");
    const executeOutput = join(dir, "execute-output.json");
    const queryArtifactPath = join(dir, ".dosql", "queries", "db_orders_prod-0930.json");
    const queryResultPath = join(dir, ".dosql", "queries", "db_orders_prod-0930-result.json");
    const psqlPath = join(dir, "fake-psql.mjs");
    const timepointState = {
      databaseAssetId: "db_orders_prod",
      timestamp: "2026-07-06T09:30:00.000Z",
      timelineNode: {
        timeline_node_id: "tln_000001",
        node_label: "dosql_000001",
        schema_fingerprint: "sha256:s1",
      },
      baselineRecords: [
        {
          baseline_id: "bln_001",
          baseline_kind: "after",
        },
      ],
      timelineArtifacts: [
        {
          artifact_id: "art_001",
          artifact_kind: "schema_snapshot",
        },
      ],
    };
    await writeFile(
      psqlPath,
      [
        "#!/usr/bin/env node",
        "let input = '';",
        "process.stdin.on('data', (chunk) => { input += chunk; });",
        "process.stdin.on('end', () => {",
        "  if (!process.env.PGDATABASE) process.exit(4);",
        "  if (!input.includes('dosql_timeline_nodes')) process.exit(5);",
        `  process.stdout.write(${JSON.stringify(`${JSON.stringify(timepointState)}\n`)});`,
        "});",
        "",
      ].join("\n"),
      "utf8",
    );
    await chmod(psqlPath, 0o755);
    await writeFile(
      renderInput,
      JSON.stringify(
        {
          operationId: "dbop_render_timepoint_query_002",
          databaseAssetId: "db_orders_prod",
          timestamp: "2026-07-06T09:30:00.000Z",
          queryArtifactPath,
        },
        null,
        2,
      ),
    );
    const renderProc = spawnSync(process.execPath, [
      CLI,
      "render-timepoint-state-query",
      "--input",
      renderInput,
      "--output",
      renderOutput,
    ], { encoding: "utf8" });
    assert.equal(renderProc.status, 0, renderProc.stderr || (await readFile(renderOutput, "utf8")));
    const queryArtifact = JSON.parse(await readFile(queryArtifactPath, "utf8"));
    await writeFile(
      executeInput,
      JSON.stringify(
        {
          operationId: "dbop_execute_timepoint_query_001",
          queryArtifactPath,
          queryResultPath,
          queriedBy: "u_001",
          queriedAt: "2026-07-06T10:00:00.000Z",
          connectionRef: "secret://dosql/metadata",
          metadataAdapter: {
            type: "postgres-psql",
            psqlPath,
            connectionUriEnv: "DOSQL_METADATA_DATABASE_URL",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "execute-timepoint-state-query",
      "--input",
      executeInput,
      "--output",
      executeOutput,
    ], {
      encoding: "utf8",
      env: {
        ...process.env,
        DOSQL_METADATA_DATABASE_URL: "postgres://metadata-db",
      },
    });
    const result = JSON.parse(await readFile(executeOutput, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.command, "execute-timepoint-state-query");
    assert.equal(result.mode, "read_only");
    assert.equal(result.result.queryResultPath, queryResultPath);
    const artifact = JSON.parse(await readFile(queryResultPath, "utf8"));
    assert.equal(artifact.schema, "dosql.timepoint-state-query-result.v1");
    assert.equal(artifact.status, "resolved");
    assert.equal(artifact.databaseAssetId, "db_orders_prod");
    assert.equal(artifact.sourceQueryFingerprint, queryArtifact.artifactFingerprint);
    assert.equal(artifact.timepointState.timelineNode.timeline_node_id, "tln_000001");
    assert.match(artifact.artifactFingerprint, /^sha256:[a-f0-9]{64}$/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("create-timepoint-state-manifest command writes a resolved state manifest", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-timepoint-manifest-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const queryResultPath = join(dir, ".dosql", "queries", "db_orders_prod-0930-result.json");
    const stateManifestPath = join(dir, ".dosql", "queries", "db_orders_prod-0930-state.json");
    await mkdir(dirname(queryResultPath), { recursive: true });
    await writeFile(
      queryResultPath,
      `${JSON.stringify(sampleTimepointQueryResultArtifact(), null, 2)}\n`,
      "utf8",
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_create_timepoint_manifest_001",
          queryResultPath,
          stateManifestPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T10:05:00.000Z",
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "create-timepoint-state-manifest",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.stateManifestPath, stateManifestPath);
    const manifest = JSON.parse(await readFile(stateManifestPath, "utf8"));
    assert.equal(manifest.schema, "dosql.timepoint-state-manifest.v1");
    assert.equal(manifest.timelineNodeId, "tln_000001");
    assert.equal(manifest.stateBaseline.schemaSnapshotRef, "baselines/db_orders_prod/000001.after.json");
    assert.equal(manifest.executionEvidence.forwardExecutions.length, 1);
    assert.equal(manifest.executionEvidence.forwardExecutions[0].forward_execution_id, "fex_test_001");
    assert.match(manifest.artifactFingerprint, /^sha256:[a-f0-9]{64}$/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("check-head-drift command blocks planning when live fingerprint differs", async () => {
  const result = await runCli("check-head-drift", {
    operationId: "dbop_check_drift_001",
    liveSchemaFingerprint: "sha256:s1-external",
    checkedAt: "2026-07-06T09:05:00.000Z",
    evidenceRef: "scans/db_orders_prod/0905.json",
    currentNode: {
      timelineNodeId: "tln_000000",
      databaseAssetId: "db_orders_prod",
      nodeSequence: 0,
      nodeLabel: "dosql_000000",
      stateStatus: "verified",
      validFrom: "2026-07-06T08:00:00.000Z",
      baselineAfterRef: "baselines/db_orders_prod/000000.after.json",
      schemaFingerprint: "sha256:s0",
    },
  });

  assert.equal(result.status, "succeeded");
  assert.equal(result.command, "check-head-drift");
  assert.equal(result.result.status, "drift_detected");
  assert.equal(result.result.canPlanChange, false);
  assert.equal(result.result.expectedSchemaFingerprint, "sha256:s0");
  assert.equal(result.result.liveSchemaFingerprint, "sha256:s1-external");
});

test("import-drift command writes drift timeline node artifact", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-import-drift-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const driftNodePath = join(dir, ".dosql", "changes", "chg_drift_001", "drift-node.json");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_import_drift_001",
          driftNodePath,
          validFrom: "2026-07-06T09:10:00.000Z",
          baselineAfterRef: "baselines/db_orders_prod/000001.drift.after.json",
          schemaFingerprint: "sha256:s1-external",
          evidenceRef: "scans/db_orders_prod/0910-drift.json",
          restoreCapability: "manual_mitigation",
          currentNode: {
            timelineNodeId: "tln_000000",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 0,
            nodeLabel: "dosql_000000",
            stateStatus: "verified",
            validFrom: "2026-07-06T08:00:00.000Z",
            baselineAfterRef: "baselines/db_orders_prod/000000.after.json",
            schemaFingerprint: "sha256:s0",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "import-drift", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    const driftNode = JSON.parse(await readFile(driftNodePath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.driftNodePath, driftNodePath);
    assert.equal(driftNode.nodeKind, "drift_import");
    assert.equal(driftNode.parentNodeId, "tln_000000");
    assert.equal(driftNode.schemaFingerprint, "sha256:s1-external");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("project-current-head command writes database version projection", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-project-head-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const projectionPath = join(dir, ".dosql", "changes", "chg_head_001", "database-version.json");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_project_head_001",
          projectionPath,
          updatedBy: "u_001",
          updatedAt: "2026-07-06T09:05:00.000Z",
          currentVersion: {
            databaseAssetId: "db_orders_prod",
            currentVersion: 0,
            currentLabel: "dosql_000000",
            currentTimelineNodeId: "tln_000000",
          },
          nextNode: {
            timelineNodeId: "tln_000001",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 1,
            nodeLabel: "dosql_000001",
            parentNodeId: "tln_000000",
            nodeKind: "change",
            stateStatus: "verified",
            validFrom: "2026-07-06T09:00:00.000Z",
            baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
            baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
            schemaFingerprint: "sha256:s1",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "project-current-head", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    const projection = JSON.parse(await readFile(projectionPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.projectionPath, projectionPath);
    assert.equal(projection.schema, "dosql.database-version-projection.v1");
    assert.equal(projection.projectedVersion.currentVersion, 1);
    assert.equal(projection.projectedVersion.currentTimelineNodeId, "tln_000001");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-metadata-commit command writes guarded timeline metadata SQL", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-metadata-commit-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const commitPath = join(dir, ".dosql", "changes", "chg_head_001", "metadata-commit.sql");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_metadata_commit_001",
          commitPath,
          updatedBy: "u_001",
          updatedAt: "2026-07-06T09:05:00.000Z",
          currentVersion: {
            databaseAssetId: "db_orders_prod",
            currentVersion: 0,
            currentLabel: "dosql_000000",
            currentTimelineNodeId: "tln_000000",
          },
          node: {
            timelineNodeId: "tln_000001",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 1,
            nodeLabel: "dosql_000001",
            parentNodeId: "tln_000000",
            operationId: "dbop_add_external_id",
            nodeKind: "change",
            stateStatus: "verified",
            validFrom: "2026-07-06T09:00:00.000Z",
            baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
            baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
            schemaFingerprint: "sha256:s1",
            dataCheckpointRef: "",
            restoreCapability: "schema_reversible",
            restoreFromNodeId: "",
            restoreTargetNodeId: "",
            createdAt: "2026-07-06T09:00:00.000Z",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "render-metadata-commit", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    const sql = await readFile(commitPath, "utf8");

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.commitPath, commitPath);
    assert.equal(result.result.commit.schema, "dosql.timeline-metadata-commit.v1");
    assert.match(sql, /insert into dosql_timeline_nodes/);
    assert.ok(
      sql.includes(
        "where database_asset_id = 'db_orders_prod'\n  and current_version = 0\n  and current_timeline_node_id = 'tln_000000'",
      ),
    );
    assert.match(sql, /else 1 \/ 0/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("create-baseline-records command writes baseline record set artifact", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-baseline-records-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const baselineRecordsPath = join(dir, "baseline-records.json");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_create_baselines_001",
          baselineRecordsPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T09:05:00.000Z",
          currentNode: {
            timelineNodeId: "tln_000000",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 0,
            nodeLabel: "dosql_000000",
            nodeKind: "baseline",
            stateStatus: "verified",
            validFrom: "2026-07-06T08:00:00.000Z",
            baselineAfterRef: "baselines/db_orders_prod/000000.after.json",
            schemaFingerprint: "sha256:s0",
          },
          timelineNode: {
            timelineNodeId: "tln_000001",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 1,
            nodeLabel: "dosql_000001",
            nodeKind: "change",
            parentNodeId: "tln_000000",
            baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
            baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
            schemaFingerprint: "sha256:s1",
          },
          records: [
            {
              baselineKind: "before",
              capturedAt: "2026-07-06T08:59:00.000Z",
              schemaSnapshotRef: "baselines/db_orders_prod/000001.before.json",
              schemaFingerprint: "sha256:s0",
              dataScope: "before_image",
              dataEvidenceRef: "baselines/db_orders_prod/000001.before-image.json",
              artifactFingerprint: "sha256:before-baseline",
            },
            {
              baselineKind: "after",
              capturedAt: "2026-07-06T09:04:00.000Z",
              schemaSnapshotRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s1",
              dataScope: "before_image",
              dataEvidenceRef: "baselines/db_orders_prod/000001.before-image.json",
              artifactFingerprint: "sha256:after-baseline",
            },
          ],
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "create-baseline-records", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.baselineRecordsPath, baselineRecordsPath);
    const artifact = JSON.parse(await readFile(baselineRecordsPath, "utf8"));
    assert.equal(artifact.schema, "dosql.baseline-record-set.v1");
    assert.equal(artifact.records.length, 2);
    assert.equal(artifact.records[0].baselineKind, "before");
    assert.equal(artifact.records[1].baselineKind, "after");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("derive-initial-baseline-from-structure-snapshot command writes initial node and baseline records", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-initial-baseline-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const initialBaselinePath = join(dir, "initial-baseline.json");
    const timelineNodePath = join(dir, "initial-node.json");
    const baselineRecordsPath = join(dir, "baseline-records.json");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_initial_baseline_001",
          databaseAssetId: "db_orders_prod",
          baselineAfterRef: "baselines/db_orders_prod/000000.after.json",
          structureSnapshot: sampleStructureSnapshot({
            capturedAt: "2026-07-06T08:00:00.000Z",
            structureFingerprint: "sha256:s0",
          }),
          initialBaselinePath,
          timelineNodePath,
          baselineRecordsPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T08:05:00.000Z",
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "derive-initial-baseline-from-structure-snapshot",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.initialBaselinePath, initialBaselinePath);
    assert.equal(result.result.timelineNodePath, timelineNodePath);
    assert.equal(result.result.baselineRecordsPath, baselineRecordsPath);
    const initialBaseline = JSON.parse(await readFile(initialBaselinePath, "utf8"));
    const timelineNode = JSON.parse(await readFile(timelineNodePath, "utf8"));
    const recordSet = JSON.parse(await readFile(baselineRecordsPath, "utf8"));
    assert.equal(initialBaseline.schema, "dosql.initial-baseline.v1");
    assert.equal(timelineNode.nodeKind, "baseline");
    assert.equal(timelineNode.schemaFingerprint, "sha256:s0");
    assert.equal(recordSet.records[0].baselineKind, "initial");
    assert.equal(recordSet.records[0].schemaSnapshotRef, "baselines/db_orders_prod/000000.after.json");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("derive-baseline-records-from-structure-snapshots command writes baseline records from snapshots", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-derived-baseline-records-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const baselineRecordsPath = join(dir, "baseline-records.json");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_derive_baselines_001",
          baselineRecordsPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T09:05:00.000Z",
          currentNode: {
            timelineNodeId: "tln_000000",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 0,
            nodeLabel: "dosql_000000",
            nodeKind: "baseline",
            stateStatus: "verified",
            validFrom: "2026-07-06T08:00:00.000Z",
            baselineAfterRef: "baselines/db_orders_prod/000000.after.json",
            schemaFingerprint: "sha256:s0",
          },
          timelineNode: {
            timelineNodeId: "tln_000001",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 1,
            nodeLabel: "dosql_000001",
            nodeKind: "change",
            parentNodeId: "tln_000000",
            baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
            baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
            schemaFingerprint: "sha256:s1",
          },
          beforeSnapshot: sampleStructureSnapshot({
            capturedAt: "2026-07-06T08:59:00.000Z",
            structureFingerprint: "sha256:s0",
          }),
          afterSnapshot: sampleStructureSnapshot({
            capturedAt: "2026-07-06T09:04:00.000Z",
            structureFingerprint: "sha256:s1",
          }),
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "derive-baseline-records-from-structure-snapshots",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.baselineRecordsPath, baselineRecordsPath);
    const artifact = JSON.parse(await readFile(baselineRecordsPath, "utf8"));
    assert.equal(artifact.schema, "dosql.baseline-record-set.v1");
    assert.deepEqual(
      artifact.records.map((record) => `${record.baselineKind}:${record.schemaFingerprint}`),
      ["before:sha256:s0", "after:sha256:s1"],
    );
    assert(artifact.records.every((record) => /^sha256:[a-f0-9]{64}$/.test(record.artifactFingerprint)));
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-baseline-records-commit command writes baseline metadata SQL", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-baseline-records-commit-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const baselineRecordsPath = join(dir, "baseline-records.json");
    const commitPath = join(dir, ".dosql", "changes", "chg_baseline_001", "baseline-records-commit.sql");
    await writeFile(
      baselineRecordsPath,
      JSON.stringify(
        {
          schema: "dosql.baseline-record-set.v1",
          databaseAssetId: "db_orders_prod",
          timelineNodeId: "tln_000001",
          nodeLabel: "dosql_000001",
          createdBy: "u_001",
          createdAt: "2026-07-06T09:05:00.000Z",
          artifactFingerprint: "sha256:record-set",
          records: [
            {
              baselineId: "bln_before",
              databaseAssetId: "db_orders_prod",
              timelineNodeId: "tln_000001",
              baselineKind: "before",
              capturedAt: "2026-07-06T08:59:00.000Z",
              schemaSnapshotRef: "baselines/db_orders_prod/000001.before.json",
              schemaFingerprint: "sha256:s0",
              dataScope: "before_image",
              dataEvidenceRef: "baselines/db_orders_prod/000001.before-image.json",
              artifactFingerprint: "sha256:before-baseline",
              createdAt: "2026-07-06T08:59:00.000Z",
            },
            {
              baselineId: "bln_after",
              databaseAssetId: "db_orders_prod",
              timelineNodeId: "tln_000001",
              baselineKind: "after",
              capturedAt: "2026-07-06T09:04:00.000Z",
              schemaSnapshotRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s1",
              dataScope: "none",
              dataEvidenceRef: "",
              artifactFingerprint: "sha256:after-baseline",
              createdAt: "2026-07-06T09:04:00.000Z",
            },
          ],
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_baseline_records_commit_001",
          baselineRecordsPath,
          commitPath,
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "render-baseline-records-commit",
      "--input",
      input,
      "--output",
      output,
    ], { encoding: "utf8" });
    const result = JSON.parse(await readFile(output, "utf8"));
    const sql = await readFile(commitPath, "utf8");

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.commitPath, commitPath);
    assert.equal(result.result.commit.schema, "dosql.baseline-records-metadata-commit.v1");
    assert.match(sql, /insert into dosql_baseline_records/);
    assert.match(sql, /'bln_before'/);
    assert.match(sql, /'before_image'/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-change-metadata-commit command writes atomic timeline and baseline SQL", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-change-metadata-commit-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const baselineRecordsPath = join(dir, "baseline-records.json");
    const commitPath = join(dir, ".dosql", "changes", "chg_001", "change-metadata-commit.sql");
    const commitArtifactPath = join(dir, ".dosql", "changes", "chg_001", "change-metadata-commit.json");
    await writeFile(
      baselineRecordsPath,
      JSON.stringify(
        {
          schema: "dosql.baseline-record-set.v1",
          databaseAssetId: "db_orders_prod",
          timelineNodeId: "tln_000001",
          nodeLabel: "dosql_000001",
          createdBy: "u_001",
          createdAt: "2026-07-06T09:05:00.000Z",
          artifactFingerprint: "sha256:record-set",
          records: [
            {
              baselineId: "bln_before",
              databaseAssetId: "db_orders_prod",
              timelineNodeId: "tln_000001",
              baselineKind: "before",
              capturedAt: "2026-07-06T08:59:00.000Z",
              schemaSnapshotRef: "baselines/db_orders_prod/000001.before.json",
              schemaFingerprint: "sha256:s0",
              dataScope: "none",
              dataEvidenceRef: "",
              artifactFingerprint: "sha256:before-baseline",
              createdAt: "2026-07-06T08:59:00.000Z",
            },
            {
              baselineId: "bln_after",
              databaseAssetId: "db_orders_prod",
              timelineNodeId: "tln_000001",
              baselineKind: "after",
              capturedAt: "2026-07-06T09:04:00.000Z",
              schemaSnapshotRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s1",
              dataScope: "none",
              dataEvidenceRef: "",
              artifactFingerprint: "sha256:after-baseline",
              createdAt: "2026-07-06T09:04:00.000Z",
            },
          ],
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_change_metadata_commit_001",
          baselineRecordsPath,
          commitPath,
          commitArtifactPath,
          updatedBy: "u_001",
          updatedAt: "2026-07-06T09:05:00.000Z",
          currentVersion: {
            databaseAssetId: "db_orders_prod",
            currentVersion: 0,
            currentLabel: "dosql_000000",
            currentTimelineNodeId: "tln_000000",
          },
          node: {
            timelineNodeId: "tln_000001",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 1,
            nodeLabel: "dosql_000001",
            parentNodeId: "tln_000000",
            operationId: "dbop_add_external_id",
            nodeKind: "change",
            stateStatus: "verified",
            validFrom: "2026-07-06T09:00:00.000Z",
            baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
            baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
            schemaFingerprint: "sha256:s1",
            dataCheckpointRef: "",
            restoreCapability: "schema_reversible",
            restoreFromNodeId: "",
            restoreTargetNodeId: "",
            createdAt: "2026-07-06T09:00:00.000Z",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "render-change-metadata-commit",
      "--input",
      input,
      "--output",
      output,
    ], { encoding: "utf8" });
    const result = JSON.parse(await readFile(output, "utf8"));
    const sql = await readFile(commitPath, "utf8");
    const artifact = JSON.parse(await readFile(commitArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.commit.schema, "dosql.change-metadata-commit.v1");
    assert.equal(result.result.commit.recordCount, 2);
    assert.equal(result.result.commitArtifactPath, commitArtifactPath);
    assert.equal(artifact.schema, "dosql.change-metadata-commit.v1");
    assert.equal(artifact.artifactFingerprint, result.result.commit.artifactFingerprint);
    assert.ok(sql.indexOf("insert into dosql_timeline_nodes") < sql.indexOf("with updated_current_head"));
    assert.ok(sql.indexOf("with updated_current_head") < sql.indexOf("insert into dosql_baseline_records"));
    assert.equal((sql.match(/^begin;$/gm) ?? []).length, 1);
    assert.equal((sql.match(/^commit;$/gm) ?? []).length, 1);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-change-metadata-commit command persists restore node and restore baselines atomically", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-restore-metadata-commit-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const baselineRecordsPath = join(dir, "restore-baseline-records.json");
    const commitPath = join(dir, ".dosql", "changes", "chg_restore_002", "restore-metadata-commit.sql");
    const commitArtifactPath = join(dir, ".dosql", "changes", "chg_restore_002", "restore-metadata-commit.json");
    await writeFile(
      baselineRecordsPath,
      JSON.stringify(
        {
          schema: "dosql.baseline-record-set.v1",
          databaseAssetId: "db_orders_prod",
          timelineNodeId: "tln_000002",
          nodeLabel: "dosql_000002",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:31:00.000Z",
          artifactFingerprint: "sha256:restore-record-set",
          records: [
            {
              baselineId: "bln_restore_before",
              databaseAssetId: "db_orders_prod",
              timelineNodeId: "tln_000002",
              baselineKind: "before",
              capturedAt: "2026-07-06T12:30:00.000Z",
              schemaSnapshotRef: "baselines/db_orders_prod/000002.before.json",
              schemaFingerprint: "sha256:s1",
              dataScope: "none",
              dataEvidenceRef: "",
              artifactFingerprint: "sha256:restore-before-baseline",
              createdAt: "2026-07-06T12:30:00.000Z",
            },
            {
              baselineId: "bln_restore_after",
              databaseAssetId: "db_orders_prod",
              timelineNodeId: "tln_000002",
              baselineKind: "after",
              capturedAt: "2026-07-06T12:30:00.000Z",
              schemaSnapshotRef: "baselines/db_orders_prod/000002.after.json",
              schemaFingerprint: "sha256:s0",
              dataScope: "snapshot",
              dataEvidenceRef: "changes/chg_restore_002/evidence/restore-report.json",
              artifactFingerprint: "sha256:restore-after-baseline",
              createdAt: "2026-07-06T12:30:00.000Z",
            },
          ],
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_restore_metadata_commit_001",
          baselineRecordsPath,
          commitPath,
          commitArtifactPath,
          updatedBy: "u_001",
          updatedAt: "2026-07-06T12:31:00.000Z",
          currentVersion: {
            databaseAssetId: "db_orders_prod",
            currentVersion: 1,
            currentLabel: "dosql_000001",
            currentTimelineNodeId: "tln_000001",
          },
          node: {
            timelineNodeId: "tln_000002",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 2,
            nodeLabel: "dosql_000002",
            parentNodeId: "tln_000001",
            operationId: "dbop_verify_restore_001",
            nodeKind: "restore",
            stateStatus: "verified",
            validFrom: "2026-07-06T12:30:00.000Z",
            baselineBeforeRef: "baselines/db_orders_prod/000002.before.json",
            baselineAfterRef: "baselines/db_orders_prod/000002.after.json",
            schemaFingerprint: "sha256:s0",
            dataCheckpointRef: "changes/chg_restore_002/restore-verification.json",
            restoreCapability: "schema_reversible",
            restoreFromNodeId: "tln_000001",
            restoreTargetNodeId: "tln_000000",
            createdAt: "2026-07-06T12:30:00.000Z",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "render-change-metadata-commit",
      "--input",
      input,
      "--output",
      output,
    ], { encoding: "utf8" });
    const result = JSON.parse(await readFile(output, "utf8"));
    const sql = await readFile(commitPath, "utf8");
    const artifact = JSON.parse(await readFile(commitArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.commit.schema, "dosql.change-metadata-commit.v1");
    assert.equal(result.result.commit.recordCount, 2);
    assert.equal(artifact.timelineNodeId, "tln_000002");
    assert.match(sql, /'restore'/);
    assert.match(sql, /'bln_restore_after'/);
    assert.ok(sql.indexOf("insert into dosql_timeline_nodes") < sql.indexOf("with updated_current_head"));
    assert.ok(sql.indexOf("with updated_current_head") < sql.indexOf("insert into dosql_baseline_records"));
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("record-metadata-commit-execution command writes verified execution evidence", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-metadata-execution-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const commitArtifactPath = join(dir, "change-metadata-commit.json");
    const executionArtifactPath = join(dir, ".dosql", "changes", "chg_001", "metadata-execution.json");
    await writeFile(
      commitArtifactPath,
      JSON.stringify(
        {
          schema: "dosql.change-metadata-commit.v1",
          status: "ready",
          databaseAssetId: "db_orders_prod",
          timelineNodeId: "tln_000001",
          recordCount: 2,
          sourceProjectionFingerprint: "sha256:projection",
          sourceRecordSetFingerprint: "sha256:record-set",
          artifactFingerprint: "sha256:commit",
          sqlText: "begin;\ncommit;\n",
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_record_metadata_execution_001",
          commitArtifactPath,
          executionArtifactPath,
          executedBy: "u_001",
          executedAt: "2026-07-06T09:06:00.000Z",
          connectionRef: "secret://dosql/metadata",
          executionResult: {
            status: "succeeded",
            transactionId: "tx_metadata_001",
            statementCount: 4,
            timelineNodeInsertCount: 1,
            currentHeadUpdateCount: 1,
            currentHeadGuardPassed: true,
            baselineRecordInsertCount: 2,
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "record-metadata-commit-execution",
      "--input",
      input,
      "--output",
      output,
    ], { encoding: "utf8" });
    const result = JSON.parse(await readFile(output, "utf8"));
    const artifact = JSON.parse(await readFile(executionArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.executionArtifactPath, executionArtifactPath);
    assert.equal(result.result.executionArtifact.schema, "dosql.metadata-commit-execution.v1");
    assert.equal(artifact.status, "verified");
    assert.equal(artifact.sourceCommitFingerprint, "sha256:commit");
    assert.equal(artifact.executionResult.currentHeadGuardPassed, true);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("execute-metadata-commit command runs psql adapter and writes verified execution evidence", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-execute-metadata-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const commitArtifactPath = join(dir, "change-metadata-commit.json");
    const executionArtifactPath = join(dir, ".dosql", "changes", "chg_001", "metadata-execution.json");
    const psqlPath = join(dir, "fake-psql.mjs");
    await writeFile(
      psqlPath,
      [
        "#!/usr/bin/env node",
        "let input = '';",
        "process.stdin.on('data', (chunk) => { input += chunk; });",
        "process.stdin.on('end', () => {",
        "  if (!process.env.PGDATABASE) process.exit(4);",
        "  if (!input.includes('dosql_timeline_nodes')) process.exit(5);",
        "  console.log('BEGIN');",
        "  console.log('INSERT 0 1');",
        "  console.log('1');",
        "  console.log('INSERT 0 1');",
        "  console.log('COMMIT');",
        "});",
        "",
      ].join("\n"),
      "utf8",
    );
    await chmod(psqlPath, 0o755);
    await writeFile(
      commitArtifactPath,
      JSON.stringify(
        {
          schema: "dosql.change-metadata-commit.v1",
          status: "ready",
          databaseAssetId: "db_orders_prod",
          timelineNodeId: "tln_000001",
          recordCount: 1,
          sourceProjectionFingerprint: "sha256:projection",
          sourceRecordSetFingerprint: "sha256:record-set",
          artifactFingerprint: "sha256:commit",
          sqlText: "begin;\ninsert into dosql_timeline_nodes values (...);\ninsert into dosql_baseline_records values (...);\ncommit;\n",
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_execute_metadata_commit_001",
          commitArtifactPath,
          executionArtifactPath,
          executedBy: "u_001",
          executedAt: "2026-07-06T09:06:00.000Z",
          connectionRef: "secret://dosql/metadata",
          metadataAdapter: {
            type: "postgres-psql",
            psqlPath,
            connectionUriEnv: "DOSQL_METADATA_DATABASE_URL",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "execute-metadata-commit",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
      env: {
        ...process.env,
        DOSQL_METADATA_DATABASE_URL: "postgres://metadata-db",
      },
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    const artifact = JSON.parse(await readFile(executionArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "metadata_write");
    assert.equal(result.result.executionArtifactPath, executionArtifactPath);
    assert.equal(artifact.schema, "dosql.metadata-commit-execution.v1");
    assert.equal(artifact.status, "verified");
    assert.equal(artifact.executionResult.timelineNodeInsertCount, 1);
    assert.equal(artifact.executionResult.baselineRecordInsertCount, 1);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("execute-metadata-commit command runs restore evidence metadata commits", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-execute-restore-evidence-metadata-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const commitArtifactPath = join(dir, "restore-evidence-commit.json");
    const executionArtifactPath = join(dir, ".dosql", "changes", "chg_restore_004", "metadata-execution.json");
    const psqlPath = join(dir, "fake-psql.mjs");
    await writeFile(
      psqlPath,
      [
        "#!/usr/bin/env node",
        "let input = '';",
        "process.stdin.on('data', (chunk) => { input += chunk; });",
        "process.stdin.on('end', () => {",
        "  if (!process.env.PGDATABASE) process.exit(4);",
        "  if (!input.includes('dosql_restore_verifications')) process.exit(5);",
        "  console.log('BEGIN');",
        "  console.log('INSERT 0 1');",
        "  console.log('INSERT 0 1');",
        "  console.log('INSERT 0 1');",
        "  console.log('COMMIT');",
        "});",
        "",
      ].join("\n"),
      "utf8",
    );
    await chmod(psqlPath, 0o755);
    await writeFile(
      commitArtifactPath,
      JSON.stringify(
        {
          schema: "dosql.restore-evidence-metadata-commit.v1",
          status: "ready",
          restorePlanId: "rplan_test_004",
          changeRequestId: "chg_restore_004",
          databaseAssetId: "db_orders_prod",
          rollbackExecutionId: "rex_test_004",
          restoreCheckExecutionId: "rchk_test_004",
          restoreVerificationId: "rver_test_004",
          sourceRollbackExecutionFingerprint: "sha256:rollback-execution",
          sourceRestoreCheckExecutionFingerprint: "sha256:restore-check-execution",
          sourceRestoreVerificationFingerprint: "sha256:restore-verification",
          artifactFingerprint: "sha256:restore-evidence-commit",
          sqlText: [
            "begin;",
            "insert into dosql_rollback_executions values (...);",
            "insert into dosql_restore_check_executions values (...);",
            "insert into dosql_restore_verifications values (...);",
            "commit;",
            "",
          ].join("\n"),
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_execute_restore_evidence_metadata_001",
          commitArtifactPath,
          executionArtifactPath,
          executedBy: "u_001",
          executedAt: "2026-07-06T12:31:00.000Z",
          connectionRef: "secret://dosql/metadata",
          metadataAdapter: {
            type: "postgres-psql",
            psqlPath,
            connectionUriEnv: "DOSQL_METADATA_DATABASE_URL",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "execute-metadata-commit",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
      env: {
        ...process.env,
        DOSQL_METADATA_DATABASE_URL: "postgres://metadata-db",
      },
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    const artifact = JSON.parse(await readFile(executionArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "metadata_write");
    assert.equal(result.result.executionArtifactPath, executionArtifactPath);
    assert.equal(artifact.schema, "dosql.restore-evidence-metadata-commit-execution.v1");
    assert.equal(artifact.executionResult.restoreVerificationInsertCount, 1);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("execute-metadata-commit command runs forward evidence metadata commits", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-execute-forward-evidence-metadata-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const commitArtifactPath = join(dir, "forward-evidence-commit.json");
    const executionArtifactPath = join(dir, ".dosql", "changes", "chg_compare_001", "metadata-execution.json");
    const psqlPath = join(dir, "fake-psql.mjs");
    await writeFile(
      psqlPath,
      [
        "#!/usr/bin/env node",
        "let input = '';",
        "process.stdin.on('data', (chunk) => { input += chunk; });",
        "process.stdin.on('end', () => {",
        "  if (!process.env.PGDATABASE) process.exit(4);",
        "  if (!input.includes('dosql_forward_executions')) process.exit(5);",
        "  console.log('BEGIN');",
        "  console.log('INSERT 0 1');",
        "  console.log('COMMIT');",
        "});",
        "",
      ].join("\n"),
      "utf8",
    );
    await chmod(psqlPath, 0o755);
    await writeFile(
      commitArtifactPath,
      JSON.stringify(
        {
          schema: "dosql.forward-evidence-metadata-commit.v1",
          status: "ready",
          forwardExecutionId: "fex_test_001",
          changeRequestId: "chg_compare_001",
          databaseAssetId: "db_orders_prod",
          timelineNodeId: "tln_000001",
          sourceForwardExecutionFingerprint: "sha256:forward-execution",
          sourceChangePlanFingerprint: "sha256:compare-change-plan",
          sourceArtifactManifestFingerprint: "sha256:forward-artifact-manifest",
          artifactFingerprint: "sha256:forward-evidence-commit",
          sqlText: [
            "begin;",
            "insert into dosql_forward_executions values (...);",
            "commit;",
            "",
          ].join("\n"),
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_execute_forward_evidence_metadata_001",
          commitArtifactPath,
          executionArtifactPath,
          executedBy: "u_001",
          executedAt: "2026-07-06T12:12:00.000Z",
          connectionRef: "secret://dosql/metadata",
          metadataAdapter: {
            type: "postgres-psql",
            psqlPath,
            connectionUriEnv: "DOSQL_METADATA_DATABASE_URL",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "execute-metadata-commit",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
      env: {
        ...process.env,
        DOSQL_METADATA_DATABASE_URL: "postgres://metadata-db",
      },
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    const artifact = JSON.parse(await readFile(executionArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "metadata_write");
    assert.equal(result.result.executionArtifactPath, executionArtifactPath);
    assert.equal(artifact.schema, "dosql.forward-evidence-metadata-commit-execution.v1");
    assert.equal(artifact.executionResult.forwardExecutionInsertCount, 1);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("plan-rollback command returns blocked result for unrestorable node", async () => {
  const result = await runCli("plan-rollback", {
    operationId: "dbop_plan_rollback_001",
    databaseAssetId: "db_orders_prod",
    currentNodeId: "tln_000001",
    targetNodeId: "tln_000000",
    nodes: [
      {
        timelineNodeId: "tln_000000",
        databaseAssetId: "db_orders_prod",
        nodeSequence: 0,
        nodeLabel: "dosql_000000",
        parentNodeId: "",
        stateStatus: "verified",
        validFrom: "2026-07-06T08:00:00.000Z",
        restoreCapability: "schema_reversible",
      },
      {
        timelineNodeId: "tln_000001",
        databaseAssetId: "db_orders_prod",
        nodeSequence: 1,
        nodeLabel: "dosql_000001",
        parentNodeId: "tln_000000",
        stateStatus: "verified",
        validFrom: "2026-07-06T09:00:00.000Z",
        baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
        baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
        dataCheckpointRef: "",
        restoreCapability: "unrestorable",
      },
    ],
  });

  assert.equal(result.status, "succeeded");
  assert.equal(result.command, "plan-rollback");
  assert.equal(result.result.status, "blocked");
  assert.equal(result.result.blockingNodeId, "tln_000001");
});

test("plan-rollback command writes restore plan artifact when requested", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-restore-plan-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_restore_001", "restore-plan.json");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_plan_rollback_002",
          changeRequestId: "chg_restore_001",
          databaseAssetId: "db_orders_prod",
          currentNodeId: "tln_000001",
          targetNodeId: "tln_000000",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:00:00.000Z",
          restorePlanPath,
          nodes: [
            {
              timelineNodeId: "tln_000000",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 0,
              nodeLabel: "dosql_000000",
              parentNodeId: "",
              stateStatus: "verified",
              validFrom: "2026-07-06T08:00:00.000Z",
              schemaFingerprint: "sha256:s0",
              restoreCapability: "schema_reversible",
            },
            {
              timelineNodeId: "tln_000001",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 1,
              nodeLabel: "dosql_000001",
              parentNodeId: "tln_000000",
              stateStatus: "verified",
              validFrom: "2026-07-06T09:00:00.000Z",
              baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
              baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s1",
              dataCheckpointRef: "",
              restoreCapability: "schema_reversible",
            },
          ],
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "plan-rollback", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    const artifact = JSON.parse(await readFile(restorePlanPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.restorePlanPath, restorePlanPath);
    assert.equal(result.result.restorePlan.restorePlanId, artifact.restorePlanId);
    assert.equal(artifact.schema, "dosql.restore-plan.v1");
    assert.equal(artifact.status, "planned");
    assert.equal(artifact.plan.steps[0].method, "derived_rollback_sql");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("plan-rollback-at command writes restore plan artifact for a timestamp target", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-restore-plan-at-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_restore_at_001", "restore-plan.json");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_plan_rollback_at_001",
          changeRequestId: "chg_restore_at_001",
          databaseAssetId: "db_orders_prod",
          currentNodeId: "tln_000002",
          timestamp: "2026-07-06T09:30:00.000Z",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:00:00.000Z",
          restorePlanPath,
          nodes: [
            {
              timelineNodeId: "tln_000000",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 0,
              nodeLabel: "dosql_000000",
              parentNodeId: "",
              stateStatus: "verified",
              validFrom: "2026-07-06T08:00:00.000Z",
              schemaFingerprint: "sha256:s0",
              restoreCapability: "schema_reversible",
            },
            {
              timelineNodeId: "tln_000001",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 1,
              nodeLabel: "dosql_000001",
              parentNodeId: "tln_000000",
              stateStatus: "verified",
              validFrom: "2026-07-06T09:00:00.000Z",
              baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
              baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s1",
              restoreCapability: "schema_reversible",
            },
            {
              timelineNodeId: "tln_000002",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 2,
              nodeLabel: "dosql_000002",
              parentNodeId: "tln_000001",
              stateStatus: "verified",
              validFrom: "2026-07-06T10:00:00.000Z",
              baselineBeforeRef: "baselines/db_orders_prod/000002.before.json",
              baselineAfterRef: "baselines/db_orders_prod/000002.after.json",
              schemaFingerprint: "sha256:s2",
              restoreCapability: "data_patch_reversible",
            },
          ],
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "plan-rollback-at", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    const artifact = JSON.parse(await readFile(restorePlanPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.command, "plan-rollback-at");
    assert.equal(result.result.targetNode.timelineNodeId, "tln_000001");
    assert.equal(result.result.restorePlanPath, restorePlanPath);
    assert.equal(artifact.schema, "dosql.restore-plan.v1");
    assert.equal(artifact.targetNode.timelineNodeId, "tln_000001");
    assert.equal(artifact.plan.steps[0].timelineNodeId, "tln_000002");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("finalize-restore command writes verified restore timeline node", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-finalize-restore-"));
  try {
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_restore_002", "restore-plan.json");
    const restoreVerificationPath = join(dir, ".dosql", "changes", "chg_restore_002", "restore-verification.json");
    const restoreNodePath = join(dir, ".dosql", "changes", "chg_restore_002", "restore-node.json");
    const planInput = join(dir, "plan-input.json");
    const planOutput = join(dir, "plan-output.json");
    const verifyInput = join(dir, "verify-input.json");
    const verifyOutput = join(dir, "verify-output.json");
    const finalizeInput = join(dir, "finalize-input.json");
    const finalizeOutput = join(dir, "finalize-output.json");

    await writeFile(
      planInput,
      JSON.stringify(
        {
          operationId: "dbop_plan_rollback_003",
          changeRequestId: "chg_restore_002",
          databaseAssetId: "db_orders_prod",
          currentNodeId: "tln_000001",
          targetNodeId: "tln_000000",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:00:00.000Z",
          restorePlanPath,
          nodes: [
            {
              timelineNodeId: "tln_000000",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 0,
              nodeLabel: "dosql_000000",
              parentNodeId: "",
              stateStatus: "verified",
              validFrom: "2026-07-06T08:00:00.000Z",
              schemaFingerprint: "sha256:s0",
              restoreCapability: "schema_reversible",
            },
            {
              timelineNodeId: "tln_000001",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 1,
              nodeLabel: "dosql_000001",
              parentNodeId: "tln_000000",
              stateStatus: "verified",
              validFrom: "2026-07-06T09:00:00.000Z",
              baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
              baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s1",
              dataCheckpointRef: "",
              restoreCapability: "schema_reversible",
            },
          ],
        },
        null,
        2,
      ),
    );
    const planProc = spawnSync(process.execPath, [CLI, "plan-rollback", "--input", planInput, "--output", planOutput], {
      encoding: "utf8",
    });
    assert.equal(planProc.status, 0, planProc.stderr || (await readFile(planOutput, "utf8")));

    await writeFile(
      verifyInput,
      JSON.stringify(
        {
          operationId: "dbop_verify_restore_001",
          restorePlanPath,
          restoreVerificationPath,
          verifiedBy: "u_001",
          verifiedAt: "2026-07-06T12:30:00.000Z",
          baselineBeforeRef: "baselines/db_orders_prod/000002.before.json",
          baselineAfterRef: "baselines/db_orders_prod/000002.after.json",
          schemaFingerprint: "sha256:s0",
          evidenceRef: "changes/chg_restore_002/evidence/restore-report.json",
          checks: [
            {
              checkName: "schema_fingerprint",
              checkStatus: "passed",
              expected: "sha256:s0",
              actual: "sha256:s0"
            }
          ],
        },
        null,
        2,
      ),
    );
    const verifyProc = spawnSync(process.execPath, [CLI, "verify-restore", "--input", verifyInput, "--output", verifyOutput], {
      encoding: "utf8",
    });
    const verifyResult = JSON.parse(await readFile(verifyOutput, "utf8"));
    const restoreVerification = JSON.parse(await readFile(restoreVerificationPath, "utf8"));
    assert.equal(verifyProc.status, 0, verifyProc.stderr || JSON.stringify(verifyResult));
    assert.equal(restoreVerification.schema, "dosql.restore-verification.v1");

    await writeFile(
      finalizeInput,
      JSON.stringify(
        {
          operationId: "dbop_finalize_restore_001",
          restoreVerificationPath,
          restoreVerificationRef: "changes/chg_restore_002/restore-verification.json",
          restoreNodePath,
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "finalize-restore", "--input", finalizeInput, "--output", finalizeOutput], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(finalizeOutput, "utf8"));
    const restoreNode = JSON.parse(await readFile(restoreNodePath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.restoreNodePath, restoreNodePath);
    assert.equal(result.result.restoreNode.timelineNodeId, restoreNode.timelineNodeId);
    assert.equal(restoreNode.nodeKind, "restore");
    assert.equal(restoreNode.restoreTargetNodeId, "tln_000000");
    assert.equal(restoreNode.schemaFingerprint, "sha256:s0");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("derive-restore-baseline-records command writes baseline records for a restore node", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-restore-baselines-"));
  try {
    const restoreVerificationPath = join(dir, ".dosql", "changes", "chg_restore_002", "restore-verification.json");
    const restoreNodePath = join(dir, ".dosql", "changes", "chg_restore_002", "restore-node.json");
    const baselineRecordsPath = join(dir, ".dosql", "changes", "chg_restore_002", "restore-baseline-records.json");
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");

    await mkdir(dirname(restoreVerificationPath), { recursive: true });
    await writeFile(
      restoreVerificationPath,
      JSON.stringify(
        {
          schema: "dosql.restore-verification.v1",
          restoreVerificationId: "rver_test_002",
          restorePlanId: "rplan_test_002",
          changeRequestId: "chg_restore_002",
          databaseAssetId: "db_orders_prod",
          operationId: "dbop_verify_restore_001",
          status: "verified",
          currentNode: {
            timelineNodeId: "tln_000001",
            nodeSequence: 1,
            nodeLabel: "dosql_000001",
            schemaFingerprint: "sha256:s1",
            validFrom: "2026-07-06T09:00:00.000Z",
          },
          targetNode: {
            timelineNodeId: "tln_000000",
            nodeSequence: 0,
            nodeLabel: "dosql_000000",
            schemaFingerprint: "sha256:s0",
            validFrom: "2026-07-06T08:00:00.000Z",
          },
          baselineBeforeRef: "baselines/db_orders_prod/000002.before.json",
          baselineAfterRef: "baselines/db_orders_prod/000002.after.json",
          schemaFingerprint: "sha256:s0",
          evidenceRef: "changes/chg_restore_002/evidence/restore-report.json",
          checks: [
            {
              checkName: "schema_fingerprint",
              checkStatus: "passed",
              expected: "sha256:s0",
              actual: "sha256:s0",
            },
          ],
          verifiedBy: "u_001",
          verifiedAt: "2026-07-06T12:30:00.000Z",
          artifactFingerprint: "sha256:restore-verification",
        },
        null,
        2,
      ),
    );
    await writeFile(
      restoreNodePath,
      JSON.stringify(
        {
          timelineNodeId: "tln_000002",
          databaseAssetId: "db_orders_prod",
          nodeSequence: 2,
          nodeLabel: "dosql_000002",
          parentNodeId: "tln_000001",
          operationId: "dbop_verify_restore_001",
          nodeKind: "restore",
          stateStatus: "verified",
          validFrom: "2026-07-06T12:30:00.000Z",
          baselineBeforeRef: "baselines/db_orders_prod/000002.before.json",
          baselineAfterRef: "baselines/db_orders_prod/000002.after.json",
          schemaFingerprint: "sha256:s0",
          dataCheckpointRef: "changes/chg_restore_002/restore-verification.json",
          restoreCapability: "schema_reversible",
          restoreFromNodeId: "tln_000001",
          restoreTargetNodeId: "tln_000000",
          derivedFromSql: false,
          createdAt: "2026-07-06T12:30:00.000Z",
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_derive_restore_baselines_001",
          restoreVerificationPath,
          restoreVerificationRef: "changes/chg_restore_002/restore-verification.json",
          restoreNodePath,
          baselineRecordsPath,
          beforeDataScope: "none",
          afterDataScope: "snapshot",
          afterDataEvidenceRef: "changes/chg_restore_002/evidence/restore-report.json",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:31:00.000Z",
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "derive-restore-baseline-records", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    const artifact = JSON.parse(await readFile(baselineRecordsPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.baselineRecordsPath, baselineRecordsPath);
    assert.equal(artifact.schema, "dosql.baseline-record-set.v1");
    assert.equal(artifact.timelineNodeId, "tln_000002");
    assert.equal(artifact.records.length, 2);
    assert.equal(artifact.records[0].baselineKind, "before");
    assert.equal(artifact.records[0].schemaFingerprint, "sha256:s1");
    assert.equal(artifact.records[1].baselineKind, "after");
    assert.equal(artifact.records[1].schemaSnapshotRef, "baselines/db_orders_prod/000002.after.json");
    assert.equal(artifact.records[1].schemaFingerprint, "sha256:s0");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("verify-rollback-restore command writes restore verification bound to rollback execution", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-verify-rollback-restore-"));
  try {
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_restore_003", "restore-plan.json");
    const rollbackExecutionPath = join(dir, ".dosql", "changes", "chg_restore_003", "rollback-execution.json");
    const restoreCheckExecutionPath = join(dir, ".dosql", "changes", "chg_restore_003", "restore-check-execution.json");
    const restoreVerificationPath = join(dir, ".dosql", "changes", "chg_restore_003", "restore-verification.json");
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    await mkdir(dirname(restorePlanPath), { recursive: true });
    await writeFile(
      restorePlanPath,
      JSON.stringify(
        {
          schema: "dosql.restore-plan.v1",
          restorePlanId: "rplan_test_003",
          changeRequestId: "chg_restore_003",
          databaseAssetId: "db_orders_prod",
          status: "planned",
          currentNode: {
            timelineNodeId: "tln_000001",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 1,
            nodeLabel: "dosql_000001",
            parentNodeId: "tln_000000",
            schemaFingerprint: "sha256:s1",
          },
          targetNode: {
            timelineNodeId: "tln_000000",
            databaseAssetId: "db_orders_prod",
            nodeSequence: 0,
            nodeLabel: "dosql_000000",
            parentNodeId: "",
            schemaFingerprint: "sha256:s0",
          },
          plan: {
            status: "planned",
            currentNodeId: "tln_000001",
            targetNodeId: "tln_000000",
            steps: [
              {
                timelineNodeId: "tln_000001",
                nodeLabel: "dosql_000001",
                restoreCapability: "schema_reversible",
                method: "derived_rollback_sql",
                baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
                baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
              },
            ],
          },
          createdBy: "u_001",
          createdAt: "2026-07-06T12:00:00.000Z",
          artifactFingerprint: "sha256:restore-plan",
        },
        null,
        2,
      ),
    );
    await writeFile(
      rollbackExecutionPath,
      JSON.stringify(
        {
          schema: "dosql.rollback-execution.v1",
          status: "verified",
          databaseAssetId: "db_orders_prod",
          manifestId: "rart_test_003",
          restorePlanId: "rplan_test_003",
          changeRequestId: "chg_restore_003",
          sourceArtifactManifestFingerprint: "sha256:rollback-manifest",
          executionCount: 1,
          connectionRef: "secret://dosql/orders",
          executedBy: "u_001",
          executedAt: "2026-07-06T12:10:00.000Z",
          artifactFingerprint: "sha256:rollback-execution",
          artifactExecutions: [
            {
              timelineNodeId: "tln_000001",
              nodeLabel: "dosql_000001",
              method: "derived_rollback_sql",
              restoreCapability: "schema_reversible",
              artifactKind: "rollback_sql",
              artifactRef: "changes/chg_restore_003/scripts/rollback.sql",
              artifactFingerprint: "sha256:rollback-sql",
              transactionId: "tx_rollback_003",
              statementCount: 1,
              affectedRows: 0,
            },
          ],
        },
        null,
        2,
      ),
    );
    await writeFile(
      restoreCheckExecutionPath,
      JSON.stringify(
        {
          schema: "dosql.restore-check-execution.v1",
          status: "verified",
          restoreCheckExecutionId: "rchk_test_003",
          restorePlanId: "rplan_test_003",
          changeRequestId: "chg_restore_003",
          databaseAssetId: "db_orders_prod",
          targetTimelineNodeId: "tln_000000",
          targetSchemaFingerprint: "sha256:s0",
          checkCount: 1,
          checks: [
            {
              checkName: "schema_fingerprint",
              checkStatus: "passed",
              expected: "sha256:s0",
              actual: "sha256:s0",
              transactionId: "tx_check_003",
              statementCount: 1,
            },
          ],
          connectionRef: "secret://dosql/orders",
          executedBy: "u_001",
          executedAt: "2026-07-06T12:25:00.000Z",
          artifactFingerprint: "sha256:restore-check-execution",
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_verify_rollback_restore_001",
          restorePlanPath,
          rollbackExecutionPath,
          rollbackExecutionRef: "changes/chg_restore_003/rollback-execution.json",
          restoreCheckExecutionPath,
          restoreCheckExecutionRef: "changes/chg_restore_003/restore-check-execution.json",
          restoreVerificationPath,
          verifiedBy: "u_001",
          verifiedAt: "2026-07-06T12:30:00.000Z",
          baselineBeforeRef: "baselines/db_orders_prod/000002.before.json",
          baselineAfterRef: "baselines/db_orders_prod/000002.after.json",
          evidenceRef: "changes/chg_restore_003/evidence/post-restore-scan.json",
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(
      process.execPath,
      [CLI, "verify-rollback-restore", "--input", input, "--output", output],
      {
        encoding: "utf8",
      },
    );
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "evidence_write");
    assert.equal(result.result.restoreVerificationPath, restoreVerificationPath);
    const artifact = JSON.parse(await readFile(restoreVerificationPath, "utf8"));
    assert.equal(artifact.schema, "dosql.restore-verification.v1");
    assert.equal(artifact.sourceRollbackExecutionFingerprint, "sha256:rollback-execution");
    assert.equal(artifact.rollbackExecutionRef, "changes/chg_restore_003/rollback-execution.json");
    assert.equal(artifact.sourceRestoreCheckExecutionFingerprint, "sha256:restore-check-execution");
    assert.equal(artifact.restoreCheckExecutionRef, "changes/chg_restore_003/restore-check-execution.json");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("execute-restore-checks command writes restore check execution evidence", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-execute-restore-checks-"));
  try {
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_restore_checks", "restore-plan.json");
    const checkExecutionPath = join(dir, ".dosql", "changes", "chg_restore_checks", "restore-check-execution.json");
    const psqlPath = join(dir, "fake-psql.mjs");
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    await mkdir(dirname(restorePlanPath), { recursive: true });
    await writeFile(
      restorePlanPath,
      JSON.stringify(
        {
          schema: "dosql.restore-plan.v1",
          restorePlanId: "rplan_checks_001",
          changeRequestId: "chg_restore_checks",
          databaseAssetId: "db_orders_prod",
          status: "planned",
          currentNode: {
            timelineNodeId: "tln_000001",
            nodeSequence: 1,
            nodeLabel: "dosql_000001",
            schemaFingerprint: "sha256:s1",
          },
          targetNode: {
            timelineNodeId: "tln_000000",
            nodeSequence: 0,
            nodeLabel: "dosql_000000",
            schemaFingerprint: "sha256:s0",
          },
          plan: {
            status: "planned",
            currentNodeId: "tln_000001",
            targetNodeId: "tln_000000",
            steps: [
              {
                timelineNodeId: "tln_000001",
                nodeLabel: "dosql_000001",
                restoreCapability: "schema_reversible",
                method: "derived_rollback_sql",
              },
            ],
          },
          artifactFingerprint: "sha256:restore-plan",
        },
        null,
        2,
      ),
    );
    await writeFile(
      psqlPath,
      [
        "#!/usr/bin/env node",
        "let input = '';",
        "process.stdin.on('data', (chunk) => { input += chunk; });",
        "process.stdin.on('end', () => {",
        "  if (!process.env.PGDATABASE) process.exit(4);",
        "  if (!input.includes('dosql_schema_fingerprint')) process.exit(5);",
        "  console.log('sha256:s0');",
        "});",
        "",
      ].join("\n"),
      "utf8",
    );
    await chmod(psqlPath, 0o755);
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_execute_restore_checks_001",
          restorePlanPath,
          checkExecutionPath,
          executedBy: "u_001",
          executedAt: "2026-07-06T12:25:00.000Z",
          connectionRef: "secret://dosql/orders",
          checkAdapter: {
            type: "postgres-psql",
            psqlPath,
            connectionUriEnv: "DOSQL_TARGET_DATABASE_URL",
          },
          checks: [
            {
              checkName: "schema_fingerprint",
              expected: "sha256:s0",
              sqlText: "select dosql_schema_fingerprint();",
            },
          ],
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(
      process.execPath,
      [CLI, "execute-restore-checks", "--input", input, "--output", output],
      {
        encoding: "utf8",
        env: {
          ...process.env,
          DOSQL_TARGET_DATABASE_URL: "postgres://orders-db",
        },
      },
    );
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "evidence_write");
    assert.equal(result.result.checkExecutionPath, checkExecutionPath);
    const artifact = JSON.parse(await readFile(checkExecutionPath, "utf8"));
    assert.equal(artifact.schema, "dosql.restore-check-execution.v1");
    assert.equal(artifact.status, "verified");
    assert.equal(artifact.checks[0].checkStatus, "passed");
    assert.equal(artifact.checks[0].actual, "sha256:s0");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-restore-evidence-metadata-commit command writes restore evidence SQL and commit artifact", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-restore-evidence-commit-"));
  try {
    const rollbackExecutionPath = join(dir, ".dosql", "changes", "chg_restore_004", "rollback-execution.json");
    const restoreCheckExecutionPath = join(
      dir,
      ".dosql",
      "changes",
      "chg_restore_004",
      "restore-check-execution.json",
    );
    const restoreVerificationPath = join(
      dir,
      ".dosql",
      "changes",
      "chg_restore_004",
      "restore-verification.json",
    );
    const commitPath = join(dir, ".dosql", "changes", "chg_restore_004", "restore-evidence-commit.sql");
    const commitArtifactPath = join(
      dir,
      ".dosql",
      "changes",
      "chg_restore_004",
      "restore-evidence-commit.json",
    );
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    await mkdir(dirname(rollbackExecutionPath), { recursive: true });
    await writeFile(rollbackExecutionPath, `${JSON.stringify(sampleCliRollbackExecution(), null, 2)}\n`, "utf8");
    await writeFile(
      restoreCheckExecutionPath,
      `${JSON.stringify(sampleCliRestoreCheckExecution(), null, 2)}\n`,
      "utf8",
    );
    await writeFile(
      restoreVerificationPath,
      `${JSON.stringify(sampleCliRestoreVerification(), null, 2)}\n`,
      "utf8",
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_restore_evidence_commit_001",
          rollbackExecutionPath,
          restoreCheckExecutionPath,
          restoreVerificationPath,
          commitPath,
          commitArtifactPath,
        },
        null,
        2,
      ),
      "utf8",
    );

    const proc = spawnSync(
      process.execPath,
      [CLI, "render-restore-evidence-metadata-commit", "--input", input, "--output", output],
      {
        encoding: "utf8",
      },
    );
    const result = JSON.parse(await readFile(output, "utf8"));
    const sql = await readFile(commitPath, "utf8");
    const artifact = JSON.parse(await readFile(commitArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "plan_only");
    assert.equal(result.result.commitPath, commitPath);
    assert.equal(result.result.commitArtifactPath, commitArtifactPath);
    assert.equal(result.result.commit.schema, "dosql.restore-evidence-metadata-commit.v1");
    assert.equal(artifact.artifactFingerprint, result.result.commit.artifactFingerprint);
    assert.match(sql, /insert into dosql_rollback_executions/);
    assert.match(sql, /insert into dosql_restore_check_executions/);
    assert.match(sql, /insert into dosql_restore_verifications/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-forward-evidence-metadata-commit command writes forward execution SQL and commit artifact", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-forward-evidence-commit-"));
  try {
    const forwardExecutionPath = join(
      dir,
      ".dosql",
      "changes",
      "chg_compare_001",
      "forward-execution.json",
    );
    const commitPath = join(dir, ".dosql", "changes", "chg_compare_001", "forward-evidence-commit.sql");
    const commitArtifactPath = join(
      dir,
      ".dosql",
      "changes",
      "chg_compare_001",
      "forward-evidence-commit.json",
    );
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    await mkdir(dirname(forwardExecutionPath), { recursive: true });
    await writeFile(forwardExecutionPath, `${JSON.stringify(sampleCliForwardExecution(), null, 2)}\n`, "utf8");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_forward_evidence_commit_001",
          forwardExecutionPath,
          commitPath,
          commitArtifactPath,
        },
        null,
        2,
      ),
      "utf8",
    );

    const proc = spawnSync(
      process.execPath,
      [CLI, "render-forward-evidence-metadata-commit", "--input", input, "--output", output],
      {
        encoding: "utf8",
      },
    );
    const result = JSON.parse(await readFile(output, "utf8"));
    const sql = await readFile(commitPath, "utf8");
    const artifact = JSON.parse(await readFile(commitArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "plan_only");
    assert.equal(result.result.commitPath, commitPath);
    assert.equal(result.result.commitArtifactPath, commitArtifactPath);
    assert.equal(result.result.commit.schema, "dosql.forward-evidence-metadata-commit.v1");
    assert.equal(artifact.artifactFingerprint, result.result.commit.artifactFingerprint);
    assert.match(sql, /insert into dosql_forward_executions/);
    assert.match(sql, /'chg_compare_001'/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-restore-plan-metadata-commit command writes restore plan SQL and commit artifact", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-restore-plan-commit-"));
  try {
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_restore_005", "restore-plan.json");
    const commitPath = join(dir, ".dosql", "changes", "chg_restore_005", "restore-plan-commit.sql");
    const commitArtifactPath = join(
      dir,
      ".dosql",
      "changes",
      "chg_restore_005",
      "restore-plan-commit.json",
    );
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    await mkdir(dirname(restorePlanPath), { recursive: true });
    await writeFile(restorePlanPath, `${JSON.stringify(sampleCliRestorePlan(), null, 2)}\n`, "utf8");
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_restore_plan_commit_001",
          restorePlanPath,
          commitPath,
          commitArtifactPath,
        },
        null,
        2,
      ),
      "utf8",
    );

    const proc = spawnSync(
      process.execPath,
      [CLI, "render-restore-plan-metadata-commit", "--input", input, "--output", output],
      {
        encoding: "utf8",
      },
    );
    const result = JSON.parse(await readFile(output, "utf8"));
    const sql = await readFile(commitPath, "utf8");
    const artifact = JSON.parse(await readFile(commitArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "plan_only");
    assert.equal(result.result.commitPath, commitPath);
    assert.equal(result.result.commitArtifactPath, commitArtifactPath);
    assert.equal(result.result.commit.schema, "dosql.restore-plan-metadata-commit.v1");
    assert.equal(artifact.artifactFingerprint, result.result.commit.artifactFingerprint);
    assert.match(sql, /insert into dosql_restore_plans/);
    assert.match(sql, /'rplan_test_005'/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-timeline-artifacts-metadata-commit command writes derived artifact metadata SQL", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-timeline-artifacts-commit-"));
  try {
    const artifactManifestPath = join(dir, ".dosql", "changes", "chg_restore_006", "rollback-artifacts.json");
    const commitPath = join(dir, ".dosql", "changes", "chg_restore_006", "timeline-artifacts-commit.sql");
    const commitArtifactPath = join(
      dir,
      ".dosql",
      "changes",
      "chg_restore_006",
      "timeline-artifacts-commit.json",
    );
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    await mkdir(dirname(artifactManifestPath), { recursive: true });
    await writeFile(
      artifactManifestPath,
      `${JSON.stringify(sampleCliRollbackArtifactManifest(), null, 2)}\n`,
      "utf8",
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_render_timeline_artifacts_commit_001",
          artifactManifestPath,
          commitPath,
          commitArtifactPath,
        },
        null,
        2,
      ),
      "utf8",
    );

    const proc = spawnSync(
      process.execPath,
      [CLI, "render-timeline-artifacts-metadata-commit", "--input", input, "--output", output],
      {
        encoding: "utf8",
      },
    );
    const result = JSON.parse(await readFile(output, "utf8"));
    const sql = await readFile(commitPath, "utf8");
    const artifact = JSON.parse(await readFile(commitArtifactPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "plan_only");
    assert.equal(result.result.commit.schema, "dosql.timeline-artifacts-metadata-commit.v1");
    assert.equal(artifact.artifactFingerprint, result.result.commit.artifactFingerprint);
    assert.match(sql, /insert into dosql_timeline_artifacts/);
    assert.match(sql, /'rollback_sql'/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-rollback-artifacts command writes rollback artifact manifest", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-rollback-artifacts-"));
  try {
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_restore_artifacts_001", "restore-plan.json");
    const artifactManifestPath = join(
      dir,
      ".dosql",
      "changes",
      "chg_restore_artifacts_001",
      "rollback-artifacts.json",
    );
    const planInput = join(dir, "plan-input.json");
    const planOutput = join(dir, "plan-output.json");
    const artifactInput = join(dir, "artifact-input.json");
    const artifactOutput = join(dir, "artifact-output.json");

    await writeFile(
      planInput,
      JSON.stringify(
        {
          operationId: "dbop_plan_rollback_artifacts_001",
          changeRequestId: "chg_restore_artifacts_001",
          databaseAssetId: "db_orders_prod",
          currentNodeId: "tln_000001",
          targetNodeId: "tln_000000",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:00:00.000Z",
          restorePlanPath,
          nodes: [
            {
              timelineNodeId: "tln_000000",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 0,
              nodeLabel: "dosql_000000",
              parentNodeId: "",
              stateStatus: "verified",
              validFrom: "2026-07-06T08:00:00.000Z",
              schemaFingerprint: "sha256:s0",
              restoreCapability: "schema_reversible",
            },
            {
              timelineNodeId: "tln_000001",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 1,
              nodeLabel: "dosql_000001",
              parentNodeId: "tln_000000",
              stateStatus: "verified",
              validFrom: "2026-07-06T09:00:00.000Z",
              baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
              baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s1",
              dataCheckpointRef: "",
              restoreCapability: "schema_reversible",
            },
          ],
        },
        null,
        2,
      ),
    );
    const planProc = spawnSync(process.execPath, [CLI, "plan-rollback", "--input", planInput, "--output", planOutput], {
      encoding: "utf8",
    });
    assert.equal(planProc.status, 0, planProc.stderr || (await readFile(planOutput, "utf8")));

    await writeFile(
      artifactInput,
      JSON.stringify(
        {
          operationId: "dbop_render_rollback_artifacts_001",
          restorePlanPath,
          artifactManifestPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T12:05:00.000Z",
          artifacts: [
            {
              timelineNodeId: "tln_000001",
              artifactKind: "rollback_sql",
              artifactRef: "changes/chg_restore_artifacts_001/scripts/rollback-dosql_000001.sql",
              artifactFingerprint: "sha256:rollback-sql",
            },
          ],
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "render-rollback-artifacts", "--input", artifactInput, "--output", artifactOutput], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(artifactOutput, "utf8"));
    const manifest = JSON.parse(await readFile(artifactManifestPath, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.artifactManifestPath, artifactManifestPath);
    assert.equal(result.result.manifest.manifestId, manifest.manifestId);
    assert.equal(manifest.schema, "dosql.rollback-artifact-manifest.v1");
    assert.equal(manifest.artifacts[0].artifactKind, "rollback_sql");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("execute-rollback-artifacts command runs rollback SQL and writes execution evidence", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-execute-rollback-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const manifestPath = join(dir, ".dosql", "changes", "chg_restore", "rollback-artifacts.json");
    const scriptPath = join(dir, ".dosql", "changes", "chg_restore", "scripts", "rollback.sql");
    const executionPath = join(dir, ".dosql", "changes", "chg_restore", "rollback-execution.json");
    const psqlPath = join(dir, "fake-psql.mjs");
    const rollbackSql = "alter table orders drop column external_id;";
    const rollbackFingerprint = `sha256:${sha256ForTest(rollbackSql)}`;
    await writeFile(
      psqlPath,
      [
        "#!/usr/bin/env node",
        "let input = '';",
        "process.stdin.on('data', (chunk) => { input += chunk; });",
        "process.stdin.on('end', () => {",
        "  if (!process.env.PGDATABASE) process.exit(4);",
        "  if (!input.includes('drop column external_id')) process.exit(5);",
        "  console.log('BEGIN');",
        "  console.log('ALTER TABLE');",
        "  console.log('COMMIT');",
        "});",
        "",
      ].join("\n"),
      "utf8",
    );
    await chmod(psqlPath, 0o755);
    await mkdir(dirname(scriptPath), { recursive: true });
    await writeFile(scriptPath, rollbackSql, "utf8");
    await mkdir(dirname(manifestPath), { recursive: true });
    await writeFile(
      manifestPath,
      JSON.stringify(
        {
          schema: "dosql.rollback-artifact-manifest.v1",
          manifestId: "rart_test_001",
          restorePlanId: "rplan_test_001",
          changeRequestId: "chg_restore",
          databaseAssetId: "db_orders_prod",
          sourceRestorePlanFingerprint: "sha256:restore-plan",
          status: "ready",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:05:00.000Z",
          artifactFingerprint: "sha256:manifest",
          artifacts: [
            {
              timelineNodeId: "tln_000001",
              nodeLabel: "dosql_000001",
              method: "derived_rollback_sql",
              restoreCapability: "schema_reversible",
              artifactKind: "rollback_sql",
              artifactRef: "scripts/rollback.sql",
              artifactFingerprint: rollbackFingerprint,
            },
          ],
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_execute_rollback_001",
          artifactManifestPath: manifestPath,
          artifactBaseDir: join(dir, ".dosql", "changes", "chg_restore"),
          rollbackExecutionPath: executionPath,
          executedBy: "u_001",
          executedAt: "2026-07-06T12:10:00.000Z",
          connectionRef: "secret://dosql/orders",
          rollbackAdapter: {
            type: "postgres-psql",
            psqlPath,
            connectionUriEnv: "DOSQL_TARGET_DATABASE_URL",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "execute-rollback-artifacts",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
      env: {
        ...process.env,
        DOSQL_TARGET_DATABASE_URL: "postgres://orders-db",
      },
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "restore_write");
    assert.equal(result.result.rollbackExecutionPath, executionPath);
    const artifact = JSON.parse(await readFile(executionPath, "utf8"));
    assert.equal(artifact.schema, "dosql.rollback-execution.v1");
    assert.equal(artifact.status, "verified");
    assert.equal(artifact.artifactExecutions[0].artifactFingerprint, rollbackFingerprint);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("execute-rollback-artifacts command runs snapshot restore artifacts and writes execution evidence", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-execute-snapshot-rollback-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const manifestPath = join(dir, ".dosql", "changes", "chg_restore_snapshot", "rollback-artifacts.json");
    const artifactPath = join(dir, ".dosql", "changes", "chg_restore_snapshot", "artifacts", "restore-dosql_000002-snapshot.json");
    const executionPath = join(dir, ".dosql", "changes", "chg_restore_snapshot", "rollback-execution.json");
    const restoreCommandPath = join(dir, "fake-snapshot-restore.mjs");
    const snapshotArtifact = JSON.stringify(
      {
        schema: "dosql.snapshot-restore-artifact.v1",
        artifactKind: "snapshot_manifest",
        timelineNodeId: "tln_000002",
        restoreTargetRef: "baselines/db_orders_prod/000002.before.json",
        evidenceRef: "snapshots/db_orders_prod/000002.snapshot.json",
        evidenceFingerprint: "sha256:snapshot-source",
      },
      null,
      2,
    );
    const snapshotFingerprint = `sha256:${sha256ForTest(snapshotArtifact)}`;
    await writeFile(
      restoreCommandPath,
      [
        "#!/usr/bin/env node",
        "let input = '';",
        "process.stdin.on('data', (chunk) => { input += chunk; });",
        "process.stdin.on('end', () => {",
        "  const request = JSON.parse(input);",
        "  if (request.artifactKind !== 'snapshot_manifest') process.exit(4);",
        "  if (request.artifactPayload.schema !== 'dosql.snapshot-restore-artifact.v1') process.exit(5);",
        "  if (request.sourceArtifactFingerprint !== request.artifactFingerprint) process.exit(6);",
        "  process.stdout.write(JSON.stringify({",
        "    status: 'succeeded',",
        "    transactionId: 'snapshot_restore_001',",
        "    statementCount: 0,",
        "    affectedRows: 0",
        "  }));",
        "});",
        "",
      ].join("\n"),
      "utf8",
    );
    await chmod(restoreCommandPath, 0o755);
    await mkdir(dirname(artifactPath), { recursive: true });
    await writeFile(artifactPath, snapshotArtifact, "utf8");
    await mkdir(dirname(manifestPath), { recursive: true });
    await writeFile(
      manifestPath,
      JSON.stringify(
        {
          schema: "dosql.rollback-artifact-manifest.v1",
          manifestId: "rart_snapshot_001",
          restorePlanId: "rplan_snapshot_001",
          changeRequestId: "chg_restore_snapshot",
          databaseAssetId: "db_orders_prod",
          sourceRestorePlanFingerprint: "sha256:restore-plan",
          status: "ready",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:05:00.000Z",
          artifactFingerprint: "sha256:manifest",
          artifacts: [
            {
              timelineNodeId: "tln_000002",
              nodeLabel: "dosql_000002",
              method: "snapshot_or_pitr_restore",
              restoreCapability: "snapshot_required",
              artifactKind: "snapshot_manifest",
              artifactRef: "artifacts/restore-dosql_000002-snapshot.json",
              artifactFingerprint: snapshotFingerprint,
            },
          ],
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_execute_snapshot_rollback_001",
          artifactManifestPath: manifestPath,
          artifactBaseDir: join(dir, ".dosql", "changes", "chg_restore_snapshot"),
          rollbackExecutionPath: executionPath,
          executedBy: "u_001",
          executedAt: "2026-07-06T12:20:00.000Z",
          connectionRef: "secret://dosql/orders",
          rollbackAdapter: {
            type: "snapshot-json-command",
            command: [restoreCommandPath],
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "execute-rollback-artifacts",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "restore_write");
    assert.equal(result.result.rollbackExecutionPath, executionPath);
    const artifact = JSON.parse(await readFile(executionPath, "utf8"));
    assert.equal(artifact.schema, "dosql.rollback-execution.v1");
    assert.equal(artifact.status, "verified");
    assert.equal(artifact.artifactExecutions[0].artifactKind, "snapshot_manifest");
    assert.equal(artifact.artifactExecutions[0].transactionId, "snapshot_restore_001");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("execute-rollback-artifacts command runs restore report artifacts and writes mitigation evidence", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-execute-report-rollback-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const manifestPath = join(dir, ".dosql", "changes", "chg_restore_report", "rollback-artifacts.json");
    const artifactPath = join(dir, ".dosql", "changes", "chg_restore_report", "artifacts", "restore-dosql_000003-report.json");
    const executionPath = join(dir, ".dosql", "changes", "chg_restore_report", "rollback-execution.json");
    const restoreCommandPath = join(dir, "fake-restore-report.mjs");
    const reportArtifact = JSON.stringify(
      {
        schema: "dosql.restore-report-artifact.v1",
        artifactKind: "restore_report",
        timelineNodeId: "tln_000003",
        mitigationRef: "ops/runbooks/manual-mitigation-000003.md",
        approvalRef: "approvals/secops-restore-000003.json",
        evidenceRef: "evidence/manual-mitigation-000003.json",
      },
      null,
      2,
    );
    const reportFingerprint = `sha256:${sha256ForTest(reportArtifact)}`;
    await writeFile(
      restoreCommandPath,
      [
        "#!/usr/bin/env node",
        "let input = '';",
        "process.stdin.on('data', (chunk) => { input += chunk; });",
        "process.stdin.on('end', () => {",
        "  const request = JSON.parse(input);",
        "  if (request.artifactKind !== 'restore_report') process.exit(4);",
        "  if (request.artifactPayload.schema !== 'dosql.restore-report-artifact.v1') process.exit(5);",
        "  if (request.method !== 'human_approved_mitigation') process.exit(6);",
        "  process.stdout.write(JSON.stringify({",
        "    status: 'succeeded',",
        "    transactionId: 'manual_mitigation_001',",
        "    statementCount: 0,",
        "    affectedRows: 0",
        "  }));",
        "});",
        "",
      ].join("\n"),
      "utf8",
    );
    await chmod(restoreCommandPath, 0o755);
    await mkdir(dirname(artifactPath), { recursive: true });
    await writeFile(artifactPath, reportArtifact, "utf8");
    await mkdir(dirname(manifestPath), { recursive: true });
    await writeFile(
      manifestPath,
      JSON.stringify(
        {
          schema: "dosql.rollback-artifact-manifest.v1",
          manifestId: "rart_report_001",
          restorePlanId: "rplan_report_001",
          changeRequestId: "chg_restore_report",
          databaseAssetId: "db_orders_prod",
          sourceRestorePlanFingerprint: "sha256:restore-plan",
          status: "ready",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:05:00.000Z",
          artifactFingerprint: "sha256:manifest",
          artifacts: [
            {
              timelineNodeId: "tln_000003",
              nodeLabel: "dosql_000003",
              method: "human_approved_mitigation",
              restoreCapability: "manual_mitigation",
              artifactKind: "restore_report",
              artifactRef: "artifacts/restore-dosql_000003-report.json",
              artifactFingerprint: reportFingerprint,
            },
          ],
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_execute_report_rollback_001",
          artifactManifestPath: manifestPath,
          artifactBaseDir: join(dir, ".dosql", "changes", "chg_restore_report"),
          rollbackExecutionPath: executionPath,
          executedBy: "u_001",
          executedAt: "2026-07-06T12:30:00.000Z",
          connectionRef: "secret://dosql/orders",
          rollbackAdapter: {
            type: "snapshot-json-command",
            command: [restoreCommandPath],
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "execute-rollback-artifacts",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "restore_write");
    assert.equal(result.result.rollbackExecutionPath, executionPath);
    const artifact = JSON.parse(await readFile(executionPath, "utf8"));
    assert.equal(artifact.schema, "dosql.rollback-execution.v1");
    assert.equal(artifact.status, "verified");
    assert.equal(artifact.artifactExecutions[0].artifactKind, "restore_report");
    assert.equal(artifact.artifactExecutions[0].transactionId, "manual_mitigation_001");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("execute-forward-artifacts command runs forward SQL and writes execution evidence", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-execute-forward-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    const manifestPath = join(dir, ".dosql", "changes", "chg_forward", "forward-artifacts.json");
    const scriptPath = join(dir, ".dosql", "changes", "chg_forward", "scripts", "forward.sql");
    const executionPath = join(dir, ".dosql", "changes", "chg_forward", "forward-execution.json");
    const psqlPath = join(dir, "fake-psql.mjs");
    const forwardSql = "alter table orders add column external_id text;";
    const forwardFingerprint = `sha256:${sha256ForTest(forwardSql)}`;
    await writeFile(
      psqlPath,
      [
        "#!/usr/bin/env node",
        "let input = '';",
        "process.stdin.on('data', (chunk) => { input += chunk; });",
        "process.stdin.on('end', () => {",
        "  if (!process.env.PGDATABASE) process.exit(4);",
        "  if (!input.includes('add column external_id')) process.exit(5);",
        "  console.log('BEGIN');",
        "  console.log('ALTER TABLE');",
        "  console.log('COMMIT');",
        "});",
        "",
      ].join("\n"),
      "utf8",
    );
    await chmod(psqlPath, 0o755);
    await mkdir(dirname(scriptPath), { recursive: true });
    await writeFile(scriptPath, forwardSql, "utf8");
    await mkdir(dirname(manifestPath), { recursive: true });
    await writeFile(
      manifestPath,
      JSON.stringify(
        {
          schema: "dosql.forward-artifact-manifest.v1",
          manifestId: "fart_test_001",
          changeRequestId: "chg_forward",
          databaseAssetId: "db_orders_prod",
          timelineNodeId: "tln_000001",
          nodeLabel: "dosql_000001",
          sourceChangePlanFingerprint: "sha256:change-plan",
          status: "ready",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:05:00.000Z",
          artifactFingerprint: "sha256:manifest",
          artifacts: [
            {
              timelineNodeId: "tln_000001",
              nodeLabel: "dosql_000001",
              descriptorIndex: 0,
              artifactKind: "forward_sql",
              artifactRef: "scripts/forward.sql",
              artifactFingerprint: forwardFingerprint,
            },
          ],
        },
        null,
        2,
      ),
    );
    await writeFile(
      input,
      JSON.stringify(
        {
          operationId: "dbop_execute_forward_001",
          artifactManifestPath: manifestPath,
          artifactBaseDir: join(dir, ".dosql", "changes", "chg_forward"),
          forwardExecutionPath: executionPath,
          executedBy: "u_001",
          executedAt: "2026-07-06T12:10:00.000Z",
          connectionRef: "secret://dosql/orders",
          forwardAdapter: {
            type: "postgres-psql",
            psqlPath,
            connectionUriEnv: "DOSQL_TARGET_DATABASE_URL",
          },
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [
      CLI,
      "execute-forward-artifacts",
      "--input",
      input,
      "--output",
      output,
    ], {
      encoding: "utf8",
      env: {
        ...process.env,
        DOSQL_TARGET_DATABASE_URL: "postgres://orders-db",
      },
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.mode, "forward_write");
    assert.equal(result.result.forwardExecutionPath, executionPath);
    const artifact = JSON.parse(await readFile(executionPath, "utf8"));
    assert.equal(artifact.schema, "dosql.forward-execution.v1");
    assert.equal(artifact.status, "verified");
    assert.equal(artifact.artifactExecutions[0].artifactFingerprint, forwardFingerprint);
    assert.match(artifact.artifactExecutions[0].transactionId, /^psqlart_/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-schema-rollback-artifacts command derives rollback SQL and manifest", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-schema-rollback-"));
  try {
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_render_schema_001", "restore-plan.json");
    const scriptDir = join(dir, ".dosql", "changes", "chg_render_schema_001", "scripts");
    const artifactManifestPath = join(dir, ".dosql", "changes", "chg_render_schema_001", "rollback-artifacts.json");
    const planInput = join(dir, "plan-input.json");
    const planOutput = join(dir, "plan-output.json");
    const renderInput = join(dir, "render-input.json");
    const renderOutput = join(dir, "render-output.json");

    await writeFile(
      planInput,
      JSON.stringify(
        {
          operationId: "dbop_plan_schema_rollback_001",
          changeRequestId: "chg_render_schema_001",
          databaseAssetId: "db_orders_prod",
          currentNodeId: "tln_000001",
          targetNodeId: "tln_000000",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:00:00.000Z",
          restorePlanPath,
          nodes: [
            {
              timelineNodeId: "tln_000000",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 0,
              nodeLabel: "dosql_000000",
              parentNodeId: "",
              stateStatus: "verified",
              validFrom: "2026-07-06T08:00:00.000Z",
              schemaFingerprint: "sha256:s0",
              restoreCapability: "schema_reversible",
            },
            {
              timelineNodeId: "tln_000001",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 1,
              nodeLabel: "dosql_000001",
              parentNodeId: "tln_000000",
              stateStatus: "verified",
              validFrom: "2026-07-06T09:00:00.000Z",
              baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
              baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s1",
              dataCheckpointRef: "",
              restoreCapability: "schema_reversible",
              changeDescriptor: {
                action: "add_column",
                table: "orders",
                column: "external_id"
              }
            },
          ],
        },
        null,
        2,
      ),
    );
    const planProc = spawnSync(process.execPath, [CLI, "plan-rollback", "--input", planInput, "--output", planOutput], {
      encoding: "utf8",
    });
    assert.equal(planProc.status, 0, planProc.stderr || (await readFile(planOutput, "utf8")));

    await writeFile(
      renderInput,
      JSON.stringify(
        {
          operationId: "dbop_render_schema_rollback_001",
          restorePlanPath,
          scriptDir,
          artifactBaseRef: "changes/chg_render_schema_001/scripts",
          artifactManifestPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T12:05:00.000Z",
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "render-schema-rollback-artifacts", "--input", renderInput, "--output", renderOutput], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(renderOutput, "utf8"));
    const manifest = JSON.parse(await readFile(artifactManifestPath, "utf8"));
    const rollbackSql = await readFile(join(scriptDir, "rollback-dosql_000001.sql"), "utf8");

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.artifactManifestPath, artifactManifestPath);
    assert.equal(rollbackSql.trim(), "alter table orders drop column external_id;");
    assert.equal(manifest.schema, "dosql.rollback-artifact-manifest.v1");
    assert.equal(manifest.artifacts[0].artifactRef, "changes/chg_render_schema_001/scripts/rollback-dosql_000001.sql");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-data-rollback-artifacts command derives rollback SQL from before images", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-data-rollback-"));
  try {
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_render_data_001", "restore-plan.json");
    const scriptDir = join(dir, ".dosql", "changes", "chg_render_data_001", "scripts");
    const artifactManifestPath = join(dir, ".dosql", "changes", "chg_render_data_001", "rollback-artifacts.json");
    const planInput = join(dir, "plan-input.json");
    const planOutput = join(dir, "plan-output.json");
    const renderInput = join(dir, "render-input.json");
    const renderOutput = join(dir, "render-output.json");

    await writeFile(
      planInput,
      JSON.stringify(
        {
          operationId: "dbop_plan_data_rollback_001",
          changeRequestId: "chg_render_data_001",
          databaseAssetId: "db_orders_prod",
          currentNodeId: "tln_000001",
          targetNodeId: "tln_000000",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:00:00.000Z",
          restorePlanPath,
          nodes: [
            {
              timelineNodeId: "tln_000000",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 0,
              nodeLabel: "dosql_000000",
              parentNodeId: "",
              stateStatus: "verified",
              validFrom: "2026-07-06T08:00:00.000Z",
              schemaFingerprint: "sha256:s0",
              restoreCapability: "schema_reversible",
            },
            {
              timelineNodeId: "tln_000001",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 1,
              nodeLabel: "dosql_000001",
              parentNodeId: "tln_000000",
              stateStatus: "verified",
              validFrom: "2026-07-06T09:00:00.000Z",
              baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
              baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s0",
              dataCheckpointRef: "baselines/db_orders_prod/000001.before-image.json",
              restoreCapability: "data_patch_reversible",
            },
          ],
        },
        null,
        2,
      ),
    );
    const planProc = spawnSync(process.execPath, [CLI, "plan-rollback", "--input", planInput, "--output", planOutput], {
      encoding: "utf8",
    });
    assert.equal(planProc.status, 0, planProc.stderr || (await readFile(planOutput, "utf8")));

    await writeFile(
      renderInput,
      JSON.stringify(
        {
          operationId: "dbop_render_data_rollback_001",
          restorePlanPath,
          scriptDir,
          artifactBaseRef: "changes/chg_render_data_001/scripts",
          artifactManifestPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T12:05:00.000Z",
          beforeImages: [
            {
              timelineNodeId: "tln_000001",
              table: "orders",
              primaryKey: { id: 101 },
              before: { status: "pending", external_id: null },
            },
          ],
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "render-data-rollback-artifacts", "--input", renderInput, "--output", renderOutput], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(renderOutput, "utf8"));
    const manifest = JSON.parse(await readFile(artifactManifestPath, "utf8"));
    const rollbackSql = await readFile(join(scriptDir, "rollback-dosql_000001-data.sql"), "utf8");

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.artifactManifestPath, artifactManifestPath);
    assert.equal(rollbackSql.trim(), "update orders set status = 'pending', external_id = null where id = 101;");
    assert.equal(manifest.schema, "dosql.rollback-artifact-manifest.v1");
    assert.equal(manifest.artifacts[0].method, "inverse_data_patch");
    assert.equal(manifest.artifacts[0].artifactKind, "rollback_sql");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("render-snapshot-restore-artifacts command writes snapshot evidence artifact and manifest", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-snapshot-restore-"));
  try {
    const restorePlanPath = join(dir, ".dosql", "changes", "chg_render_snapshot_001", "restore-plan.json");
    const artifactDir = join(dir, ".dosql", "changes", "chg_render_snapshot_001", "artifacts");
    const artifactManifestPath = join(dir, ".dosql", "changes", "chg_render_snapshot_001", "rollback-artifacts.json");
    const planInput = join(dir, "plan-input.json");
    const planOutput = join(dir, "plan-output.json");
    const renderInput = join(dir, "render-input.json");
    const renderOutput = join(dir, "render-output.json");

    await writeFile(
      planInput,
      JSON.stringify(
        {
          operationId: "dbop_plan_snapshot_rollback_001",
          changeRequestId: "chg_render_snapshot_001",
          databaseAssetId: "db_orders_prod",
          currentNodeId: "tln_000001",
          targetNodeId: "tln_000000",
          createdBy: "u_001",
          createdAt: "2026-07-06T12:00:00.000Z",
          restorePlanPath,
          nodes: [
            {
              timelineNodeId: "tln_000000",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 0,
              nodeLabel: "dosql_000000",
              parentNodeId: "",
              stateStatus: "verified",
              validFrom: "2026-07-06T08:00:00.000Z",
              schemaFingerprint: "sha256:s0",
              restoreCapability: "schema_reversible",
            },
            {
              timelineNodeId: "tln_000001",
              databaseAssetId: "db_orders_prod",
              nodeSequence: 1,
              nodeLabel: "dosql_000001",
              parentNodeId: "tln_000000",
              stateStatus: "verified",
              validFrom: "2026-07-06T09:00:00.000Z",
              baselineBeforeRef: "baselines/db_orders_prod/000001.before.json",
              baselineAfterRef: "baselines/db_orders_prod/000001.after.json",
              schemaFingerprint: "sha256:s1",
              dataCheckpointRef: "snapshots/db_orders_prod/000001.snapshot.json",
              restoreCapability: "snapshot_required",
            },
          ],
        },
        null,
        2,
      ),
    );
    const planProc = spawnSync(process.execPath, [CLI, "plan-rollback", "--input", planInput, "--output", planOutput], {
      encoding: "utf8",
    });
    assert.equal(planProc.status, 0, planProc.stderr || (await readFile(planOutput, "utf8")));

    await writeFile(
      renderInput,
      JSON.stringify(
        {
          operationId: "dbop_render_snapshot_rollback_001",
          restorePlanPath,
          artifactDir,
          artifactBaseRef: "changes/chg_render_snapshot_001/artifacts",
          artifactManifestPath,
          createdBy: "u_001",
          createdAt: "2026-07-06T12:05:00.000Z",
          restoreEvidence: [
            {
              timelineNodeId: "tln_000001",
              artifactKind: "snapshot_manifest",
              evidenceRef: "snapshots/db_orders_prod/000001.snapshot.json",
              evidenceFingerprint: "sha256:snapshot-source",
              restoreTargetRef: "baselines/db_orders_prod/000001.before.json",
              capturedAt: "2026-07-06T08:59:00.000Z",
            },
          ],
        },
        null,
        2,
      ),
    );

    const proc = spawnSync(process.execPath, [CLI, "render-snapshot-restore-artifacts", "--input", renderInput, "--output", renderOutput], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(renderOutput, "utf8"));
    const manifest = JSON.parse(await readFile(artifactManifestPath, "utf8"));
    const snapshotArtifact = JSON.parse(await readFile(join(artifactDir, "restore-dosql_000001-snapshot.json"), "utf8"));

    assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
    assert.equal(result.status, "succeeded");
    assert.equal(result.result.artifactManifestPath, artifactManifestPath);
    assert.equal(snapshotArtifact.schema, "dosql.snapshot-restore-artifact.v1");
    assert.equal(snapshotArtifact.restoreTargetRef, "baselines/db_orders_prod/000001.before.json");
    assert.equal(manifest.schema, "dosql.rollback-artifact-manifest.v1");
    assert.equal(manifest.artifacts[0].method, "snapshot_or_pitr_restore");
    assert.equal(manifest.artifacts[0].artifactKind, "snapshot_manifest");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("unknown command fails with machine-readable error", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-agent-test-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    await writeFile(input, JSON.stringify({ operationId: "dbop_unknown_001" }));
    const proc = spawnSync(process.execPath, [CLI, "unknown", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.notEqual(proc.status, 0);
    assert.equal(result.status, "failed");
    assert.match(result.error.message, /Unsupported command/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("command without operationId fails before running", async () => {
  const dir = await mkdtemp(join(tmpdir(), "dosql-agent-test-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    await writeFile(input, JSON.stringify({ engine: "mysql", statement: "select 1" }));
    const proc = spawnSync(process.execPath, [CLI, "classify", "--input", input, "--output", output], {
      encoding: "utf8",
    });
    const result = JSON.parse(await readFile(output, "utf8"));

    assert.notEqual(proc.status, 0);
    assert.equal(result.status, "failed");
    assert.match(result.error.message, /operationId is required/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

async function runCli(command, payload, options = {}) {
  const { proc, result } = await runCliResult(command, payload, options);
  assert.equal(proc.status, 0, proc.stderr || JSON.stringify(result));
  return result;
}

async function responseQueryFixture(t) {
  const dir = realpathSync(await mkdtemp(join(tmpdir(), "dosql-response-reservation-")));
  t.after(() => rm(dir, { recursive: true, force: true }));
  const outputPath = join(dir, "response.json");
  const dispatchPath = join(dir, "dispatches.jsonl");
  const runnerPath = join(dir, "fake-runner.mjs");
  await writeFile(runnerPath, `#!/usr/bin/env node
import { appendFileSync, writeFileSync } from "node:fs";
let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => input += chunk);
process.stdin.on("end", () => {
  const request = JSON.parse(input);
  appendFileSync(process.env.DOSQL_TEST_DISPATCH_PATH, JSON.stringify(request) + "\\n");
  const mode = process.env.DOSQL_TEST_RUNNER_MODE;
  if (mode.startsWith("publish-response")) {
    writeFileSync(process.env.DOSQL_TEST_RESPONSE_PATH, "competing response", { flag: "wx" });
  }
  if (mode === "publish-response-audit-failure") {
    writeFileSync(process.env.DOSQL_TEST_EVIDENCE_PATH, "competing evidence", { flag: "wx" });
  }
  if (mode === "publish-response-driver-failure") {
    console.error("synthetic driver failure");
    process.exitCode = 1;
  } else {
    console.log("1");
  }
});
`, "utf8");
  await chmod(runnerPath, 0o755);
  const payload = (operationId) => ({
    ...databaseOperationPayload({
      dir, operationId, engine: "postgres",
      operation: { statement: "select 1" },
      adapter: {
        type: "doops-go", engine: "postgres", runnerPath,
        namespace: "synthetic", database: "test", secretName: "synthetic-binding",
      },
    }),
    journalPath: join(dir, ".dosql", `${operationId}.journal.jsonl`),
    evidencePath: join(dir, ".dosql", `${operationId}.evidence.json`),
  });
  return {
    dir, outputPath, dispatchPath, payload,
    async run(operationId, mode = "success") {
      const request = payload(operationId);
      const inputPath = join(dir, `${operationId}.input.json`);
      await writeFile(inputPath, JSON.stringify(request));
      return new Promise((resolve, reject) => {
        execFile(process.execPath, [
          CLI, "query", "--input", inputPath, "--output", outputPath,
        ], {
          cwd: dir,
          timeout: 10000,
          env: {
            ...process.env,
            DOSQL_TEST_DISPATCH_PATH: dispatchPath,
            DOSQL_TEST_RESPONSE_PATH: outputPath,
            DOSQL_TEST_EVIDENCE_PATH: request.evidencePath,
            DOSQL_TEST_RUNNER_MODE: mode,
          },
        }, (error, stdout, stderr) => {
          if (error && typeof error.code !== "number") return reject(error);
          resolve({ status: error?.code ?? 0, stdout, stderr });
        });
      });
    },
  };
}

async function runCliResult(command, payload, options = {}) {
  const dir = await mkdtemp(join(tmpdir(), "dosql-agent-test-"));
  try {
    const input = join(dir, "input.json");
    const output = join(dir, "output.json");
    await writeFile(input, JSON.stringify(payload, null, 2));
    const proc = spawnSync(process.execPath, [CLI, command, "--input", input, "--output", output], {
      encoding: "utf8",
      env: { ...process.env, ...(options.env ?? {}) },
      cwd: options.cwd,
    });
    const result = JSON.parse(await readFile(output, "utf8"));
    return { proc, result };
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

function databaseOperationPayload({ dir, engine, operationId, operation, adapter }) {
  const canonicalDir = realpathSync(dir);
  return {
    operationId,
    projectId: "proj_cli",
    databaseAssetId: `db_cli_${engine}`,
    environmentId: "test",
    environment: "test",
    engine,
    databaseName: "test",
    actor: { type: "user", id: "u_cli" },
    ...operation,
    requestedAt: "2026-07-17T02:10:00.000Z",
    startedAt: "2026-07-17T02:10:01.000Z",
    finishedAt: "2026-07-17T02:10:02.000Z",
    verifiedAt: "2026-07-17T02:10:03.000Z",
    journalPath: join(canonicalDir, ".dosql", "journal.jsonl"),
    evidencePath: join(canonicalDir, ".dosql", "evidence.json"),
    adapter,
  };
}

function adapterForEngine(engine, clientPath) {
  if (engine === "postgres") {
    return {
      type: "postgres-psql",
      psqlPath: clientPath,
      connectionUriEnv: "DOSQL_TEST_PG_URI",
    };
  }
  if (engine === "mysql") {
    return {
      type: "mysql-cli",
      mysqlPath: clientPath,
      hostEnv: "DOSQL_TEST_MYSQL_HOST",
      userEnv: "DOSQL_TEST_MYSQL_USER",
      passwordEnv: "DOSQL_TEST_MYSQL_PASSWORD",
    };
  }
  return {
    type: "mongodb-mongosh",
    mongoshPath: clientPath,
    connectionUriEnv: "DOSQL_TEST_MONGO_URI",
  };
}

function databaseTestEnv(dir) {
  return {
    HOME: join(dir, "home"),
    DOSQL_TEST_PG_URI: "postgresql://test:test@localhost/test",
    DOSQL_TEST_MYSQL_HOST: "localhost",
    DOSQL_TEST_MYSQL_USER: "root",
    DOSQL_TEST_MYSQL_PASSWORD: "test-password",
    DOSQL_TEST_MONGO_URI: "mongodb://test:test@localhost/test",
  };
}

async function createFakeDatabaseClient(dir) {
  const path = join(dir, "fake-db-client.mjs");
  await writeFile(
    path,
    `#!/usr/bin/env node
let input = "";
process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => input += chunk);
process.stdin.on("end", () => {
  const text = (input + " " + process.argv.slice(2).join(" ")).toUpperCase();
  if (text.includes("UPDATE")) console.log("UPDATE 1");
  else if (text.includes("INSERT")) console.log("INSERT 0 1");
  else if (text.includes("DELETE")) console.log("DELETE 1");
  else console.log("1");
});
`,
    "utf8",
  );
  await chmod(path, 0o755);
  return path;
}

async function journalStatuses(path) {
  return (await readFile(path, "utf8"))
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line).status);
}

function sha256ForTest(value) {
  return createHash("sha256").update(String(value)).digest("hex");
}

function sampleCliForwardExecution() {
  return {
    schema: "dosql.forward-execution.v1",
    status: "verified",
    manifestId: "fart_test_001",
    changeRequestId: "chg_compare_001",
    databaseAssetId: "db_orders_prod",
    timelineNodeId: "tln_000001",
    nodeLabel: "dosql_000001",
    sourceChangePlanFingerprint: "sha256:compare-change-plan",
    sourceArtifactManifestFingerprint: "sha256:forward-artifact-manifest",
    executionCount: 1,
    connectionRef: "secret://dosql/orders",
    executedBy: "u_001",
    executedAt: "2026-07-06T12:10:00.000Z",
    artifactFingerprint: "sha256:forward-execution",
    artifactExecutions: [
      {
        timelineNodeId: "tln_000001",
        nodeLabel: "dosql_000001",
        descriptorIndex: 0,
        artifactKind: "forward_sql",
        artifactRef: "changes/chg_compare_001/scripts/forward-dosql_000001-001-orders-external_id.sql",
        artifactFingerprint: "sha256:forward-sql",
        transactionId: "tx_forward_001",
        statementCount: 1,
        affectedRows: 0,
      },
    ],
  };
}

function sampleCliRollbackExecution() {
  return {
    schema: "dosql.rollback-execution.v1",
    status: "verified",
    databaseAssetId: "db_orders_prod",
    manifestId: "rart_test_004",
    restorePlanId: "rplan_test_004",
    changeRequestId: "chg_restore_004",
    sourceArtifactManifestFingerprint: "sha256:rollback-manifest",
    executionCount: 1,
    connectionRef: "secret://dosql/orders",
    executedBy: "u_001",
    executedAt: "2026-07-06T12:10:00.000Z",
    artifactFingerprint: "sha256:rollback-execution",
    artifactExecutions: [
      {
        timelineNodeId: "tln_000001",
        nodeLabel: "dosql_000001",
        method: "derived_rollback_sql",
        restoreCapability: "schema_reversible",
        artifactKind: "rollback_sql",
        artifactRef: "changes/chg_restore_004/scripts/rollback.sql",
        artifactFingerprint: "sha256:rollback-sql",
        transactionId: "tx_rollback_004",
        statementCount: 1,
        affectedRows: 0,
      },
    ],
  };
}

function sampleCliRestorePlan() {
  return {
    schema: "dosql.restore-plan.v1",
    status: "planned",
    restorePlanId: "rplan_test_005",
    changeRequestId: "chg_restore_005",
    databaseAssetId: "db_orders_prod",
    currentNode: {
      timelineNodeId: "tln_000001",
      nodeSequence: 1,
      nodeLabel: "dosql_000001",
      schemaFingerprint: "sha256:s1",
    },
    targetNode: {
      timelineNodeId: "tln_000000",
      nodeSequence: 0,
      nodeLabel: "dosql_000000",
      schemaFingerprint: "sha256:s0",
    },
    plan: {
      status: "planned",
      currentNodeId: "tln_000001",
      targetNodeId: "tln_000000",
      steps: [
        {
          timelineNodeId: "tln_000001",
          nodeLabel: "dosql_000001",
          restoreCapability: "schema_reversible",
          method: "derived_rollback_sql",
        },
      ],
    },
    createdBy: "u_001",
    createdAt: "2026-07-06T12:00:00.000Z",
    artifactFingerprint: "sha256:restore-plan",
  };
}

function sampleCliRollbackArtifactManifest() {
  return {
    schema: "dosql.rollback-artifact-manifest.v1",
    status: "ready",
    manifestId: "rart_test_006",
    restorePlanId: "rplan_test_006",
    changeRequestId: "chg_restore_006",
    databaseAssetId: "db_orders_prod",
    sourceRestorePlanFingerprint: "sha256:restore-plan",
    createdBy: "u_001",
    createdAt: "2026-07-06T12:05:00.000Z",
    artifactFingerprint: "sha256:rollback-artifact-manifest",
    artifacts: [
      {
        databaseAssetId: "db_orders_prod",
        timelineNodeId: "tln_000001",
        nodeLabel: "dosql_000001",
        method: "derived_rollback_sql",
        restoreCapability: "schema_reversible",
        artifactKind: "rollback_sql",
        artifactRef: "changes/chg_restore_006/scripts/rollback-dosql_000001.sql",
        artifactFingerprint: "sha256:rollback-sql",
      },
    ],
  };
}

function sampleCliRestoreCheckExecution() {
  return {
    schema: "dosql.restore-check-execution.v1",
    status: "verified",
    restoreCheckExecutionId: "rchk_test_004",
    restorePlanId: "rplan_test_004",
    changeRequestId: "chg_restore_004",
    databaseAssetId: "db_orders_prod",
    targetTimelineNodeId: "tln_000000",
    targetSchemaFingerprint: "sha256:s0",
    checkCount: 1,
    checks: [
      {
        checkName: "schema_fingerprint",
        checkStatus: "passed",
        expected: "sha256:s0",
        actual: "sha256:s0",
        transactionId: "tx_check_004",
        statementCount: 1,
      },
    ],
    connectionRef: "secret://dosql/orders",
    executedBy: "u_001",
    executedAt: "2026-07-06T12:25:00.000Z",
    artifactFingerprint: "sha256:restore-check-execution",
  };
}

function sampleCliRestoreVerification() {
  return {
    schema: "dosql.restore-verification.v1",
    status: "verified",
    restoreVerificationId: "rver_test_004",
    restorePlanId: "rplan_test_004",
    changeRequestId: "chg_restore_004",
    databaseAssetId: "db_orders_prod",
    operationId: "dbop_restore_004",
    currentNode: {
      timelineNodeId: "tln_000001",
      nodeLabel: "dosql_000001",
      schemaFingerprint: "sha256:s1",
    },
    targetNode: {
      timelineNodeId: "tln_000000",
      nodeLabel: "dosql_000000",
      schemaFingerprint: "sha256:s0",
    },
    baselineBeforeRef: "baselines/db_orders_prod/000002.before.json",
    baselineAfterRef: "baselines/db_orders_prod/000002.after.json",
    schemaFingerprint: "sha256:s0",
    evidenceRef: "changes/chg_restore_004/evidence/post-restore-scan.json",
    checks: sampleCliRestoreCheckExecution().checks,
    rollbackExecutionRef: "changes/chg_restore_004/rollback-execution.json",
    sourceRollbackExecutionFingerprint: "sha256:rollback-execution",
    restoreCheckExecutionRef: "changes/chg_restore_004/restore-check-execution.json",
    sourceRestoreCheckExecutionFingerprint: "sha256:restore-check-execution",
    verifiedBy: "u_001",
    verifiedAt: "2026-07-06T12:30:00.000Z",
    artifactFingerprint: "sha256:restore-verification",
  };
}

function sampleComparisonSnapshots() {
  return [
    {
      snapshotFingerprint: "sha256:dev-structure",
      capturedAt: "2026-07-06T09:00:00.000Z",
      assets: [
        {
          databaseAssetId: "db_orders_dev",
          engine: "mysql",
          version: { current: 0, label: "dosql_000000" },
          objects: [
            {
              kind: "table",
              name: "orders",
              columns: [
                { kind: "column", name: "id", dataType: "bigint", nullable: false, key: "PRI" },
                { kind: "column", name: "external_id", dataType: "varchar(64)", nullable: true, key: "" },
                { kind: "column", name: "status", dataType: "varchar(32)", nullable: false, key: "" },
              ],
            },
          ],
        },
      ],
    },
    {
      snapshotFingerprint: "sha256:prod-structure",
      capturedAt: "2026-07-06T09:05:00.000Z",
      assets: [
        {
          databaseAssetId: "db_orders_prod",
          engine: "mysql",
          version: { current: 0, label: "dosql_000000" },
          objects: [
            {
              kind: "table",
              name: "orders",
              columns: [
                { kind: "column", name: "id", dataType: "bigint", nullable: false, key: "PRI" },
                { kind: "column", name: "status", dataType: "varchar(16)", nullable: true, key: "" },
              ],
            },
          ],
        },
      ],
    },
  ];
}

function sampleStructureSnapshot({ capturedAt, structureFingerprint }) {
  return {
    snapshotFingerprint: `snap_${capturedAt}`,
    capturedAt,
    assets: [
      {
        databaseAssetId: "db_orders_prod",
        engine: "mysql",
        version: { current: 1, label: "dosql_000001" },
        structureFingerprint,
        objects: [
          {
            kind: "table",
            name: "orders",
            columns: [
              { kind: "column", name: "id", dataType: "bigint", nullable: false, key: "PRI" },
            ],
          },
        ],
      },
    ],
  };
}

function sampleTimepointQueryResultArtifact() {
  return {
    schema: "dosql.timepoint-state-query-result.v1",
    status: "resolved",
    databaseAssetId: "db_orders_prod",
    timestamp: "2026-07-06T09:30:00.000Z",
    sourceQueryFingerprint: "sha256:query",
    connectionRef: "secret://dosql/metadata",
    executionResult: {
      status: "succeeded",
      transactionId: "tx_timepoint_001",
      statementCount: 1,
    },
    timepointState: {
      databaseAssetId: "db_orders_prod",
      timestamp: "2026-07-06T09:30:00.000Z",
      timelineNode: {
        timeline_node_id: "tln_000001",
        database_asset_id: "db_orders_prod",
        node_sequence: 1,
        node_label: "dosql_000001",
        node_kind: "change",
        state_status: "verified",
        valid_from: "2026-07-06T09:00:00.000Z",
        baseline_after_ref: "baselines/db_orders_prod/000001.after.json",
        schema_fingerprint: "sha256:s1",
        restore_capability: "schema_reversible",
      },
      baselineRecords: [
        {
          baseline_kind: "after",
          captured_at: "2026-07-06T09:04:00.000Z",
          schema_snapshot_ref: "baselines/db_orders_prod/000001.after.json",
          schema_fingerprint: "sha256:s1",
          data_scope: "none",
          data_evidence_ref: "",
          artifact_fingerprint: "sha256:after-baseline",
        },
      ],
      timelineArtifacts: [],
      executionEvidence: {
        forwardExecutions: [
          {
            forward_execution_id: "fex_test_001",
            timeline_node_id: "tln_000001",
            artifact_fingerprint: "sha256:forward-execution",
          },
        ],
        restoreVerifications: [],
      },
    },
    queriedBy: "u_001",
    queriedAt: "2026-07-06T10:00:00.000Z",
    artifactFingerprint: "sha256:timepoint-query-result",
  };
}

function sampleCompareChangePlan() {
  return {
    schema: "dosql.compare-change-plan.v1",
    status: "draft",
    changeRequestId: "chg_compare_001",
    sourceComparisonId: "dcmp_test_001",
    sourceComparisonFingerprint: "sha256:comparison",
    referenceDatabaseAssetId: "db_orders_dev",
    targetPlans: [
      {
        databaseAssetId: "db_orders_prod",
        changeDescriptors: [
          {
            action: "add_column",
            table: "orders",
            column: "external_id",
            dataType: "varchar(64)",
            nullable: true,
            key: "",
          },
        ],
        manualDifferences: [],
      },
    ],
    createdBy: "u_001",
    createdAt: "2026-07-06T08:55:00.000Z",
    artifactFingerprint: "sha256:compare-change-plan",
  };
}

function sampleProbe() {
  return {
    target: {
      name: "gw-oilan-node",
      cluster: "doops-oilan",
      instance: "oilan-node",
    },
    namespace: "test",
    services: [
      {
        name: "mysql",
        engine: "mysql",
        host: "mysql.test.svc.cluster.local",
        port: 3306,
        version: "8.0.45",
        database: "test",
      },
      {
        name: "mongodb",
        engine: "mongodb",
        host: "mongodb.test.svc.cluster.local",
        port: 27017,
        version: "mongosh-present",
        database: "test",
      },
    ],
    mysql: {
      tables: ["users"],
      columns: {
        users: [
          { name: "id", type: "varchar(64)", nullable: false, key: "PRI" },
          { name: "username", type: "varchar(255)", nullable: true },
        ],
      },
    },
    mongodb: {
      databases: ["admin", "config", "local", "test"],
      collections: ["ai_billing_record", "code_copilot_threads"],
    },
  };
}
