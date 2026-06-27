# Doops Usage Guide

This guide holds command-level usage details. The skill file is only the entry point.

## Command Choice

| Need | Command | Notes |
| :--- | :--- | :--- |
| List configured targets | `doops list` | Local config only; not live gateway state. |
| List live gateway agents | `doops targets --target <gateway-target>` | First check for gateway troubleshooting. |
| Run deterministic shell | `doops -session <s> exec --target <t> --cmd '<cmd>'` | Best for known checks and deploy scripts. |
| Ask the edge agent to reason | `doops -session <s> ask --target <t> --msg '<task>'` | Best for diagnostics and multi-step investigation. |
| Push a local directory | `doops -session <s> push --target <t> --src <dir>` | Syncs into `/root/ws/<session>`. |
| Pull a remote workspace | `doops -session <s> pull --target <t> --dest <dir>` | Use for large files and binary assets. |
| Read small text | `doops -session <s> read --target <t> --path <file>` | Small text only. |
| Write small text | `doops -session <s> write --target <t> --path <file> --file <local>` | Also supports `--content` and stdin. |
| Clean workspace | `doops -session <s> clean --target <t>` | Cleans `/root/ws/<session>`. |
| Cache token | `doops login ...` | Gateway login can exchange username/password for user token. |
| Issue token as admin | `doops admin token create ...` | Requires a gateway token with `admin` permission. |
| Remove cached token | `doops logout --target <t>` | Removes local token cache. |

## Basic Checks

```bash
doops list
doops targets --target unicom
doops -session smoke exec --target unicom --cmd 'hostname && date -u +%FT%TZ'
```

`doops list` reads local config. `doops targets` asks gateway for live connected agents. A target can be listed locally but offline in gateway.

`doops targets` separates target-wide busy from normal per-session activity:

- `busy=true` means the whole target is blocked by an exclusive operation or target queue, so new work may return `target busy`.
- `status=active` with `busy=false` means one or more sessions/resources are running, but other sessions can still use the same agent.
- `resources` shows locked resource keys such as `workspace:<session>` or `session:<session>`.

If gateway returns `user operation limit exceeded`, list active gateway operations and cancel stale ones with an admin token:

```bash
doops admin operations list --target <gateway-target>
doops admin operations cancel --target <gateway-target> --id <operation-id>
```

On upgraded agents, cancel terminates local shell-style commands by process group. For `ask`, treat cancel as stopping the doops waiting path first; verify the remote session if doagent already started work.

## Gateway Admin Tokens

给用户签发 token 时，优先用 CLI 管理入口，不要 SSH 到 gateway 主机手工跑
`doops-gateway token create`：

```bash
doops admin token create \
  --target jm \
  --user gaojiaqi \
  --name maintenance \
  --expires 720h
```

`--target` 必须是本地配置里的 gateway target，并且它当前使用的 token 要有
`admin` 权限。新 token 只打印一次；需要写入本机缓存时显式使用：

```bash
doops admin token create \
  --target jm \
  --user gaojiaqi \
  --name maintenance \
  --expires 720h \
  --save-as jm
```

## Exec

```bash
doops -session ops exec --target jm --cmd 'hostname && df -h && free -h'
```

Use `exec` for deterministic commands, read-only checks, and known deployment scripts.

## Ask

```bash
doops -session ops ask --target jm --msg '只读检查 Kubernetes 节点状态，给出结论和风险'
```

Use `ask` for diagnostics that require exploration. The target must have doagent/model configuration available.

## Push

```bash
doops -session build push --target jm --src .
doops -session build exec --target jm --cmd 'ls -la /root/ws/build | head'
```

`push` uses Git workspace sync in both direct and gateway modes:

- Direct agent: `/git/<session>.git`
- Gateway: `/v1/git/<cluster>/<instance>/<session>.git`

Do not reimplement push with tar/base64/chunk upload. `doops_workspace_*` chunk tools are low-level compatibility JSON-RPC tools, not the standard CLI/deploy path; seeing 512KB chunk logs means a script is bypassing the supported Git HTTP path. Push respects `.gitignore` and `.doopsignore`; do not hide deploy manifests or scripts behind ignore rules unless intentional.

## Pull

```bash
doops -session build pull --target jm --dest ./doops-build-output
```

Use `pull` for large files, archives, SQL dumps, images, videos, and generated assets. Do not use `read` or `exec base64` for downloads.

## Read And Write

```bash
doops -session ops read --target jm --path /root/ws/ops/deploy.sh
doops -session ops write --target jm --path /root/ws/ops/notes.txt --content 'hello'
doops -session ops write --target jm --path /root/ws/ops/pod.yaml --file ./pod.yaml
cat ./pod.yaml | doops -session ops write --target jm --path /root/ws/ops/pod.yaml
```

`read` and `write` are for small text files and short scripts. Use `push/pull` for directories and binary assets.

## Clean

```bash
doops -session build clean --target jm
```

Confirm the session name before cleaning. Do not share one session across unrelated tasks.

## Login And Logout

```bash
doops login --target jm --gateway https://gateway.example.com --username alice
doops logout --target jm
```

`auth.json` only affects local CLI token selection. Gateway permissions still come from gateway user scopes.

## Upgrade

```bash
doops -session upgrade_20260519 upgrade \
  --target unicom \
  --cluster doops-jm \
  --instance jm-228 \
  --image docker.cnb.cool/l8ai/ai/doops.sh:v1.1 \
  --mode k8s \
  --namespace doops-system \
  --workload deployment/doops-agent \
  --container doops-agent \
  --dry-run
```

Remove `--dry-run` only after reviewing the command. Gateway upgrade requires `agent:upgrade` or `admin`.

## BuildKit Example

```bash
doops -session build exec --target jm --cmd \
  'cd /root/ws/build && buildctl --addr unix:///run/buildkit/buildkitd.sock build \
   --progress=plain \
   --frontend dockerfile.v0 \
   --local context=. \
   --local dockerfile=. \
   --opt filename=Dockerfile \
   --output type=image,name=repo.example.com/team/app:latest,push=true'
```

For long builds, redirect output to a file and poll with short `exec` calls. Do not leave a foreground websocket call waiting indefinitely when the command is expected to run for many minutes.
