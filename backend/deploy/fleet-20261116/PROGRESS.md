---
name: fleet-agent-upgrade-20261116
scope: deployment
status: in-progress
---

# Agent Fleet Upgrade

User authorization: update all 13 configured targets, including the build nodes,
and both Gateway environments. Public test: l8ai; internal: aiedulab.cn.
Existing authorization includes commit, push, merge, build and deployment.

## Completion Evidence

Each target requires a terminal controller receipt, live release commit and
imageID readback, Ready and Gateway reconnect, plus an execution/model smoke.
The CLI `version` subcommand reports the local CLI, even with `--target`;
it is never evidence of a remote Agent version. Use `targets` and audited
`exec /app/doops-agent -version` instead.

## Verified Baseline

Fresh read: 2026-11-16 22:50 China time, session fleet-inventory-20261116.
11 targets were online; two configured routes remain unavailable. As of
2026-11-17 00:22 China time, gw-test is verified on v0.3.11. The other 12
targets have not been upgraded to that release.

| Target | Current Agent commit | State / next action |
|---|---|---|
| gw-test | 32ccf453d6a0744fba590b4c11b0027d65c7800b | v0.3.11 verified; revision 27; Ready, digest, reconnect, execution, model and browser smoke passed |
| gw-edu-coder | 8683d67174e19a1750e9b19de99a0816cdb8ea79 | Online; kz-ops/doops-agent; mounted admin kubeconfig; BuildKit available |
| gw-hdu-235 | 76abae08a36d920c0058486d2d8ab86880c4c121 | Online; ai/doops-agent, revision 5; mounted kubeconfig |
| gw-zy | 8683d67174e19a1750e9b19de99a0816cdb8ea79 | Online; independent first-upgrade Job required |
| gw-zyj | 081f24ab4e92535bce87c23889e242a1ee5f0886 | Two preflight Jobs failed before replacement; revision 49 unchanged; old registry index is missing |
| oilan | 76abae08a36d920c0058486d2d8ab86880c4c121 | Online; first-upgrade Job and existing SA permissions review |
| unicom-1 | 8683d67174e19a1750e9b19de99a0816cdb8ea79 | Online; hostname k8s-master; distinct from the doops-system/doops-agent Pod |
| unicom-k8s | 76abae08a36d920c0058486d2d8ab86880c4c121 | Online; doops-system/doops-agent, ServiceAccount doops-unicom-agent |
| gw-scu | 3cba4acdfa904bd8d5673eda4b1097125eed2585 | Online; doops-system/doops-agent-scu, release doops-scu, ServiceAccount doops-agent |
| gw-silicon | 76abae08a36d920c0058486d2d8ab86880c4c121 | Online standalone Docker; use independent socket controller |
| gw-keyi | 57bd0ec05b5661dce4b384a823b51780c8f0eca5 | Online; systemd doops-agent.service, /usr/local/bin/doops-agent.bin |
| gw-114-k8s | Unavailable | Not visible to current Gateway login; check inventory/grant before calling offline |
| gw-jm | Unavailable | Not visible to current Gateway login; check inventory/grant before calling offline |

## Release Baseline And Source Audit

- PR 20 merged as a16905b44509178f106a53d0f0c8789bc13c1970.
- PR 21 merged as d5004a2b29f4b370bfe26c07a33b77e134c30ef7.
- Release v0.3.9 source: 0d389204508ce989bf6bca7e06438edfbb425f0a;
  release workflow 43743890095 completed successfully.
- Internal Gateway last verified at v0.3.9. Public l8ai TLS was reset;
  internal verification does not prove public environment delivery.
- DoAgent source: af62d43828ce4d83e7cc8c4fa41a8ae128bcad56;
  image digest sha256:2dd40b6013559019d975169831b6e67d3ea9bb9c60843b8b6863cd84ac5ac5bb.
- Prior DoOps image: sha256:fa703925af64f0cc92f057dbf231c8c709e134240846a48ddb3315586678e982.
  It used uncommitted build-input edits and an old base after a failed base check.
  Audit and rebuild the committed inputs before expanding rollout; a label alone
  is insufficient provenance.
- Confirmation-removal backend changes were pushed at
  55df739bb9493554086f1fa28aef85648ec19e42; both GitHub CI runs passed.
  Console changes were pushed at 4144db60eaed9d4b059a2a60ce9e1235617f56f7;
  all CI checks passed. These changes are included in formal v0.3.11;
  v0.3.10 has no published tag or assets.
- The one-shot first-upgrade controller now has render/run/accept stages,
  immutable Secret backups, rollback, Gateway acceptance and redacted step
  diagnostics. Twenty-four local transaction, registry and timing cases pass.
  It currently supports mounted independent kubeconfig identities only; the
  old-Agent pilot has not passed. Keep it separate from gw-test's successful
  native upgrade controller.

## Current Steps

1. Done: commit/push, CI, PR 22 merge, v0.3.11 publication, matched CLI/Skill
   installation, and base/Agent/Web builds from source 32ccf453.
2. Done: native gw-test upgrade and live terminal/model/browser acceptance.
3. Pending: recover the exact old registry artifacts before another old-Agent
   pilot, or prepare a reviewed replacement transaction with verified recovery.
4. Pending: implement and verify the independent SA and Docker/systemd paths;
   resolve the two unavailable targets. Existing socket-controller review found
   concrete recovery gaps; it is not accepted for live replacement.
5. Pending: Gateway/console host publication and both environments' user paths.
   The human question about the SSH-only scripts versus the caller's no-daily-
   SSH rule remains unanswered; do not infer approval from elapsed time.

## Resumed Delivery Evidence

- Runtime source: `32ccf453d6a0744fba590b4c11b0027d65c7800b`, pushed on
  `fix/public-helm-failure-receipts`; remote visibility and main ancestry verified.
  PR 22 merged as `b77b8515b0abbc75af51a312587abcf1f1fd80ab`.
- CI runs `43894521978` and `43895559905` passed. Release v0.3.11 workflow
  `43899255019` completed successfully, including published-asset verification.
- Matching image builds completed on gw-edu-coder. Runtime inputs were checked
  against Git blobs before build; base and Agent in-image probes passed.
  Agent task `12fab5071aa13459` and Web task `c0eb5478bdd2694c` exited 0.
  Base: `docker.cnb.cool/l8ai/ai/doops.sh/base-light@sha256:f6f80b13d155aed47df005ba180e7ef7ad0c03956e5ad59c0fa54c2dbf97aeea`.
  Agent: `docker.cnb.cool/l8ai/ai/doops.sh@sha256:6db6060f3b84e7814f08baeb72c35c4adf1bcae3da2e9d3c8be1c1b622d52f16`.
  Web: `docker.cnb.cool/l8ai/ai/doops.sh/web@sha256:2568fc82672b8c547483fd5dd30f25b225f961bca6f6d3d720c45f9ffdf8141a`.
- The first push attempts exited 1 with registry HTTP 401: the builder's existing
  authfile contained Harbor entries only. The active `cnb-upstream-dev`
  opaqueSecret binding passed credential preflight. The build wrapper used its
  existing materialized Secret in a private 0600 temporary authfile, removed at
  subprocess exit; no global authfile or credential grant was changed.
- gw-zyj pilot Job `fleet-zyj-v0311-r49` failed before mutation. Helm revision 49
  remained deployed. The Job exited in its start second while checking its own
  kubelet-published Ready state. The controller now waits up to 60 seconds for
  verified self-identity and preserves helper error categories. Twenty-four
  transaction/registry/timing cases pass; the corrected real pilot is pending.
- gw-hdu-235 render passed server-side dry-run; no upgrade Job was submitted
  there while another build task was using that target.
- Independent readback found the SCU and Unicom Kubernetes SAs/RBAC are external
  to their Agent releases. Their rollback manifests and operation permissions
  still need verification before adding that identity mode to the controller.
- Docker socket-controller review found missing external acceptance, incomplete
  rollback health checks and an ambiguous-create recovery gap. Do not deploy
  gw-silicon or unicom-1 with that incomplete controller.
- Internal Gateway still reports v0.3.9. Public Gateway and console have separate
  TLS/static-resource drift; neither environment is accepted as v0.3.11 yet.
  Browser baseline is space 324; internal console currently shows login.

## Acceptance And Remaining Dependencies (2026-11-17)

- Native gw-test operation `upgrade_13eb7d2543975e66a7d9aba0a16c478b`
  returned success; its preflight and apply Jobs completed. Terminal receipt:
  `agents-test/doops-upgrade-receipt-7db56c508d1daeb5c668c0d0-apply`.
  Running Pod `doops-agent-test-6dc8467b5b-58r2w`, UID
  `d2f792ab-d209-400d-8ed8-39be787bf89a`, reports the exact Agent digest above,
  Ready true and release commit 32ccf453. Helm revision 27 is deployed;
  `/health/ready` returns ready; DoAgent is 0.2.46.
- Audited exec and a real read-only model tool call returned the same hostname
  and release commit. Internal console login, selecting zyj-test, and terminal
  `hostname && /app/doops-agent -version` returned exit code 0. Captured browser
  network failures and runtime exceptions were both zero. Screenshot:
  `.local/fleet-test-browser-20261117.png`. Browser space 324 was finished.
  This verifies the existing console against the new Agent, not a new Web
  deployment or new frontend behavior online.
- Corrected pilot `ai/fleet-zyj-v0311-r49b` failed before replacement at
  registry-pull. Live Helm revision 49 remains deployed. Separate registry
  readbacks confirm old gw-zyj 352fa6ef, hdu 778312b1 and edu fd20c510
  references return artifact not found.
- vm-0-11 cache verification found old 352fa6ef index bytes and the amd64
  ce91070a manifest, plus 25 config/layer blobs totaling 571163619 bytes, all
  hash/size verified. Referenced attestation manifest e1ef14e8 is absent, so a
  platform-only export cannot prove restoration of the original index digest.
  No export, push, grant expansion or missing-baseline override was performed.
- gw-114-k8s and gw-jm still return target unavailable through the current
  Gateway login. Their runtime state and recovery channel remain unverified.
- Follow-up operational source `03e1a52c1b160e9ac7026869f4b616e399b876b4`
  is pushed and remote-visible; CI runs 43902413371 and 43904928141 passed.
  PR 23 covers the temporary build auth and controller timing correction;
  it does not move the immutable v0.3.11 runtime tag.
- Downloaded Gateway v0.3.11 asset SHA256 is
  `8e7b165632fa61b7a0543ff1821887aa7c6fb9e9260d761ae23c5e5da902619b`.
  Its live installation and both new Web deployments remain pending the
  explicit execution-method decision requested from the human.

## Bounds And Recovery

This resumed delivery round is bounded to 120 minutes. Use at most three
corrective attempts per unchanged failure; no speculative mutation replay.
Unknown controller outcomes require readback before another write. Never weaken
CI, receipt verification, credential isolation or rollback checks. Unreachable
targets remain incomplete with concrete evidence. If the round cannot progress,
persist the last state and exact dependency; do not claim all targets updated.
