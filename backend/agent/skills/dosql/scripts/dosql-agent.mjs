#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { lstat, mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, resolve, sep } from "node:path";

import { writeAuditArtifact } from "../Server/lib/audit-artifacts.mjs";
import { classifyDatabaseOperation } from "../Server/lib/change-policy.mjs";
import {
  compareDatabaseStructures,
  deriveChangePlanFromComparison,
} from "../Server/lib/database-compare.mjs";
import {
  buildDatabaseDiscovery,
  resolveDatabaseReference,
} from "../Server/lib/database-discovery.mjs";
import { runDatabaseScan } from "../Server/lib/database-scanner.mjs";
import { discoverAndPersistResources } from "../Server/lib/resource-discovery.mjs";
import {
  createDatabaseCliAdapter,
  executeControlledDatabaseOperation,
} from "../Server/lib/database-operation-executor.mjs";
import {
  appendJournalEvent,
  createSqlExecutionEvents,
  inspectJournal,
  migrateLegacyJournal,
  readJournalEvents,
  replayEnvironmentTimeline,
} from "../Server/lib/execution-journal.mjs";
import {
  buildDatabaseAssetInventory,
  createMaintenanceChecklist,
  createStructureSnapshot,
} from "../Server/lib/structure-management.mjs";
import {
  checkTimelineHeadDrift,
  createBaselineRecordSet,
  createBaselineRecordSetFromStructureSnapshots,
  createForwardArtifactManifest,
  createInitialBaselineFromStructureSnapshot,
  createRestoreBaselineRecordSetFromVerificationArtifact,
  createTimepointStateManifest,
  createDriftImportTimelineNode,
  createRollbackArtifactManifest,
  createRestoreTimelineNodeFromVerificationArtifact,
  createRestorePlanArtifact,
  createRestoreVerificationArtifact,
  createRestoreVerificationFromRollbackExecution,
  planRollbackToNode,
  projectDatabaseVersionHead,
  renderDataRollbackArtifacts,
  renderForwardChangeArtifacts,
  renderSchemaRollbackArtifacts,
  renderSnapshotRestoreArtifacts,
  resolveTimelineNodeAt,
} from "../Server/lib/timeline-baseline.mjs";
import {
  renderBaselineRecordsMetadataCommit,
  renderChangeMetadataCommit,
  renderForwardEvidenceMetadataCommit,
  renderRestoreEvidenceMetadataCommit,
  renderRestorePlanMetadataCommit,
  renderTimelineArtifactsMetadataCommit,
  renderTimelineNodeMetadataCommit,
  renderTimepointStateQuery,
} from "../Server/lib/timeline-metadata-store.mjs";
import { createPostgresPsqlMetadataAdapter } from "../Server/lib/timeline-metadata-adapters.mjs";
import {
  createMetadataCommitExecutionArtifact,
  executeMetadataCommit,
  executeTimepointStateQuery,
} from "../Server/lib/timeline-metadata-executor.mjs";
import { executeForwardArtifactManifest } from "../Server/lib/forward-executor.mjs";
import { executeRollbackArtifactManifest } from "../Server/lib/rollback-executor.mjs";
import { executeRestoreChecks } from "../Server/lib/restore-check-executor.mjs";

const READ_ONLY_COMMANDS = new Set([
  "check-head-drift",
  "classify",
  "discover-databases",
  "discover-resources",
  "plan-rollback",
  "plan-rollback-at",
  "query",
  "register-database",
  "resolve-database",
  "resolve-timeline-at",
  "execute-timepoint-state-query",
  "replay-timeline",
  "inspect-journal",
  "scan",
]);

const EVIDENCE_WRITE_COMMANDS = new Set([
  "migrate-journal",
  "execute-restore-checks",
  "record-execution",
  "record-metadata-commit-execution",
  "verify-rollback-restore",
]);

const METADATA_WRITE_COMMANDS = new Set([
  "execute-metadata-commit",
]);

const RESTORE_WRITE_COMMANDS = new Set([
  "execute-rollback-artifacts",
]);

const FORWARD_WRITE_COMMANDS = new Set([
  "execute-forward-artifacts",
]);

// Command catalogue shown by --help. Keep it in sync with the dispatch switch;
// the test suite asserts the two never drift apart.
const COMMAND_GROUPS = {
  "discover and read": [
    "classify",
    "query",
    "scan",
    "discover-databases",
    "discover-resources",
    "register-database",
    "resolve-database",
  ],
  change: [
    "compare-databases",
    "derive-change-plan-from-comparison",
    "render-forward-change-artifacts",
    "execute-forward-artifacts",
    "render-forward-evidence-metadata-commit",
    "render-change-metadata-commit",
    "render-metadata-commit",
    "execute-metadata-commit",
    "record-metadata-commit-execution",
    "record-execution",
  ],
  journal: ["inspect-journal", "migrate-journal"],
  timeline: [
    "replay-timeline",
    "resolve-timeline-at",
    "project-current-head",
    "check-head-drift",
    "import-drift",
    "create-baseline-records",
    "create-timepoint-state-manifest",
    "derive-baseline-records-from-structure-snapshots",
    "derive-initial-baseline-from-structure-snapshot",
    "render-baseline-records-commit",
    "render-timeline-artifacts-metadata-commit",
    "render-timepoint-state-query",
    "execute-timepoint-state-query",
  ],
  "rollback and restore": [
    "plan-rollback",
    "plan-rollback-at",
    "render-rollback-artifacts",
    "render-data-rollback-artifacts",
    "render-schema-rollback-artifacts",
    "execute-rollback-artifacts",
    "verify-rollback-restore",
    "derive-restore-baseline-records",
    "render-restore-evidence-metadata-commit",
    "render-restore-plan-metadata-commit",
    "render-snapshot-restore-artifacts",
    "execute-restore-checks",
    "verify-restore",
    "finalize-restore",
  ],
};

function helpText() {
  const lines = [
    "dosql-agent <command> --input <json> --output <path>",
    "",
    "Every command takes a JSON input file and writes a JSON response to --output.",
    "",
    "Commands:",
  ];
  for (const [group, names] of Object.entries(COMMAND_GROUPS)) {
    lines.push(`  ${group}:`);
    for (const name of names) lines.push(`    ${name}`);
  }
  return lines.join("\n");
}

async function main(argv) {
  const requested = argv[2];
  if (!requested || requested === "help" || requested === "--help" || requested === "-h") {
    process.stdout.write(helpText() + "\n");
    return;
  }
  const { command, inputPath, outputPath } = parseArgs(argv);
  let response;
  let operationId;
  let input;
  let inputError;
  try {
    input = JSON.parse(await readFile(inputPath, "utf8"));
    operationId = requireOperationId(input);
  } catch (error) {
    inputError = error;
  }

  try {
    await reserveResponsePath(outputPath, { command, operationId });
  } catch (error) {
    reportFailure(failureResponse(command, operationId, error, "not_started"));
    return;
  }

  try {
    if (inputError) throw inputError;
    response = {
      operationId,
      status: "succeeded",
      command,
      mode: commandMode(command),
      result: await runCommand(command, input),
    };
  } catch (error) {
    response = failureResponse(
      command, operationId, error, inputError || command === "query" ? "not_started" : "unknown",
    );
  }

  try {
    await writeAuditArtifact(outputPath, response);
  } catch (error) {
    const executionOutcome = response.error?.executionOutcome ??
      response.result?.executionOutcome ?? (response.status === "succeeded" ? "succeeded" : "unknown");
    const failure = failureResponse(command, operationId, Object.assign(error, {
      auditErrorCode: error.code ?? "UNKNOWN",
      code: "DOSQL_RESPONSE_PERSISTENCE_FAILED",
    }), executionOutcome);
    if (response.error) failure.commandError = response.error;
    reportFailure(failure);
    return;
  }
  if (response.status !== "succeeded") {
    process.exitCode = 1;
  }
}

async function reserveResponsePath(outputPath, claim) {
  try {
    await assertResponseAbsent(outputPath);
    await writeAuditArtifact(`${outputPath}.claim`, {
      schema: "dosql.response-claim.v1",
      ...claim,
    });
    await assertResponseAbsent(outputPath);
  } catch (error) {
    throw Object.assign(error, {
      auditErrorCode: error.code ?? "UNKNOWN",
      code: error.code === "EEXIST" ? "DOSQL_RESPONSE_ALREADY_EXISTS" : "DOSQL_RESPONSE_RESERVATION_FAILED",
    });
  }
}

async function assertResponseAbsent(outputPath) {
  try {
    await lstat(outputPath);
  } catch (error) {
    if (error.code === "ENOENT") return;
    throw error;
  }
  throw Object.assign(new Error("Response output already exists; inspect it before starting another operation"), {
    code: "EEXIST",
  });
}

function failureResponse(command, operationId, error, executionOutcome) {
  return {
    operationId,
    status: "failed",
    command,
    error: {
      name: error.name,
      message: error.message,
      code: error.code,
      lineNumber: error.lineNumber,
      auditErrorCode: error.auditErrorCode,
      executionOutcome: error.executionOutcome ?? executionOutcome,
      retryable: false,
    },
  };
}

function reportFailure(response) {
  console.error(JSON.stringify(response));
  process.exitCode = 1;
}

async function runCommand(command, input) {
  switch (command) {
    case "check-head-drift":
      return checkHeadDrift(input);
    case "classify":
      return classifyDatabaseOperation(input);
    case "compare-databases":
      return compareDatabases(input);
    case "create-baseline-records":
      return createBaselineRecords(input);
    case "create-timepoint-state-manifest":
      return createTimepointStateManifestCommand(input);
    case "derive-change-plan-from-comparison":
      return deriveChangePlanFromComparisonCommand(input);
    case "derive-baseline-records-from-structure-snapshots":
      return deriveBaselineRecordsFromStructureSnapshots(input);
    case "derive-restore-baseline-records":
      return deriveRestoreBaselineRecords(input);
    case "derive-initial-baseline-from-structure-snapshot":
      return deriveInitialBaselineFromStructureSnapshot(input);
    case "discover-databases":
      return buildDatabaseDiscovery(input);
    case "discover-resources":
      return discoverAndPersistResources(input);
    case "register-database":
      return registerDatabase(input);
    case "import-drift":
      return importDrift(input);
    case "record-execution":
      return recordExecution(input);
    case "inspect-journal":
      return inspectJournal({ journalPath: input.journalPath });
    case "migrate-journal":
      return migrateJournal(input);
    case "record-metadata-commit-execution":
      return recordMetadataCommitExecution(input);
    case "execute-metadata-commit":
      return executeMetadataCommitCommand(input);
    case "execute-timepoint-state-query":
      return executeTimepointStateQueryCommand(input);
    case "execute-forward-artifacts":
      return executeForwardArtifacts(input);
    case "execute-rollback-artifacts":
      return executeRollbackArtifacts(input);
    case "execute-restore-checks":
      return executeRestoreChecksCommand(input);
    case "finalize-restore":
      return finalizeRestore(input);
    case "render-rollback-artifacts":
      return renderRollbackArtifacts(input);
    case "render-metadata-commit":
      return renderMetadataCommit(input);
    case "render-baseline-records-commit":
      return renderBaselineRecordsCommit(input);
    case "render-change-metadata-commit":
      return renderChangeMetadataCommitCommand(input);
    case "render-timeline-artifacts-metadata-commit":
      return renderTimelineArtifactsMetadataCommitCommand(input);
    case "render-timepoint-state-query":
      return renderTimepointStateQueryCommand(input);
    case "render-restore-plan-metadata-commit":
      return renderRestorePlanMetadataCommitCommand(input);
    case "render-restore-evidence-metadata-commit":
      return renderRestoreEvidenceMetadataCommitCommand(input);
    case "render-forward-evidence-metadata-commit":
      return renderForwardEvidenceMetadataCommitCommand(input);
    case "render-data-rollback-artifacts":
      return renderDataRollbackArtifactsCommand(input);
    case "render-forward-change-artifacts":
      return renderForwardChangeArtifactsCommand(input);
    case "render-schema-rollback-artifacts":
      return renderSchemaRollbackArtifactsCommand(input);
    case "render-snapshot-restore-artifacts":
      return renderSnapshotRestoreArtifactsCommand(input);
    case "plan-rollback":
      return planRollback(input);
    case "plan-rollback-at":
      return planRollbackAt(input);
    case "project-current-head":
      return projectCurrentHead(input);
    case "query":
      return executeDatabaseCommand(input, "query");
    case "replay-timeline":
      return replayTimeline(input);
    case "resolve-database":
      return resolveDatabaseReference(input);
    case "resolve-timeline-at":
      return resolveTimelineAt(input);
    case "scan":
      return runDatabaseScan(input);
    case "verify-restore":
      return verifyRestore(input);
    case "verify-rollback-restore":
      return verifyRollbackRestore(input);
    default:
      throw new Error(`Unsupported command: ${command}`);
  }
}

function registerDatabase(input) {
  const inventory = buildDatabaseAssetInventory({
    projectId: input.projectId,
    environmentId: input.environmentId,
    probe: input.probe,
    naming: input.naming,
  });
  const structureSnapshot = createStructureSnapshot({
    inventory,
    probe: input.probe,
    capturedAt: input.capturedAt,
  });
  const maintenanceChecklist = createMaintenanceChecklist({ inventory });

  return {
    inventory,
    structureSnapshot,
    maintenanceChecklist,
  };
}

function checkHeadDrift(input) {
  return checkTimelineHeadDrift({
    currentNode: input.currentNode,
    liveSchemaFingerprint: input.liveSchemaFingerprint,
    checkedAt: input.checkedAt,
    evidenceRef: input.evidenceRef,
  });
}

async function compareDatabases(input) {
  const comparison = compareDatabaseStructures({
    structureSnapshots: input.structureSnapshots,
    referenceDatabaseAssetId: input.referenceDatabaseAssetId,
    targetDatabaseAssetIds: input.targetDatabaseAssetIds,
    comparedAt: input.comparedAt,
    comparedBy: input.comparedBy,
  });
  if (input.comparisonArtifactPath) {
    await mkdir(dirname(input.comparisonArtifactPath), { recursive: true });
    await writeFile(input.comparisonArtifactPath, `${JSON.stringify(comparison, null, 2)}\n`, "utf8");
  }
  return {
    comparison,
    comparisonArtifactPath: input.comparisonArtifactPath ?? "",
  };
}

async function deriveChangePlanFromComparisonCommand(input) {
  const comparison = input.comparison ?? JSON.parse(await readFile(input.comparisonArtifactPath, "utf8"));
  const changePlan = deriveChangePlanFromComparison({
    comparison,
    changeRequestId: input.changeRequestId,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.changePlanPath) {
    await mkdir(dirname(input.changePlanPath), { recursive: true });
    await writeFile(input.changePlanPath, `${JSON.stringify(changePlan, null, 2)}\n`, "utf8");
  }
  return {
    changePlan,
    changePlanPath: input.changePlanPath ?? "",
  };
}

async function importDrift(input) {
  const driftNode = createDriftImportTimelineNode({
    currentNode: input.currentNode,
    operationId: input.operationId,
    validFrom: input.validFrom,
    baselineAfterRef: input.baselineAfterRef,
    schemaFingerprint: input.schemaFingerprint,
    evidenceRef: input.evidenceRef,
    restoreCapability: input.restoreCapability,
    createdAt: input.createdAt,
  });
  if (input.driftNodePath) {
    await mkdir(dirname(input.driftNodePath), { recursive: true });
    await writeFile(input.driftNodePath, `${JSON.stringify(driftNode, null, 2)}\n`, "utf8");
  }
  return {
    driftNode,
    driftNodePath: input.driftNodePath ?? "",
  };
}

async function projectCurrentHead(input) {
  const projection = projectDatabaseVersionHead({
    currentVersion: input.currentVersion,
    nextNode: input.nextNode,
    updatedBy: input.updatedBy,
    updatedAt: input.updatedAt,
  });
  if (input.projectionPath) {
    await mkdir(dirname(input.projectionPath), { recursive: true });
    await writeFile(input.projectionPath, `${JSON.stringify(projection, null, 2)}\n`, "utf8");
  }
  return {
    projection,
    projectionPath: input.projectionPath ?? "",
  };
}

async function recordExecution(input) {
  const events = createSqlExecutionEvents(input.execution);
  for (const event of events) {
    await appendJournalEvent({
      journalPath: input.journalPath,
      event,
    });
  }
  return {
    journalPath: input.journalPath,
    appendedEvents: events.length,
    eventTypes: events.map((event) => event.eventType),
    timeline: replayEnvironmentTimeline(events),
  };
}

async function migrateJournal(input) {
  return migrateLegacyJournal({
    sourceJournalPath: input.sourceJournalPath,
    journalPath: input.journalPath,
    operationId: input.operationId,
    actor: input.actor,
    createdAt: input.createdAt,
    artifactRoot: input.artifactRoot,
  });
}

async function executeDatabaseCommand(input, mode) {
  return executeControlledDatabaseOperation({
    ...input,
    mode,
    adapter: createDatabaseCliAdapter(input.adapter),
  });
}

async function recordMetadataCommitExecution(input) {
  const commit = input.commit ?? JSON.parse(await readFile(input.commitArtifactPath, "utf8"));
  const executionArtifact = createMetadataCommitExecutionArtifact({
    commit,
    executionResult: input.executionResult,
    executedBy: input.executedBy,
    executedAt: input.executedAt,
    connectionRef: input.connectionRef,
  });
  if (input.executionArtifactPath) {
    await mkdir(dirname(input.executionArtifactPath), { recursive: true });
    await writeFile(input.executionArtifactPath, `${JSON.stringify(executionArtifact, null, 2)}\n`, "utf8");
  }
  return {
    executionArtifact,
    executionArtifactPath: input.executionArtifactPath ?? "",
  };
}

async function executeMetadataCommitCommand(input) {
  const commit = input.commit ?? JSON.parse(await readFile(input.commitArtifactPath, "utf8"));
  const executionArtifact = await executeMetadataCommit({
    commit,
    adapter: createMetadataAdapter(input.metadataAdapter),
    executedBy: input.executedBy,
    executedAt: input.executedAt,
    connectionRef: input.connectionRef,
  });
  if (input.executionArtifactPath) {
    await mkdir(dirname(input.executionArtifactPath), { recursive: true });
    await writeFile(input.executionArtifactPath, `${JSON.stringify(executionArtifact, null, 2)}\n`, "utf8");
  }
  return {
    executionArtifact,
    executionArtifactPath: input.executionArtifactPath ?? "",
  };
}

async function renderTimepointStateQueryCommand(input) {
  const queryArtifact = renderTimepointStateQuery({
    databaseAssetId: input.databaseAssetId,
    timestamp: input.timestamp,
  });
  if (input.queryArtifactPath) {
    await mkdir(dirname(input.queryArtifactPath), { recursive: true });
    await writeFile(input.queryArtifactPath, `${JSON.stringify(queryArtifact, null, 2)}\n`, "utf8");
  }
  return {
    queryArtifact,
    queryArtifactPath: input.queryArtifactPath ?? "",
  };
}

async function executeTimepointStateQueryCommand(input) {
  const queryArtifact =
    input.queryArtifact ?? JSON.parse(await readFile(input.queryArtifactPath, "utf8"));
  const queryResult = await executeTimepointStateQuery({
    queryArtifact,
    adapter: createMetadataAdapter(input.metadataAdapter),
    queriedBy: input.queriedBy,
    queriedAt: input.queriedAt,
    connectionRef: input.connectionRef,
  });
  if (input.queryResultPath) {
    await mkdir(dirname(input.queryResultPath), { recursive: true });
    await writeFile(input.queryResultPath, `${JSON.stringify(queryResult, null, 2)}\n`, "utf8");
  }
  return {
    queryResult,
    queryResultPath: input.queryResultPath ?? "",
  };
}

async function createTimepointStateManifestCommand(input) {
  const queryResultArtifact =
    input.queryResultArtifact ?? JSON.parse(await readFile(input.queryResultPath, "utf8"));
  const stateManifest = createTimepointStateManifest({
    queryResultArtifact,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.stateManifestPath) {
    await mkdir(dirname(input.stateManifestPath), { recursive: true });
    await writeFile(input.stateManifestPath, `${JSON.stringify(stateManifest, null, 2)}\n`, "utf8");
  }
  return {
    stateManifest,
    stateManifestPath: input.stateManifestPath ?? "",
  };
}

async function executeRollbackArtifacts(input) {
  const manifest = JSON.parse(await readFile(input.artifactManifestPath, "utf8"));
  const artifactTexts = {};
  for (const artifact of Array.isArray(manifest.artifacts) ? manifest.artifacts : []) {
    const artifactRef = String(artifact.artifactRef ?? "").trim();
    artifactTexts[artifactRef] = await readFile(
      resolveArtifactPath(input.artifactBaseDir, artifactRef),
      "utf8",
    );
  }
  const rollbackExecution = await executeRollbackArtifactManifest({
    manifest,
    artifactTexts,
    adapter: createRollbackAdapter(input.rollbackAdapter),
    executedBy: input.executedBy,
    executedAt: input.executedAt,
    connectionRef: input.connectionRef,
  });
  if (input.rollbackExecutionPath) {
    await mkdir(dirname(input.rollbackExecutionPath), { recursive: true });
    await writeFile(
      input.rollbackExecutionPath,
      `${JSON.stringify(rollbackExecution, null, 2)}\n`,
      "utf8",
    );
  }
  return {
    rollbackExecution,
    rollbackExecutionPath: input.rollbackExecutionPath ?? "",
  };
}

async function executeForwardArtifacts(input) {
  const manifest = JSON.parse(await readFile(input.artifactManifestPath, "utf8"));
  const artifactTexts = {};
  for (const artifact of Array.isArray(manifest.artifacts) ? manifest.artifacts : []) {
    const artifactRef = String(artifact.artifactRef ?? "").trim();
    artifactTexts[artifactRef] = await readFile(
      resolveArtifactPath(input.artifactBaseDir, artifactRef),
      "utf8",
    );
  }
  const forwardExecution = await executeForwardArtifactManifest({
    manifest,
    artifactTexts,
    adapter: createForwardAdapter(input.forwardAdapter),
    executedBy: input.executedBy,
    executedAt: input.executedAt,
    connectionRef: input.connectionRef,
  });
  if (input.forwardExecutionPath) {
    await mkdir(dirname(input.forwardExecutionPath), { recursive: true });
    await writeFile(
      input.forwardExecutionPath,
      `${JSON.stringify(forwardExecution, null, 2)}\n`,
      "utf8",
    );
  }
  return {
    forwardExecution,
    forwardExecutionPath: input.forwardExecutionPath ?? "",
  };
}

async function executeRestoreChecksCommand(input) {
  const restorePlan = JSON.parse(await readFile(input.restorePlanPath, "utf8"));
  const checkExecution = await executeRestoreChecks({
    restorePlan,
    checks: input.checks,
    adapter: createCheckAdapter(input.checkAdapter),
    executedBy: input.executedBy,
    executedAt: input.executedAt,
    connectionRef: input.connectionRef,
  });
  if (input.checkExecutionPath) {
    await mkdir(dirname(input.checkExecutionPath), { recursive: true });
    await writeFile(
      input.checkExecutionPath,
      `${JSON.stringify(checkExecution, null, 2)}\n`,
      "utf8",
    );
  }
  return {
    checkExecution,
    checkExecutionPath: input.checkExecutionPath ?? "",
  };
}

async function replayTimeline(input) {
  const events = await readJournalEvents({ journalPath: input.journalPath });
  return {
    journalPath: input.journalPath,
    eventCount: events.length,
    timeline: replayEnvironmentTimeline(events),
  };
}

async function createBaselineRecords(input) {
  const recordSet = createBaselineRecordSet({
    node: input.timelineNode,
    records: input.records,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.baselineRecordsPath) {
    await mkdir(dirname(input.baselineRecordsPath), { recursive: true });
    await writeFile(input.baselineRecordsPath, `${JSON.stringify(recordSet, null, 2)}\n`, "utf8");
  }
  return {
    recordSet,
    baselineRecordsPath: input.baselineRecordsPath ?? "",
  };
}

async function deriveBaselineRecordsFromStructureSnapshots(input) {
  const beforeSnapshot =
    input.beforeSnapshot ?? JSON.parse(await readFile(input.beforeSnapshotPath, "utf8"));
  const afterSnapshot =
    input.afterSnapshot ?? JSON.parse(await readFile(input.afterSnapshotPath, "utf8"));
  const recordSet = createBaselineRecordSetFromStructureSnapshots({
    currentNode: input.currentNode,
    node: input.timelineNode,
    beforeSnapshot,
    afterSnapshot,
    dataScope: input.dataScope,
    beforeDataScope: input.beforeDataScope,
    afterDataScope: input.afterDataScope,
    dataEvidenceRef: input.dataEvidenceRef,
    beforeDataEvidenceRef: input.beforeDataEvidenceRef,
    afterDataEvidenceRef: input.afterDataEvidenceRef,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.baselineRecordsPath) {
    await mkdir(dirname(input.baselineRecordsPath), { recursive: true });
    await writeFile(input.baselineRecordsPath, `${JSON.stringify(recordSet, null, 2)}\n`, "utf8");
  }
  return {
    recordSet,
    baselineRecordsPath: input.baselineRecordsPath ?? "",
  };
}

async function deriveRestoreBaselineRecords(input) {
  const restoreVerification =
    input.restoreVerification ?? JSON.parse(await readFile(input.restoreVerificationPath, "utf8"));
  const restoreNode = input.restoreNode ?? JSON.parse(await readFile(input.restoreNodePath, "utf8"));
  const recordSet = createRestoreBaselineRecordSetFromVerificationArtifact({
    restoreVerification,
    restoreNode,
    restoreVerificationRef: input.restoreVerificationRef,
    beforeCapturedAt: input.beforeCapturedAt,
    afterCapturedAt: input.afterCapturedAt,
    beforeDataScope: input.beforeDataScope,
    afterDataScope: input.afterDataScope,
    beforeDataEvidenceRef: input.beforeDataEvidenceRef,
    afterDataEvidenceRef: input.afterDataEvidenceRef,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.baselineRecordsPath) {
    await mkdir(dirname(input.baselineRecordsPath), { recursive: true });
    await writeFile(input.baselineRecordsPath, `${JSON.stringify(recordSet, null, 2)}\n`, "utf8");
  }
  return {
    recordSet,
    baselineRecordsPath: input.baselineRecordsPath ?? "",
  };
}

async function deriveInitialBaselineFromStructureSnapshot(input) {
  const structureSnapshot =
    input.structureSnapshot ?? JSON.parse(await readFile(input.structureSnapshotPath, "utf8"));
  const initialBaseline = createInitialBaselineFromStructureSnapshot({
    databaseAssetId: input.databaseAssetId,
    structureSnapshot,
    baselineAfterRef: input.baselineAfterRef,
    validFrom: input.validFrom,
    dataScope: input.dataScope,
    dataEvidenceRef: input.dataEvidenceRef,
    dataCheckpointRef: input.dataCheckpointRef,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.initialBaselinePath) {
    await mkdir(dirname(input.initialBaselinePath), { recursive: true });
    await writeFile(
      input.initialBaselinePath,
      `${JSON.stringify(initialBaseline, null, 2)}\n`,
      "utf8",
    );
  }
  if (input.timelineNodePath) {
    await mkdir(dirname(input.timelineNodePath), { recursive: true });
    await writeFile(
      input.timelineNodePath,
      `${JSON.stringify(initialBaseline.timelineNode, null, 2)}\n`,
      "utf8",
    );
  }
  if (input.baselineRecordsPath) {
    await mkdir(dirname(input.baselineRecordsPath), { recursive: true });
    await writeFile(
      input.baselineRecordsPath,
      `${JSON.stringify(initialBaseline.recordSet, null, 2)}\n`,
      "utf8",
    );
  }
  return {
    initialBaseline,
    initialBaselinePath: input.initialBaselinePath ?? "",
    timelineNodePath: input.timelineNodePath ?? "",
    baselineRecordsPath: input.baselineRecordsPath ?? "",
  };
}

async function finalizeRestore(input) {
  const restoreVerification = JSON.parse(await readFile(input.restoreVerificationPath, "utf8"));
  const restoreNode = createRestoreTimelineNodeFromVerificationArtifact({
    restoreVerification,
    restoreVerificationRef: input.restoreVerificationRef,
  });
  if (input.restoreNodePath) {
    await mkdir(dirname(input.restoreNodePath), { recursive: true });
    await writeFile(input.restoreNodePath, `${JSON.stringify(restoreNode, null, 2)}\n`, "utf8");
  }
  return {
    restoreNode,
    restoreNodePath: input.restoreNodePath ?? "",
  };
}

async function verifyRestore(input) {
  const restorePlan = JSON.parse(await readFile(input.restorePlanPath, "utf8"));
  const restoreVerification = createRestoreVerificationArtifact({
    restorePlan,
    operationId: input.operationId,
    verifiedBy: input.verifiedBy,
    verifiedAt: input.verifiedAt,
    baselineBeforeRef: input.baselineBeforeRef,
    baselineAfterRef: input.baselineAfterRef,
    schemaFingerprint: input.schemaFingerprint,
    evidenceRef: input.evidenceRef,
    checks: input.checks,
  });
  if (input.restoreVerificationPath) {
    await mkdir(dirname(input.restoreVerificationPath), { recursive: true });
    await writeFile(
      input.restoreVerificationPath,
      `${JSON.stringify(restoreVerification, null, 2)}\n`,
      "utf8",
    );
  }
  return {
    restoreVerification,
    restoreVerificationPath: input.restoreVerificationPath ?? "",
  };
}

async function verifyRollbackRestore(input) {
  const restorePlan = JSON.parse(await readFile(input.restorePlanPath, "utf8"));
  const rollbackExecution = JSON.parse(await readFile(input.rollbackExecutionPath, "utf8"));
  const restoreCheckExecution = JSON.parse(
    await readFile(input.restoreCheckExecutionPath, "utf8"),
  );
  const restoreVerification = createRestoreVerificationFromRollbackExecution({
    restorePlan,
    rollbackExecution,
    rollbackExecutionRef: input.rollbackExecutionRef,
    restoreCheckExecution,
    restoreCheckExecutionRef: input.restoreCheckExecutionRef,
    operationId: input.operationId,
    verifiedBy: input.verifiedBy,
    verifiedAt: input.verifiedAt,
    baselineBeforeRef: input.baselineBeforeRef,
    baselineAfterRef: input.baselineAfterRef,
    evidenceRef: input.evidenceRef,
  });
  if (input.restoreVerificationPath) {
    await mkdir(dirname(input.restoreVerificationPath), { recursive: true });
    await writeFile(
      input.restoreVerificationPath,
      `${JSON.stringify(restoreVerification, null, 2)}\n`,
      "utf8",
    );
  }
  return {
    restoreVerification,
    restoreVerificationPath: input.restoreVerificationPath ?? "",
  };
}

async function renderRollbackArtifacts(input) {
  const restorePlan = JSON.parse(await readFile(input.restorePlanPath, "utf8"));
  const manifest = createRollbackArtifactManifest({
    restorePlan,
    artifacts: input.artifacts,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.artifactManifestPath) {
    await mkdir(dirname(input.artifactManifestPath), { recursive: true });
    await writeFile(input.artifactManifestPath, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
  }
  return {
    manifest,
    artifactManifestPath: input.artifactManifestPath ?? "",
  };
}

async function renderMetadataCommit(input) {
  const commit = renderTimelineNodeMetadataCommit({
    node: input.node,
    currentVersion: input.currentVersion,
    updatedBy: input.updatedBy,
    updatedAt: input.updatedAt,
  });
  if (input.commitPath) {
    await mkdir(dirname(input.commitPath), { recursive: true });
    await writeFile(input.commitPath, commit.sqlText, "utf8");
  }
  return {
    commit,
    commitPath: input.commitPath ?? "",
  };
}

async function renderBaselineRecordsCommit(input) {
  const recordSet = JSON.parse(await readFile(input.baselineRecordsPath, "utf8"));
  const commit = renderBaselineRecordsMetadataCommit({ recordSet });
  if (input.commitPath) {
    await mkdir(dirname(input.commitPath), { recursive: true });
    await writeFile(input.commitPath, commit.sqlText, "utf8");
  }
  return {
    commit,
    commitPath: input.commitPath ?? "",
  };
}

async function renderChangeMetadataCommitCommand(input) {
  const recordSet = JSON.parse(await readFile(input.baselineRecordsPath, "utf8"));
  const commit = renderChangeMetadataCommit({
    node: input.node,
    recordSet,
    currentVersion: input.currentVersion,
    updatedBy: input.updatedBy,
    updatedAt: input.updatedAt,
  });
  if (input.commitPath) {
    await mkdir(dirname(input.commitPath), { recursive: true });
    await writeFile(input.commitPath, commit.sqlText, "utf8");
  }
  if (input.commitArtifactPath) {
    await mkdir(dirname(input.commitArtifactPath), { recursive: true });
    await writeFile(input.commitArtifactPath, `${JSON.stringify(commit, null, 2)}\n`, "utf8");
  }
  return {
    commit,
    commitPath: input.commitPath ?? "",
    commitArtifactPath: input.commitArtifactPath ?? "",
  };
}

async function renderTimelineArtifactsMetadataCommitCommand(input) {
  const artifactManifest = JSON.parse(await readFile(input.artifactManifestPath, "utf8"));
  const commit = renderTimelineArtifactsMetadataCommit({ artifactManifest });
  if (input.commitPath) {
    await mkdir(dirname(input.commitPath), { recursive: true });
    await writeFile(input.commitPath, commit.sqlText, "utf8");
  }
  if (input.commitArtifactPath) {
    await mkdir(dirname(input.commitArtifactPath), { recursive: true });
    await writeFile(input.commitArtifactPath, `${JSON.stringify(commit, null, 2)}\n`, "utf8");
  }
  return {
    commit,
    commitPath: input.commitPath ?? "",
    commitArtifactPath: input.commitArtifactPath ?? "",
  };
}

async function renderRestorePlanMetadataCommitCommand(input) {
  const restorePlan = JSON.parse(await readFile(input.restorePlanPath, "utf8"));
  const commit = renderRestorePlanMetadataCommit({ restorePlan });
  if (input.commitPath) {
    await mkdir(dirname(input.commitPath), { recursive: true });
    await writeFile(input.commitPath, commit.sqlText, "utf8");
  }
  if (input.commitArtifactPath) {
    await mkdir(dirname(input.commitArtifactPath), { recursive: true });
    await writeFile(input.commitArtifactPath, `${JSON.stringify(commit, null, 2)}\n`, "utf8");
  }
  return {
    commit,
    commitPath: input.commitPath ?? "",
    commitArtifactPath: input.commitArtifactPath ?? "",
  };
}

async function renderRestoreEvidenceMetadataCommitCommand(input) {
  const rollbackExecution = JSON.parse(await readFile(input.rollbackExecutionPath, "utf8"));
  const restoreCheckExecution = JSON.parse(
    await readFile(input.restoreCheckExecutionPath, "utf8"),
  );
  const restoreVerification = JSON.parse(await readFile(input.restoreVerificationPath, "utf8"));
  const commit = renderRestoreEvidenceMetadataCommit({
    rollbackExecution,
    restoreCheckExecution,
    restoreVerification,
  });
  if (input.commitPath) {
    await mkdir(dirname(input.commitPath), { recursive: true });
    await writeFile(input.commitPath, commit.sqlText, "utf8");
  }
  if (input.commitArtifactPath) {
    await mkdir(dirname(input.commitArtifactPath), { recursive: true });
    await writeFile(input.commitArtifactPath, `${JSON.stringify(commit, null, 2)}\n`, "utf8");
  }
  return {
    commit,
    commitPath: input.commitPath ?? "",
    commitArtifactPath: input.commitArtifactPath ?? "",
  };
}

async function renderForwardEvidenceMetadataCommitCommand(input) {
  const forwardExecution = JSON.parse(await readFile(input.forwardExecutionPath, "utf8"));
  const commit = renderForwardEvidenceMetadataCommit({
    forwardExecution,
  });
  if (input.commitPath) {
    await mkdir(dirname(input.commitPath), { recursive: true });
    await writeFile(input.commitPath, commit.sqlText, "utf8");
  }
  if (input.commitArtifactPath) {
    await mkdir(dirname(input.commitArtifactPath), { recursive: true });
    await writeFile(input.commitArtifactPath, `${JSON.stringify(commit, null, 2)}\n`, "utf8");
  }
  return {
    commit,
    commitPath: input.commitPath ?? "",
    commitArtifactPath: input.commitArtifactPath ?? "",
  };
}

async function renderSchemaRollbackArtifactsCommand(input) {
  const restorePlan = JSON.parse(await readFile(input.restorePlanPath, "utf8"));
  const rendered = renderSchemaRollbackArtifacts({
    restorePlan,
    artifactBaseRef: input.artifactBaseRef,
  });
  if (input.scriptDir) {
    await mkdir(input.scriptDir, { recursive: true });
    for (const artifact of rendered.artifacts) {
      const fileName = artifact.artifactRef.split("/").at(-1);
      await writeFile(`${input.scriptDir}/${fileName}`, `${artifact.artifactText}\n`, "utf8");
    }
  }
  const manifest = createRollbackArtifactManifest({
    restorePlan,
    artifacts: rendered.artifacts,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.artifactManifestPath) {
    await mkdir(dirname(input.artifactManifestPath), { recursive: true });
    await writeFile(input.artifactManifestPath, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
  }
  return {
    rendered,
    manifest,
    artifactManifestPath: input.artifactManifestPath ?? "",
  };
}

async function renderForwardChangeArtifactsCommand(input) {
  const changePlan = input.changePlan ?? JSON.parse(await readFile(input.changePlanPath, "utf8"));
  const rendered = renderForwardChangeArtifacts({
    changePlan,
    targetDatabaseAssetId: input.targetDatabaseAssetId,
    timelineNode: input.timelineNode,
    artifactBaseRef: input.artifactBaseRef,
  });
  if (input.scriptDir) {
    await mkdir(input.scriptDir, { recursive: true });
    for (const artifact of rendered.artifacts) {
      const fileName = artifact.artifactRef.split("/").at(-1);
      await writeFile(`${input.scriptDir}/${fileName}`, `${artifact.artifactText}\n`, "utf8");
    }
  }
  const manifest = createForwardArtifactManifest({
    changePlan,
    targetDatabaseAssetId: input.targetDatabaseAssetId,
    timelineNode: input.timelineNode,
    artifacts: rendered.artifacts,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.artifactManifestPath) {
    await mkdir(dirname(input.artifactManifestPath), { recursive: true });
    await writeFile(input.artifactManifestPath, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
  }
  return {
    rendered,
    manifest,
    artifactManifestPath: input.artifactManifestPath ?? "",
  };
}

async function renderDataRollbackArtifactsCommand(input) {
  const restorePlan = JSON.parse(await readFile(input.restorePlanPath, "utf8"));
  const rendered = renderDataRollbackArtifacts({
    restorePlan,
    artifactBaseRef: input.artifactBaseRef,
    beforeImages: input.beforeImages,
  });
  if (input.scriptDir) {
    await mkdir(input.scriptDir, { recursive: true });
    for (const artifact of rendered.artifacts) {
      const fileName = artifact.artifactRef.split("/").at(-1);
      await writeFile(`${input.scriptDir}/${fileName}`, `${artifact.artifactText}\n`, "utf8");
    }
  }
  const manifest = createRollbackArtifactManifest({
    restorePlan,
    artifacts: rendered.artifacts,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.artifactManifestPath) {
    await mkdir(dirname(input.artifactManifestPath), { recursive: true });
    await writeFile(input.artifactManifestPath, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
  }
  return {
    rendered,
    manifest,
    artifactManifestPath: input.artifactManifestPath ?? "",
  };
}

async function renderSnapshotRestoreArtifactsCommand(input) {
  const restorePlan = JSON.parse(await readFile(input.restorePlanPath, "utf8"));
  const rendered = renderSnapshotRestoreArtifacts({
    restorePlan,
    artifactBaseRef: input.artifactBaseRef,
    restoreEvidence: input.restoreEvidence,
  });
  if (input.artifactDir) {
    await mkdir(input.artifactDir, { recursive: true });
    for (const artifact of rendered.artifacts) {
      const fileName = artifact.artifactRef.split("/").at(-1);
      await writeFile(`${input.artifactDir}/${fileName}`, `${artifact.artifactText}\n`, "utf8");
    }
  }
  const manifest = createRollbackArtifactManifest({
    restorePlan,
    artifacts: rendered.artifacts,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.artifactManifestPath) {
    await mkdir(dirname(input.artifactManifestPath), { recursive: true });
    await writeFile(input.artifactManifestPath, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
  }
  return {
    rendered,
    manifest,
    artifactManifestPath: input.artifactManifestPath ?? "",
  };
}

function resolveTimelineAt(input) {
  const node = resolveTimelineNodeAt({
    nodes: input.nodes,
    databaseAssetId: input.databaseAssetId,
    timestamp: input.timestamp,
  });
  return {
    status: node ? "resolved" : "not_found",
    node,
  };
}

async function planRollback(input) {
  const plan = planRollbackToNode({
    nodes: input.nodes,
    databaseAssetId: input.databaseAssetId,
    currentNodeId: input.currentNodeId,
    targetNodeId: input.targetNodeId,
  });
  if (!input.changeRequestId && !input.restorePlanPath) return plan;

  const currentNode = findTimelineNode(input.nodes, input.currentNodeId, "currentNodeId");
  const targetNode = findTimelineNode(input.nodes, input.targetNodeId, "targetNodeId");
  const restorePlan = createRestorePlanArtifact({
    changeRequestId: input.changeRequestId,
    databaseAssetId: input.databaseAssetId,
    currentNode,
    targetNode,
    plan,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.restorePlanPath) {
    await mkdir(dirname(input.restorePlanPath), { recursive: true });
    await writeFile(input.restorePlanPath, `${JSON.stringify(restorePlan, null, 2)}\n`, "utf8");
  }
  return {
    ...plan,
    restorePlan,
    restorePlanPath: input.restorePlanPath ?? "",
  };
}

async function planRollbackAt(input) {
  const targetNode = resolveTimelineNodeAt({
    nodes: input.nodes,
    databaseAssetId: input.databaseAssetId,
    timestamp: input.timestamp,
  });
  if (!targetNode) {
    throw new Error(`No verified timeline node found at timestamp: ${input.timestamp}`);
  }
  const plan = planRollbackToNode({
    nodes: input.nodes,
    databaseAssetId: input.databaseAssetId,
    currentNodeId: input.currentNodeId,
    targetNodeId: targetNode.timelineNodeId,
  });
  if (!input.changeRequestId && !input.restorePlanPath) {
    return {
      ...plan,
      targetNode,
    };
  }

  const currentNode = findTimelineNode(input.nodes, input.currentNodeId, "currentNodeId");
  const restorePlan = createRestorePlanArtifact({
    changeRequestId: input.changeRequestId,
    databaseAssetId: input.databaseAssetId,
    currentNode,
    targetNode,
    plan,
    createdBy: input.createdBy,
    createdAt: input.createdAt,
  });
  if (input.restorePlanPath) {
    await mkdir(dirname(input.restorePlanPath), { recursive: true });
    await writeFile(input.restorePlanPath, `${JSON.stringify(restorePlan, null, 2)}\n`, "utf8");
  }
  return {
    ...plan,
    targetNode,
    restorePlan,
    restorePlanPath: input.restorePlanPath ?? "",
  };
}

function findTimelineNode(nodes, timelineNodeId, fieldName) {
  const node = Array.isArray(nodes)
    ? nodes.find((candidate) => candidate.timelineNodeId === timelineNodeId)
    : undefined;
  if (!node) throw new Error(`${fieldName} not found: ${timelineNodeId}`);
  return node;
}

function commandMode(command) {
  if (READ_ONLY_COMMANDS.has(command)) return "read_only";
  if (EVIDENCE_WRITE_COMMANDS.has(command)) return "evidence_write";
  if (METADATA_WRITE_COMMANDS.has(command)) return "metadata_write";
  if (RESTORE_WRITE_COMMANDS.has(command)) return "restore_write";
  if (FORWARD_WRITE_COMMANDS.has(command)) return "forward_write";
  return "plan_only";
}

function createForwardAdapter(input) {
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    throw new Error("forwardAdapter is required");
  }
  if (input.type === "postgres-psql") {
    return createPostgresPsqlMetadataAdapter({
      psqlPath: input.psqlPath,
      connectionUriEnv: input.connectionUriEnv,
    });
  }
  throw new Error(`Unsupported forwardAdapter.type: ${input.type}`);
}

function createRollbackAdapter(input) {
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    throw new Error("rollbackAdapter is required");
  }
  if (input.type === "postgres-psql") {
    return createPostgresPsqlMetadataAdapter({
      psqlPath: input.psqlPath,
      connectionUriEnv: input.connectionUriEnv,
    });
  }
  if (input.type === "snapshot-json-command") {
    return {
      executeSnapshotRestoreArtifact(request) {
        return executeJsonCommandAdapter(input.command, request, "rollbackAdapter.command");
      },
      executeRestoreReportArtifact(request) {
        return executeJsonCommandAdapter(input.command, request, "rollbackAdapter.command");
      },
    };
  }
  throw new Error(`Unsupported rollbackAdapter.type: ${input.type}`);
}

function executeJsonCommandAdapter(command, request, fieldName) {
  if (!Array.isArray(command) || command.length === 0) {
    throw new Error(`${fieldName} must be a non-empty array`);
  }
  const [executable, ...args] = command.map((part) => String(part));
  if (!executable.trim()) {
    throw new Error(`${fieldName}[0] is required`);
  }
  const proc = spawnSync(executable, args, {
    input: JSON.stringify(request),
    encoding: "utf8",
    env: process.env,
  });
  if (proc.error) {
    throw proc.error;
  }
  if (proc.status !== 0) {
    const detail = String(proc.stderr || proc.stdout || "").trim();
    throw new Error(
      detail
        ? `snapshot restore command failed with status ${proc.status}: ${detail}`
        : `snapshot restore command failed with status ${proc.status}`,
    );
  }
  try {
    return JSON.parse(proc.stdout);
  } catch (error) {
    throw new Error(`snapshot restore command returned invalid JSON: ${error.message}`);
  }
}

function createCheckAdapter(input) {
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    throw new Error("checkAdapter is required");
  }
  if (input.type === "postgres-psql") {
    return createPostgresPsqlMetadataAdapter({
      psqlPath: input.psqlPath,
      connectionUriEnv: input.connectionUriEnv,
    });
  }
  throw new Error(`Unsupported checkAdapter.type: ${input.type}`);
}

function createMetadataAdapter(input) {
  if (!input || typeof input !== "object" || Array.isArray(input)) {
    throw new Error("metadataAdapter is required");
  }
  if (input.type === "postgres-psql") {
    return createPostgresPsqlMetadataAdapter({
      psqlPath: input.psqlPath,
      connectionUriEnv: input.connectionUriEnv,
    });
  }
  throw new Error(`Unsupported metadataAdapter.type: ${input.type}`);
}

function resolveArtifactPath(artifactBaseDir, artifactRef) {
  if (typeof artifactBaseDir !== "string" || artifactBaseDir.trim() === "") {
    throw new Error("artifactBaseDir is required");
  }
  if (typeof artifactRef !== "string" || artifactRef.trim() === "") {
    throw new Error("artifact.artifactRef is required");
  }
  const base = resolve(artifactBaseDir);
  const target = resolve(base, artifactRef);
  const basePrefix = base.endsWith(sep) ? base : `${base}${sep}`;
  if (target !== base && !target.startsWith(basePrefix)) {
    throw new Error(`artifactRef escapes artifactBaseDir: ${artifactRef}`);
  }
  return target;
}

function requireOperationId(input) {
  if (typeof input.operationId !== "string" || input.operationId.trim() === "") {
    throw new Error("operationId is required");
  }
  return input.operationId;
}

function parseArgs(argv) {
  const command = argv[2];
  let inputPath = "";
  let outputPath = "";

  for (let index = 3; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--input") {
      inputPath = argv[index + 1] ?? "";
      index += 1;
      continue;
    }
    if (arg === "--output") {
      outputPath = argv[index + 1] ?? "";
      index += 1;
      continue;
    }
    throw new Error(`Unknown argument: ${arg}`);
  }

  if (!command) throw new Error("command is required");
  if (!inputPath) throw new Error("--input is required");
  if (!outputPath) throw new Error("--output is required");

  return { command, inputPath, outputPath };
}

main(process.argv).catch((error) => {
  reportFailure(failureResponse(process.argv[2], undefined, error, "unknown"));
});
