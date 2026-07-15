# Semantic CI/CD

DoOps CI/CD has one user-facing execution path:

```text
DeploymentTemplate -> Push -> Ask($doops-cicd) -> DeploymentRun
```

The repository workflow declares intent. `doops cicd run` validates the
invocation, pushes the exact Git workspace to the selected Agent, and asks
doagent to execute the `$doops-cicd` Skill. There is no second CI/CD RPC,
generated shell pipeline, or hidden deployment fallback.

## Declaration

Each release workflow is a versioned `DeploymentTemplate`:

```yaml
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: example-release
spec:
  parameters:
    releaseId:
      required: true
  application: example
  release:
    source:
      repository: https://example.test/app.git
      revision: ${inputs.releaseId}
      branch: main
  environment: oilan
  configurationSource: backend/deploy/environments.yaml
```

The declaration contains logical release intent. Physical targets, credentials,
Helm values, registry bindings, and verification requirements remain in the
versioned environment and deployment configuration referenced by the workflow.

## Execution

`doops cicd run` requires:

- an explicit `-session`;
- an explicit configured Gateway `-target`;
- a workflow file inside a clean Git workspace;
- declared `--set key=value` inputs.

The command performs these steps:

1. Push the repository snapshot to `/root/ws/<session>`.
2. Capture the immutable workspace commit returned by the Agent.
3. Send one structured `doops_agent_prompt` request selecting `$doops-cicd`.
4. Bind the request to the pushed workspace commit.
5. Use ordinary Ask for dry-run and the explicitly validated apply operation
   for mutation.
6. Wait for doagent's authoritative `turn_finished` event.

Apply is accepted only for the structured `$doops-cicd` instruction generated
by the CLI. Ordinary prompts cannot request the mutation-capable mode.

## Result

Before its terminal response, doagent writes one JSON artifact to the
Gateway-designated session path:

```json
{
  "apiVersion": "doops.sh/v2",
  "kind": "DeploymentRun",
  "metadata": {
    "workspaceCommit": "0123456789abcdef0123456789abcdef01234567"
  },
  "spec": {
    "mode": "dry-run"
  },
  "status": {
    "phase": "planned",
    "mutationCount": 0,
    "capabilities": {},
    "evidence": []
  }
}
```

The Gateway rejects missing, symlinked, oversized, Markdown-wrapped, or
non-object artifacts. It binds every evidence item to a completed ACP tool call,
adds runtime attestation, and computes `status.resultDigest`.

The CLI accepts only `doops.sh/v2` `DeploymentRun` results whose:

- workspace commit matches the pushed workspace;
- mode matches the requested dry-run or apply operation;
- phase is `planned`, `converged`, `blocked`, `failed`, or `outcome-unknown`;
- mutation count is present and non-negative;
- dry-run mutation count is zero;
- result digest is a valid SHA-256 digest;
- capability snapshot and evidence are present.

Text such as `admitted`, `completed`, or `looks healthy` is not deployment
evidence.

## CLI Artifacts

`scripts/build-cli.sh --all` builds every supported CLI artifact with
deterministic Go build settings and writes
`skills/doops-cli/bin/checksums.txt`.

`scripts/install.sh` verifies the selected prebuilt binary against that
manifest before installation. `scripts/verify-cli-artifacts.sh` rebuilds and
byte-compares every checked-in CLI artifact.
