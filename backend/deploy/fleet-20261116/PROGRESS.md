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
11 targets are online; two configured routes remain unavailable. No target has
yet been verified on the new v0.3.11 candidate.

| Target | Current Agent commit | State / next action |
|---|---|---|
| gw-test | 0d389204508ce989bf6bca7e06438edfbb425f0a | v0.3.9 deployed; retain revision 26, recheck image and smoke |
| gw-edu-coder | 8683d67174e19a1750e9b19de99a0816cdb8ea79 | Online; kz-ops/doops-agent; mounted admin kubeconfig; BuildKit available |
| gw-hdu-235 | 76abae08a36d920c0058486d2d8ab86880c4c121 | Online; ai/doops-agent, revision 5; mounted kubeconfig |
| gw-zy | 8683d67174e19a1750e9b19de99a0816cdb8ea79 | Online; independent first-upgrade Job required |
| gw-zyj | 081f24ab4e92535bce87c23889e242a1ee5f0886 | Online; ai/doops-agent, revision 49; mounted admin kubeconfig; other sessions active |
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
  its Contract tests and Secret scan passed, Go tests are still running.
  Consolidate these changes into v0.3.11 after the final merge; v0.3.10 has
  no published tag or assets. Build only the final immutable committed snapshot.
- The one-shot first-upgrade controller now has render/run/accept stages,
  immutable Secret backups, rollback, Gateway acceptance and redacted step
  diagnostics. Fourteen local transaction cases passed. Real-cluster pilot
  verification is still required; it currently supports mounted independent
  kubeconfig identities only.

## Current Steps

1. Commit the reviewed controller and progress, push the member branch, verify
   GitHub CI and merge PR 22. Record remote-visible full commit identities.
2. Publish matching v0.3.11 CLI/Gateway/Skill assets and build the committed
   base-light/Agent/Web inputs on the authorized builder. Verify checksums,
   runtime probes and registry digests; install the matched CLI and Skill.
3. Pilot gw-test and one old mounted-kubeconfig target. Require terminal Job
   receipt, Ready, exact imageID/commit, reconnect and execution/model smoke.
4. Continue remaining Kubernetes and host runtimes with their independent
   controllers. Read back unknown outcomes before any further mutation.
5. Reconcile both Gateway/console environments, browser user paths and all 13
   target results. Unavailable targets remain explicitly incomplete.

## Bounds And Recovery

This resumed delivery round is bounded to 120 minutes. Use at most three
corrective attempts per unchanged failure; no speculative mutation replay.
Unknown controller outcomes require readback before another write. Never weaken
CI, receipt verification, credential isolation or rollback checks. Unreachable
targets remain incomplete with concrete evidence. If the round cannot progress,
persist the last state and exact dependency; do not claim all targets updated.
