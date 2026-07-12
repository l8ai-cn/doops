# Inputs

- Helm release: `doops-agent`
- Namespace: `doops-system`
- Candidate image: `docker.cnb.cool/l8ai/ai/doops.sh:<releaseId>`
- Runtime Secret references: `doops-agent-runtime`, `doops-agent-settings`, and `doops-registry-auth`

# Deployment

The versioned `DeploymentTemplate` is reconciled by a registered DoOps Agent.
The agent resolves the immutable source and environment registry, builds the
candidate image, and creates the bootstrap Job. Its candidate image contains
the chart and Helm binary. The Job first adds Helm ownership metadata to the
existing Deployment, then runs `helm upgrade --install` with
`deploy/environments/oilan-values.yaml` and the immutable `releaseId` image
tag. Each release creates a distinct Job and must wait for that Job's result.

The chart supplies the public TLS gateway URL and cluster identity as ordinary
configuration. The registration token remains a Kubernetes Secret reference;
no token or endpoint is embedded in the Deployment command.

# Rollback

Use the Helm release history in `doops-system`:

```bash
helm -n doops-system history doops-agent
helm -n doops-system rollback doops-agent <revision> --wait --timeout 10m
```

# Verification

```bash
helm -n doops-system status doops-agent
kubectl -n doops-system rollout status deployment/doops-agent --timeout=10m
kubectl -n doops-system get deployment doops-agent -o jsonpath='{.spec.template.spec.containers[0].image}'
```
