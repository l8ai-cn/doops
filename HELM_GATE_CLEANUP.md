---
name: helm-gate-cleanup
scope: task
status: in-progress
---

# Helm Gate Cleanup

## Goal

Allow Helm to manage any valid Chart, including releases containing only
NetworkPolicy, ConfigMap, RBAC, CRD, custom resources, or Jobs. Remove DoOps
resource-kind, image, Ingress, and redundant confirmation gates from code,
prompts, and canonical skills.

## Acceptance

- Helm receives the requested release, namespace, Chart, and values without
  a DoOps manifest preflight or mandatory reset of existing values.
- A successful upgrade returns its actual deployed revision and release state.
  Failed commands, invalid receipts, and interrupted calls remain failures.
- Public downloads never receive a private repository's credentials.
- A NetworkPolicy-only Chart completes through the managed Helm tool.
- Canonical skills and documentation describe the implemented behavior.
- A rejected Helm call can be corrected within the same authorized turn.
  A known execution failure can also be repaired by a later serial success for
  the same release and intent. Receipts preserve both attempts; unknown outcomes
  and a different rollback revision or Chart cannot be hidden by later success.
- A connected task with no output for 60 seconds remains active. Execution has
  a total deadline, cancellation waits for the original RPC terminal result,
  and missing evidence remains unknown.

## Progress

- [x] Locate the rejection: `validateHelmRenderedContent` requires a workload
  or Ingress, and rejects all other otherwise valid Charts.
- [x] Identify related gates in command building, attestation validation,
  model prompts, and operator/runtime skills.
- [x] Remove custom Helm policy and preserve native command/result handling.
- [x] Verify functional success/failure paths and run relevant regression checks.
- [x] Review the Helm gate and receipt changes, commit/push, and verify CI.
- [ ] Publish/install matching artifacts and verify the deployed target.

## Current Findings

- The initial Loki incident compared the unrelated CNB credential origin
  `helm.cnb.cool:443` with the public Chart origin `github.com:443`.
- Existing diagnostic session evidence permits verification without creating
  Loki resources. The public Chart also requires storage configuration, so a
  successful anonymous download alone is not proof of a deployable Loki setup.
- On 2026-11-14, `gw-test` still reports
  `76abae08a36d920c0058486d2d8ab86880c4c121`; the changes are not deployed.
- Review found a 45-second first-output timer terminating otherwise connected
  console operations, and historical parameter failures poisoning later
  successful corrections. Both are being fixed with functional coverage.
- Work is split between result/repair handling, origin credential selection,
  and console transport; each worker has disjoint files.
- 2026-11-14 follow-through: extend recovery beyond unexecuted validation and
  preparation errors to known Helm execution failures, preserving the original
  intent and both receipts. Verify stdio-to-WebSocket behavior and unknown
  outcome controls before committing.
- PR #12 merged the gate cleanup and receipt changes as
  `fe01a931ebef3eda84c1099303b0151b88210508`. Main CI run `43617931524`
  and release run `43619006463` both passed; GitHub Release `v0.3.2` is
  published.

## Execution Bounds

Use at most three corrective iterations for the same failing check before
recording its root cause and external dependency. Do not replay a mutation
with an unknown terminal outcome. Stop release work when no authorized
build artifact, reachable target, or required release access is available;
record the exact blocker and completed evidence here.

## Guidance Cleanup (2026-11-14)

- [x] Define business delivery in `backend/docs/NATURAL_LANGUAGE_DEPLOYMENT.md`;
  operator rules, build/model docs and README entries link to that owner.
- [x] Remove the unused `backend/skills/config/servers.json` and duplicated
  Helm workspace contract. CLI configuration remains the existing user store.
- [x] Remove retired local-controller examples and generic version-equality,
  CI-proof and build-location requirements from business delivery guidance.
- [x] Scope `image-build` to requested builds and delegate Helm operations to
  the `k8s` skill. Keep target grants, credential isolation and truthful receipts.
- [x] Review residual prerequisite lists and preserve the supported dedicated
  ServiceAccount upgrade path in the canonical SOP and its API references.
- [x] Validate 114 documentation, installer, managed-release and host-access
  checks; focused Helm and CLI tests pass, including NetworkPolicy-only Charts,
  native values, release receipts and canonical configuration parsing.
- [x] Package the actual Skill/docs bundle, verify its checksum and compare the
  packaged entrypoint with source; deleted files are absent. `git diff --check`
  passes.

These checks verify the local source and package. They do not establish a
published release, update installed Skill copies or prove online acceptance.

## Verification Follow-through

- Agent server package completed successfully in 747 seconds on 2026-11-14.
  The earlier run hit the default 10-minute limit; the completed JSON test log
  contains no failed tests and ends with package `pass`.
- Console regression tests: 640 passed, zero failed or skipped. CLI package and
  natural-language deployment contract checks passed.
- A remaining false positive classified `compat` as a PAT field. Match actual
  PAT field boundaries and known provider names, and exercise both the accepted
  Chart key and rejected credential keys.
- Online reread confirms `gw-test` routes to `doops-zyj/zyj-test`, still running
  `76abae08a36d920c0058486d2d8ab86880c4c121`. Published and deployed acceptance is
  not yet established.

## Autonomous Recovery Follow-through (2026-11-15)

- [x] Reread `gw-test`: the installed Agent still reports the old commit,
  doagent is `0.2.42`, and no `loki` release exists in namespace `test`.
- [x] Enable the existing doagent completion review for authorized apply turns;
  preserve the single turn, original mutation authority and all tool receipts.
- [x] Review and verify against the pinned doagent `0.2.44` source commit
  `e4d052e62392fbc7a3ef02cb22765215f53e301f`. Follow-up
  `cec8f0fd2c5bab57bbc2e5ca64d46f819b1169a3` is pushed in PR #13;
  both CI runs passed Go, contract and secret checks.
- [x] Confirm CLI and global Skill installation from release `v0.3.2`,
  commit `fe01a931ebef3eda84c1099303b0151b88210508`.
- [ ] Obtain an immutable Agent artifact from the existing controlled build
  path and verify the target version.
- [ ] Verify a NetworkPolicy-only release and the original Loki Chart through
  the deployed managed tool; retain the tool result, exit state and readback.

Recovery completes only when actual tool evidence confirms the requested result.
The existing doagent completion review bounds continuation, and the DoOps prompt
has a 30-minute deadline. Unknown mutation outcomes require readback before any
new action; a model summary cannot resolve them. Three corrective iterations
without new evidence identify a concrete dependency rather than repeat a mutation.

## Recovery And Installed Guidance Verification (2026-11-15)

- Pinned doagent tests executed an initial failing tool call, rejected a
  premature final reply, and executed a corrected call successfully in the same
  user request. Three tests passed, covering repair, bounded continuation and
  invalid review output. Model replies were deterministic fixtures; this does
  not establish deployed model behavior.
- The Agent MCP/SSE/WebSocket tests verify complete failure and correction
  receipts, progress, terminal results and preservation of unknown outcomes.
- The development link script now moves old Skill/docs and historical backup
  entries outside the Skill load tree, preserves old links, and does not
  recreate backups for an unchanged link. The 22 focused shell/document
  contract tests pass.
- Old `docs.release` and `*.devlink-backup` entries were moved out of the
  repository's installed Skill tree. The parent workspace also held a separate
  old Skill copy. Its formal reinstall failed before replacement with a
  GitHub Release TLS error; it is now explicitly linked to the canonical
  development sources, with the old files retained under `.local/`.
  This development link is not a successful release installation.
- The release task published `v0.3.2`, but Agent image push failed because the
  controlled Docker builder lacks registry authentication. The current registry
  credential setup verifies a Kubernetes Secret and cannot provision this
  standalone builder. No target upgrade is claimed: `gw-test` still reports
  `76abae08a36d920c0058486d2d8ab86880c4c121`.

## Remaining Entry Points (2026-11-15)

- [x] Recheck the live target: `gw-test` still runs Agent
  `76abae08a36d920c0058486d2d8ab86880c4c121` and do-agent `0.2.42`;
  `test/loki` is absent. This is not deployed acceptance of the source fixes.
- [x] Find an additional caller-side gate in the sibling DoAgent repository:
  `plugins/doagent-ops` requires an image, CI evidence, a fixed Chart directory
  and a repository push. Remove those prerequisites from its Skill and task renderer.
- [x] Verify the renderer's Chart-only path and preservation of optional task facts.
- [ ] Independently review the continuation loop and remaining reachable Helm gates.
- [ ] Resolve the controlled builder's registry delivery dependency, then verify the
  installed managed flow. Preserve the diagnostic session's no-Loki-mutation boundary.

## Remaining Recovery Defects (2026-11-15)

- Tool-evidence reviews consumed the final-answer continuation budget while
  discarding their repair instructions. Repair the existing DoAgent loop in
  the sibling `doagent/src/agent_loop`, not the ignored review checkout.
- Reject truncated review status JSON; bound review by cancellation and the
  request deadline; keep recent execution evidence in the review context.
- Preserve multiline Helm stderr through MCP and final receipts. Add a
  nonterminal diagnosis event and pass the remaining request deadline to ACP.
- Verify ordinary Chart `data` values and custom-resource diagnosis without
  exposing credential payloads or mistaking process text for success.
- Online reread still reports Agent
  `76abae08a36d920c0058486d2d8ab86880c4c121`; deployment acceptance is pending.

## Integration Verification (2026-11-15)

- Focused Agent Helm, receipt and evidence tests passed, as did CLI apply/ask
  receipt checks and seven deployment/host-access documentation checks.
- The DoAgent deployment renderer accepts a remote Chart with only target,
  release and namespace. Optional repository, image, reason and acceptance
  facts are retained when supplied.
- A fresh Gateway read still reports the old Agent commit on `gw-test`.
  Published CLI/Skill artifacts do not establish deployment of the Agent.
- Registry publication is being resolved using existing audited build and
  image-copy paths. Private credentials remain on their granted targets.
- Independent review found structured stderr redaction gaps and a progress
  display error that could terminate SSE. Both are corrected: credential
  canaries are removed from JSON/YAML errors, unsafe progress is explicitly
  withheld, and receipt collection continues to the actual terminal event.
- All 444 Python contract tests and 8 subtests passed. Focused stdio-to-SSE
  receipt, correction, progress and redaction checks also passed.
- The remaining runtime image depends on DoAgent CI. Current CNB quota is
  7,372,800 CPU seconds with 7,371,050 used (1,750 remaining); its pipeline
  requires 4,800 CPU seconds to start. The previous build
  `cnb-hnp-1k7aqktuh` failed in Prepare for this reason, before compiling.
  No billing or quota change has been made.

## Delivery Recheck (2026-11-15)

1. Finish recovery, deadline, cancellation and receipt verification, including
   the default-stack nested-agent regression before committing DoAgent.
2. Commit only task-owned changes in both repositories, push and confirm the
   exact remote SHAs. Preserve unrelated ACP cleanup changes.
3. Obtain matching immutable runtime artifacts and verify the installed flow.
   A candidate binary or fixture result does not establish target rollout.

The fresh Gateway read of `gw-test` confirms
`doops-zyj/zyj-test`, Agent
`76abae08a36d920c0058486d2d8ab86880c4c121`, DoAgent `0.2.42` and no
`test/loki` release. The CLI is already `v0.3.2` /
`fe01a931ebef3eda84c1099303b0151b88210508`.

CNB quota and build-stage APIs independently confirm the recorded build
dependency: only 1,750 CI CPU seconds remain, while the failed 8-CPU
pipeline requires 4,800 before it can start. Its build and push stages were
skipped. No quota, billing, credential grant or Loki resource was changed.

Current candidate-source probes through the Gateway passed on the actual target:

- `loki-readonly-recheck-20261115` downloaded and validated the original public
  Loki Chart anonymously: 168,364 bytes, digest
  `sha256:f0026f251da5b49cea636a24143dbbadf15b39f51fc4639c9e0d08a4bc975ee3`,
  no mutation, exit 0. This supersedes the earlier failed download rechecks.
- `helm-policy-recheck-20261115` executed the actual managed MCP executor with
  real Helm: parameter rejection, known template failure, corrected
  NetworkPolicy-only release deployed at revision 1, then successful uninstall
  and an empty release readback. All attempt results were present, exit 0.
- The source-side MCP/SSE receipt bridge, nonterminal diagnosis, Chart data and
  multiline diagnostic checks passed; six deployment contract checks passed.

The task-local receipt is `.local/helm-recovery-check/receipt.json`. These probes
use freshly compiled candidate code; they do not replace the installed Agent or
prove the new DoAgent loop is deployed.

## Runtime Delivery (2026-11-15)

- Independent Go review found no remaining blockers in multiline diagnostics,
  progress or failure receipts; focused MCP/SSE/WebSocket scenarios passed.
- DoAgent default-stack agent-loop checks passed 196/196; default and
  browser/i18n/web builds passed. Review token accounting and the ACP fork
  deadline propagation are included in the current candidate.
- The existing `gw-silicon` node supports native amd64 BuildKit builds and
  dependency downloads. Use the same committed runtime Dockerfile and retain
  its own terminal receipt, then publish the OCI artifact from `gw-hdu-235`
  using that node's existing registry authentication. This does not change
  the failed CNB result or account quota.
- Remaining: push exact source commits, build and verify immutable runtime
  artifacts, update the DoOps runtime pin, and validate the installed target.

DoAgent source `64f075e37ccf06122ee4f88646b0ebd1548351da` is pushed to
`l8ai/ai/doagent` main and confirmed through `origin/main`. Its CNB build
`cnb-ps6-1k7dsnrgr` failed before compilation; the quota dependency remains.
The controlled build uses session `doagent-build-64f075e` on `gw-silicon`.
Its first task `cf7a321ad247afc5` exited 1 because source sync omitted
the committed `settings.json`: non-Git snapshot filtering ignored the
`!/settings.json` inclusion. The CLI now delegates ignore matching to Git
and preserves already-filtered files in the transport commit.

The missing public configuration contract was delivered using `doops write`;
source and remote SHA256 both equal
`9619e118559697e0a92956dabaaa6a432d0b1caab3bcb53d3f143cea4548ede8`.
Build task `b7993003801fdc10` was still in progress at this checkpoint; its
terminal result and artifact verification are recorded below.
The CLI suite and 444 Python contract tests plus 8 subtests passed after the
snapshot fix. Production-feature agent-loop scenarios passed 144/144,
including token accounting, correction, cancellation and deadline exits.

## Verified Runtime Artifact (2026-11-15)

- BuildKit exported the committed DoAgent `64f075e37ccf06122ee4f88646b0ebd1548351da`
  source with the existing `Dockerfile.npc-runtime`, including browser/i18n/web.
  The OCI archive is 370,936,320 bytes, SHA256
  `8054a251ad9f193c1ae7b5f62633ededbaeadec9a1771c7150e001f40610cad3`.
- Task `b7993003801fdc10` subsequently failed while Skopeo imported into Docker:
  its API 1.22 client is below the daemon's minimum 1.24. This failure remains
  recorded; it did not invalidate or rebuild the exported OCI artifact.
- Corrective task `39f9242fc2cac01a` converted that same OCI to Docker archive,
  used native `docker load`, and verified `do-agent 0.2.45`, the exact source
  revision label and manifest SHA256. It finished with exit 0.
- The linux/amd64 manifest is
  `sha256:14e753702568bc2ed8bcc213c7a23a2c600788b3a0b989d84676c7a172ccef06`.
  Registry publication and matching DoOps build remain to be verified.
- The target baseline is release `agents-test/doops-agent-test`, deployed
  revision 18. Its old image is still available. The installed Agent lacks
  `upgradeAudit v1`, so an independent controller submitted through Gateway
  exec is required for this first upgrade; ordinary `doops upgrade` cannot
  execute against this baseline.
- Native `pull --storage gateway` generated the remote snapshot but rejected
  the returned marker because standalone display heartbeats were joined into
  command stdout. The CLI now excludes that complete control notification
  before joining output, without relaxing commit parsing or terminal checks.
  Focused pull/capture tests and the updated runtime lock checks pass.
- The publisher `gw-hdu-235` has a working linux/amd64 BuildKit worker and its
  existing registry authfile. It can build and publish the final DoOps base
  and Agent through Gateway exec after the verified runtime artifact arrives.

## Installed Runtime Rollout (2026-11-15)

- Registry readback confirms the DoAgent artifact at
  `sha256:14e753702568bc2ed8bcc213c7a23a2c600788b3a0b989d84676c7a172ccef06`.
- PR #13 passed both push and pull-request CI at
  `86bca584f5ba3302cfec4accce5a4e27d0062776` and merged as
  `6b95035459611542b3db789afc7a8c217a2c5b00`.
- The revision-18 first-upgrade controller is prepared under
  `backend/deploy/instances/zyj-test/`. Independent review found mixed
  candidate/restored identity in rollback receipts; the repair now restores
  image, source commit, Pod identity and digest and clears candidate acceptance.
  Eleven local transaction scenarios pass, including accepted-then-failed.
- Remaining checks: publish v0.3.3 CLI/Skill and immutable DoOps images,
  execute the independent upgrade, verify the exact routed Pod and runtime,
  then exercise managed NetworkPolicy repair and read-only public Loki
  validation. Preserve the original no-Loki-resource-creation boundary.
