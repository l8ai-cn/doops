# Oilan DoOps Agent Release Declaration

The Oilan Agent release is declared by:

- environment registry: `deploy/environments.yaml`;
- deployment template: the repository DeploymentTemplate passed to `doops cicd`;
- chart: `deploy/helm/doops-agent`;
- values: `deploy/environments/oilan-values.yaml`.

`deploy/environments.yaml` is authoritative for the physical binding, namespace,
Helm release, values, and health checks. This document must not repeat that
mapping, and it must not be inferred from the Oilan name, a domain, or a
historical cluster alias.

# Registry Credentials

The runtime Secret references are `doops-agent-runtime`,
`doops-agent-settings`, `doops-registry-auth`, and `doops-registry-pull`.

`doops-registry-auth` and `doops-registry-pull` are generated from the same
standard Docker configuration and must both contain an `auths.docker.cnb.cool`
entry:

- `doops-registry-auth` is an `Opaque` Secret with key `config.json`, mounted
  into the Agent for BuildKit push and pull.
- `doops-registry-pull` is a `kubernetes.io/dockerconfigjson` Secret with key
  `.dockerconfigjson`, referenced by the Deployment and bootstrap Job through
  `imagePullSecrets`.

The environment contract requires both Secrets before deployment. This keeps
registry authorization outside Git while keeping Secret references and release
behavior versioned.

# Execution And Verification

`doops cicd run` resolves a `DeploymentPlan` from the declaration, synchronizes
the repository to `/root/ws/<session>`, then invokes Ask. doagent is the sole
executor: it interprets the resolved target profile and artifact contract,
reaches the declared desired state, and verifies every `requiredEvidence` item.

On failure, doagent preserves the failure evidence, restores the last known good
revision, and reports the blocking fact. There is no manual Helm, kubectl, SSH,
shell-script, CNB, or dedicated CI/CD RPC release procedure for this
environment.
