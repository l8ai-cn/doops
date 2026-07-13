# doops Model & Deployment Standards

This document defines the model configuration used by `doops ask` through the
embedded doagent runtime.

## Runtime Configuration

`doops-agent` starts doagent as an ACP HTTP service on `127.0.0.1:9000`. The
effective runtime settings are stored at:

```text
/root/.agent/runtime-settings.json
```

Kubernetes mounts `/opt/doagent_config/settings.json` from the
`doagent-model-settings` Secret. The entrypoint materializes that mounted
Secret into the runtime path without modifying the Secret. The public
`doops.sh` repository keeps only a placeholder Secret template;
environment-specific deployments must generate the real Secret from their
private secret store. Do not commit real API keys to the public repository.

## Kubernetes Secret Deployment

`doagent-model-settings` is the required model configuration object for
`doops ask`. It must exist before the `doops-agent` Pod starts:

```bash
kubectl create namespace doops-system --dry-run=client -o yaml | kubectl apply -f -

# Public template, suitable only after replacing the placeholder apiKey.
kubectl -n doops-system apply -f agent/agent-config.yaml

# In production, apply the environment-specific Secret from the private store.
# kubectl -n doops-system apply -f /path/to/private/doagent-model-settings.yaml

kubectl -n doops-system apply -f agent/agent.yaml
kubectl -n doops-system rollout status ds/doops-agent --timeout=180s
```

The Secret must provide:

```text
data.settings.json
```

with a configured `provider.minimax.options.apiKey`. The image must not supply
a second provider, infer a credential, or reset the selected model during an
upgrade.

## Standard Endpoint

```text
https://api.minimaxi.com/anthropic
```

## Standard Model

| Task Type | Model ID |
|-----------|----------|
| Default / Coding / Summary | `minimax/MiniMax-M3` |

## Troubleshooting

If `doops ask` fails:

1. Check `DO_AGENT_URL`, default `http://127.0.0.1:9000`.
2. Verify `/usr/local/bin/do-agent --help` works inside the final image.
3. Verify the mounted `doagent-model-settings/settings.json` has a configured
   provider and non-empty API key without printing it.
4. Run `doops exec` against the same target to confirm the gateway fast path is
   healthy.
