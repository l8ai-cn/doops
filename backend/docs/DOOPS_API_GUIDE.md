# Doops API 对接指南

本文档面向需要通过 API 调用 Doops 的平台、CI 系统和内部工具。推荐的公共入口是 Doops Gateway 的 HTTP / WebSocket 服务；目标集群内的 doops-agent 主动连接 Gateway，不要求调用方能直连内网节点。

## 资产台账

资产保存在 Gateway SQLite。首次启用前备份数据库，用目标环境已授权的 DoOps 会话按顺序执行下列 SQL，保留执行回执。首次使用这些接口依赖迁移已完成，Gateway 启动不执行资产 DDL。

1. [机器与导入批次](migrations/machine_inventory.sql)
2. [集群来源与命名空间](migrations/cluster_inventory.sql)
3. [Node 与机器关联](migrations/inventory_node_links.sql)

已有机器台账升级时，需重新执行幂等的机器迁移以创建机器账号关联表。Gateway 需配置并持续保管原有 `DOOPS_GATEWAY_SECRET_KEY`，接入页面和 Gateway 间使用受保护的连接；本机测试可使用回环地址。
4. [业务系统与部署关系](migrations/business_inventory.sql)
5. [服务器服役状态与覆盖率基线](migrations/asset_coverage.sql)
6. [监控样本与规则](migrations/asset_monitor.sql)
7. [巡检计划、运行与取证记录](migrations/asset_inspection.sql)
8. [集群盘点底册及冻结版本](migrations/asset_coverage_registers.sql)
9. [工作负载运行快照及绑定](migrations/asset_runtime_topology.sql)
10. [机器指纹绑定及诊断样本](migrations/machine_diagnostics.sql)

升级已有 GPU 监控库时，按 [设备维度迁移说明](migrations/asset_monitor_devices.sql) 在专用数据库连接上执行迁移。新建库直接使用当前的 `asset_monitor.sql`。已有机器诊断库可重跑 `machine_diagnostics.sql` 增加隧道采样记录。所有迁移均按环境留存执行回执。

逐项核对目标数据库的迁移记录。`asset_coverage.sql` 包含一次性列变更。回滚需要使用本次变更前的数据库备份，并评估备份时点之后的资产写入。

`GET` 请求按登录身份返回可见记录。平台管理员维护资产。普通用户的机器、集群和 Node 关系按目标授权过滤，关系两端都可见才会展示。去掉目标授权后，后续查询立即采用新的可见范围。写入沿用 Gateway 身份和审计。

| 接口 | 请求和回执 |
| --- | --- |
| `GET /v1/assets/machines` | `machines` 数组包含资产编号、规格、宿主、目标和版本。管理员用 `?batch_id=…` 查最近一次 CSV 确认的回执，以 `attempt_id` 指定历史回执。 |
| `POST /v1/assets/machines` | `action=create/update` 提交 `machine`，编辑还需 `expected_revision`。版本冲突返回 409。`action=preview` 提交 `csv` 字符串并取得 `preview_token`。`action=import` 提交相同内容和该标识。若确认时行的导入结果已变化，返回 409，重新预览后再确认。两步的 `scope` 应反映转换前的导入要求；省略时沿用 `machine_metadata`。`machine_and_credentials` 接受 `username,password` 两列，并在确认事务中保存机器、加密的凭据及其关联。仅给已存在资产补录时，使用 `asset_id,username,password`。`credential_result.expected` 是本批待写凭据数，`saved` 是实际写入数，`existing` 是已保存且内容一致的凭据数。变更预览、数据冲突或凭据存储不可用时不会部分确认。台账模式和历史台账回执仍报告本批保存的机器凭据为 0。 |
| `GET /v1/assets/clusters` | `clusters` 按可见来源返回集群编号、来源目标及 `namespaces`。同一资产编号可以有多个已验证来源，每个来源维护自己的观测范围。 |
| `POST /v1/assets/clusters` | `action=discover/import`，指定 `cluster`、`instance`。保存时须提供 `asset_id`、`name`。提供 `namespace` 将在该空间验证 `list pods`，省略时读取 Namespace 列表。保存由管理员执行。 |
| `GET/POST /v1/assets/node-links` | 读取可见 Node 与机器的手工映射；管理员提交 `action=bind/unbind` 和 `asset_id`、`cluster`、`instance`、`node_name`、`machine_id`。解绑只解除台账关系。 |
| `GET /v1/assets/dashboard` | 一个读取快照返回 `machines`、`systems`、`clusters` 和 `unassigned`。每个集群按 `asset_id` 合并可见来源，分别去重命名空间、关联机器与在役系统。 |
| `GET/POST /v1/assets/systems` | 获取 `systems` 及其可见 `deployments`。管理员以 `create/update` 提交 `system`，更新另传 `expected_revision`。部署关系标明来源目标、命名空间、应用和可选数据库。 |
| `GET/POST /v1/assets/topology` | GET 返回可见节点、关系及来源证据，`cluster_id` 限定到台账中的稳定集群编号。POST `collect` 在已纳管命名空间读取工作负载及 Pod 的运行关系。管理员用 `bind/unbind` 维护已登记应用与工作负载 UID 的关联，写入时核对来源与快照版本。 |
| `GET/POST /v1/assets/machine-diagnostics` | 按机器 `asset_id` 和绑定的 `cluster/instance` 读取主机采样，可用 `range` 和 `to_ms` 限定时窗。主机指纹、采样来源与时间共同标识证据；数据不足时界面显示具体缺口。POST `bind` 以管理员独立核对的指纹完成首次试采和绑定，`refresh` 沿被授权来源的 Agent 反向通道刷新。 |
| `GET/POST /v1/assets/coverage` | `baseline_id=live` 预览当前台账，省略时读取最近冻结版本。`kind=machines/systems/all`、`status=covered/gap/all` 可筛选明细，`format=csv` 下载。`summary=1` 单独使用时仅返回看板摘要。管理员提交 `action=freeze` 与唯一 `baseline_id` 冻结，或以 `set_machine_active`、`machine_id`、`active`、`expected_revision` 调整机器服役状态。旧版本仍按读取者当前授权筛选。 |
| `GET/POST /v1/assets/monitor` | 按来源 `cluster`、`instance` 读取对象或时间序列，支持 `cluster_id`、`namespace`、`type`、`asset_id`、`metric`、`origin`、`range` 等筛选，网卡、挂载点和 GPU 可用 `metric` 与 `device_id` 精确查询。普通账号可对授权目标执行 `collect` 采集 Pod CPU、内存；外部指标 `ingest` 和阈值 `rule` 需要平台管理员。见 [宿主采样、中心提交与页面验收](ASSET_MONITOR_DEVICES.md)。 |
| `GET/POST /v1/assets/inspections` | 读取计划与运行概览，`view=report/download` 加 `run_id` 读取本次可见报告。可预览、保存及暂停计划，启动运行、发起补检、提交证据或登记整改。`tick` 供独立执行账号处理自身到期计划。执行账号停用、到期超过取证窗口时，管理员可提交 `reconcile` 为漏期生成审计记录及报告。 |

无 Agent 的机器可作为未接入资产登记。新集群通过已有授权 Agent 接入，配置见 [安装指南](CLUSTER_INSTALL_GUIDE.md)。控制台的操作步骤见 [资产台账](DOOPS_WEBUI.md#资产台账)。

集群看板使用 `cluster_id` 请求覆盖率，分母来自该集群的盘点底册。平台与集群概览通过 `monitor?view=alerts` 读取未恢复异常及采集中断，通过 `inspections?view=weekly` 读取本周计划和执行状态；两个接口都按当前授权筛选后汇总。

监控采集器的可用输入见 [采集脚本](../scripts/asset_monitor_collector.py)，自动周检执行账号、数据源、调度周期及故障处理见 [巡检执行器](../inspection/README.md)。

## 受管凭据配置

运行时引用凭据只要求该 target 的 grant，不要求额外凭据权限或逐用户绑定。
目标视图也继承操作权限；返回无 payload 的目标绑定元数据。创建、轮换、吊销仍检查 owner/admin。
`skill=dosql` 请求直接用服务器保存的 database 绑定解析 namespace/version，不运行 scope 规划 prompt。
DoSql 的自动绑定、`sql_credential` 选择和字段组合以
[DoSql 参数契约](#dosql-参数契约) 为准。
缺少版本、目标绑定、格式不合法或目标离线分别返回真实错误类别，不合并为 `credential_invalid`。

CLI `credential configure` 顺序调用既有的 managed list/create、payload/import-secret、
grants、verify 和 promote；不增加另一套凭据资源或提升权限的 API。
新增 `POST /v1/credentials/<id>/import-secret` 接受 metadata-only JSON：

```json
{
  "session_id": "credential-setup",
  "cluster": "exact-cluster",
  "instance": "exact-instance",
  "namespace": "apps",
  "secret_name": "database",
  "keys": ["DB_PASSWORD"],
  "key_map": {"DB_PASSWORD": "SPRING_DATASOURCE_PASSWORD"},
  "connection_uri": "postgresql://app@database:5432/app",
  "password_key": "DB_PASSWORD"
}
```

`key_map` 可将 source Secret key 映射为运行时 key；每个 source 必须出现在 `keys` 中，
runtime key 不能重复。未映射的 key 原样保留。`connection_uri` 和 `password_key` 一起使用或一起省略。URI 不得含密码、query 或 fragment，
必须是含用户名、host、数据库路径的 postgres/postgresql/mongodb URI；组合时只允许选中密码 key。
该 endpoint 要求对应 target 的 grant 和可管理的 active opaque resource，沿用凭据资源锁、
加密主密钥和审计。它只在当前精确 target 内读取指定 Opaque Secret，不修改源 Secret。
Agent 通过内部 `doops_credential_import` 控制通道返回选中 payload，Gateway 加密为 staged
version 后只向 HTTP 返回版本元数据。公开 RPC 禁止调用该工具，流式通知不会进入导入结果。
错误只返回稳定类别；无权限、无加密主密钥或审计不可用时不读取 Secret。内部调用失败的类别与状态见
`CREDENTIAL_RESOURCES.md` 的 `--from-secret` 边界。
导入本身不建立 grant 或激活；configure 继续走既有 grant/verify/promote。

## 架构

```text
调用方 / CI / 平台
  -> doops-gateway
     -> 目标 cluster/instance 内的 doops-agent
        -> shell / kubectl / buildkit / doagent AI
```

Gateway 负责鉴权、授权、目标路由、按 target/session 作用域的并发隔离（target 级独占只用于 Agent 自升级）、审计记录、WebSocket JSON-RPC 转发和 Git HTTP 反向隧道。

## 基础地址和鉴权

生产环境建议使用 HTTPS / WSS：

```text
https://gateway.example.com
wss://gateway.example.com
```

生产 Gateway 使用 HTTPS / WSS。CLI 默认仅允许 loopback HTTP / WS；受控内网测试须显式
设置 `DOOPS_ALLOW_INSECURE_GATEWAY=1`，该选项不应作为生产 TLS 故障的绕过方式。

所有用户侧请求都必须使用 Gateway user token 鉴权：

```http
Authorization: Bearer <DOOPS_GATEWAY_USER_TOKEN>
```

Gateway 凭证校验失败返回 HTTP `401`，响应体包含稳定错误分类，避免与目标业务服务
或受管凭据的认证失败混淆：

```json
{
  "error": "unauthorized",
  "code": "gateway_auth_expired",
  "auth_domain": "gateway",
  "message": "Gateway user token has expired",
  "next_action": "Run doops login --gateway <gateway-url> for this Gateway; do not replace the Gateway token with a managed credential or business API key"
}
```

凭证无效返回 HTTP `401` 和 Bearer challenge，`code` 区分原因；鉴权存储不可用返回 HTTP `503`，
不提示凭证无效，也不发出重新认证 challenge：

- `gateway_auth_missing`：没有 Bearer token。
- `gateway_auth_invalid`：格式、token ID、hash 或归属校验失败。
- `gateway_auth_expired`：token 已过期。
- `gateway_auth_revoked`：token 已撤销。
- `gateway_auth_user_disabled`（401）：token 所属用户已停用，需要管理员启用账号。
- `gateway_auth_invalid_expiry`（401）：token 的到期字段损坏，需要管理员修复并重新签发。
- `gateway_auth_unavailable`（503）：Gateway 鉴权存储不可用，不能据此判断 token 状态。

Gateway 签发的用户 token 格式为
`dgw_user_<8 位十六进制前缀>_tok_<24 位十六进制 ID>_<64 位十六进制密钥>`。
没有可解析 token ID 的输入直接返回 `gateway_auth_invalid`，不扫描历史 token 或逐条尝试 bcrypt；
有 ID 的凭证只读取对应记录，hash 匹配后再判断撤销、过期和用户停用状态。

这些错误只描述 Gateway user token，不表示受管凭据 `credential_invalid`。如果响应只有裸
`401 Unauthorized`，调用方必须将认证域视为未确定，不得直接把错误归因于受管凭据。

`/v1/agent/connect` 在该 target 还没有确认 enrollment 时保持开放注册。确认之后必须带绑定到该
cluster/instance 的 enrollment token，失败时返回 `code=agent_enrollment_auth_failed` 和
`auth_domain=agent_enrollment`。已在线的 Agent 不会因为签发或升级而断开。
`/v1/edge/connect` 使用另一份 enrollment token。失败时返回
`code=edge_enrollment_auth_failed` 和 `auth_domain=edge_enrollment`。
两处都不能使用 Gateway user token 或受管凭据替代。

`X-Doops-Key` 不属于当前协议，Gateway 不接受该字段。

## 健康检查

```http
GET /health
```

响应：

```text
ok
```

## 用户登录换取 Token

密码登录是可选能力，返回可续期的 user token。长期自动化任务应使用 Gateway 管理员签发的长期 user token。
默认登录 token 的滑动有效期为 30 天：只要仍在使用，过期时间会延长为当前时间加 30 天，但距签发时间最长不超过 90 天。可用 `-login-token-ttl` 和 `-login-token-max-lifetime` 调整。管理员签发的 token（含不设过期时间的服务 token）不会自动续期。
每次成功登录都会创建新的 token ID；同一用户未过期的密码登录 token 超过 10 个时，最早的旧 token 会被撤销。客户端应根据 `token_id`、`issued_at` 和 `expires_at` 记录替换关系。
登录不删除过期 token 的记录；旧凭证继续返回明确的过期原因，便于审计和轮换核对。

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
  "username": "alice",
  "name": "ci deploy token",
  "issued_at": "2026-11-08T08:39:00Z",
  "expires_at": "2026-11-09T08:39:00Z"
}
```

常见错误：

- `401 Unauthorized`：用户名或密码错误。
- `503 Service Unavailable` / `gateway_auth_unavailable`：无法读取或校验账号认证数据，不能据此判断密码错误。
- `400 Bad Request`：请求体不是合法 JSON。

## 当前身份与 Token 元数据

```http
GET /v1/auth/me
Authorization: Bearer <DOOPS_GATEWAY_USER_TOKEN>
```

这是 CLI token 导入、控制台 token 登录和会话恢复的统一校验入口。任何已认证用户都可调用，
不要求管理员身份或 target grant。它只返回请求中该 token 的身份与元数据；query 参数不能选择
其他用户或 token，也不会返回 token 明文。

```json
{
  "token_id": "tok_xxx",
  "user_id": "usr_xxx",
  "username": "alice",
  "is_admin": false,
  "name": "ci deploy token",
  "issued_at": "2026-11-08T08:39:00Z",
  "expires_at": "2026-11-09T08:39:00Z"
}
```

响应为 `Cache-Control: no-store`。永不过期 token 省略 `expires_at`；返回的身份、角色和
到期时间以数据库当前记录为准，客户端不得靠用户输入的用户名或管理员接口探测推断。
认证失败保留上面的具体 401 类别，存储不可用为 503；非 GET 请求返回 405。
管理端 token list 如果遇到损坏的 expiry，会明确报错，不把它当成永不过期。

## 管理员创建和维护用户

这些接口要求调用方持有 `admin` 权限。用户密码只允许出现在 HTTPS JSON request body，
不得写入 URL、argv、日志或文档。

列出或创建用户：

```http
GET /v1/admin/users
Authorization: Bearer <ADMIN_USER_TOKEN>
```

```http
POST /v1/admin/users
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{"name":"alice","password":"<PASSWORD_FROM_SECURE_INPUT>","admin":false}
```

`POST /v1/admin/users` 只创建账号，不创建任何 target grant。交付账号前必须创建精确的
`cluster/instance` grant；grant 覆盖整个 target，没有 actions 维度。创建 grant 失败时
立即停用用户。

设置密码和启用/停用用户：

```http
POST /v1/admin/users/password
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{"user_id":"usr_xxx","password":"<PASSWORD_FROM_SECURE_INPUT>"}
```

```http
POST /v1/admin/users/disable
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{"user_id":"usr_xxx","disabled":true}
```

## 管理员维护 Target Grants

列出用户 grant 时，`user` 查询参数接收 `user_id`：

```http
GET /v1/admin/grants?user=usr_xxx
Authorization: Bearer <ADMIN_USER_TOKEN>
```

创建精确 grant：

```http
POST /v1/admin/grants
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{
  "user_id": "usr_xxx",
  "cluster": "doops-oilan",
  "instance": "oilan-node"
}
```

删除 grant：

```http
DELETE /v1/admin/grants?id=<grant-id>
Authorization: Bearer <ADMIN_USER_TOKEN>
```

grant 覆盖整个 target，没有 action 维度；`actions` 可省略，只接受 `["target:use"]`，其它值返回 `400`。
平台管理员是用户的 `is_admin` 标记，创建用户时用 `"admin": true` 设置。

## 管理员查看实例

`GET /v1/admin/instances` 列出已注册到 Gateway 的边缘节点及其连接状态。只有平台管理员可以读；普通用户返回 `403`。GET 不产生 mutation 审计。

默认只返回活跃实例。历史实例要显式包含：

```http
GET /v1/admin/instances?include=history
Authorization: Bearer <ADMIN_USER_TOKEN>
```

`include` 只接受 `history`，其它值返回 `400`。活跃窗口、清理时机，以及清理不触碰的授权、令牌、凭据、审计和路由，见 [DOOPS_GATEWAY_TUNNEL.md](DOOPS_GATEWAY_TUNNEL.md) 的 Reliability Notes。

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
  "username": "alice",
  "name": "ci deploy token",
  "issued_at": "2026-11-08T08:39:00Z",
  "expires_at": "2028-07-16T08:39:00Z"
}
```

CLI 推荐入口：

```bash
doops admin token create \
  --target demo \
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
      "release_commit": "0123456789abcdef0123456789abcdef01234567",
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

列表按调用方的 grant 过滤；没有任何匹配 grant 时返回空 `targets` 数组，不返回 `403`。

`busy` 只表示 Agent 自升级持有 target 级独占 lease；普通变更和排队 session 使用
`target + session` 作用域的共享 lease，同一 session 内的 Shell/SSH/bg/apply 也可并发，
只有该 session 的 workspace 写入要等它们结束。`status=active` 且 `busy=false` 表示有
workspace/resource 正在执行，但其他 session 仍可使用同一 Agent。`resources` 列出
`workspace:<session>` 等资源锁；session 是并发归属，不是操作系统沙箱。
取消和审计收尾复用同一冲突规则，不阻塞其它 session；普通 mutation 缺少有效 session
时直接拒绝，不回退到 target 独占。历史 `outcome_unknown` 只保留事实，不参与新任务准入。

## Git 工作区同步

`push/pull` 使用 Git HTTP，不使用 stdout、base64 或 tar 分块传输。标准公共 API 只通过 Gateway 访问：

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

Gateway 和 Agent 的 `serverInfo.version` 使用同一个 DoOps 发布标签，
`serverInfo.releaseCommit` 使用该发布对应的完整 40 位源码 commit。两者是同一个发布
身份的版本与 provenance，不能把 commit 当作版本号；旧 Agent 缺少完整
`releaseCommit` 时不会被当作当前发布验收通过。`cli-contract/v3` 只表示 CLI 协议
兼容性，由 `doops --contract` 单独检查。

`executionLifecycle` 只限制 `doops_bg`，不限制普通前台 shell、SSH、prompt/apply 或查询。
Agent 在 `capabilities.protocols.executionLifecycle` 声明
`schemaVersion=doops.sh/execution-lifecycle/v1`，且 `terminalResponse`、
`backgroundLifetime`、`promptCancellation` 必须全部为布尔 `true`，不得缺字段或额外字段。
CLI 和 Gateway（包括内部调度调用）在后台任务派发前校验，不满足则拒绝且不占执行锁。
`backgroundLifetime` 表示任务受控并等待原始 RPC 终态，不承诺跨断连存活。
终态、取消和未知结果仍由 Gateway 统一处理。workspace 写入等本地操作缺失终态时同样
如实保留未知结果；执行结束及连接清理后释放 lease。审计持久化失败报告
`audit_completion_failed` 并独立重试原始终态，不保留执行锁；历史未知审计不参与新任务准入。
`notifications/message` 中的工具状态通知会继续转发到 CLI；CLI 将工具开始、进展和结束状态
作为 `[tool:<name>] <status>` 进度行显示。它们只表示观察到的工具状态，不替代原始 RPC
终态或 apply 的可信结构化回执。

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
    "request_id": 2,
    "sessionID": "deploy_20260514",
    "data": "一行 stdout 或 stderr\n",
    "kind": "raw"
  }
}
```

最终响应会使用与请求相同的 `id`。通知的 `params.request_id` 由 Gateway 按发起请求的 `id` 填入，是调用方关联流式输出与请求的唯一依据；`params.kind` 取 `raw`、`tool`、`error`、`assistant_delta` 之一。

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

需要该 target 的 grant（审计动作记为 `exec`）。

适用于 CI/CD、固定部署脚本、健康检查、BuildKit 构建和 Kubernetes 命令。

### `doops_ssh`

通过 Agent 的出站 SSH 客户端执行结构化远端 argv。`host`、SSH `options` 和远端
`command` 在 JSON 中分别传输；Agent 只在 OpenSSH 的远端命令边界把 `command` 的
每个元素做 POSIX 单引号编码，避免多层 SSH 的嵌套引号被本地或远端 shell 重解析。

Agent 在 `initialize` 响应的 `capabilities.protocols.structuredSSH` 中声明
`doops.sh/structured-ssh/v1`。Gateway 只把该能力转发给已声明的 target，并在能力
缺失时拒绝调用；CLI 也会在发送 `tools/call` 前失败关闭。旧 Agent 不会再以
`Unknown tool over WS: doops_ssh` 作为能力不匹配的唯一提示，必须先按受管 Agent
升级流程替换。

能力声明格式：

```json
{
  "schemaVersion": "doops.sh/structured-ssh/v1",
  "tool": "doops_ssh",
  "structuredArgv": true
}
```

参数：

```json
{
  "session_id": "ops_20260927",
  "host": "ops@app.internal",
  "options": ["-o", "BatchMode=yes"],
  "command": ["bash", "-lc", "hostname && systemctl status api"]
}
```

需要该 target 的 grant（审计动作记为 `exec`）。

CLI 对应入口：

```bash
doops -session ops ssh --target demo --host ops@app.internal \
  --option=BatchMode=yes -- bash -lc 'hostname && systemctl status api'
```

CLI 的 `--option` 只接受不会改写连接目标的旗标：`-4`、`-6`、`-p`、`-i`，以及
`BatchMode`、`StrictHostKeyChecking`、`ConnectTimeout`、`IdentitiesOnly`、
`IdentityFile` 和 `Port`。`key=value` 简写编码为 `-o<key>=<value>`。位置参数、
`ProxyCommand`、`ProxyJump`、端口转发、`HostName` 和 `KnownHostsCommand` 会被拒绝。

不要把嵌套 SSH 写进 `doops_shell.command`。原始 Shell 仍按单个
`bash -c` 字符串执行，无法替结构化 SSH argv 推断用户想保留的引号层级。

### `doops_bg`

提交由 agent 管理的后台任务。进度通知先返回任务 ID、PID 和日志路径，原始 RPC
在进程及其进程组终止后才返回最终结果；CLI 使用一次流式 RPC，不轮询或重试任务。
`doops_task_status` 可独立查询已有任务日志；终态无法确认时返回
`execution_outcome_unknown`，状态查询显示 `unknown`，不得把提交成功当成执行完成。

参数：

```json
{
  "session_id": "deploy_20260514",
  "command": "cd /root/ws/deploy_20260514 && buildctl build ..."
}
```

需要该 target 的 grant（审计动作记为 `exec`）。

不要把 `doops_shell` 里的命令用 `&`、`nohup` 或 `setsid` 转为后台任务。普通 shell 调用结束时，agent 会终止其子进程；长构建必须使用 `doops_bg`。

### `doops_task_status`

查询 `doops_bg` 提交任务的状态和日志尾部。

参数：

```json
{
  "session_id": "deploy_20260514",
  "task_id": "task_abc123",
  "lines": "100"
}
```

需要该 target 的 grant（审计动作记为 `exec`）。

### `doops_agent_prompt`

The unified DoWeb domain uses `skill=doweb` and the same target/session and
Gateway account. `mcp_doops_web_read` accepts only `render`, `check`,
`config-get`, `domain-list` and `site-list`. The latter returns `{ "sites": [...] }` with the site ID, domain, current/previous release and update time from each DoWeb release record. The mutating `mcp_doops_web` requires
a runtime-authorized apply, with no additional confirmation field. A read tool cannot stand in for
mutation evidence. The deterministic `doops_web` RPC maps reads to `ask` and
mutations to `exec`; website writes hold a target-wide lease because Nginx
activation/reload is shared. See `DOWEB_INTEGRATION.md` for parameters and mounts.

向目标 AI agent 下发诊断、运维或发布任务。发布任务使用
`operation=apply`，并明确 target、release、namespace 和 Chart；镜像、原因和业务验收按任务需要提供。
DoOps 不把业务信息转换成全局发布门禁。

参数：

```json
{
  "session_id": "diagnose_20260514",
  "instruction": "检查目标节点磁盘和容器运行时状态，只返回诊断结果，不修改资源。",
  "skill": "doweb",
  "model": "openai/gpt-5.4"
}
```

需要该 target 的 grant。审计动作：普通诊断或只读任务记为 `ask`，`operation=apply` 记为 `exec`。

适用于诊断、信息收集和用户明确要求的运维或发布任务。需要凭据时，CLI 只传递
`--credential <resource-name>` 的 metadata-only 引用；Gateway 在绑定用户、target 和
session 后按 `cluster/instance/use` 授权和物化，namespace、workload、image、Helm 和
required keys 只作为执行元数据。普通发布使用 `operation=apply`；DoSql 使用下面的
自动绑定契约。CLI 用户应优先使用 `doops sql`，它会设置正确的 DoSql skill/tool boundary。
通用 `ask` 不会自动注入 DoSql 工具，也不能附带数据库凭据。payload 永远不进入 prompt、日志或返回事件。
Helm、Kubernetes 和 DoWeb 的明确变更请求都不要求额外的 `confirm` 字段。
Gateway target grant、只读工具边界和运行时 apply 授权保持有效。Helm 的成功、失败和凭据 origin 边界见
[`NATURAL_LANGUAGE_DEPLOYMENT.md`](NATURAL_LANGUAGE_DEPLOYMENT.md)。

#### DoSql 参数契约

DoSql 请求使用 `skill=dosql`。自然语言 query、plan、discover 都是 `operation=ask`；
query 使用 `credential_mode=auto`，plan 和 discover 不连接数据库，CLI 不传凭据模式。
模型只获得 `mcp_dosql_read`。变更必须提供原样 `sql` 并设置 `operation=apply` 和
`credential_mode=auto`，不由模型把 instruction 翻译成变更后执行。

自动模式按资源 ID 绑定当前 target 上全部 active 的 database 凭据；Agent 用
`sql_database` 匹配库名。`sql_credential`（资源名或 ID）收窄为一个已绑定凭据，
要求 `skill=dosql` + `credential_mode=auto`，否则返回 `-32602`。
未绑定返回 `dosql_credential_not_bound`，同名多 owner 返回 `credential_reference_ambiguous`。
DoSql 的非空 `credential_refs` 返回 `-32602 dosql_credential_refs_unsupported`；
`credential_bindings` 只能由 Gateway 注入。CLI 的 `--credential` 只用于 `--sql/--file`，
映射为 `sql_credential`。

以下是 `doops_agent_prompt` 的 arguments 示例。自然语言只读查询：

```json
{
  "session_id": "db-query",
  "skill": "dosql",
  "operation": "ask",
  "credential_mode": "auto",
  "instruction": "只读查询 orders 数据库中 orders 表的行数。"
}
```

已明确授权的原样 SQL 变更，选择目标上绑定的迁移账号：

```json
{
  "session_id": "db-change",
  "skill": "dosql",
  "operation": "apply",
  "credential_mode": "auto",
  "sql_credential": "orders-migrate",
  "sql_database": "orders",
  "sql": "UPDATE orders SET status = 'paid' WHERE id = 1",
  "sql_verification": "SELECT status FROM orders WHERE id = 1",
  "instruction": "原样执行本次已授权的订单状态更新，并返回验证结果。"
}
```

原样只读 SQL 使用 `operation=ask` + `sql` + `credential_mode=auto`。
`sql_dry_run=true` 仅适用于可回滚的 apply 批次；MySQL/Oracle 隐式提交 DDL 与 MongoDB
非事务操作拒绝 dry-run。事务、混批限制及回读验收见
[使用指南](DOOPS_USAGE_GUIDE.md#dosql-直接执行-sql)。已有明确授权不需要每次重复批准；
目标、数据库或变更范围不清或超出授权时才补充确认。

组件对话的结构化事件消费者先发送 `mode=metadata`。支持本协议的 Agent 返回
`event_schema=doops.sh/acp-events/v1` 和 `event_formats=["acp"]`。模型尚未下发时也返回
这两项，并用 `status=awaiting_model_provider` 表示没有可同步的模型，而不是缺少协议。
下发时机见 [CREDENTIAL_RESOURCES.md](CREDENTIAL_RESOURCES.md#model-provider)。
消费者显式发送 `event_format=acp` 时，`notifications/message` 的
`kind=acp_update`，`data` 是已投影并脱敏的原生 ACP JSON；通知保留匹配的
`sessionID` 和 Gateway 根请求 `request_id`。任意其他非空 event format 被拒绝。
显示事件不携带凭据 payload、mutation proof 或执行授权。`turn_finished` 不替代
Gateway 最终 RPC 结果及既有 Helm 证据验签；组件只在两者均满足时显示完成。
验签后的 Helm 成功若伴随模型收尾失败，桥接以 `sessionUpdate=doops_notice`
发送结构化警告，并保留 Gateway 已验签的成功结果，不把旧格式警告混入 ACP 流。
未声明该协议的 Agent 必须升级，组件不自动转回旧事件消费者。未请求此格式的
CLI 保持原有输出契约，不能以该路径绕过组件的版本检查。

### `doops_file_read`

查看小文本文件。

参数：

```json
{"session_id": "deploy_20260514", "path": "deploy.sh"}
```

需要该 target 的 grant（审计动作记为 `read`）。

`path` 必须是当前 session 工作区内的相对路径。唯一例外是控制台知识库：绝对路径必须正好是 `/root/kb/<文件名>.md`。文件名只能是单层 Markdown，不能是子目录、`..` 或符号链接。该目录不在 session 工作区内，因此不会随工作区清理删除。其它绝对路径一律拒绝。

不要用它下载大文件、二进制、压缩包、数据库导出或图片。大文件和目录下载使用 `doops pull`。

### `doops_file_write`

写入小文本文件。

参数：

```json
{
  "session_id": "deploy_20260514",
  "path": "pod.yaml",
  "content": "apiVersion: v1\nkind: Pod\n..."
}
```

需要该 target 的 grant（审计动作记为 `write`）。路径规则与 `doops_file_read` 相同：session 内相对路径，或 `/root/kb/<文件名>.md`。

### `cos` 存储模式

`read`、`write`、`push`、`pull` 默认走 `cos`。`--storage gateway` 才使用原来的文件 RPC 和 Git HTTP。COS 的 endpoint、region、bucket 和密钥只来自 Gateway 进程配置，调用方传的 `--credential` 不参与选择。Gateway 未配置密钥时失败类别是 `cos_not_configured`，停在 `cos`，不回退。

桶固定为 `doops-1253657668`。对象 key 只落在调用方自己的前缀：

```text
sessions/<user-id>/<session-id>/files/<path>
sessions/<user-id>/<session-id>/snapshots/<commit>/<path>
```

`..`、绝对路径，以及任何跳出该前缀的 key 都会被拒绝。16 MiB 及以下由 Gateway 用自己的 COS 配置单次 PutObject，并返回 object key 和 24 小时 presigned GET。超过 16 MiB 时 Gateway 签发短期 PresignPutObject，客户端直接 PUT 到 COS，Gateway 再用 HeadObject 核对大小，不在 Gateway 落盘拼大文件。`read --storage cos` 只返回 presigned GET，对象内容留在 COS。`push --storage cos` 仍先做本地隔离快照，再把快照文件放进 `snapshots/<commit>/`。清单是同一前缀下的小 JSON 对象 `snapshots/<commit>/.doops/manifest.json`，`snapshots/latest.json` 指向最近一次提交。`pull --storage cos` 按这份清单把对象拉回本地目录。

Gateway 配置项是 `DOOPS_COS_ACCESS_KEY_ID`、`DOOPS_COS_SECRET_ACCESS_KEY`、`DOOPS_COS_ENDPOINT`、`DOOPS_COS_REGION`、`DOOPS_COS_BUCKET`。后三项省略时分别是 `https://cos.ap-guangzhou.myqcloud.com`、`ap-guangzhou`、`doops-1253657668`。密钥不进入 prompt、审计、日志或仓库。HeadBucket 失败时返回 `cos_bucket_unavailable`。

清单中的每个普通文件必须携带布尔型 `executable`，写入者以快照 Git tree 的 `100755` / `100644` 为准。下载和 Agent 同步均在整份清单校验通过后才写文件，并将新建及覆盖文件的权限设为 `0755` / `0644`，不继承目的文件的旧执行位。不承诺完整 POSIX mode、UID/GID、ACL 或扩展属性保真。缺失或 null 返回 `cos_snapshot_permissions_missing` 并提示从原来源重新 push；错误类型返回 `cos_manifest_invalid`。旧对象保留，但不能伪造缺失的执行位并报告恢复成功。

COS 新快照只接受 Git tree 中的普通文件；符号链接、gitlink 等其它类型返回 `cos_snapshot_file_type_unsupported`，在任何对象上传前停止，不将它们静默展开成普通文件。Gateway 写入清单对象时也执行相同的元数据校验，旧客户端不能用缺执行位的清单覆盖已经完整的快照清单。

Gateway 在 `initialize.capabilities.gatewayProtocols.cosSnapshotExecutable` 声明自身支持；Agent 在自己的 `initialize.capabilities.protocols.cosSnapshotExecutable` 声明支持，两者都使用 `schemaVersion="doops.sh/cos-snapshot-executable/v1"` 与 `executable=true`。CLI 对每次实际连接核对 Gateway 自有能力，不能把上次连接或 Agent 的声明当作 Gateway 的证明。Gateway 对实际选中的同一个 Agent 在派发前核对能力；重连后不会把新实例默认为支持。普通 `read/write` 不受快照能力要求影响。

Agent 在线且支持该协议时，`push --storage cos` 上传完成后通知该 session 的 Agent 把对象同步到 `/root/ws/<session>`。Gateway 不支持时 CLI 在上传前返回 `cos_snapshot_executable_unsupported`；Agent 不支持时返回 `cos_agent_snapshot_executable_unsupported`，不派发同步、不报告成功。其它同步失败仍写明 `cos_workspace_not_updated`。Agent 不在线时对象仍然有效，结果写明 `cos_workspace_not_synced`。Git HTTP 仍只服务 `gateway` 模式。

### `doops_workspace_read`

为 Agent UI 代码工作台提供当前 session 工作区的只读文件树、源码、变更、Diff、搜索和 Git 信息。
该工具只需要目标 `read` 权限，不执行 shell，也不修改工作区。
结构化文件与工作区 `read` 使用并发只读通道；同一 session 的多个读取可同时执行，
也可查看正在执行的 session。写入/执行仍互斥，Agent 升级期间的维护隔离仍生效。

```json
{"session_id":"console_abc123","operation":"list","path":"src"}
```

`operation` 支持 `list`、`read`、`changes`、`diff`、`search`、`git-summary` 和 `git-log`；
`path`、`query`、`ref` 按操作使用。成果面板中的工作区产物仍通过 session-scoped `read` 边界加载。
读取以当前 session 目录为文件系统根，拒绝越界符号链接。尚未创建的会话工作区显示为空；
没有 Git 仓库时仍返回实际文件供成果查看，并明确标记 `gitStatus=no_repository`。
`read` 返回 `content`、`encoding`（`utf-8` 或 `base64`）、`contentType`、`bytes` 和 `binary`；
预览沿用 `DOOPS_MAX_FILE_READ_BYTES` 上限（默认 1 MiB），远端生成的大资产使用 `doops pull --storage gateway`；默认 COS pull 下载此前上传的快照。
搜索直接遍历当前工作区，不依赖宿主机 grep；跳过 `.git`、`node_modules`、符号链接和二进制，
遇到超过预览上限的文件返回 `truncated=true`，实际读取错误明确失败。

### Workspace 同步协议

显式选择 `--storage gateway` 的工作区同步 CLI 使用 Git HTTP（默认 COS 快照模式见上文）：

```text
doops push --storage gateway -> /v1/git/<cluster>/<instance>/<session>.git -> agent /git/<session>.git
doops pull --storage gateway <- /v1/git/<cluster>/<instance>/<session>.git <- agent /git/<session>.git
```

`doops_workspace_begin`、`doops_workspace_chunk`、`doops_workspace_commit`、`doops_workspace_pull_begin`、`doops_workspace_pull_chunk` 是低层 JSON-RPC 文件传输工具，不是声明式发布协议的一部分。
低层 pull 在当前 HEAD 创建快照提交，并保持当前分支或 detached HEAD 状态；bundle 中的 `master` 来自专用 bare 仓库的快照引用，不依赖工作区有名为 `master` 的分支。当前分支本身为 master 时，快照提交会正常推进该分支。

需要同步当前远端工作区时直接执行：

```bash
doops -session <session> push --storage gateway --target <target> --src <dir>
doops -session <session> pull --storage gateway --target <target> --dest <dir>
```

### `doops_node_info`

返回节点基础信息和通用镜像工具链状态，包括 `buildctl`、`skopeo` 和 BuildKit
socket。镜像构建使用 CLI 的通用 `image build`，高级场景仍可通过 `doops exec` 调用
明确给出的 `buildctl` 命令；OCI 镜像复制和 manifest 校验使用 `image copy` 或
`registry sync`，不依赖业务项目脚本。

参数：

```json
{"session_id": "smoke"}
```

指定 `{"session_id":"smoke","section":"admission"}` 时，只读取 do-agent
`runtime/gcStats.admission.current`，返回名额、占用 turn 和排队 session；
字段定义及失败语义见 [使用指南](DOOPS_USAGE_GUIDE.md)。该诊断不依赖模型准入。

需要该 target 的 grant（审计动作记为 `info`）。

### `doops_database_inventory`

返回当前 target 上可识别的数据库实例。它调用与 `mcp_dosql_read operation=discover`
相同的非 Secret Kubernetes 元数据扫描，不读取 Secret，不连接数据库，也不把结果存成
第二份资产库。没有 kubectl 的节点返回 `dosql_discovery_unavailable`。

参数：

```json
{"session_id": "console-1"}
```

结果是 JSON，包含 `candidates[]`：`database_asset_id`、`namespace`、`service`、
`engine`、`port`、`database_name` 和 `status`。缺少库名的候选状态为
`needs_database_name_or_credential`，调用方必须由用户补充，不能猜测。

需要该 target 的 grant（审计动作记为 `info`）。只读查询仍走 `doops_agent_prompt`
的 `skill=dosql`，变更仍走 `doops sql --mode apply`。

### `doops_check_deployment`

检查线上部署镜像是否与最新构建一致。

需要该 target 的 grant（审计动作记为 `check`）。

### `doops_clean_workspace`

删除 `/root/ws/<session>` 和相关临时 Git 仓库。

参数：

```json
{"workspace": "deploy_20260514"}
```

需要该 target 的 grant（审计动作记为 `clean`）。

### `doops_agent_upgrade`

由目标 Agent 创建集群内 Job，升级承载自己的 Helm Deployment。Agent 进程不执行 Helm mutation。

参数：

```json
{
  "image": "registry.example.com/doops-agent@sha256:<digest>",
  "mode": "k8s",
  "release": "doops-agent-live",
  "upgrade_operation_id": "upg-example-20261111",
  "session_id": "agent-upgrade",
  "expected_release_commit": "1111111111111111111111111111111111111111",
  "dry_run": false
}
```

需要该 target 的 grant；每次升级 RPC 独占 target。Agent 必须声明 `upgradeAudit` v1
（`{"version":1,"structuredResult":true}`）；Gateway 在派发前拒绝缺少该能力的目标。
`upgrade_operation_id` 使用 1–128 位字母数字、`_`、`-`，首位为字母数字。
CLI/控制台为同一次目标升级的所有阶段复用编号；直接调用省略时 Gateway 为该 RPC 生成编号。
`requester_id`、`audit_id` 由 Gateway 按认证身份覆盖，不能由调用方指定归属。

`image` 接受 `repository:tag` 或 `repository@sha256:<64 hex>`，包括 `dry_run: true` 的预演请求。
tag 使用 `Always` 拉取并校验实际 `imageID`；回滚仍只接受历史已验证的 digest 镜像。
`dry_run: true` 只返回预演。
`observe_job` 读取 Job 状态。`rollback: true` 创建回滚 Job。namespace 必须是 Agent
自己的 namespace。Job 使用目标 Agent 的 Kubernetes 身份，不接收操作机 kubeconfig。
新 Pod 未持续就绪时 Job 回滚；CLI 在成功后检查 `structuredSSH`，探针失败会再创建回滚 Job。
`preflight: true` 创建独立预检 Job，成功后才提交实际升级。正式升级必须给出
`expected_release_commit`（完整 40 位小写 SHA）；可选 `source_commit`、`ci_url`
记录调用方核验的 CI 来源。CI URL 必须为 HTTPS，禁止用户信息、查询参数和 fragment。

Agent 的控制器及共享 kubeconfig / 专用 ServiceAccount 前提统一见
[构建与部署 SOP](BUILD_DEPLOY_SOP.md#四-agent-部署与热更新机制)。
`self-upgrade-accepted` 仅表示 Job 已提交；调用方继续以精确 `job + namespace`
观察 `self-upgrade-running/failed/succeeded` 首行状态，不能从日志中的成功字样推断结果。
操作规范要求显式指定 CLI `-session`；实现未传时自动生成并打印，不应依赖它作为操作关联键。
MCP `result.structuredContent` 给出 `phase/outcome/category/job/namespace/release/image/digest`、
`expected_release_commit/upgrade_operation_id` 等适用字段；失败、busy、unknown 同时
返回 `isError=true`。Gateway 审计中受理为 `accepted`、普通轮询为 `observing`，
失败为 `error`，成功阶段为 `success`，未知为 `outcome_unknown`。预演不构成部署证据。
审计 JSON 保存请求元数据、结构化结果及脱敏输出；通过 `correlation_id` 关联整条升级链。

终态审计写入失败返回 JSON-RPC `-32603 / audit_completion_failed`；
`error.data={audit_id,upgrade_operation_id,result}` 保留原 MCP 结果供继续观察与验收。
客户端不能因此重发变更或显示成功；CLI 最终返回非零。断连不能证明独立 Job 已停止。
最终协议和版本验收通过带 `upgrade_operation_id/session_id` 及
`expected_release_commit/required_protocol` 的 `initialize` 请求进入审计，阶段为 `verify`。
Gateway 记录目标 Agent 实际报告的 commit 与协议能力，不使用 Gateway 自身版本替代。
不匹配返回明确错误并保留同一操作编号，后续回滚可关联到失败的验收记录。

Controller Job 固定在当前 Agent 节点并保留 runtime class，仍为非特权且只共享
kubeconfig。Deployment/Pod 的 spec 镜像必须精确匹配请求的不可变引用；实际 imageID
与同一 pin 的 controller 运行身份比较，不假设 CRI 返回 registry manifest digest。
自动回滚核对升级前捕获的运行身份。完整验证后持久化 revision/image/Pod UID/runtime
证据到自身 namespace 的 `doops-upgrade-verified-<release>` ConfigMap；写入失败不报告成功。
手动回滚只选有该证据且仍 superseded 的具体 revision，不推断所有 superseded 都成功。
回执必须包含已验证的实际 `release_commit`；Job 与客户端均核验恢复版本。无历史 commit
的回执明确拒绝回滚，不能跳过验证；需单独审查经 Gateway 提交的独立恢复方案。
Job 名按 operation 和 phase 确定，预检不被 apply 删除，同编号同阶段不会重新执行。
Job 留存 7 天，Gateway 已持久化的结果证据不依赖 Job TTL。

## 自然语言发布 API 对接模式

平台通过 `doops ask --apply` 发送明确的发布任务。任务明确 target、release、
namespace 和 Chart；其它参数和业务验收按任务需要提供，不由 DoOps 统一裁决。
需要凭据时，调用方显式提供一个或多个 metadata-only credential resource
name；Gateway 根据用户对该 target 的 grant 直接读取 target/use 绑定并物化，
不调用模型生成授权 scope。凭据 payload 不进入公开 prompt、doagent 上下文、CLI 输出、日志或
审计记录；Gateway 仅通过内部凭据物化调用向目标 Agent 短暂传递 payload。

内部 Helm binding 元数据增加 `credential_id`、`grant_id`、`version_id`、`helm_release`
与 `helm_chart`，由 Gateway 从运行位置元数据填写；公开调用方不得提交 binding。
Helm target binding 只绑定 target 的 cluster/instance 和 use=helmPull；namespace、workload、release
和 Chart URL 不再参与 Gateway 或 Agent 授权匹配。认证只发送给配置的 HTTPS origin，
仓库路径不作权限条件。Agent 仅在本次 prompt 存续期间建立无 payload 的绑定租约，
检查 session、凭据身份与 resourceVersion 后消费凭据。公开 MCP 不增加密码/token 参数。
受管 Helm 发布允许任务指定的 `value_files` 和非敏感 `set_strings`，不强制 bundled-only，
认证、target 权限、包校验与单次 staging 保留；失败不改走匿名或旧登录态。

`mcp_doops_helm upgrade-install` 接收运行时绑定的 `session_id`、`release`、`namespace` 和
`chart`，以及可选的 `asset_source`、有序 `value_files`、非敏感 `set_strings`、`timeout`、
`atomic` 和 `take_ownership`。Helm 发布不增加二次确认字段或全局镜像匹配字段。
`set_strings` 的值可以是字符串、数字或布尔，均按 `--set-string` 的字符串形式传给原生
Helm；对象和数组没有单一字符串形式，会被拒绝。
`asset_source` 默认是 `remote`（HTTPS `.tgz`，当前不支持 OCI），也可显式使用 `workspace`。
DoOps 按给定顺序把 values 和覆盖传给原生 Helm，不强制重置历史 values 或 server-side
dry-run，也不要求 Chart 包含 workload 或 Ingress。`upgrade-install` 使用
`helm upgrade --install --wait`；成功回执只有在本次 release 的 status 为 `deployed` 且
revision、description 都归属于本次 mutation 时才包含 `release deployed: revision <n>`。
明确的原生 Helm 命令也可通过 `doops_shell`/`exec` 执行，其退出状态和回读证据独立于
结构化 `ask --apply` 证明。完整参数、安全边界、目标授权、凭据 origin 和失败语义见
[`NATURAL_LANGUAGE_DEPLOYMENT.md`](NATURAL_LANGUAGE_DEPLOYMENT.md)。
`ask --apply` 的成功或失败以可信结构化工具回执为准；模型最后一句话、普通流式文本或
其它无关工具的成功不能替代或覆盖本次 Helm 结果。明确标记为未执行的 validation/preparation
失败后，针对同一 target、release、namespace 的修正调用可以继续成功；真实执行失败和
`outcome_unknown` 不能由无关成功清除。

业务服务的生产发布由目标 Agent 执行。`doops-agent` 自身镜像替换不走 `ask --apply`，
走 `doops upgrade`。调用方必须以同一 session 的真实健康检查和关键业务验收作为完成证据。

## 进展和可追溯性

Doops 有三层进展数据：

- WebSocket 流：实时 `notifications/message` 输出。
- Gateway 审计库：按 user、token、target、action、session、status、命令摘要、输出尾部和时间记录持久事件。
- 目标工作区审计：`/root/ws/<session>/.doops-audit-log` 记录该 session 内执行过的 shell 命令。

已登录用户通过 Gateway API 查询审计：

```http
GET /v1/audit?cluster=doops-oilan&instance=oilan-node&session=deploy_20260514&limit=50
Authorization: Bearer <USER_TOKEN>
```

查询权限：

- 普通用户返回自己的全部操作，以及引用其拥有的凭据的事件。`user_id` 可省略、等于当前
  用户 ID 或 `self`；指定其他用户返回 `403`。
- 平台管理员可查询全部用户；`view=global` 要求平台管理员，否则 `403`。`view=global`
  不取消其他过滤条件，平台管理员也可用 `user_id=self` 只查本人。
- 过滤和权限检查在 `limit` 截断前完成；`limit` 为 1–500，省略或超范围时为 100。
  审计存储读取失败返回 `503 audit_unavailable`，不伪装成空列表。
- `before_id` 为上一页末条 ID，返回更早记录；`correlation_id` 精确匹配操作编号；
  `since`（含）/`until`（不含）使用 RFC3339。非法游标、时间或时间范围返回 `400`。
  `hide_upgrade_observe=true` 仅隐藏 `status=observing` 的普通轮询，终态仍可见。

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
GET /v1/audit?user_id=self
GET /v1/audit?view=global
GET /v1/audit?view=global&session=cred_xxx
GET /v1/audit?correlation_id=upg-example-20261111&limit=50&before_id=123
GET /v1/audit?action=agent:upgrade&hide_upgrade_observe=true&since=2026-11-01T00:00:00Z
```

凭据的资源级审计（写入版本、激活、授权、验证、吊销）不带 `cluster/instance`，
`session` 字段存凭据 ID，所以按凭据查询用 `session=<credential_id>`。创建凭据时还没有
ID，这条记录的 `session` 为空。部署时使用凭据（`credential:use`）和从 Secret 导入记在
部署会话下，按 target 或该会话查询。控制台审计页的「按凭据」范围发的就是这个查询。

### 执行生命周期

Gateway 对前台、后台、SSH、prompt 和 `ask --apply` 统一维护执行生命周期。只有
后台 `doops_bg` 在派发前要求 Agent 声明精确的 `executionLifecycle` 能力；前台调用不受
该能力门禁影响。每个 `tools/call` 都必须收到对应的终态响应；
`notifications/message` 必须在 `params.request_id` 携带本次请求 ID，Gateway 和 CLI
按请求 ID 路由流式事件，不按 session 覆盖并发请求。取消 ACK 不是原始调用的终态；
调用被取消、超时或断连且没有足够终态证据时写入 `outcome_unknown`，不能把本地等待
结束当成远端 mutation 已停止。结构化 SSH 仍由 `structuredSSH` 参数协议保护。

Web Agent 会话在 Gateway `initialize` 成功后撤销 45 秒连接计时，普通 prompt/apply
从派发起采用 60 分钟总期限，不再按首输出等待时间取消；后续进展也不会重置总期限。
`mode=metadata` 仍有独立的 45 秒请求期限。控制台 `/api/rpc` 的 `doops_agent_prompt`
同样采用 60 分钟总期限。到期后请求取消，并最多等待 15 秒接收原 RPC 的实际终态；
取消 ACK 不结束等待，仍缺少终态时报告未知结果，不自动重放变更。
CLI 继续转发工具开始、进展和结束状态通知，供调用方观察工具生命周期；这些通知是进度，
不是 apply 完成证据。Apply 的可信成功或失败回执由 Gateway/Agent 的结构化工具结果决定。

CLI/Web 可在原 `/v1/rpc` WebSocket 上取消自己的请求，不需要管理员权限：

```json
{"jsonrpc":"2.0","id":"cancel-2","method":"tools/cancel","params":{"id":2}}
```

`params.id` 必须是该连接尚未结束的原始请求 ID，不能取消另一连接的任务。
返回 `result.canceled=true` 仅表示已请求取消；原请求仍通过其原 ID 返回实际终态。
连接内重复的活动请求 ID 会被拒绝，不允许覆盖已有执行的所有权。

取消只确认请求，资源所有权保留到原始请求终态。
终态不确定时审计持久化为 `outcome_unknown`，仍报告失败，不自动重放 mutation。
`running` 包含准入等待及无需变更结果保护的只读执行，不能据此认定请求尚未派发。
Gateway 重启时遗留的 `running` 落为 `error`；已派发且需要变更结果保护的 `executing`
在失去终态证据时落为 `outcome_unknown`。终态落盘失败重试原始结果，不用推测结果替换。
历史未知审计不参与新任务准入，不改为 success，也不删除；活动任务仍受资源锁、排队和
取消机制约束，执行结束及清理完成后释放 lease。`finalizing` 仅表示审计待落盘，
不参与锁冲突；持久化失败不能把已结束任务变成永久阻塞。
已验签的 Helm 工具明确失败按失败终态处理，不封锁后续操作；同轮任何明确失败
不得被另一条成功调用掩盖。工具请求错误与执行失败保持区分，执行失败返回 MCP
`result.isError=true`，取消或缺失终态不转换成普通失败。

管理员 mutation 审计：

- 管理员 GET 接口不要求 session，也不会产生 mutation 审计；远端 RPC/CLI 仍使用显式 session。
- `/v1/admin/` 下的用户创建、密码、启停、grant 增删、token 签发/撤销、实例归档/恢复、代理路由增改删均记录调用者 `user_id + token_id`、动作和资源元数据、
  开始/结束时间与成功、拒绝或失败结果。token 签发只有一条审计，不重复记录。
- 此类事件的 `action=admin`；`command_summary` 是白名单 JSON 元数据，包含注册路由、
  HTTP method/status，以及可用的资源 ID、subject user ID、授权动作或启停值。
  请求体、密码、token 明文、配置、响应和原始错误均不进入
  summary/tail；失败只记录稳定分类（如 `http_403`、`http_502`），tail 为空。
- 全局用户创建/密码/启停、grant 增删、token 签发/撤销必须通过 `UserIsPlatformAdmin`；仅有某个 target 的 `admin`
  不得修改这些全局资源或给自己追加平台权限。此限制在 mutation 审计入口执行，
  不改变现有管理员 GET 的权限或 session 约定。
- 管理员身份不替代目标授权：代理路由检查对应目标。
  普通用户查询本人审计不会获得任何 mutation 权限。
- 配置下发使用既有 `doops_file_write` RPC 和目标 `write` 权限，不新增 admin 配置接口。
  继续使用唯一的 RPC 审计，保留 target/session/文件路径元数据，不保存文件内容、
  下游输出或原始错误；下游 tool error 记录为失败。其他 RPC/凭据沿用其既有审计。
- 管理 mutation 与配置写入在执行前检查审计 INSERT 和结果 UPDATE 可写性，失败返回
  `503 audit_unavailable`（RPC 为 `-32603`），不执行 mutation。执行后才发生的审计故障
  返回 `audit_completion_failed; mutation outcome requires verification`，不返回成功响应；
  持久的 pending 记录保留。该错误不表示远端动作已经回滚，调用方必须先核验真实状态，
  不能盲目重试。

### 实例归档

平台管理员可将已离线的实例记录从控制台的常规历史视图中归档，也可恢复归档。归档仅标记
`agent_status` 记录，绝不删除或更改该实例的用户授权、令牌、凭据绑定或审计历史。Agent 下次
成功连接时，Gateway 自动清除归档标记。

Gateway 在启动时以及之后每小时清理离线实例记录，保留期规则见
[DOOPS_GATEWAY_TUNNEL.md](DOOPS_GATEWAY_TUNNEL.md) 的 Reliability Notes。清理只作用于实例记录；
用户授权、令牌、凭据绑定和审计历史不动。Agent 再次连接会写入一条新的实例记录。
尚未删除的离线记录可按下述接口归档或恢复。

```http
POST /v1/admin/instances/archive
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{"cluster":"doops-oilan","instance":"oilan-node"}
```

恢复使用相同请求体：

```http
POST /v1/admin/instances/unarchive
Authorization: Bearer <ADMIN_USER_TOKEN>
Content-Type: application/json

{"cluster":"doops-oilan","instance":"oilan-node"}
```

成功响应为实例元数据，例如：

```json
{
  "cluster":"doops-oilan",
  "instance":"oilan-node",
  "status":"offline",
  "busy":false,
  "active_ops":0,
  "queued_ops":0,
  "archived":true,
  "archived_at":"2026-11-01T06:00:00Z",
  "archived_by":"usr_xxx"
}
```

`GET /v1/admin/instances` 默认只返回活跃窗口内的记录；`include=history` 才包含保留期内的
历史和已归档记录，以便管理员发现并恢复归档实例；每条记录新增
`archived`、可选的 `archived_at` 和 `archived_by`。归档在线实例返回 `409`；未知实例返回
`404`。重复归档和对已恢复记录再次恢复都是幂等的成功操作。

审计清理也是管理员 API：

```http
DELETE /v1/audit?before=2026-01-01T00:00:00Z
Authorization: Bearer <ADMIN_USER_TOKEN>
```

清理与 `audit:purge` 回执在同一事务提交；回执含管理员身份、
截止时间和删除数量，写入失败则清理回滚并返回 `503 audit_purge_failed`。运行中、
执行中、未知结果及清理回执不删除。`accepted/observing` 只有存在同一目标、操作编号和
Job 的后续成功或失败终态证据才可删除。未知结果的保留规则见执行生命周期，不自动归档或到期。

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
- `403`：token 有效，但缺少该 target 的 grant，或非平台管理员调用了管理接口。
- `404`：路由或请求的资源不存在。
- `405`：HTTP 方法不允许。
- `409`：请求与当前资源状态冲突，例如尝试归档在线实例。
- `503`：Gateway 鉴权或审计存储不可用（如 `gateway_auth_unavailable`、`audit_unavailable`），请求未被接受。

JSON-RPC 错误：

- `-32601`：未知方法或未知 Doops 工具。
- `-32602`：请求参数非法。
- `-32603`：Gateway 或目标 agent 内部执行失败。
- `-32003`：缺少该 target 的 grant、凭据控制面工具被公开调用，或代理上游不在白名单。
- `-32004`：目标离线。
- `-32005`：目标忙、目标队列满或等待超时。
- `-32007`：操作取消；不能单凭客户端取消 ACK 推断执行停止。
- `-32008`：`execution_outcome_unknown`，本次调用缺少可信终态或外部 mutation 被中断，
  不代表成功，不自动重放；历史未知审计不阻断新任务，活动任务仍受实际资源锁约束。
  message 为 `execution_outcome_unknown: <原因>` 时，原因是 Agent 无法证明的那一项
  （如 `process group did not terminate`、`command output drain timed out`），按原因核对；
  prompt 审计只保留稳定类别，自由文本原因在审计里收敛为裸错误码。

## 权限

用户有匹配 `cluster/instance` 的 grant 才能操作 target，grant 覆盖该 target 的全部操作。
管理接口要求平台管理员（用户 `is_admin`）。新建用户没有任何 grant。返回 `403` 时检查该用户的 grant。

## 操作规则

- 所有远程操作必须设置有意义的 `session_id`。
- 文件传输使用 `push/pull`，`read/write` 只用于小文本。
- 可重复部署和明确的原生 Helm 命令使用 `doops_shell`；首次适配、排障和自然语言发布使用
  `doops_agent_prompt`。Helm 发布沿用原生 values 与等待语义，具体回执和失败处理见
  [`NATURAL_LANGUAGE_DEPLOYMENT.md`](NATURAL_LANGUAGE_DEPLOYMENT.md)。
- 升级 agent 镜像前，先确认新镜像已能在目标环境拉取。
- 生成的脚本和报告放在 `/root/ws/<session>`，成为产品流程后要拉回源码仓库。
