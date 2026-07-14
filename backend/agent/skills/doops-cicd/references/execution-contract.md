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

## Run Evidence

Write a run-local YAML result, for example:

```yaml
apiVersion: doops.sh/v2
kind: DeploymentRun
metadata:
  workflow: example-release
  runId: generated-by-runtime
spec:
  mode: dry-run
  inputs:
    version: release-20260714
status:
  phase: blocked
  mutationCount: 0
  evidence:
    - subject: source
      module: runtime-module-name
      observedAt: "2026-07-14T00:00:00Z"
      result:
        revision: immutable-identity
  failures:
    - code: required-capability-unavailable
      subject: deployment-executor
```

Evidence must contain the runtime module identity, observation time, declared
subject and structured result. Do not store Secret values.

The result is an execution report, not an admission acknowledgement. It must
not use `admitted` as a terminal phase. A dry-run result must set
`mutationCount: 0`; an apply result must report the observed mutation count
and current post-mutation evidence. The digest, when present, must be derived
from the resolved declaration and these structured observations, never from
status text alone.

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
