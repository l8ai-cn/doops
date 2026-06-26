# doops Model & Deployment Standards

This document defines the model configuration used by `doops ask` through the embedded doagent runtime.

## Runtime Configuration

`doops-agent` starts doagent as an ACP HTTP service on `127.0.0.1:9000`. doagent reads its model configuration from:

```text
/root/.agent/settings.json
```

Kubernetes deployments mount this file from the `doagent-config` ConfigMap. The public `doops.sh` repository keeps only a placeholder template; environment-specific deployments should generate the real ConfigMap from the private secret repository, for example `https://cnb.cool/l8ai/l8ai-secret`. Do not commit real API keys to the public `doops.sh` repository.

## Kubernetes ConfigMap Deployment

`doagent-config` is the standard configuration object for `doops ask`. It must exist before the `doops-agent` Pod starts:

```bash
kubectl create namespace doops-system --dry-run=client -o yaml | kubectl apply -f -

# Public template, suitable only after replacing the placeholder apiKey.
kubectl -n doops-system apply -f agent/agent-config.yaml

# Or, in production, apply the environment-specific ConfigMap generated from l8ai-secret.
# Example shape:
# kubectl -n doops-system apply -f /path/to/l8ai-secret/doops/doagent-config.yaml

kubectl -n doops-system apply -f agent/agent.yaml
kubectl -n doops-system rollout status ds/doops-agent --timeout=180s
```

The ConfigMap must provide:

```text
data.settings.json
```

with a non-empty `provider.openai.options.apiKey`.

## Standard Endpoint

Use the token gateway without an explicit port:

```text
https://api.example.com
https://api.example.com/v1
```

## Standard Models

| Task Type | Model ID |
|-----------|----------|
| Default / Coding | `openai/gpt-5.4` |
| Lightweight summary | `openai/gpt-5.4-mini` |

## Standalone Fallback Environment Variables

The recommended Kubernetes deployment path is the `doagent-config` ConfigMap. The entrypoint still supports generating a minimal `settings.json` from environment variables for standalone Docker or emergency repair:

| Variable | Meaning |
|----------|---------|
| `OPENAI_API_KEY` | API key for the `openai` provider |
| `API_BASE_URL` | OpenAI-compatible base URL, default `https://api.example.com/v1` |
| `DO_AGENT_MODEL` | Default model, default `openai/gpt-5.4` |

## Troubleshooting

If `doops ask` fails:

1. Check `DO_AGENT_URL`, default `http://127.0.0.1:9000`.
2. Verify `/usr/local/bin/do-agent --help` works inside the final image.
3. Verify `/root/.agent/settings.json` contains a configured provider and non-empty API key.
4. Run `doops exec` against the same target to confirm the gateway fast path is healthy.
