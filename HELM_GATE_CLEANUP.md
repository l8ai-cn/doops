---
name: helm-gate-cleanup
scope: task
status: complete
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
- [x] Publish/install matching artifacts and verify the deployed target.

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

Use at most six corrective iterations for the same failing check before
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

## Deployed Acceptance (2026-11-15)

- Main CI `43646319787` and Release `43646361767` succeeded. Release v0.3.3
  and the installed CLI/Skill all identify source
  `6b95035459611542b3db789afc7a8c217a2c5b00`; repository, parent workspace
  and global Skill copies were installed and compared against that release.
- Audited build tasks `f5dee7b910ed8c34` and `75a414d90415ec58` exited 0.
  The Agent image is
  `sha256:cb6046bea9618044806a14371de1d58760f7bb5c2fe9fd3181bcec1d28f24511`,
  and embeds DoAgent 0.2.45 / `64f075e37ccf06122ee4f88646b0ebd1548351da`.
- The first upgrade exposed drift between the actual Deployment Gateway URL
  and stored Helm values. Gateway acceptance failed and the independent Job
  rolled back. The corrected controller reconciled the proven original URL,
  retained the failed attempt, and completed revision 21. Recovery checks
  cover response loss, concurrent spec changes and failure before revision
  creation; 17 local scenarios pass.
- `gw-test` now reports the released commit. The routed Pod UID
  `bf4c2815-7abb-46bd-b56a-a39760e0260e`, actual image digest, DoAgent
  version and Ready state match. The controller recorded `phase=complete`.
- Real `ask --apply` session `helm-autorepair-live-20261115` returned
  `doops.verified-mutation`, `verified=true`, `outcome=succeeded`.
  It retained path-validation failures and the actual template failure,
  then corrected `probeEnabled` in the same request. The execution failure
  `call_01a1dc3ebdf77009ba4d93ed` links through `resolved_by` to successful
  `call_01a1dc3ec37a70d58d58ddeb`. No further user authorization was requested.
- Independent readback confirmed a NetworkPolicy-only manifest, deployed
  revision 1 and an unchanged template digest. The test release was removed,
  and subsequent readback was empty.
- On the installed Agent, the original Loki URL returned HTTP 200,
  168,364 bytes and SHA256
  `f0026f251da5b49cea636a24143dbbadf15b39f51fc4639c9e0d08a4bc975ee3`.
  Native default rendering returned the real missing
  `loki.storage.bucketNames.chunks` error. `test/loki` remains absent, as
  required by the original diagnostic scope.
- Temporary upgrade Jobs, code ConfigMaps and controller SA/CRBs are removed.
  Immutable backups and metadata receipts remain for recovery and audit.
  Complete tool output is retained in the task-local
  `.local/helm-recovery-check/` receipt and log artifacts.

## Direct Execution Follow-through (2026-11-16)

The user explicitly requested direct execution without repeated confirmation.
An explicit operation or apply request carries the user's execution intent;
target authentication, credential isolation, read-only requests, operation
ownership and truthful terminal evidence still apply.

- [x] Read the live baseline through `helm-delivery-20261116`: `gw-test` is
  `doops-zyj/zyj-test`, Agent v0.3.9 at
  `0d389204508ce989bf6bca7e06438edfbb425f0a`, Helm revision 26 deployed.
  The installed local CLI is v0.3.7. Source and runtime status are distinct.
- [x] Read `test/loki`: revision 10 failed with `context deadline exceeded`;
  current Pods include ImagePullBackOff and Pending. Do not report the
  platform release as a successful Loki deployment.
- [x] Match the runtime lock to deployed DoAgent source
  `af62d43828ce4d83e7cc8c4fa41a8ae128bcad56`. Registry readback verifies amd64
  digest `sha256:2dd40b6013559019d975169831b6e67d3ea9bb9c60843b8b6863cd84ac5ac5bb`
  and matching source label. The prior source lock would have downgraded the
  deadline repair/progress and six-attempt completion-review fixes.
- [x] Remove duplicate K8s/Web confirmation parameters and console confirmation
  controls; verify explicit actions execute once and read-only tools stay read-only.
- [x] Align canonical skills, API/help and user documentation; scan for retired
  confirmation instructions.
- [x] Run functional checks, production build and browser workflows; independently
  review authorization and terminal-evidence behavior.
- [ ] Commit/push on the existing member branch, verify CI, publish/install matched
  assets, upgrade the test Agent and record real execution receipts.

Execution bounds: at most six corrective attempts per failing check, at most
three identical attempts without new evidence, and at most 120 minutes for this
delivery round. A failed build, unreachable service or missing release access
must be recorded with its actual error; do not weaken verification, replay an
unknown mutation, or claim an unpublished change is deployed. Persist each
completed stage here so the next round resumes from tool evidence.

### Resumed Delivery (2026-11-16)

- Fresh Gateway read at 13:37 UTC still reports source
  `0d389204508ce989bf6bca7e06438edfbb425f0a`; `agents-test/doops-agent-test`
  remains deployed at revision 26. The local CLI remains v0.3.7.
  The confirmation-removal changes are not deployed.
- Backend removal and canonical documentation are implemented. Focused
  CLI-to-Gateway-to-Agent and Nginx functional checks passed in the prior
  verification; the final package and contract checks are being rerun.
- Independent review identified duplicate-click submission in administrative
  actions and loss of the previous unknown-outcome receipt when starting a
  session. Both are being fixed without reinstating confirmation dialogs.
- This round will finish browser verification, push the existing
  `fix/public-helm-failure-receipts` branch, verify CI and immutable artifacts,
  install matching CLI/Skill assets, then upgrade the selected Agent and Web
  console. Each deployment requires a version and live-operation readback.
- `test/loki` is still revision 10 / failed. Its state is separate from the
  platform rollout and must not be reported as a successful Loki deployment.
- Backend/CLI/skill changes were pushed as
  `55df739bb9493554086f1fa28aef85648ec19e42`; GitHub CI runs
  `43885132839` and `43885121461` passed all checks.
- Final console checks passed 636/636, TypeScript passed and the production
  Web build succeeded. Browser verification is in progress on that build.
- The concurrent fleet task owns v0.3.10. This task prepares v0.3.11 with
  the completed confirmation-removal UI, and limits rollout to `gw-test`
  and the Web console. Other fleet/Gateway changes are preserved.
- Source upload exposed a separate reproducible Gateway defect:
  `validCOSSnapshotRel` rejected `[...path]` because it searched for any `..`
  substring instead of a parent-directory segment. The regression failed
  with the actual `cos_key_rejected` before the fix; all COS tests passed
  afterward. The v0.3.11 Gateway asset must be installed and the same COS
  upload retried to verify this correction online.

### Artifact And Online Verification Phase

The implementation phase is complete. The additional COS defect requires a
Gateway release as well as the originally planned Agent/Web rollout. This
phase starts at 15:25 UTC with a 90-minute ceiling, three attempts per unchanged
failure, and no repeat of a mutation whose result is unknown.

- Release source is pinned to
  `b01d7bff48fb95714ee49cbc05a9114a603b9946`, including the COS fix.
  The shared member branch subsequently contains the separate fleet controller
  commit; this task's build uses its pinned source.
- Audited source acquisition task `f6d5d5e766b3d4e4` exited 0 after retrieving
  the public Git repository anonymously and verifying that exact commit.
  The failed COS upload remains recorded and must be verified after Gateway
  rollout; it is not treated as successful.
- Agent/base build task: `816d8b7ca819dab2`, session
  `doops-direct-b01d7bff`; Web build task: `ee689546ea5dcbcb`, session
  `doops-web-b01d7bff`. Both run on `gw-hdu-235` using its existing registry
  authentication. Artifacts are pending, not yet deployed.
- Browser K8s scale submitted exactly once without `confirm`; the production
  Web build and console test suite have passed. Remaining browser scenarios
  and online receipts are pending.

### Formal Release And Live Acceptance (2026-11-17)

- The formal v0.3.11 tag identifies
  `32ccf453d6a0744fba590b4c11b0027d65c7800b`. The earlier b01d candidate
  Agent build exited 0, but its Web build exited 1 during npm downloads.
  Formal artifacts use the shared fleet build from the release commit.
- CLI and installed global Skill both report the formal release commit.
  The installed Skill states that an explicit task does not require repeated
  confirmation; Kubernetes commands no longer use `--confirm`.
- Independent Gateway readback confirms `gw-test` is v0.3.11, Helm revision
  27 deployed, Pod UID `d2f792ab-d209-400d-8ed8-39be787bf89a`, Ready true,
  and imageID
  `docker.cnb.cool/l8ai/ai/doops.sh@sha256:6db6060f3b84e7814f08baeb72c35c4adf1bcae3da2e9d3c8be1c1b622d52f16`.
  DoAgent reports 0.2.46.
- The independent production-build browser review passed direct submission,
  double-click prevention, failure/403/busy states, retained unknown receipts
  and the 390px viewport. Evidence:
  `.local/direct-execution-20261116/browser-handoff-acceptance.md`.
- Live direct-operation and NetworkPolicy recovery checks use session
  `direct-helm-v0311-20261117`. Gateway/Web deployment and online browser
  acceptance remain to be established; do not infer them from Agent rollout.
- Business `test/loki` remains revision 10 / failed with a recorded Helm
  deadline error. Platform release success does not establish Loki readiness.
- Live Kubernetes restart and scale both completed without a confirmation
  flag. Readback found generation 2 and the new restart annotation; the
  zero-replica verification Deployment was deleted afterward.
- The root-directory Helm Chart exposed another real rejection:
  `cleanWorkspaceRelativePath` rejected `.` even though it denotes the bound
  session directory. The model repeatedly tried invalid alternative paths;
  the caller interrupted the turn (exit 130), then independently verified
  that the test release and NetworkPolicy did not exist. No Helm success
  is claimed for that interrupted turn.
- The same root-directory case failed locally before the fix. Removing only
  the `.` rejection makes both `.` and `./` render successfully with real
  Helm; related command, native-values and path tests pass. Directory values,
  traversal and absolute paths remain rejected. This correction is prepared
  for v0.3.12; installed acceptance must repeat the same root Chart scenario.

### Control Plane Delivery And Root-Chart Release

- The existing repository-specific control-plane scripts deployed Gateway and
  Web v0.3.11. Web uses
  `docker.cnb.cool/l8ai/ai/doops.sh/web@sha256:2568fc82672b8c547483fd5dd30f25b225f961bca6f6d3d720c45f9ffdf8141a`;
  `/console` returns 200. Its prior container is retained as
  `doops-web-rollback-20261116T164129Z-92714`.
- Gateway deployment exited 0. The running executable and release asset both
  hash to `8e7b165632fa61b7a0543ff1821887aa7c6fb9e9260d761ae23c5e5da902619b`.
  Health returns 200, unauthenticated target listing returns 401, and the ten
  targets observed before replacement reconnected. The binary backup remains
  at `/usr/local/bin/doops-gateway.rollback-bc8f61264989-20261116T164815Z`.
- The internal endpoint is reachable normally. A forced-host public-name
  probe returned 200 on the internal host, but subsequent DNS inspection
  proved the public hostname uses a different host. That probe does not
  establish public deployment. The public browser still serves old assets.
  No DNS, VPN or proxy configuration was changed.
- Session `cos-catchall-v0311-20261117` uploaded three real tracked catch-all
  route files, including `[...path]` and `[[...path]]`; COS reported
  `workspace: synced` at snapshot `9c2581473341`.
- Root-Chart source `e1d2ba7c24b4283efe7a1d2c0c1fa19bba960358` is pushed
  on the existing member branch and merged through PR 24. CI runs
  `43909029618` and `43909573950` passed every check. Tag v0.3.12 points
  to that exact source; Release run `43912252775` is in progress.
- Controlled build task `e47ebcbc4e1f2f00` exited 0 with Agent image
  `docker.cnb.cool/l8ai/ai/doops.sh@sha256:9b7435cb01c71bd7290b9f7c3b604455e0b28dac2f6a28d5433ddf0aaad5bd11`.
  Its source, base inputs and runtime probes passed. Release installation
  and live root-Chart recovery remain pending.

The original deployment phase established v0.3.11 delivery. The newly
reproduced root-Chart defect has a separate final verification phase, ending
by 17:25 UTC on November 16 (01:25 China time on November 17), with at most
three corrective attempts for an unchanged failure. Do not replay the
interrupted turn; inspect release state first and use a fresh acceptance
session after deploying the corrected runtime.

### Browser Follow-through

- The real double-click test sent one scale followed by an unintended cancel:
  the Execute control became Stop in the same position. Session
  `console-9u0g1a5t`, request `e9d6373c-f4f8-4533-82ce-d5961025f1e0`
  ended `execution_outcome_unknown`; it was not replayed. Readback found
  the isolated Deployment at replicas 0 / generation 1, then it was removed.
- The new UI has no confirmation field or dialog. A stable Execute control
  and separate Stop control are being verified before the final Web update.
  The image also omitted `public`; the follow-up includes those tracked assets.
- Release v0.3.12 finished successfully, including published-asset verification.
  The newly exposed UI defect is a separate bounded follow-up: at most three
  corrective attempts and completion or a recorded dependency by 18:00 UTC
  (02:00 China time). Root-Chart acceptance continues within its existing bound.

### Resumed Acceptance (2026-11-17 01:20 China Time)

The user reiterated continuing the release and direct execution without another
confirmation. Resume from live receipts; authorization is already established.

1. Verify the deployed root-Chart repair with a real managed Helm failure,
   correction, successful NetworkPolicy-only release, readback and cleanup.
2. Finish browser validation of the stable Execute control and separate Stop
   control, commit/push the scoped Web changes, and verify CI and the image.
3. Deploy the Web artifact with the existing release procedure, retain rollback
   state, and verify the actual endpoint version and browser operation.

This resumed round ends by 02:30 China time, with at most three corrective
attempts for the same unchanged failure. Unknown mutation results require
readback before another write. Unavailable external delivery remains explicitly
incomplete, never a reason to replace tool evidence with a model claim.

- Fresh target and CLI reads both report v0.3.12 source
  `e1d2ba7c24b4283efe7a1d2c0c1fa19bba960358`.
- Upgrade `upgrade_911cf42ebe95a19ccc8938f21eb4f947` completed with Helm
  revision 28, Pod UID `4c8971d6-313b-4c66-aae3-2d9d0d248704`, expected
  runtime digest, structuredSSH and release-commit acceptance. The receipt is
  `agents-test/doops-upgrade-receipt-0d461309497d0b4a5b38d0e3-apply`.
- Release workflow `43912252775` is completed successfully at the same commit.
- The stable Execute/Stop correction and public asset copy are still local;
  their deployed acceptance is pending.
- Installed managed Helm acceptance `helm-root-v0312-20261117` exited 0.
  Template failure `call_mI8XEEN4CtVxPRQN6KMkGW6C` is retained and resolves
  to `call_z2U8YLxx23CWaqccogDzIBHb`; the verified mutation receipt reports
  `outcome=succeeded`, `turn.status=completed`. Both calls used the same
  workspace-root Chart and release, without another user confirmation.
- Independent readback confirmed revision 1 deployed, a NetworkPolicy-only
  manifest and matching template SHA256
  `35b1be59daa302be09ef1d2b1e249154dc93c089e3a6c36637cb999562334576`.
  The initial readback's relative-path hash command failed because ordinary
  exec has a different cwd; an absolute session path confirmed the hash.
  Cleanup returned exit 0, an empty Helm listing and no same-name policy.
  Raw receipts and readbacks are in
  `.local/direct-execution-20261116/root-chart-*-v0312.*`.
- The console suite passed 637 checks. Independent review then reproduced a
  fast-result double-click that resubmitted after the first RPC had ended.
  Two focused regressions reproduced both fast success and fast failure before
  the fix. Execute now ignores later clicks within the same pointer gesture,
  while accepting keyboard activation and a subsequent separate click.
  The 161 focused K8s/transport checks pass; browser verification uses this
  final source rather than the earlier button-position-only build.

### Web Release v0.3.13

- Source `6b25b830a92ae16ae3bbd3ae2917e5f84e6e2c0f` is pushed and verified
  at the remote member branch. Push CI `43918987441` and PR CI `43919245178`
  passed Go, contract and secret checks. Independent review accepted the
  final pointer-gesture correction.
- Production browser checks passed slow and fast-success/fast-failure
  double-clicks, explicit stop, keyboard Enter/Space, 390px failure/pending
  states, no confirmation controls, and public image availability. Each
  double-click sent one scale and zero cancels. Report:
  `.local/direct-execution-20261116/local-final-k8s-acceptance.md`.
- The old `gw-edu-coder` Agent rejected snapshot synchronization with
  `cos_agent_snapshot_executable_unsupported`. That failure is unresolved
  for this older Agent. Audited task `43eba05c4a0c8755` instead checked out
  the already-pushed public Git commit with credential helpers disabled,
  verified its exact SHA and clean checkout, and exited 0.
- Build task `3909e67da6b81a3a` exited 0. Registry readback verifies amd64,
  version v0.3.13 and the exact source label at
  `docker.cnb.cool/l8ai/ai/doops.sh/web@sha256:349c41b609cc463a87018c33b9527efb9a0160f965eca300066b024ff915b2df`.
  The build reused the existing managed registry binding in a private temporary
  authfile; no credential grant or global authentication file changed.
- Internal Web deployment completed with exit 0. Runtime readback confirms
  that image and source, build ID `xDO1lqHi65YOhA1URwszQ`, and `/console` HTTP
  200. Prior container `doops-web-rollback-20261116T174054Z-32848` remains
  available for rollback.
- Online browser acceptance passed at 01:45-01:47 China time. A real
  double-click against `test/doops-direct-smoke-v0313` on
  `doops-zyj/zyj-test` sent exactly one scale, zero cancels and no `confirm`
  field. Request `25ca379a-6ab9-463d-9556-5565cbaf1902`, session
  `console-9u0g1a5t`, returned `open -> output -> result -> done` with
  `Kubernetes operation complete.` No confirmation dialog was shown.
  All 15 JavaScript assets and four brand images returned 200; console and
  network checks had no errors. Desktop and 390px screenshots passed.
- Independent readback verified the isolated Deployment UID and zero replicas,
  then cleanup deleted it with exit 0 and an empty final readback. Evidence:
  `.local/direct-execution-20261116/online-v0313-acceptance.md`,
  `online-v0313-scale-evidence.json`, and `console-cleanup-v0313.log`.
- PR 25 merged as `5365ab33727045ba853cac1c56d4aa0f39523a6f`.
  Its source is verified as an ancestor of main. Tag v0.3.13 points to the
  exact tested/build source and is pushed. Release workflow `43922104165`
  completed successfully, including published-asset verification.
- CLI v0.3.13 and its exact-source Skill/docs were installed into the repository,
  parent workspace, and existing global Agent/Codex Skill locations. The first
  download failed with a GitHub TLS interruption before replacement; the
  same-version retry succeeded. `doops version` reports source
  `6b25b830a92ae16ae3bbd3ae2917e5f84e6e2c0f`; target readback still works
  without another login. Agent v0.3.12 remains the verified Helm runtime.
- The installed Agent K8s Skill hash equals the canonical source:
  `70c83646f478f269354546df4933c79a3ee39473a2d7efdcc21d15df3da9fff3`.
  A final deterministic check of the Helm and browser JSON receipts passed:
  preserved failed attempt, correcting success, successful terminal turn,
  one online scale, zero cancels and zero browser errors.
- Public `doops.l8ai.cn` belongs to a separate entry route associated with
  `gw-114-k8s`, not this Web host. The target is unavailable through the
  current Gateway; the existing recovery connection also failed. Its
  backend identity and release are unverified. No public replacement,
  authorization change, DNS change or proxy change is claimed.
- Final read-only business check still finds `test/loki` revision 10 failed,
  with several Pods in ImagePullBackOff or Pending. This task verified the
  requested platform behavior on `doops-zyj/zyj-test`; it did not mutate that
  separate Loki deployment or complete the remaining fleet rollout.
