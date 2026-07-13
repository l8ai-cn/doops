# Semantic CI/CD

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
    releaseId:
      required: true
  plan:
    release:
      source:
        repository: https://example.test/app.git
        revision: ${inputs.releaseId}
    target:
      environment: oilan
    desiredState:
      application: example
      delivery: immutable-release
      configurationSource: deploy/environments.yaml
      authorization: reconcile
    acceptance:
      requiredEvidence: [source-identity, runtime-state]
      requiredFailureEvidence: [rollback-state]
    policy:
      mutation: require-explicit-approval
      convergence: until-verified
      failureMode: restore-last-known-good
      maxAttempts: 3
      maxNoProgress: 1
```

Declarations must not contain scripts, commands, stages, physical target
coordinates, Helm values, raw registry credentials, or an alternate deployment
route. Those details are resolved from the repository environment registry into
the generated plan.

## Plan

`doops cicd lint` validates the declaration. `doops cicd plan` resolves declared
inputs and the environment profile and emits one `DeploymentPlan` with a
canonical digest. The plan contains:

- immutable source or manifest identity;
- resolved environment profile and its digest;
- artifact contract;
- desired state and acceptance evidence;
- explicit mutation, convergence, rollback, attempt, and no-progress policy.

There is no signing key, public key, private key, attestation RPC, source URL
override, or caller-supplied physical target.

## Execution

`doops cicd run` requires `-session` and `--allow-mutate` unless it is a dry
run. It resolves the plan target from the plan, checks the configured Gateway
binding, then:

1. calls `doops push` for the repository root and session;
2. calls `doops_agent_prompt` with the plan and `response_format=json`;
3. validates the returned `ReconciliationResult`.

The Gateway is transport-neutral: `response_format=json` assigns one
session-scoped result artifact under the synchronized workspace. It removes any
stale artifact before prompting, requires doagent to write exactly one JSON
object atomically, and returns that object as MCP `structuredContent`. Terminal
text is never parsed as the machine result. The Gateway does not interpret
CI/CD fields or implement a second release controller.

The doagent owns semantic execution. It may choose the operational actions
needed to reach the declared state, but cannot replace the plan with a fixed
stage graph, command list, generated shell script, or another target.

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
  "evidence": [],
  "failureEvidence": []
}
```

`status` is one of `converged`, `blocked`, or `failed`.

- `converged` is valid only when every `requiredEvidence` item is present.
- `blocked` and `failed` are valid only when every
  `requiredFailureEvidence` item is present.
- Attempts and no-progress attempts must be within the plan policy.
- Any textual success message, missing evidence, mismatched digest, or invalid
  JSON is a failure, not a successful release.

## CLI Artifacts

`scripts/build-cli.sh --all` builds every supported CLI artifact with
deterministic Go build settings and writes `skills/doops-cli/bin/checksums.txt`.
`scripts/install.sh` verifies the selected prebuilt binary against that
manifest before installing it. `scripts/verify-cli-artifacts.sh` rebuilds and
byte-compares every prebuilt artifact with the checked-in release set.
