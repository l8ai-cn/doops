# Oilan DoOps Agent Release Declaration

The shared Agent-native CI/CD boundary is documented in
[`AGENT_NATIVE_CICD.md`](../../docs/AGENT_NATIVE_CICD.md).

The Oilan Agent release is declared by:

- environment registry: `backend/deploy/environments.yaml`;
- deployment template: the repository DeploymentTemplate passed to `doops cicd`;
- chart: `backend/deploy/helm/doops-agent`;
- values: `backend/deploy/environments/oilan-values.yaml`.

`backend/deploy/environments.yaml` is authoritative for the physical target,
typed Helm executor, verification profile, and artifact contract. This document
must not repeat that mapping, and it must not be inferred from the Oilan name, a
domain, or a historical cluster alias.

The Deployment reads the gateway registration token from the declared
`gateway.agentTokenSecret` reference and passes it to `doops-agent`. The token
is bound to the declared gateway cluster and instance.

# Model Routing

Model routing is not duplicated in the CI/CD environment registry.
`backend/deploy/environments/oilan-values.yaml` declares
`modelRouting.policy: single-model` and references only the
`doagent-model-settings/settings.json` Secret. The Secret remains the source of
provider configuration and credentials. At startup the Agent materializes a
separate runtime settings file without `model_tiers`, because this policy
intentionally routes every task class to the one configured model. An image
upgrade cannot replace, synthesize, or persist model credentials.

# Registry Credentials

The Agent candidate image is public. Its runtime declaration therefore does not
require an image pull or registry credential Secret. A release plan that needs
to mirror a private artifact must declare its credential reference explicitly;
the Agent does not infer one from historical workload configuration.

# Execution And Verification

`doops cicd run` resolves a `DeploymentPlan` from the declaration, synchronizes
the clean repository at the exact declared commit to `/root/ws/<session>`, then
invokes Ask with machine-readable reconciliation metadata, including the exact
workspace commit produced by the push. The Gateway authorizes every executable
agent prompt as `ActionReconcile`; `ActionAsk` is reserved for read-only
metadata and history. After acquiring the workspace lock, the agent verifies
that commit against `.doops-ready` before starting doagent. doagent is the sole executor: it
interprets the resolved target, Helm executor, artifact contract, and
verification profile, reaches the declared desired state, and returns one
`ReconciliationResult`. The CLI accepts the release only after its digest and
every `requiredEvidence` item validate against the plan. The bridge waits for
doagent's authoritative `turn_finished`, injects `executionEvidence` from real
ACP tool events, and binds evidence to that trace before the CLI validates it.

The doops-agent sets native doagent mode `plan` for dry runs and `build` for
authorized apply requests. Planning, multi-Agent delegation, Skill composition,
and tool selection remain inside doagent. The bridge does not auto-approve
permissions.

On failure, doagent reports only observations it actually collected. A
pre-mutation capability or permission block does not fabricate recovery
evidence. Recovery is attempted only when mutation occurred and the resolved
environment explicitly provides a reversible recovery capability; otherwise
the result remains honestly `blocked` or `failed`. There is no manual Helm,
kubectl, SSH, shell-script, CNB, signing key, or dedicated CI/CD RPC release
procedure for this environment.
