---
name: doweb-unified-runtime
scope: website-operations
status: source-validation
---

# DoWeb In The Unified Workbench

DoWeb runs inside `doops.sh/backend/agent/skills/doweb`. Users use the existing
Gateway account, Console, target routes and `doops` executable. DoWeb does not
have its own login, user store, token cache or target registry.

Mutation tools are scoped by the explicit skill: ordinary workload apply may use
Helm, while `skill=doweb` may use the website tool, not Helm. DoSql and unrecognized
skills do not inherit either mutation tool. This does not make the unfinished
restricted DoSql database execution path available.

```bash
doops -session website-check web --target <target> --mode check --runtime host \
  --site demo --request '检查站点的 Nginx 配置和 HTTP 状态'

doops -session website-plan web --target <target> --mode plan --runtime host \
  --site demo --domain demo.example.com --dist dist \
  --request '生成网站发布计划、验收条件和回滚方案'

doops -session website-release web --target <target> --mode apply --runtime host \
  --site demo --domain demo.example.com --dist dist \
  --request '发布当前 session 的 dist；执行 nginx -t、reload 和指定健康检查'
```

`dist` is relative to the current session workspace. Transfer a customer-built
artifact with `doops push` when needed; DoWeb does not build source code on a
business target. Choose Docker explicitly for a Docker Nginx instance. Missing
tools, wrong runtime modes and missing assets fail rather than changing target
or execution path. Nginx bootstrap requires an explicit runtime mode.

## Authorization Ownership

| Entry | Authentication | Authority | Execution And Evidence |
| --- | --- | --- | --- |
| Console website page | Existing HttpOnly Gateway session | Gateway target grants | Same prompt transport and audit |
| `doops web` | Existing Gateway auth store | Same target grants | Shared CLI skill prompt client, also used by `doops sql` |
| Inspection/plan prompt | Gateway user | `ask` on the target | Only `mcp_doops_web_read` reads website state |
| Publish/config/rollback prompt | Gateway user | `exec` and explicit `apply` | `mcp_doops_web`, runtime attestation |
| Deterministic `doops_web` RPC | Gateway user | `ask` for reads, `exec` for changes | Same Agent executor and outcome lifecycle |
| Credential reference | Existing managed resource | Existing exact credential grant/use | Gateway materialization; no payload in the skill |

The explicit operation records the intended change; there is no confirmation
field or second approval step. The MCP session is bound by the runtime and
excluded from the model-visible schema.
Prompt mutations require the exact `skill=doweb` marker so the Gateway acquires
the shared Nginx resource lease. Generic apply exposes Helm, not website
mutation tools; DoWeb apply exposes website, not Helm mutation tools.
Config/snippet text can be supplied to the structured tool; files are limited
to the current session, including symlink resolution. The exact Nginx resource
lease protects shared activation/reload without excluding other target sessions.

The website tool uses already mounted TLS certificate/key paths. It does not
download credential payloads, read Kubernetes Secret values or turn a workload
credential grant into a host-wide permission.

## Target Layout

The Agent image includes Node.js and the frozen DoWeb script. The target must
make the selected Nginx instance or Docker wrapper and managed directories
accessible from the Agent. A Gateway route does not mount a host's Nginx
filesystem into an Agent container.

For a custom or mounted layout the operator sets `DOWEB_RUNTIME_CONFIG` to a
JSON file. This is runtime configuration, not a credential or permission store.
Unsupported fields fail explicitly. Example:

```json
{
  "base-dir": "/var/lib/doweb",
  "nginx-available-dir": "/etc/doweb/nginx/sites-available",
  "nginx-enabled-dir": "/etc/nginx/conf.d",
  "nginx-snippet-dir": "/etc/doweb/nginx/snippets",
  "nginx-bin": "/usr/local/bin/doweb-nginx",
  "reload-argv": ["/usr/local/bin/doweb-nginx", "-s", "reload"]
}
```

These settings belong in the target's versioned deployment configuration and
are never accepted from model-generated MCP parameters. `DOWEB_SCRIPT` can name
the script in a local source checkout; images use
`/app/skills/doweb/scripts/doweb.mjs`.

## Acceptance

1. One Gateway login can execute both domain CLI commands. Denied actions or
   targets are rejected and recorded by the existing Gateway.
2. `web_read` rejects mutations; `web` executes an authorized operation without
   a confirmation field and rejects supplied session and workspace escapes.
3. A real disposable Nginx serves release A, then B, then A after rollback.
4. Invalid Nginx config and failed HTTP health remain failures; restored files
   are validated and reloaded and the old artifact remains served.
5. A snippet can supply candidate text without Bash/file-write privileges.
6. The Console supports runtime selection, direct submission of explicit
   operations, disabled controls while running, failure states and target isolation.
7. Failed evidence persistence restores both the previous activation and
   release-state index. Supplied snippet health checks are enforced. Failure
   to validate/reload the restored activation produces an unknown outcome,
   not an ordinary completed failure that can be replayed.
8. Rollback follows only the recorded previous release and stops at the oldest
   release. Snippet activation checks Nginx's loaded configuration, not just a
   successful write of an unused file.

The real-Nginx test is `backend/agent/internal/server/web_tool_test.go`.
Local verification is separate from production rollout. Deploy immutable CI
artifacts and verify target runtime/mounts and real user paths before reporting
online availability.
