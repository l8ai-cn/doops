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

Fresh read: 2026-11-16 21:52 China time, session fleet-inventory-20261116.

| Target | Current Agent commit | State / next action |
|---|---|---|
| gw-test | 0d389204508ce989bf6bca7e06438edfbb425f0a | v0.3.9 deployed; retain revision 26, recheck image and smoke |
| gw-edu-coder | 8683d67174e19a1750e9b19de99a0816cdb8ea79 | Online; independent first-upgrade Job required |
| gw-hdu-235 | 76abae08a36d920c0058486d2d8ab86880c4c121 | Online; independent first-upgrade Job required |
| gw-zy | 8683d67174e19a1750e9b19de99a0816cdb8ea79 | Online; independent first-upgrade Job required |
| gw-zyj | 081f24ab4e92535bce87c23889e242a1ee5f0886 | Online; independent first-upgrade Job required; other sessions active |
| oilan | 76abae08a36d920c0058486d2d8ab86880c4c121 | Online; first-upgrade Job and existing SA permissions review |
| unicom-1 | 8683d67174e19a1750e9b19de99a0816cdb8ea79 | Online; verify actual workload identity before Job |
| unicom-k8s | 76abae08a36d920c0058486d2d8ab86880c4c121 | Online; independent first-upgrade Job required |
| gw-scu | 3cba4acdfa904bd8d5673eda4b1097125eed2585 | Online; independent first-upgrade Job required |
| gw-silicon | 76abae08a36d920c0058486d2d8ab86880c4c121 | Online standalone Docker; use independent socket controller |
| gw-keyi | 57bd0ec05b5661dce4b384a823b51780c8f0eca5 | Online old protocol; discover runtime and controller access |
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
- The member branch contains concurrent confirmation-removal work. Preserve it;
  do not mix a moving worktree into this fleet deployment artifact.

## Current Steps

1. Audit exact source and prepare pinned, repeatable build inputs; compare checksums.
2. Review reuse of the released independent controller, including rollback and
   Gateway acceptance. Pilot one old target before wider rollout.
3. Roll target by target, retain per-target terminal receipts, verify imageID,
   commit, health and user path, then reconcile GitOps and remote Git visibility.

## Bounds And Recovery

This resumed delivery round is bounded to 120 minutes. Use at most three
corrective attempts per unchanged failure; no speculative mutation replay.
Unknown controller outcomes require readback before another write. Never weaken
CI, receipt verification, credential isolation or rollback checks. Unreachable
targets remain incomplete with concrete evidence. If the round cannot progress,
persist the last state and exact dependency; do not claim all targets updated.
