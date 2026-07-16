# Execution Contract

## Native Module Delegation

Discover runtime capabilities before planning. Map each declared responsibility
to an available DoOps module or installed Skill:

- source and repository inspection;
- artifact build, resolution, transfer and readback;
- deployment executor;
- authorization and credential-reference checks;
- workload, endpoint, log and release verification;
- rollback observation or execution when declared.

The coordinator may use multiple native subagents for independent work. It must
not invent module names or assume a module is installed because this reference
mentions a responsibility.

## Modes

### dry-run

- Perform no mutation.
- Observe source identity, artifacts, target state, permissions and declared
  verification endpoints.
- Report each mutation that apply would require.
- Return blocked when a required fact cannot be observed.

### apply

- Require explicit mutation authorization.
- Recheck source and target identity immediately before mutation.
- Execute through runtime modules selected for the declared executor.
- Stop later mutations after any failed gate.
- Observe deployment state after mutation.
- Execute rollback only when the declaration provides a rollback capability and
  the run has actually mutated state.

## Self-Hosted Agent Release

An executor declaring `lifecycle: detached-kubernetes-job` must run Helm only
inside the generated Kubernetes Job. The coordinator may create that one Job
but must not invoke `helm upgrade`, `helm install` or equivalent mutation in
its own process. The Job uses a separately declared, previously verified
executor image and passes the immutable candidate digest only to Helm for the
target workload. It uses the control Agent's declared host paths and exact
workspace chart, then runs `helm upgrade --install --atomic --wait`. Its active
deadline must exceed the Helm timeout by the declared rollback buffer. This
keeps Helm alive while Kubernetes replaces the Agent Pod.
The workflow's control target must use a different Gateway cluster/instance
identity from the workload being replaced. If those identities match, stop
before creating the Job.

## Shared Release Convergence

For a semantic composition, selected-service artifact resolution, target
availability and verification may run concurrently when their declared
dependencies permit. Deployment mutation is different: services sharing one
release require exactly one shared-release convergence.

Before convergence, observe the current image identity for every unselected
service and bind it as `preserve-observed`. Aggregate those identities with the
selected target digests and invoke the deployment executor once.

Artifact mismatch, unreadable target artifact, missing selected target digest,
or missing unselected service identity requires zero deployment executor
invocations. Return `blocked` with the missing or mismatched subject. Do not
silently select all services and do not let a `ServiceRelease` invoke Helm or
another shared-release executor independently.

After convergence, re-observe selected workload image IDs and every unselected
service identity. A changed unselected identity is a failed preservation gate,
not a successful partial release.

## Run Evidence

For an Ask request with `response_format=json`, the Gateway supplies the exact
session-local artifact path. Write exactly one JSON object to that path using a
temporary file in the same directory followed by an atomic rename. The terminal
response is not the machine result. Do not create a YAML result or a second
artifact.

```json
{
  "apiVersion": "doops.sh/v2",
  "kind": "DeploymentRun",
  "metadata": {
    "workflow": "example-release",
    "runId": "generated-by-runtime",
    "workspaceCommit": "immutable-pushed-workspace-identity"
  },
  "spec": {
    "mode": "dry-run",
    "inputs": {
      "version": "release-20260714"
    }
  },
  "status": {
    "phase": "blocked",
    "mutationCount": 0,
    "resultDigest": "sha256:resolved-declaration-and-evidence",
    "capabilities": {
      "source-observer": {
        "status": "available",
        "version": "runtime-reported-version"
      },
      "deployment-executor": {
        "status": "missing"
      }
    },
    "evidence": [
      {
        "subject": "source",
        "module": "runtime-module-name",
        "toolCallId": "runtime-tool-call-id",
        "observedAt": "2026-07-14T00:00:00Z",
        "result": {
          "revision": "immutable-identity"
        }
      }
    ],
    "failures": [
      {
        "code": "required-capability-unavailable",
        "subject": "deployment-executor"
      }
    ]
  }
}
```

Evidence must contain the runtime module identity, runtime `toolCallId`,
observation time, declared subject and structured result. Do not store Secret
values. Every evidence item must reference a completed tool call from this
doagent turn.
The Gateway supplies a session-local runtime tool call catalog alongside the
result path. Read that catalog immediately before writing the result. Copy
`toolCallId` exactly from a completed entry and set `module` exactly to that
entry's `toolName`; semantic aliases and invented identifiers are invalid.
Evidence is eligible only when the completed call carries
`doops.tool-attestation/v1` from the trusted `doops_plan` reconciliation
context. Generic Bash, filesystem, browser and unattested MCP calls are
ineligible. An eligible runtime call must output exactly one JSON object shaped
as `{"subject":"...","observedAt":"...","data":{...}}`. The evidence `result`
must be that complete object without rewriting. The Gateway canonicalizes and
compares it to the actual runtime output, and also requires the outer
`subject` and `observedAt` to match.

`metadata.workspaceCommit` must equal the commit returned by the preceding
workspace push. The Gateway adds `status.runtimeAttestation` from completed ACP
tool-call events and computes `status.resultDigest` from the structured result
plus that attestation. The Agent must not invent either field. A status message
or admission text is never a digest input. `status.capabilities` must be a
non-empty snapshot of the
runtime-discovered capabilities, including availability and version or identity
where reported; missing required capabilities must be represented as blocked
evidence, not hidden by a fallback.

The result is an execution report, not an admission acknowledgement. It must
not use `admitted` as a terminal phase. A dry-run result must set
`mutationCount: 0`; an apply result must report the observed mutation count
and current post-mutation evidence. The runtime attestation binds each evidence
item to an actual completed tool output digest from the same turn.

Allowed phases are `planned`, `converged`, `blocked`, `failed` and
`outcome-unknown`.

## Failure Rules

- Missing declaration: stop and report the exact path.
- Missing runtime module: stop; no fallback to another executor.
- Permission or credential reference unavailable: stop before mutation.
- Artifact mismatch: do not invoke the deployment executor.
- Deployment failure after mutation: collect actual state, then use only the
  declared rollback capability.
- Unknown mutation outcome: report `outcome-unknown`; do not retry or claim
  success without external state evidence.
