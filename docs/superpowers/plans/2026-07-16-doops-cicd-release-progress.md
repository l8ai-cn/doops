---
status: in-progress
owner_thread_id: 019f5f93-fc4b-74c1-aa37-5a54561dd7d2
updated_at: 2026-07-16T05:06:35+08:00
---

# DoOps CICD Release Progress

## Goal

Ship the Skill/YAML-driven CICD and credential control plane, update Gateway and
Agents without losing management connectivity, and complete one real deployment
from a single versioned workflow YAML.

## Completion Checks

- `main` is committed, pushed, and the commit is visible on the configured remote.
- CNB produces immutable Gateway and Agent artifacts for that exact commit.
- Gateway and Agent upgrades have an explicit rollback artifact and configuration.
- An Agent replacement becomes healthy before the old runtime exits; every
  previously online required target reconnects within 60 seconds.
- Every registered environment passes target discovery, version inspection,
  remote execution, and service health checks using a named DoOps session.
- One committed workflow YAML produces a converged `doops.sh/v2 DeploymentRun`.
- The deployed workload, HTTP contract, audit evidence, and rollback path are
  verified after the real deployment.
- Credential values do not appear in Git, workspace files, prompts, CLI output,
  audit records, logs, or `DeploymentRun` evidence.

## Stop Conditions

- Any credential value is exposed outside the encrypted Gateway store or the
  target-scoped Agent materialization call.
- An upgrade loses the only management path or a required target for over 60
  seconds.
- The release has no verified rollback artifact.
- A deployment result is unknown or lacks deterministic workload and HTTP
  evidence.

## Loop Budget

- Maximum iterations: 12
- Maximum elapsed time: 6 hours
- No-progress threshold: 2 consecutive iterations
- Escalation: stop propagation at the failing environment and report its exact
  state; do not weaken validation or switch to an untracked deployment path.

## Current State

- [x] Credential control plane reviewed and functionally verified
- [ ] YAML CICD execution reviewed and functionally verified
- [ ] Zero-downtime Gateway/Agent upgrade verified
- [x] Current commit pushed and immutable artifacts identified
- [x] Gateway upgraded and healthy
- [ ] All registered Agents upgraded and healthy
- [ ] One real YAML deployment converged
- [ ] Workload, HTTP, audit, and rollback evidence collected

## Iteration 1

- Current `main` includes remote `fbdabf9`; no force-push or history rewrite is
  planned.
- Credential Resource/version/grant/Bundle storage, exact-ID authorization,
  Agent-side materialization, batch rollback, CLI management, and
  `push -> credential prepare -> Ask` ordering pass local Go tests.
- Gateway replacement script now keeps a unique backup of the actual previous
  binary, acquires a remote deployment lock, arms rollback before replacement,
  and requires all baseline Agents to reconnect.
- Agent Helm chart renders a rolling replacement with readiness bound to the
  Gateway connection; chart lint passes with the Oilan values and an immutable
  digest.
- Pending before propagation: independent diff review, full verification,
  commit/push, immutable artifact identification, and Gateway access.

## Iteration 2

- Independent review found four P1 issues: superseded runtime recovery,
  concurrent credential promotion, independent grant revocation, and concurrent
  Gateway deployment.
- The runtime registry now permits an old Pod to recover only when no current
  Agent session exists; a healthy replacement still rejects the old runtime.
- Credential target mutations share one Gateway mutation lock, grants support
  authenticated independent revocation through API and CLI, and Gateway deploys
  use an owner-checked remote lock plus a unique upload directory.
- Directed RED/GREEN coverage passes. Full verification is being rerun before
  commit.

## Iteration 3

- Follow-up review found and closed grant/apply serialization, current platform
  permission enforcement, and revoked-grant restoration semantics.
- Independent read-only review now reports PASS with no confirmed P0/P1.
- Full Agent and CLI Go tests, module verification/read-only listing, Gateway
  deploy contract, shell syntax, Helm lint, and diff checks pass.
- Local source is ready for commit and push; generated `.agent/` and
  `backend/bin/` content remains excluded.

## Iteration 4

- Gateway was deployed with rollback protection and all three baseline Agents
  reconnected. Public health remained HTTP 200.
- CNB built commit `7598d08002f5fb1bcaac110f633dc592e27e0558`;
  the linux/amd64 manifest is
  `sha256:21b1188945faadaecd15c21524628dfcaab1327c5ecb37b876bcdc3775bbeefe`.
- The education Agent rolled from the bootstrap image to that immutable
  manifest with `maxUnavailable: 0`, then a real YAML run reconciled the Helm
  chart, removed the temporary API host alias, and produced an in-cluster
  kubeconfig using `https://10.96.0.1:443`.
- The workload stayed Ready and the Gateway stayed healthy, but the executing
  Agent was replaced before Helm and Ask could return. Helm was recovered from
  `pending-install` to `deployed` without deleting the workload.

## Iteration 5

- A second idempotent YAML run observed the converged workload but Gateway
  rejected its fabricated evidence call IDs. Commit
  `89992d811367940d6188359db7251de1d1b42201` now publishes the completed
  runtime-call catalog; CNB built linux/amd64 manifest
  `sha256:7898919c294688a2eb25f4b588ad7b1517ed47f6c4f2094ce97e15407209a1fd`.
- Independent review found that call-ID binding alone did not bind evidence
  content and that the same Agent cannot reliably run Helm while replacing
  itself.
- Current uncommitted RED/GREEN work requires evidence JSON to match the actual
  runtime tool output, runs structured prompts in fresh doagent sessions,
  declares a distinct `doops-edu/release-runner` control target, and executes
  Helm in a detached Kubernetes Job.
- A real read-only detached Job probe completed on the education cluster using
  the immutable Agent image, Helm, and the in-cluster Kubernetes API; the probe
  Job was removed afterward.
