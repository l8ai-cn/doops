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

For an `image-set` release, the compiler resolves the declared source tag to
the declared deployment platform manifest digest. Oilan declares
`linux/amd64`; that child manifest digest is the only digest permitted in the
source reference, target reference, Helm values, and runtime image identity.
An OCI index digest remains provenance only. Each target repository is read
from the exact declared image binding path
`<binding>.image.repository`; an explicit empty binding denotes
`image.repository`. The service key is the source artifact name unless
`sourceArtifactNames` declares a different name. No repository, tag, digest,
or values path is guessed.

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

`doops cicd run` requires an explicit target and exactly one of `--dry-run` or
`--allow-mutate`. It synchronizes the repository to `/root/ws/<session>` with
`doops push`, then invokes ordinary Ask with the `$doops-cicd` Skill and the
resulting workspace commit. The Gateway uses the normal `ActionAsk` permission
and session resource lock; it has no reconciliation-only admission path.

doagent is the sole Agent executor. It selects existing DoOps modules, observes
the declared target, and writes a run-local `DeploymentRun` YAML. `planned`,
`converged`, `blocked`, `failed`, and `outcome-unknown` are valid phases.
`admitted` text, generic attestation, or an Agent claim without structured
observed data is not evidence. Dry-run mutation count must be zero.

The bridge waits for doagent's authoritative `turn_finished` event and does
not answer permission requests or synthesize deployment evidence. The
reconciliation-only Go control plane, `doops_plan` MCP, dedicated CICD tools,
shell/kubectl/SSH adapters, and manual CNB deployment events are removed.
