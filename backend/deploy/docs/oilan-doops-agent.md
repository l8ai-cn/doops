# Oilan DoOps Agent Release Declaration

The Oilan Agent release is declared by:

- environment registry: `deploy/environments.yaml`;
- deployment template: the repository DeploymentTemplate passed to `doops cicd`;
- chart: `deploy/helm/doops-agent`;
- values: `deploy/environments/oilan-values.yaml`.

`deploy/environments.yaml` is authoritative for the physical binding, namespace,
declared workload, values, and health checks. This document must not repeat that
mapping, and it must not be inferred from the Oilan name, a domain, or a
historical cluster alias.

# Model Routing

The Oilan declaration records `modelRouting.policy: single-model`. The mounted
`doagent-config` ConfigMap remains the source of the model identifier and
provider configuration. At startup the Agent materializes a separate runtime
settings file without `model_tiers`, because this policy intentionally routes
every task class to the one declared model. No mounted configuration object is
rewritten.

# Registry Credentials

The Agent candidate image is public. Its runtime declaration therefore does not
require an image pull or registry credential Secret. A release plan that needs
to mirror a private artifact must declare its credential reference explicitly;
the Agent does not infer one from historical workload configuration.

# Execution And Verification

`doops cicd run` resolves a `DeploymentPlan` from the declaration, synchronizes
the repository to `/root/ws/<session>`, then invokes Ask. doagent is the sole
executor: it interprets the resolved target profile and artifact contract,
reaches the declared desired state, and returns one
`ReconciliationResult`. The CLI accepts the release only after its digest,
attempt bounds, and every `requiredEvidence` item validate against the plan.

On failure, doagent preserves the failure evidence, restores the last known good
revision, and reports `blocked` or `failed` with every
`requiredFailureEvidence` item. There is no manual Helm, kubectl, SSH,
shell-script, CNB, signing key, or dedicated CI/CD RPC release procedure for
this environment.
