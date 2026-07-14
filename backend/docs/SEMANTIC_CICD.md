# Declarative CI/CD

The current CI/CD contract is documented in
[`AGENT_NATIVE_CICD.md`](AGENT_NATIVE_CICD.md) and implemented by the
`$doops-cicd` Skill.

The execution path is:

```text
DeploymentTemplate -> doops push -> Ask -> doagent -> doops-cicd -> DeploymentRun
```

Use an explicit target and exactly one execution mode:

```bash
doops -session <session> cicd run \
  -f backend/deploy/workflows/example.yaml \
  -target <configured-target> \
  --dry-run
```

or:

```bash
doops -session <session> cicd run \
  -f backend/deploy/workflows/example.yaml \
  -target <configured-target> \
  --allow-mutate
```

The CLI pushes the repository snapshot, invokes ordinary `doops_agent_prompt`,
and asks doagent to run `$doops-cicd`. The Skill uses existing DoOps modules and
writes a run-local `DeploymentRun` YAML. `planned`, `converged`, `blocked`,
`failed`, and `outcome-unknown` are valid phases; `admitted` is not a result.
The Gateway does not synthesize `executionEvidence`; only structured facts
written by the Skill or returned by existing modules can support validation.
Gateway-generated `toolCallId` and trace binding are intentionally absent from
this contract; the removed bridge's `toolDigest` is absent as well.

No dedicated reconciliation MCP, Go release controller, shell/kubectl/SSH
adapter, or manual deployment event is part of this contract.
