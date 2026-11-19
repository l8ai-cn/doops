# DoWeb CLI Reference

All current DoWeb operations run inside the selected DoOps Agent target. Users
normally enter through `doops web`; the script below is the internal skill
entrypoint and does not perform user authentication.

```bash
node backend/agent/skills/doweb/scripts/doweb.mjs <command> [flags]
```

The CLI exits non-zero and prints JSON to stderr on failure:

```json
{
  "status": "failed",
  "error": "..."
}
```

## Common Path Flags

Structured tools require `dist`, `config_file` and `snippet_file` to be relative
to the current DoOps session workspace. Absolute paths and paths escaping that
workspace are rejected; the examples below use workspace-relative inputs.

Use these when running against a non-default layout or a host-mode install:

```bash
--base-dir /var/lib/doweb
--nginx-available-dir /etc/doweb/nginx/sites-available
--nginx-enabled-dir /etc/nginx/conf.d
--nginx-snippet-dir /etc/doweb/nginx/snippets
--nginx-bin nginx
--reload-cmd 'nginx -s reload'
```

Docker mode usually uses deploy flags returned by `install-nginx`.

## `install-nginx`

Prepare the selected target for DoWeb-managed Nginx. Use `--local true`; direct
SSH/password bootstrap is outside the normal user path.

Explicit Docker mode:

```bash
node backend/agent/skills/doweb/scripts/doweb.mjs install-nginx \
  --local true \
  --mode docker
```

Host mode for an existing Nginx on 80/443:

```bash
node backend/agent/skills/doweb/scripts/doweb.mjs install-nginx \
  --local true \
  --mode host
```

Important flags:

- `--mode docker|host`: required for bootstrap; choose the existing runtime explicitly.
- `--local true`: execute bootstrap on the selected target.
- `--dry-run true`: prints the remote script and deploy flags without changing
  the host.
- `--http-port` and `--https-port`: Docker port bindings.
- `--container-name` and `--image`: Docker runtime settings.

The bundled executor rejects SSH and password arguments. Normal operations
reuse the Gateway login, target and grants.

## `site-list`

`node /app/skills/doweb/scripts/doweb.mjs site-list` reads the configured DoWeb `state/` directory and returns `{ "sites": [...] }`. Each record contains `site_id`, `domain`, `current_release`, `previous_release` and `updated_at`. The timestamp belongs to the last recorded deployment or rollback. A missing state directory yields an empty list. Invalid release records and filesystem read errors fail the request.

## `domain-set` and `domain-list`

Record trusted base-domain metadata for automatic subdomain assignment.

```bash
node /app/skills/doweb/scripts/doweb.mjs domain-set \
  --domain example.com \
  --host-ip <agent-host-ip> \
  --provider aliyun

node /app/skills/doweb/scripts/doweb.mjs domain-list
```

`domain-set` does not update provider DNS. It writes DoWeb metadata under
`/var/lib/doweb/domains/<domain>.json`.

## `render`

Render a candidate Nginx config without writing it.

```bash
node /app/skills/doweb/scripts/doweb.mjs render \
  --site-id demo \
  --domain demo.example.com \
  --dist dist \
  --proxy /api/=https://api.example.com
```

Use this before risky changes to inspect the generated server block.

## `deploy`

Publish a completed `dist/` release and activate Nginx config.

Static frontend:

```bash
node /app/skills/doweb/scripts/doweb.mjs deploy \
  --site-id vibe-demo \
  --domain-base example.com \
  --dist dist \
  --health-url http://vibe-demo.example.com/
```

Root reverse proxy:

```bash
node /app/skills/doweb/scripts/doweb.mjs deploy \
  --site-id proxy-106 \
  --domain proxy-106.example.com \
  --dist dist \
  --proxy /=http://<gateway-ip>/ \
  --health-url http://proxy-106.example.com/
```

Load-balanced proxy:

```bash
node /app/skills/doweb/scripts/doweb.mjs deploy \
  --site-id api-demo \
  --domain api-demo.example.com \
  --dist dist \
  --upstream app=http://127.0.0.1:3001,http://127.0.0.1:3002 \
  --proxy /api/=upstream:app
```

TLS by explicit paths:

```bash
node /app/skills/doweb/scripts/doweb.mjs deploy \
  --site-id secure-demo \
  --domain secure.example.com \
  --dist dist \
  --tls-cert /etc/letsencrypt/live/secure.example.com/fullchain.pem \
  --tls-key /etc/letsencrypt/live/secure.example.com/privkey.pem
```

Current TLS behavior: when both `--tls-cert` and `--tls-key` are present, the
rendered server listens on `443 ssl http2`; otherwise it listens on `80`.

## `check`

Validate Nginx and optional health URLs.

```bash
node /app/skills/doweb/scripts/doweb.mjs check \
  --site-id demo \
  --health-url http://demo.example.com/
```

`check` does not alter site state.

## `rollback`

Restore the previous release and rendered config.

```bash
node /app/skills/doweb/scripts/doweb.mjs rollback \
  --site-id demo
```

Rollback fails if there is no previous release or if the previous release files
are missing. It restores activation on validation/reload failure.

## `config-get` and `config-set`

Read or replace the complete managed Nginx config for a site.

```bash
node /app/skills/doweb/scripts/doweb.mjs config-get \
  --site-id demo

node /app/skills/doweb/scripts/doweb.mjs config-set \
  --site-id demo \
  --config-file candidate.conf
```

`config-set` must contain an Nginx `server {}` block. It writes the candidate,
runs `nginx -t`, reloads, and restores the previous config if validation or
reload fails.

The structured `mcp_doops_web` tool also accepts a `config` string, or a `snippet`
string with `snippet_name`, so a natural-language change can create its candidate
without granting shell or general file-writing access. Text and file inputs are
mutually exclusive.

## `snippet-set`

Write a named server-block snippet.

```bash
node /app/skills/doweb/scripts/doweb.mjs snippet-set \
  --site-id demo \
  --name headers \
  --snippet-file headers.conf
```

Snippets are included inside the generated `server` block. They must not contain
`events {}`, `http {}` or `stream {}` blocks.
