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
`doops-agent-settings` Secret remains the only source of the model identifier
and provider credentials. At startup the Agent materializes a runtime copy of
that settings document without `model_tiers`, because this policy intentionally
routes every task class to the one declared model. The mounted Secret is never
rewritten, and a persisted `/root/.agent/settings.json` cannot override it.

# Registry Credentials

The runtime Secret references are `doops-agent-runtime`,
`doops-agent-settings`, and `doops-registry-pull`.

`doops-registry-pull` is a `kubernetes.io/dockerconfigjson` Secret with key
`.dockerconfigjson` and an `auths.docker.cnb.cool` entry. It is referenced by
the declared Deployment through `imagePullSecrets` and mounted into the Agent
as `/root/.docker/config.json` for BuildKit push and pull.

The environment contract requires this Secret before deployment. This keeps
registry authorization outside Git while keeping its one reference and release
behavior versioned.

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
