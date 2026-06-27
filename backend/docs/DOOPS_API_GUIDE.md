# Doops API 对接指南

本文档面向需要通过 API 调用 Doops 的平台、CI 系统和内部工具。推荐的公共入口是 Doops Gateway 的 HTTP / WebSocket 服务；目标集群内的 doops-agent 主动连接 Gateway，不要求调用方能直连内网节点。

## 架构

```text
调用方 / CI / 平台
  -> doops-gateway
     -> 目标 cluster/instance 内的 doops-agent
        -> shell / kubectl / buildkit / doagent AI
```

Gateway 负责鉴权、授权、目标路由、单目标 busy 隔离、审计记录、WebSocket JSON-RPC 转发和 Git HTTP 反向隧道。

## 基础地址和鉴权

生产环境建议使用 HTTPS / WSS：

```text
https://gateway.example.com
wss://gateway.example.com
```

内网和受控测试环境也支持 HTTP / WS。客户端不能因为 Gateway 是 HTTP 就拒绝连接；生产环境仍建议使用 TLS。

所有用户侧请求都必须使用 user token 鉴权：

```http
Authorization: Bearer <DOOPS_GATEWAY_USER_TOKEN>
```

兼容字段 `X-Doops-Key: <token>` 仍可用，但新系统应优先使用 `Authorization: Bearer`。

## 健康检查

```http
GET /health
```

响应：

```text
ok
```

## 用户登录换取 Token

密码登录是可选能力，返回短期 user token。长期自动化任务应使用 Gateway 管理员签发的服务 token。

```http
POST /v1/auth/login
Content-Type: application/json

{
  "username": "alice",
  "password": "secret",
  "name": "ci deploy token"
}
```

响应：

```json
{
  "token": "dgw_user_xxx",
  "token_id": "tok_xxx",
  "token_type": "user",
  "username": "alice"
}
```

常见错误：

- `401 Unauthorized`：用户名或密码错误。
- `400 Bad Request`：请求体不是合法 JSON。

## 管理员签发用户 Token

管理员可通过 gateway API 给指定用户签发新的 user token。调用方必须使用拥有
`admin` 权限的 gateway user token。返回的明文 token 只出现一次；gateway
数据库只保存哈希。

```http
POST /v1/admin/tokens
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{
  "user": "alice",
  "name": "ci deploy token",
  "expires": "720h"
}
```

响应：

```json
{
  "token": "dgw_user_xxx",
  "token_id": "tok_xxx",
  "token_type": "user",
  "username": "alice"
}
```

CLI 推荐入口：

```bash
doops admin token create \
  --target jm \
  --user alice \
  --name ci-deploy \
  --expires 720h
```

常见错误：

- `401 Unauthorized`：管理员 token 缺失、无效、过期或已撤销。
- `403 Forbidden`：token 有效，但没有 `admin` 权限。
- `404 Not Found`：目标用户不存在。
- `400 Bad Request`：请求体非法，或 `expires` 不是合法正数 duration。

## 查询在线目标

```http
GET /v1/targets
Authorization: Bearer <USER_TOKEN>
```

响应：

```json
{
  "targets": [
    {
      "cluster": "doops-oilan",
      "instance": "oilan-node",
      "key": "doops-oilan/oilan-node",
	      "remote": "10.0.0.12:47128",
	      "connected_at": "2026-05-14T12:00:00Z",
	      "last_seen": "2026-05-14T12:00:10Z",
	      "busy": false,
	      "status": "active",
	      "active_ops": 1,
	      "queued_ops": 0,
	      "resources": ["workspace:deploy-20260519"],
	      "sessions": ["deploy-20260519"]
	    }
	  ]
	}
```

常见错误：

- `401 Unauthorized`：token 缺失、无效、过期或已撤销。
- `403 Forbidden`：token 有效，但无权查询目标列表。

`busy` 只表示 target-wide 不可用，例如独占升级、无 session 的旧式操作或 target 队列阻塞。`status=active` 且 `busy=false` 表示有 session/resource 正在执行，但同一 agent 仍可接收其它 session 的操作。`resources` 用于说明当前被锁的 `workspace:<session>`、`session:<session>` 或 `path:<path>`。

## Git 工作区同步

`push/pull` 使用 Git HTTP，不使用 stdout、base64 或 tar 分块传输。直连 agent 时访问 agent 的 `/git/<session>.git/...`；gateway 模式访问：

```http
GET/POST /v1/git/<cluster>/<instance>/<session>.git/...
Authorization: Basic doops:<USER_TOKEN>
```

CLI 会自动把 user token 放到 Git Basic Auth 的密码字段。Gateway 验证 token 后，按 `<cluster>/<instance>` 找到在线 agent，并通过该 agent 已建立的 `/v1/agent/connect` WebSocket 连接反向透传 Git HTTP 请求。Agent 在本地调用自己的 `/git/<session>.git/...` handler，完成 Git push 或 fetch。

权限映射：

- `git-receive-pack`：需要 `push` 权限。
- `git-upload-pack`：需要 `pull` 权限。

典型 API 路径：

```text
GET  /v1/git/doops-oilan/oilan-node/deploy_20260514.git/info/refs?service=git-upload-pack
POST /v1/git/doops-oilan/oilan-node/deploy_20260514.git/git-upload-pack
GET  /v1/git/doops-oilan/oilan-node/deploy_20260514.git/info/refs?service=git-receive-pack
POST /v1/git/doops-oilan/oilan-node/deploy_20260514.git/git-receive-pack
```

外部系统通常不需要手写 Git HTTP 报文，直接调用 CLI 更安全：

```bash
doops -session deploy_20260514 push --target oilan --src .
doops -session deploy_20260514 pull --target oilan --dest ./deploy-output
```

## 执行操作

所有操作通过 WebSocket JSON-RPC 调用：

```text
WS /v1/rpc?cluster=<cluster>&instance=<instance>
Authorization: Bearer <USER_TOKEN>
```

连接示例：

```bash
websocat \
  -H "Authorization: Bearer ${DOOPS_GATEWAY_USER_TOKEN}" \
  "wss://gateway.example.com/v1/rpc?cluster=doops-oilan&instance=oilan-node"
```

初始化：

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
```

工具调用格式：

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tools/call",
  "params": {
    "name": "doops_shell",
    "arguments": {
      "session_id": "deploy_20260514",
      "command": "hostname && kubectl get nodes"
    }
  }
}
```

流式输出通过通知返回：

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/message",
  "params": {
    "sessionID": "deploy_20260514",
    "data": "一行 stdout 或 stderr\n"
  }
}
```

最终响应会使用与请求相同的 `id`。

## 工具列表

### `doops_shell`

在目标 agent 上执行确定性的 Shell 命令。

参数：

```json
{
  "session_id": "deploy_20260514",
  "command": "cd /root/ws/deploy_20260514 && ./deploy.sh"
}
```

需要权限：`exec`。

适用于 CI/CD、固定部署脚本、健康检查、BuildKit 构建和 Kubernetes 命令。

### `doops_agent_prompt`

向目标 AI agent 下发自然语言任务。

参数：

```json
{
  "session_id": "deploy_20260514",
  "instruction": "检查仓库，生成部署计划，执行构建并更新 deployment/app。保留日志和脚本。",
  "model": "openai/gpt-5.4"
}
```

需要权限：`ask`。

适用于首次适配环境、排障修复、生成部署脚本等探索型任务。AI 跑通后，应把生成的脚本或命令固化回代码仓库。

### `doops_file_read`

查看小文本文件。

参数：

```json
{"path": "/root/ws/deploy_20260514/deploy.sh"}
```

需要权限：`read`。

不要用它下载大文件、二进制、压缩包、数据库导出或图片。大文件和目录下载使用 `doops pull`。

### `doops_file_write`

写入小文本文件。

参数：

```json
{
  "path": "/root/ws/deploy_20260514/pod.yaml",
  "content": "apiVersion: v1\nkind: Pod\n..."
}
```

需要权限：`write`。

### Workspace 同步协议

标准 CLI 和发布脚本必须使用 Git HTTP 同步：

```text
doops push -> /v1/git/<cluster>/<instance>/<session>.git -> agent /git/<session>.git
doops pull <- /v1/git/<cluster>/<instance>/<session>.git <- agent /git/<session>.git
```

`doops_workspace_begin`、`doops_workspace_chunk`、`doops_workspace_commit`、`doops_workspace_pull_begin`、`doops_workspace_pull_chunk` 是低层兼容 JSON-RPC 工具，保留给旧客户端或定制客户端做兼容，不是发布、升级、构建脚本的标准入口。

如果标准部署过程中看到 `512KB` 分块上传日志，通常说明调用方绕过了当前 CLI 的 `doops push/pull`，仍在走兼容工具。应修复调用方，让它执行：

```bash
doops -session <session> push --target <target> --src <dir>
doops -session <session> pull --target <target> --dest <dir>
```

### `doops_node_info`

返回节点基础信息。

参数：

```json
{"session_id": "smoke"}
```

需要权限：`info`。

### `doops_check_deployment`

检查线上部署镜像是否与最新构建一致。

需要权限：`check`。

### `doops_clean_workspace`

删除 `/root/ws/<session>` 和相关临时 Git 仓库。

参数：

```json
{"workspace": "deploy_20260514"}
```

需要权限：`clean`。

### `doops_agent_upgrade`

要求在线 agent 通过目标编排层升级自身。

参数：

```json
{
  "image": "docker.cnb.cool/l8ai/ai/doops.sh:v1.1",
  "mode": "k8s",
  "namespace": "doops-system",
  "workload": "deployment/doops-agent",
  "container": "doops-agent",
  "dry_run": true
}
```

需要权限：`agent:upgrade`。

推荐流程：

1. 先使用 `"dry_run": true`。
2. 确认 namespace、workload、container 正确。
3. 正式执行升级。
4. 查询 `/v1/targets`，等待目标重新上线。

## 自然语言部署 API 对接模式

平台可以对外提供更高层的“部署这个仓库”接口，然后在内部组合 Doops Gateway 调用。一个典型的平台接口可以是：

```http
POST /your-platform/deployments
Content-Type: application/json

{
  "repository": "https://cnb.cool/org/project.git",
  "ref": "main",
  "cluster": "doops-oilan",
  "instance": "oilan-node",
  "mode": "ai",
  "instruction": "构建并部署这个仓库，保留部署脚本和报告。"
}
```

平台内部映射为以下 Doops 操作：

1. 生成 `session_id`，例如 `deploy_${repo}_${timestamp}`。
2. 查询 `/v1/targets`，确认目标在线；`busy=false` 表示没有 target-wide 阻塞，`status=active` 仍可继续使用不同 session。
3. 使用 `doops push` 或 workspace upload 工具上传仓库内容。
4. 调用 `doops_agent_prompt`，使用边界清晰的指令：

   ```text
   你正在部署仓库 <repo>。只能在 /root/ws/<session> 内工作。
   先检查项目结构。如果已有 deploy.sh，优先使用它。
   使用 BuildKit 构建镜像，Kubernetes 变更必须先 server-side dry-run，
   再真实 apply，等待 rollout 完成，并把最终报告写入
   /root/ws/<session>/doops-report.md。所有命令必须可追溯。
   ```

5. 从 `notifications/message` 实时推送进展给调用方。
6. 通过 `/v1/audit?session=<session>` 查询持久审计记录。
7. 使用 `doops pull` 拉回 `/root/ws/<session>`，保存生成的脚本和报告。
8. 向调用方返回最终状态、审计 ID、报告路径和拉回的产物位置。

对于可重复的生产部署，应把 AI 首次探索成功后的结果固化为仓库内脚本，后续通过 `doops_shell` 调用固定脚本。

## 进展和可追溯性

Doops 有三层进展数据：

- WebSocket 流：实时 `notifications/message` 输出。
- Gateway 审计库：按 user、token、target、action、session、status、命令摘要、输出尾部和时间记录持久事件。
- 目标工作区审计：`/root/ws/<session>/.doops-audit-log` 记录该 session 内执行过的 shell 命令。

管理员通过 Gateway API 查询审计：

```http
GET /v1/audit?cluster=doops-oilan&instance=oilan-node&session=deploy_20260514&limit=50
Authorization: Bearer <ADMIN_USER_TOKEN>
```

响应：

```json
{
  "events": [
    {
      "id": 123,
      "user_id": "usr_xxx",
      "token_id": "tok_xxx",
      "cluster": "doops-oilan",
      "instance": "oilan-node",
      "action": "ask",
      "session": "deploy_20260514",
      "command_summary": "检查仓库，生成部署计划...",
      "status": "success",
      "tail": "rollout complete\n",
      "bytes_in": 268,
      "bytes_out": 4096,
      "started_at": "2026-05-14T12:00:00Z",
      "ended_at": "2026-05-14T12:03:21Z"
    }
  ]
}
```

过滤示例：

```http
GET /v1/audit?action=ask&status=error&limit=100
GET /v1/audit?user_id=usr_xxx
GET /v1/audit?session=deploy_20260514
```

审计清理也是管理员 API：

```http
DELETE /v1/audit?before=2026-01-01T00:00:00Z
Authorization: Bearer <ADMIN_USER_TOKEN>
```

响应：

```json
{
  "deleted": 42
}
```

## 错误码

HTTP 错误：

- `400`：查询参数或请求体缺失、非法。
- `401`：token 缺失、无效、过期或已撤销。
- `403`：token 有效，但缺少目标或动作权限。
- `404`：路由不存在。
- `405`：HTTP 方法不允许。

JSON-RPC 错误：

- `-32601`：未知方法或未知 Doops 工具。
- `-32602`：请求参数非法。
- `-32603`：Gateway 或目标 agent 内部执行失败。
- `-32003`：目标或动作权限不足。
- `-32004`：目标离线。
- `-32005`：目标忙、目标队列满或等待超时。
- `-32006`：全局或单用户并发限制超限。

## 权限

目标动作：

```text
targets:list exec ask read write push pull info check clean agent:upgrade
```

管理员动作：

```text
admin
```

默认 user token 拥有完整目标操作权限，除非该用户被显式配置了更窄的 grant。如果调用返回 `403`，检查该用户在具体 `cluster/instance/action` 上的授权。

## 操作规则

- 所有远程操作必须设置有意义的 `session_id`。
- 文件传输使用 `push/pull`，`read/write` 只用于小文本。
- 可重复部署使用 `doops_shell`；首次适配、排障和探索使用 `doops_agent_prompt`。
- Kubernetes 变更必须先做 server-side dry-run，再真实 apply。
- 升级 agent 镜像前，先确认新镜像已能在目标环境拉取。
- 生成的脚本和报告放在 `/root/ws/<session>`，成为产品流程后要拉回源码仓库。
