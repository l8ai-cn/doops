---
name: doweb-agent
description: DoWeb 网站发布与运维助手。用于通过 doagent 管理 Nginx 站点发布、TLS、反向代理、静态资源部署、站点健康检查、日志巡检和回滚。
metadata:
  family: executable
  runtime: node
  entrypoint: scripts/doweb.mjs
---

# DoWeb Agent

DoWeb is a skill inside the DoOps Agent runtime. The user authenticates once to
the DoOps Gateway, selects a target, and asks doagent to publish or operate a
website. DoWeb does not implement login, target grants, Gateway token parsing or
a second credential store.

DoWeb's MVP deploys a customer-built dist directory. It does not build source
code on the server. The user compiles locally or in CI, then DoWeb publishes the
finished static artifact into an Nginx-backed web container.

## Atomic Tooling

Use structured `mcp_doops_web` for target operations and `scripts/doweb.mjs` as
the deterministic implementation behind that tool. Do not edit live Nginx
files by hand or use Bash/SSH to bypass the structured tool.
The script runs on the selected DoOps target. It must not receive SSH host,
username, password or user token from a prompt.

```bash
node /app/skills/doweb/scripts/doweb.mjs install-nginx \
  --local true \
  --mode docker

node /app/skills/doweb/scripts/doweb.mjs domain-set \
  --domain example.com \
  --host-ip <agent-host-ip> \
  --provider aliyun

node /app/skills/doweb/scripts/doweb.mjs deploy \
  --site-id demo \
  --domain-base example.com \
  --dist dist \
  --proxy /api/=https://api.example.com
```

`dist` is relative to the current DoOps session workspace. Push or otherwise
prepare that workspace before publishing; absolute paths and paths escaping the
workspace are rejected.

Supported commands:

- `render`: render a candidate Nginx server block from a SiteSpec.
- `install-nginx --local true`: bootstrap Nginx on the selected target. The
  installation mode must be explicit. Docker mode runs the
  `doweb-nginx` container, and create `/usr/local/bin/doweb-nginx` for later
  `nginx -t` validation. Use `--mode host` explicitly when a host already runs
  Nginx on 80/443. SSH bootstrap is a separate first-install procedure and is
  not a normal DoWeb user entrypoint.
- `domain-set`: register a usable base domain such as `example.com` with its
  host IP and provider metadata.
- `domain-list`: list registered base domains.
- `deploy`: copy a customer-built dist release, activate `current`, write Nginx
  config, run `nginx -t`, reload, and optionally run health checks.
- `config-get`: read the current managed Nginx config.
- `config-set`: replace a managed Nginx config through the same validation and
  reload path. This is the direct Nginx config editing path.
- `snippet-set`: write a site-level snippet included inside the server block.
- `check`: run `nginx -t` and optional HTTP checks.
- `rollback`: restore the previous release and its rendered config.
  Repeated rollback stops at the oldest release; it never selects a newer
  release because no earlier version exists.

## Intended Capabilities

- Create or update Nginx server blocks from a SiteSpec.
- Publish customer-built dist artifacts.
- Configure static SPA fallback.
- Configure reverse proxy routes such as `/api/`.
- Configure named upstreams for load balancing.
- Configure TLS certificate paths when provided.
- Bootstrap Nginx on the target through `install-nginx --local true`.
- Maintain domain resources with `domain-set` and allocate subdomains from
  `--domain-base` while checking for site conflicts.
- Support controlled Nginx edits through `config-set` and `snippet-set`.
- Validate `nginx -t`, reload Nginx and verify HTTP/HTTPS.
- Inspect access/error logs and site health.
- Run rollback when a release or config change fails.

Default mode is cautious: validate before reload, back up config before writing,
and return evidence instead of assuming success.

## Required Safety Rule

DoWeb has no silent fallback. If `nginx -t`, reload, health check, artifact copy,
or rollback fails, retain the failed operation's result. Inspect the cause,
correct it within the requested task and verify the result, without asking the
user to approve the same task again. Do not mask a broken change by silently
switching to a different path or skipping validation.

## Resource Authority

- The selected Gateway target is the only target authority. Read the live
  DoWeb domain registry through `mcp_doops_web_read operation=domain-list` and
  site configuration through `config-get` on that target.
- Do not ship example sites, DNS addresses or health statuses as live inventory.
  Validate DNS, Nginx, TLS and HTTP health before reporting a site as ready.

## References

- `docs/README.md`
- `docs/cli-reference.md`
- `docs/site-lifecycle.md`
- `docs/dns-and-routing.md`
- `docs/troubleshooting.md`
- `docs/remote-operations.md`
- `references/nginx-site-model.md`
- `references/change-safety.md`

## Operating Notes

- When a browser or user reports an unreachable site, separate DNS from Nginx
  with `curl --resolve <domain>:80:<gateway-ip> http://<domain>/`.
- Watch for wildcard or multi-A DNS records. Every returned address must serve
  the same hostname, or users may see intermittent failures.
- HTTPS is only supported by the current CLI when explicit `--tls-cert` and
  `--tls-key` paths are provided and the target Nginx owns the matching 443
  server block. Existing stream listeners on 443 can intercept HTTPS before a
  DoWeb server block sees it.
- The current CLI has no first-class `delete` command. For removal tests, use
  the reversible disable procedure in `docs/site-lifecycle.md`.

## Unified Authorization Boundary

- Gateway authenticates the user and checks the grant on the exact
  `cluster/instance`. One DoOps Agent is one authorization unit: a grant opens
  every operation on that target, and `list`/`ask`/`exec` are internal action
  labels the Gateway derives from the call, never separate permissions.
- DoWeb receives only the selected target, site request and optional managed
  credential resource names. Passwords, SSH keys and Gateway tokens never enter
  SiteSpec, state JSON, prompts or logs.
- `skill=doweb` is the explicit marker for unified website requests. A read-only
  health or config inspection does not need a second login. Website mutation
  requires `operation=apply` with this exact skill marker, or an explicitly
  authorized deterministic tool. Ordinary apply cannot call website mutation
  tools without the Nginx resource lease, and website apply cannot call Helm.
  The explicit operation carries execution intent; there is no additional
  confirmation field or per-call user approval.
- DoWeb evidence includes SiteSpec, candidate config, `nginx -t`, reload, health
  and rollback status. The Gateway target grant and DoWeb validation
  evidence are separate concerns.
