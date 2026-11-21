import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import {
  existsSync,
  readFileSync,
  realpathSync,
  statSync,
} from "node:fs";
import { chmod, lstat, mkdir } from "node:fs/promises";
import { basename, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  classifyDatabaseOperation,
  createDatabaseOperationRecord,
} from "./change-policy.mjs";
import { appendJournalEvent } from "./execution-journal.mjs";
import { reserveOperationId, writeAuditArtifact } from "./audit-artifacts.mjs";

const PSQL_ARGS = ["--no-psqlrc", "--set", "ON_ERROR_STOP=1", "--no-align", "--tuples-only"];
const MYSQL_ARGS = ["--batch", "--raw", "--skip-column-names"];
const TRUSTED_SKILL_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../..");

export async function executeControlledDatabaseOperation(input) {
  try {
    return await executeQuery(input);
  } catch (error) {
    error.executionOutcome ??= "not_started";
    error.retryable = false;
    throw error;
  }
}

async function executeQuery(input) {
  input = normalizeDatabaseRequest(input);
  const mode = requireMode(input.mode);
  const classification = classifyDatabaseOperation(input);
  const operation = createDatabaseOperationRecord(input);
  const artifactRoot = await prepareArtifactRoot(input);
  const journalPath = await prepareManagedOutputPath(
    input.journalPath,
    artifactRoot,
    "journalPath",
  );
  const evidencePath = await prepareManagedOutputPath(
    input.evidencePath,
    artifactRoot,
    "evidencePath",
  );
  input = { ...input, journalPath, evidencePath, artifactRoot };
  const startedAt = input.startedAt ?? new Date().toISOString();
  const baseEvent = createBaseEvent({ input, operation, classification });

  try {
    await reserveQueryArtifacts(input);
    await appendJournalEvent({
      journalPath,
      rejectDuplicateOperation: true,
      event: {
        ...baseEvent,
        eventType: "database.operation.planned",
        status: "planned",
        createdAt: operation.requestedAt,
      },
    });
  } catch (error) {
    throw auditPersistenceError(error, "not_started");
  }

  let adapter;
  try {
    enforceOperationPolicy({ input, mode, classification });
    adapter = requireAdapter(input.adapter);
    if (adapter.trustedClient !== true) {
      throw new Error("query requires a trusted database client executable");
    }
  } catch (error) {
    error.executionOutcome = "not_started";
    const failedAt = input.finishedAt ?? new Date().toISOString();
    await recordFailedOperation({
      input,
      operation,
      classification,
      journalPath,
      evidencePath,
      baseEvent,
      error,
      startedAt,
      failedAt,
    });
    throw error;
  }

  try {
    await appendJournalEvent({
      journalPath,
      event: { ...baseEvent, eventType: "database.operation.running", status: "running", createdAt: startedAt },
    });
  } catch (error) {
    throw auditPersistenceError(error, "not_started");
  }

  let executionResult;
  try {
    executionResult = normalizeExecutionResult(
      await adapter.execute({
        engine: operation.engine,
        operationId: operation.operationId,
        statement: operation.normalizedStatement,
        databaseName: input.databaseName,
        phase: "operation",
        readOnly: true,
        requireReadOnlyCredential: operation.engine === "mongodb",
        mongoOperation: input.mongoOperation,
      }),
    );
  } catch (error) {
    error.executionOutcome = "unknown";
    const failedAt = input.finishedAt ?? new Date().toISOString();
    await recordFailedOperation({
      input,
      operation,
      classification,
      journalPath,
      evidencePath,
      baseEvent,
      error,
      startedAt,
      failedAt,
    });
    throw error;
  }

  const finishedAt = input.finishedAt ?? new Date().toISOString();
  try {
    await appendJournalEvent({
      journalPath,
      event: {
        ...baseEvent,
        eventType: "database.operation.succeeded",
        status: "succeeded",
        executionOutcome: "succeeded",
        affectedRows: executionResult.affectedRows,
        evidenceRef: evidencePath,
        createdAt: finishedAt,
      },
    });
    const verifiedAt = input.verifiedAt ?? new Date().toISOString();
    const evidence = createEvidence({
      input,
      operation,
      classification,
      status: "verified",
      executionOutcome: "succeeded",
      executionResult,
      verificationResult: executionResult,
      startedAt,
      finishedAt,
      verifiedAt,
    });
    await writeAuditArtifact(evidencePath, persistableEvidence(evidence));
    await appendJournalEvent({
      journalPath,
      event: {
        ...baseEvent,
        eventType: "database.operation.verified",
        status: "verified",
        executionOutcome: "succeeded",
        evidenceRef: evidencePath,
        createdAt: verifiedAt,
      },
    });
    return evidence;
  } catch (error) {
    // Audit failure cannot undo a successful query or justify replacing evidence.
    throw auditPersistenceError(error, "succeeded");
  }
}

async function reserveQueryArtifacts({ artifactRoot, operationId, journalPath, evidencePath }) {
  try {
    await lstat(evidencePath);
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
    const claim = {
      schema: "dosql.database-operation-claim.v1",
      operationId: requireText(operationId, "operationId"),
      journalPath,
      evidencePath,
    };
    await reserveOperationId({ artifactRoot, operationId, claim });
    try {
      await writeAuditArtifact(`${evidencePath}.claim`, claim);
    } catch (error) {
      if (error.code === "EEXIST") throw evidenceAlreadyExists();
      throw error;
    }
    return;
  }
  throw evidenceAlreadyExists();
}

function evidenceAlreadyExists() {
  return Object.assign(new Error(
    "evidence path already exists or is claimed; inspect the existing operation before submitting any new SQL",
  ), { code: "DOSQL_EVIDENCE_ALREADY_EXISTS", executionOutcome: "unknown" });
}

function auditPersistenceError(error, executionOutcome) {
  if (error.code === "DOSQL_OPERATION_ALREADY_EXISTS" ||
      error.code === "DOSQL_EVIDENCE_ALREADY_EXISTS") return error;
  return Object.assign(new Error(
    `DoSql audit persistence failed; database execution outcome is ${executionOutcome}: ${error.message}`,
    { cause: error },
  ), {
    code: "DOSQL_AUDIT_PERSISTENCE_FAILED",
    auditErrorCode: error.code ?? "UNKNOWN",
    executionOutcome,
    ...(error.lineNumber === undefined ? {} : { lineNumber: error.lineNumber }),
  });
}

async function recordFailedOperation({
  input,
  operation,
  classification,
  journalPath,
  evidencePath,
  baseEvent,
  error,
  startedAt,
  failedAt,
}) {
  const evidence = createEvidence({
    input,
    operation,
    classification,
    status: "failed",
    executionOutcome: error.executionOutcome,
    executionResult: { status: "failed", errorMessage: error.message },
    startedAt,
    finishedAt: failedAt,
  });
  try {
    await appendJournalEvent({
      journalPath,
      event: {
        ...baseEvent,
        eventType: "database.operation.failed",
        status: "failed",
        executionOutcome: error.executionOutcome,
        errorMessage: "database operation failed",
        errorFingerprint: `sha256:${sha256(error.message)}`,
        evidenceRef: evidencePath,
        createdAt: failedAt,
      },
    });
    await writeAuditArtifact(evidencePath, persistableEvidence(evidence));
  } catch (auditError) {
    throw auditPersistenceError(auditError, error.executionOutcome);
  }
}

export function createDatabaseCliAdapter(input = {}) {
  const type = requireText(input.type, "adapter.type");
  const baseEnv = input.env ?? process.env;
  const spawnFile = input.spawnFile ?? runProcess;
  if (type === "doops-go") {
    const engine = requireText(input.engine, "adapter.engine");
    const runnerPath = requireText(input.runnerPath ?? "/app/doops-agent", "adapter.runnerPath");
    const namespace = requireText(input.namespace, "adapter.namespace");
    const database = requireText(input.database, "adapter.database");
    const secretName = requireText(input.secretName, "adapter.secretName");
    return {
      trustedClient: true,
      targetFingerprint() {
        return connectionFingerprint({
          engine, transport: "doops-agent-go", namespace, database, secretName,
          clientFingerprint: input.clientFingerprint ?? "go-database-runner-v1",
        });
      },
      execute(request) {
        const runnerRequest = {
          operation_id: request.operationId,
          phase: request.phase,
          engine,
          namespace,
          database_name: database,
          secret_name: secretName,
          client_image: input.clientImage,
          uri_key: input.uriKey,
          url_key: input.urlKey,
          user_key: input.userKey,
          password_key: input.passwordKey,
          read_only: request.readOnly === true,
          statement: request.statement,
          mongo_operation: request.mongoOperation,
          mongo_script: engine === "mongodb" ? mongoEvalScript() : undefined,
        };
        return executeProcess({
          command: runnerPath,
          args: ["database-runner"],
          stdin: `${JSON.stringify(runnerRequest)}\n`,
          env: runtimeEnvironment(baseEnv),
          spawnFile,
        });
      },
    };
  }
  if (type === "postgres-psql") {
    const client = resolveClientDescriptor({
      configuredPath: input.psqlPath,
      defaultName: "psql",
      allowedWrapperNames: ["docker-db-client.sh"],
      spawnFile,
    });
    const connectionUriEnv = requireText(input.connectionUriEnv, "adapter.connectionUriEnv");
    return processAdapter({
      command: client.command,
      args: PSQL_ARGS,
      env: () => ({
        ...baseEnv,
        ...postgresEnvFromUri(requireText(baseEnv[connectionUriEnv], connectionUriEnv)),
      }),
      sqlDialect: "postgres",
      targetFingerprint() {
        const pg = postgresEnvFromUri(
          requireText(baseEnv[connectionUriEnv], connectionUriEnv),
        );
        return connectionFingerprint({
          engine: "postgres",
          host: pg.PGHOST,
          port: pg.PGPORT,
          user: pg.PGUSER,
          database: pg.PGDATABASE,
          clientFingerprint: client.fingerprint,
        });
      },
      trustedClient: client.trusted,
      spawnFile,
    });
  }
  if (type === "mysql-cli") {
    const client = resolveClientDescriptor({
      configuredPath: input.mysqlPath,
      defaultName: "mysql",
      allowedWrapperNames: ["docker-db-client.sh"],
      spawnFile,
    });
    const hostEnv = requireText(input.hostEnv, "adapter.hostEnv");
    const userEnv = requireText(input.userEnv, "adapter.userEnv");
    const passwordEnv = requireText(input.passwordEnv, "adapter.passwordEnv");
    return {
      trustedClient: client.trusted,
      targetFingerprint(request) {
        return connectionFingerprint({
          engine: "mysql",
          host: requireText(baseEnv[hostEnv], hostEnv),
          port: String(input.port ?? 3306),
          user: requireText(baseEnv[userEnv], userEnv),
          database: requireText(request.databaseName, "databaseName"),
          clientFingerprint: client.fingerprint,
        });
      },
      execute(request) {
        return executeProcess({
          command: client.command,
          args: [
            ...MYSQL_ARGS,
            "--host",
            requireText(baseEnv[hostEnv], hostEnv),
            "--port",
            String(input.port ?? 3306),
            "--user",
            requireText(baseEnv[userEnv], userEnv),
            requireText(request.databaseName, "databaseName"),
          ],
          stdin: formatSqlInput(request, "mysql"),
          env: {
            ...baseEnv,
            MYSQL_PWD: requireText(baseEnv[passwordEnv], passwordEnv),
          },
          spawnFile,
        });
      },
    };
  }
  if (type === "mongodb-mongosh") {
    const client = resolveClientDescriptor({
      configuredPath: input.mongoshPath,
      defaultName: "mongosh",
      allowedWrapperNames: ["docker-db-client.sh"],
      spawnFile,
    });
    const connectionUriEnv = requireText(input.connectionUriEnv, "adapter.connectionUriEnv");
    return {
      trustedClient: client.trusted,
      targetFingerprint() {
        return connectionFingerprint({
          ...databaseTargetFromUri(
            requireText(baseEnv[connectionUriEnv], connectionUriEnv),
            "mongodb",
          ),
          clientFingerprint: client.fingerprint,
        });
      },
      async execute(request) {
        const mongoOperation = normalizeMongoOperation(request.mongoOperation);
        const connectionUri = requireText(baseEnv[connectionUriEnv], connectionUriEnv);
        return executeProcess({
          command: client.command,
          args: [
            "--nodb",
            "--quiet",
            "--eval",
            mongoEvalScript(),
          ],
          env: {
            ...runtimeEnvironment(baseEnv),
            DOSQL_MONGODB_URI: connectionUri,
            DOSQL_MONGODB_OPERATION: JSON.stringify(mongoOperation),
            DOSQL_MONGODB_REQUIRE_READ_ONLY_ROLE: request.requireReadOnlyCredential ? "1" : "0",
          },
          spawnFile,
        });
      },
    };
  }
  throw new Error(`Unsupported adapter.type: ${type}`);
}

function processAdapter({
  command,
  args,
  env,
  sqlDialect,
  targetFingerprint,
  trustedClient,
  spawnFile,
}) {
  return {
    targetFingerprint,
    trustedClient,
    execute(request) {
      return executeProcess({
        command,
        args,
        stdin: formatSqlInput(request, sqlDialect),
        env: env(),
        spawnFile,
      });
    },
  };
}

async function executeProcess({ command, args, stdin = "", env, spawnFile }) {
  const result = await spawnFile({ command, args, stdin, env });
  const status = Number(result.status);
  const stdout = String(result.stdout ?? "");
  const stderr = String(result.stderr ?? "");
  if (status !== 0) {
    throw new Error(`${command} exited with status ${status}: ${stderr || stdout}`.trim());
  }
  return {
    status: "succeeded",
    stdout,
    stderr,
    affectedRows: parseAffectedRows(stdout),
    transactionId: `dbtx_${sha256(`${command}\u001f${args.join("\u001f")}\u001f${stdout}`).slice(0, 16)}`,
  };
}

function enforceOperationPolicy({ input, mode, classification }) {
  if (classification.engine === "mongodb" && !input.mongoOperation) {
    throw new Error("mongoOperation is required; raw MongoDB JavaScript is not executable");
  }
  ensureSingleSqlStatement(input.engine, input.statement, "statement");
  if (mode === "query" && classification.engine === "mysql" &&
      /\bINTO\b|\/\*(?:!|M!)/i.test(input.statement)) {
    throw new Error("query does not allow MySQL INTO clauses or executable comments");
  }
  if (mode === "query" && classification.operationKind !== "read") {
    throw new Error("query only accepts read-only database operations");
  }
}

function createBaseEvent({ input, operation, classification }) {
  return {
    operationId: requireText(input.operationId, "operationId"),
    changeRequestId: input.changeRequestId ? String(input.changeRequestId).trim() : "",
    projectId: operation.projectId,
    environmentId: operation.environmentId,
    databaseAssetId: operation.databaseAssetId,
    engine: operation.engine,
    actor: operation.actor,
    operationKind: classification.operationKind,
    statementFingerprint: operation.statementFingerprint,
  };
}

function createEvidence({
  input,
  operation,
  classification,
  status,
  executionOutcome,
  executionResult,
  verificationResult,
  startedAt,
  finishedAt,
  verifiedAt,
}) {
  return {
    schema: "dosql.database-operation-evidence.v1",
    operationId: requireText(input.operationId, "operationId"),
    status,
    executionOutcome,
    mode: requireMode(input.mode),
    projectId: operation.projectId,
    databaseAssetId: operation.databaseAssetId,
    environmentId: operation.environmentId,
    engine: operation.engine,
    operationKind: classification.operationKind,
    statementFingerprint: operation.statementFingerprint,
    changeRequestId: input.changeRequestId ? String(input.changeRequestId).trim() : "",
    executionResult,
    verificationResult,
    requestedAt: operation.requestedAt,
    startedAt,
    finishedAt,
    verifiedAt,
    evidenceFingerprint: `sha256:${sha256(
      JSON.stringify({ operationId: input.operationId, status, executionResult, verificationResult }),
    )}`,
  };
}

function normalizeExecutionResult(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("adapter execution result must be an object");
  }
  if (value.status !== "succeeded") throw new Error("adapter execution result must succeed");
  return {
    status: "succeeded",
    transactionId: requireText(value.transactionId, "result.transactionId"),
    affectedRows: Number(value.affectedRows ?? 0),
    stdout: String(value.stdout ?? ""),
    stderr: String(value.stderr ?? ""),
  };
}

function persistableEvidence(evidence) {
  return {
    ...evidence,
    executionResult: summarizePersistedResult(evidence.executionResult),
    verificationResult: evidence.verificationResult
      ? summarizePersistedResult(evidence.verificationResult)
      : null,
  };
}

function summarizePersistedResult(result) {
  if (!result || typeof result !== "object") return result;
  const stdout = String(result.stdout ?? "");
  const stderr = String(result.stderr ?? "");
  const errorMessage = String(result.errorMessage ?? "");
  return {
    status: result.status,
    transactionId: result.transactionId ?? "",
    affectedRows: Number(result.affectedRows ?? 0),
    stdoutBytes: Buffer.byteLength(stdout),
    stdoutFingerprint: stdout ? `sha256:${sha256(stdout)}` : "",
    stderrBytes: Buffer.byteLength(stderr),
    stderrFingerprint: stderr ? `sha256:${sha256(stderr)}` : "",
    errorFingerprint: errorMessage ? `sha256:${sha256(errorMessage)}` : "",
  };
}

function requireAdapter(value) {
  if (
    !value ||
    typeof value.execute !== "function" ||
    typeof value.targetFingerprint !== "function"
  ) {
    throw new Error("adapter.execute and adapter.targetFingerprint are required");
  }
  return value;
}

function requireMode(value) {
  if (value !== "query") throw new Error("mode must be query; changes run through the Agent's direct --sql path");
  return value;
}

function parseAffectedRows(output) {
  const matches = [...String(output).matchAll(/(?:INSERT|UPDATE|DELETE)\s+(?:\d+\s+)?(\d+)/gi)];
  return matches.reduce((sum, match) => sum + Number(match[1]), 0);
}

function ensureSingleSqlStatement(engine, statement, fieldName) {
  if (engine === "mongodb") return;
  const normalized = requireText(statement, fieldName).replace(/;+$/g, "");
  if (normalized.includes(";")) {
    throw new Error(`${fieldName} must contain exactly one SQL statement`);
  }
}

async function prepareArtifactRoot(input) {
  const requestedRoot =
    typeof input.artifactRootProvider === "function"
      ? await input.artifactRootProvider()
      : resolve(process.cwd(), ".dosql");
  const root = resolve(requireText(requestedRoot, "artifact root"));
  await mkdir(root, { recursive: true, mode: 0o700 });
  const metadata = await lstat(root);
  if (!metadata.isDirectory() || metadata.isSymbolicLink()) {
    throw new Error("DoSql artifact root must be a real directory");
  }
  await chmod(root, 0o700);
  return realpathSync(root);
}

async function prepareManagedOutputPath(value, root, fieldName) {
  const path = managedPath(value, root, fieldName);
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const parent = realpathSync(dirname(path));
  if (!isPathInside(parent, root)) {
    throw new Error(`${fieldName} must stay inside the DoSql artifact root`);
  }
  return path;
}

function managedPath(value, root, fieldName) {
  const path = resolve(requireText(value, fieldName));
  if (!isPathInside(path, root)) {
    throw new Error(`${fieldName} must stay inside the DoSql artifact root`);
  }
  return path;
}

function isPathInside(path, root) {
  return path === root || path.startsWith(`${root}/`);
}

function normalizeDatabaseRequest(input) {
  if (input.engine !== "mongodb" || !input.mongoOperation) return input;
  const mongoOperation = normalizeMongoOperation(input.mongoOperation);
  return {
    ...input,
    mongoOperation,
    statement: renderMongoOperation(mongoOperation),
    ...(input.verificationMongoOperation
      ? {
          verificationMongoOperation: normalizeMongoOperation(input.verificationMongoOperation),
          verificationStatement: renderMongoOperation(input.verificationMongoOperation),
        }
      : {}),
  };
}

function normalizeMongoOperation(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("mongoOperation must be an object");
  }
  const method = requireText(value.method, "mongoOperation.method");
  const supportedMethods = new Set([
    "aggregate",
    "countDocuments",
    "createCollection",
    "deleteMany",
    "deleteOne",
    "distinct",
    "drop",
    "estimatedDocumentCount",
    "find",
    "findOne",
    "getCollectionNames",
    "insertMany",
    "insertOne",
    "replaceOne",
    "updateMany",
    "updateOne",
  ]);
  if (!supportedMethods.has(method)) {
    throw new Error(`Unsupported mongoOperation.method: ${method}`);
  }
  const args = value.args ?? [];
  if (!Array.isArray(args)) throw new Error("mongoOperation.args must be an array");
  const collection =
    method === "getCollectionNames" || method === "createCollection"
      ? ""
      : requireText(value.collection, "mongoOperation.collection");
  if (method === "createCollection" && args.length !== 1) {
    throw new Error("createCollection requires exactly one argument");
  }
  return { method, collection, args: structuredClone(args) };
}

function renderMongoOperation(value) {
  const operation = normalizeMongoOperation(value);
  const args = operation.args.map((entry) => JSON.stringify(entry)).join(", ");
  if (operation.method === "getCollectionNames") return "db.getCollectionNames()";
  if (operation.method === "createCollection") return `db.createCollection(${args})`;
  return `db.getCollection(${JSON.stringify(operation.collection)}).${operation.method}(${args})`;
}

function mongoEvalScript() {
  return [
    "const db = connect(process.env.DOSQL_MONGODB_URI);",
    "if (process.env.DOSQL_MONGODB_DATABASE && db.getName() !== process.env.DOSQL_MONGODB_DATABASE) throw new Error('MongoDB database does not match the authorized asset');",
    "const spec = JSON.parse(process.env.DOSQL_MONGODB_OPERATION);",
    "if (process.env.DOSQL_MONGODB_REQUIRE_READ_ONLY_ROLE === '1') {",
    "  const roles = db.runCommand({connectionStatus: 1}).authInfo.authenticatedUserRoles;",
    "  const allowed = new Set(['read', 'readAnyDatabase', 'clusterMonitor']);",
    "  if (!roles.length || roles.some((entry) => !allowed.has(entry.role))) throw new Error('MongoDB query requires a read-only database role');",
    "}",
    "const collection = spec.collection ? db.getCollection(spec.collection) : null;",
    "let result;",
    "switch (spec.method) {",
    "  case 'getCollectionNames': result = db.getCollectionNames(); break;",
    "  case 'createCollection': result = db.createCollection(...spec.args); break;",
    "  default: result = collection[spec.method](...spec.args);",
    "}",
    "EJSON.stringify(result && typeof result.toArray === 'function' ? result.toArray() : result);",
  ].join(" ");
}

function runtimeEnvironment(baseEnv) {
  return Object.fromEntries(
    Object.entries(baseEnv).filter(
      ([name]) =>
        ["HOME", "KUBECONFIG", "LANG", "LC_ALL", "PATH", "TMPDIR"].includes(name) ||
        name.startsWith("DOSQL_"),
    ),
  );
}

function stableJson(value) {
  if (Array.isArray(value)) return `[${value.map(stableJson).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${stableJson(value[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

function formatSqlInput(request, dialect) {
  const statement = requireText(request.statement, "request.statement");
  if (!request.readOnly) return `${statement};\n`;
  if (dialect === "postgres") {
    return `BEGIN READ ONLY;\n${statement};\nCOMMIT;\n`;
  }
  if (dialect === "mysql") {
    return `START TRANSACTION READ ONLY;\n${statement};\nROLLBACK;\n`;
  }
  throw new Error(`Unsupported SQL dialect: ${dialect}`);
}

function postgresEnvFromUri(connectionUri) {
  let parsed;
  try {
    parsed = new URL(connectionUri);
  } catch (error) {
    throw new Error(`PostgreSQL connection URI is invalid: ${error.message}`);
  }
  if (parsed.protocol !== "postgres:" && parsed.protocol !== "postgresql:") {
    throw new Error(`Unsupported PostgreSQL connection URI protocol: ${parsed.protocol}`);
  }
  return {
    PGHOST: requireText(parsed.hostname, "connectionUri.hostname"),
    PGPORT: parsed.port || "5432",
    PGUSER: requireText(decodeURIComponent(parsed.username), "connectionUri.username"),
    PGPASSWORD: decodeURIComponent(parsed.password),
    PGDATABASE: requireText(decodeURIComponent(parsed.pathname.replace(/^\/+/, "")), "connectionUri.database"),
  };
}

function databaseTargetFromUri(connectionUri, engine) {
  let parsed;
  try {
    parsed = new URL(connectionUri);
  } catch (error) {
    throw new Error(`${engine} connection URI is invalid: ${error.message}`);
  }
  return {
    engine,
    host: requireText(parsed.hostname, "connectionUri.hostname"),
    port: parsed.port || (engine === "mongodb" ? "27017" : "5432"),
    user: requireText(decodeURIComponent(parsed.username), "connectionUri.username"),
    database: requireText(
      decodeURIComponent(parsed.pathname.replace(/^\/+/, "")),
      "connectionUri.database",
    ),
  };
}

function connectionFingerprint(target) {
  return `sha256:${sha256(stableJson(target))}`;
}

function resolveClientDescriptor({
  configuredPath,
  defaultName,
  allowedWrapperNames,
  spawnFile,
}) {
  if (spawnFile !== runProcess) {
    return {
      command: configuredPath ?? defaultName,
      fingerprint: "sha256:injected-test-client",
      trusted: true,
    };
  }
  const candidates = configuredPath
    ? [configuredPath]
    : [
        `/usr/bin/${defaultName}`,
        `/usr/local/bin/${defaultName}`,
        `/opt/homebrew/bin/${defaultName}`,
      ];
  const existing = candidates.find((path) => existsSync(path));
  if (!existing) {
    throw new Error(
      `${defaultName} client is not installed at an approved absolute path`,
    );
  }
  const approvedPath = resolve(existing);
  const command = realpathSync(existing);
  const metadata = statSync(command);
  if (!metadata.isFile()) throw new Error(`${defaultName} client must be a file`);
  const fileName = basename(command);
  const trustedSystemClient =
    basename(approvedPath) === defaultName &&
    [
      `/usr/bin/${defaultName}`,
      `/usr/local/bin/${defaultName}`,
      `/opt/homebrew/bin/${defaultName}`,
    ].includes(approvedPath);
  const trustedSkillWrapper =
    command.startsWith(`${TRUSTED_SKILL_ROOT}/`) &&
    allowedWrapperNames.includes(fileName);
  return {
    command,
    fingerprint: `sha256:${sha256Bytes(readFileSync(command))}`,
    trusted: trustedSystemClient || trustedSkillWrapper,
  };
}

async function runProcess({ command, args, stdin, env }) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { env, stdio: ["pipe", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    child.stdout.setEncoding("utf8");
    child.stderr.setEncoding("utf8");
    child.stdout.on("data", (chunk) => (stdout += chunk));
    child.stderr.on("data", (chunk) => (stderr += chunk));
    child.on("error", reject);
    child.on("close", (status) => resolve({ status, stdout, stderr }));
    child.stdin.end(stdin);
  });
}

function requireText(value, fieldName) {
  if (value === undefined || value === null || String(value).trim() === "") {
    throw new Error(`${fieldName} is required`);
  }
  return String(value).trim();
}

function sha256(value) {
  return createHash("sha256").update(String(value)).digest("hex");
}

function sha256Bytes(value) {
  return createHash("sha256").update(value).digest("hex");
}
