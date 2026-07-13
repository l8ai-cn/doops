# Semantic CI/CD

The architecture and ownership boundary are defined in
[`AGENT_NATIVE_CICD.md`](AGENT_NATIVE_CICD.md). This document describes the
`doops.sh/v2` data contract and CLI behavior.

DoOps CI/CD has one declarative reconciliation loop:

```text
DeploymentTemplate -> DeploymentPlan -> Push -> Ask -> ReconciliationResult
```

The template describes intent. `deploy/environments.yaml` resolves the physical
environment profile. The plan is the immutable execution contract. `doops push`
creates the session workspace, and `doops_agent_prompt` asks doagent to
reconcile that plan. The CLI accepts completion only after it validates the
structured result against the same plan.

## Declaration

Each repository keeps a versioned `DeploymentTemplate`.

```yaml
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: example-release
spec:
  parameters:
    version:
      required: true
    reason:
      required: true
  application: example
  release:
    version: ${inputs.version}
  environment: oilan
  configurationSource: deploy/environments.yaml
```

Declarations must not contain scripts, commands, stages, physical target
coordinates, Helm values, raw registry credentials, or an alternate deployment
route. The parser uses a strict schema, so unknown fields fail with their YAML
field name. Physical details are resolved from the repository environment
registry into the generated plan.

The environment registry owns three separate concerns:

```yaml
artifactContract:
  type: image-set
  sourceRegistry: docker.example.test/example
  sourceRepository: https://example.test/app.git
  sourceBranch: main
  services: [api]
  imageTagPattern: "^release-[0-9]{8}$"
  imageTagTimeZone: Asia/Shanghai

verificationProfiles:
  production:
    requiredEvidence: [source-identity, runtime-state]

environments:
  oilan:
    target:
      name: gw-oilan
      cluster: oilan
      instance: worker-1
    executor:
      type: helm
      config:
        namespace: app
        release: example
        chart: deploy/chart
        values: deploy/values.yaml
        registry: registry.example.test/app
        imageBindings:
          api: api
    verificationProfile: production
```

`target` is the security boundary, `executor` contains implementation-specific
configuration, and `verificationProfile` selects the environment-owned success
checks. Application runtime settings remain in the executor's referenced
Chart/values/Secret sources rather than being duplicated in the CI/CD registry.
The generic compiler does not special-case application names.

## Plan

`doops cicd lint` validates the declaration. `doops cicd plan` resolves declared
inputs and the environment profile and emits one `DeploymentPlan` with a
canonical digest. The plan contains:

- release selector: image version, source revision, or manifest reference;
- requested image versions are validated against the declared image tag pattern;
- resolved environment profile and its digest;
- a release-kind-specific artifact contract;
- the typed executor configuration;
- desired application state and resolved verification evidence.

There is no signing key, public key, private key, attestation RPC, source URL
override, caller-supplied physical target, or template-defined execution policy.
Mutation authorization comes from the `cicd run` request, not from a ceremonial
field copied into the plan.

For an `image-set` release, `release.version` is the deployment selector. The
same version may be reconciled repeatedly. Source revision and image digest may
be recorded by the build system for troubleshooting, but neither is required as
a deployment input and neither blocks another deployment of that version.

## Execution

`doops cicd run` requires `-session` and `--allow-mutate` unless it is a dry
run. `--dry-run` and `--allow-mutate` are mutually exclusive. For source
releases, the local repository must be clean and its exact `HEAD` must equal the
declared immutable revision. It then resolves the plan target, checks the
configured Gateway binding, and:

1. calls `doops push` for the repository root and session;
2. calls `doops_agent_prompt` with a minimal JSON task envelope containing the
   selected `semantic-deployment` Skill, execution mode, and complete plan,
   plus machine-readable `operation=reconcile`, the plan digest, and the exact
   workspace commit produced by that push;
3. validates the returned `ReconciliationResult`.

The Gateway is transport-neutral: `response_format=json` assigns one
session-scoped result artifact under the synchronized workspace. It removes any
stale artifact before prompting, requires doagent to write exactly one JSON
object atomically, and returns that object as MCP `structuredContent`. Terminal
text is never parsed as the machine result. The Gateway does not interpret
CI/CD fields or implement a second release controller.

The prompt does not duplicate the Skill with stages, commands, retries, recovery
instructions, or tool names. The doops-agent maps each request to a native
doagent mode and resets that mode before every prompt:

| Request | Native mode |
| :--- | :--- |
| general Ask | `auto` |
| reconciliation dry run | `plan` |
| reconciliation apply | `build` |

The Gateway validates the reconciliation metadata and authorizes every
executable agent prompt as `ActionReconcile`; `ActionAsk` is limited to
read-only metadata and history requests. Push and reconciliation use the same
workspace resource lock. After the reconciliation lock is acquired, the agent
compares the requested workspace commit with `/root/ws/<session>/.doops-ready`
before it starts doagent, so a concurrent push cannot silently replace the
source being deployed.


The doagent owns semantic execution, including context management, planning,
multi-Agent delegation, Skill composition, and tool selection. It may choose
the operational actions needed to reach the declared state, but cannot replace
the plan with a fixed stage graph, command list, generated shell script, or
another target.

The doops-agent never answers `permission.updated` on behalf of doagent or the
user. An unexpected permission request fails the operation with the observed
permission details. Permission policy remains in native doagent modes and
runtime configuration.

`session/prompt` success is admission only. The bridge waits for the
authoritative `turn_finished` event; earlier `agent_message` and `usage_update`
events cannot complete the operation. Admission errors are returned directly
instead of being hidden behind an SSE timeout.

For reconciliation, the bridge hashes the real terminal ACP tool events in
their original SSE order and injects an `executionEvidence` object containing
the turn ID, source revision, exact workspace commit, tool summaries, and a
trace digest. Every Agent-authored evidence item supplies a `toolRef` with the
exact tool name and its one-based ordinal among terminal events for that tool.
The bridge resolves the reference, accepts only completed observation calls,
and injects the real `toolCallId` and `toolDigest`; the CLI recomputes the trace
and cross-checks both identities and bindings.

The target image bundles the `semantic-deployment` Skill. Structured
`DeploymentPlan` requests select that Skill, which defines the desired state,
required evidence, recovery boundary, and `ReconciliationResult` contract
without embedding a command workflow in the declaration.

## Result

The machine result artifact is one object:

```json
{
  "apiVersion": "doops.sh/v2",
  "kind": "ReconciliationResult",
  "planDigest": "sha256:...",
  "status": "converged",
  "attempts": 2,
  "noProgressAttempts": 0,
  "evidence": [
    {
      "kind": "runtime-state",
      "subject": "service",
      "observedAt": "2026-07-13T00:00:00Z",
      "value": "ready",
      "toolRef": {
        "tool": "WebFetch",
        "ordinal": 1
      }
    }
  ],
  "failureEvidence": []
}
```

After the authoritative `turn_finished`, the bridge replaces each `toolRef`
with the observed `toolCallId`, `toolDigest`, and `traceDigest`, then adds
`executionEvidence` containing `turnId`, `sourceRevision`, `workspaceCommit`,
and the terminal tool events in original SSE order.

`status` is one of `converged`, `blocked`, or `failed`.

- `converged` is valid only when every `requiredEvidence` item is present.
- `converged` also requires a completed observation tool call and a valid
  bridge-generated execution trace bound to the pushed workspace commit.
- Each evidence item must bind to its own matching completed observation call.
  Missing, out-of-range, failed, generic execution, write, and other
  non-observation tool references are rejected even if another observation
  call completed in the same turn.
- `blocked` and `failed` require actual observed `failureEvidence`; unavailable
  evidence must not be invented.
- `rollback-state` is valid only when mutation occurred, the environment
  declared a reversible recovery capability, and recovery was actually
  attempted and observed.
- Attempts are observations reported by the agent. The CLI checks that they are
  positive and internally consistent; it does not pretend they are a host-side
  retry controller.
- Any textual success message, missing evidence, mismatched digest, or invalid
  JSON is a failure, not a successful release.

## CLI Artifacts

`scripts/build-cli.sh --all` builds every supported CLI artifact with
deterministic Go build settings and writes `skills/doops-cli/bin/checksums.txt`.
`scripts/install.sh` verifies the selected prebuilt binary against that
manifest before installing it. `scripts/verify-cli-artifacts.sh` rebuilds and
byte-compares every prebuilt artifact with the checked-in release set.
