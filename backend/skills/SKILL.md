---
name: doops-server
description: |
  用 doops 通过 doops-gateway 连接和操作主动注册的 doops-agent 节点。
---

## 定位

本 skill 只放红线、命令选择和文档导航。规则细节只在对应主题文档里展开，按需读取：
DoOps 提供执行通道、target 授权和审计；是否发布及业务准入要求由用户和项目决定。

| 主题 | 文档 |
| :--- | :--- |
| 安装、接入、Agent 入口选择 | `docs/CLUSTER_INSTALL_GUIDE.md` |
| 日常命令、用户与 token、凭据命令 | `docs/DOOPS_USAGE_GUIDE.md` |
| 业务发布（`ask --apply`、Helm 资产） | `docs/NATURAL_LANGUAGE_DEPLOYMENT.md` |
| 凭据生命周期与错误类别 | `docs/CREDENTIAL_RESOURCES.md` |
| Harbor 镜像复制与上游同步 | `docs/HARBOR_REPLICATION_SPEC.md` |
| Agent / Gateway 发布与回滚 | `docs/BUILD_DEPLOY_SOP.md` |
| 执行生命周期、并发、取消、代理 | `docs/DOOPS_GATEWAY_TUNNEL.md` |
| HTTP / WebSocket API | `docs/DOOPS_API_GUIDE.md` |
| 模型与 ask 配置 | `docs/MODEL_STANDARDS.md` |
| 物理机 / 宿主机硬件检查 | `docs/PHYSICAL_HOST_ACCESS.md` |
| Web 控制台 | `docs/DOOPS_WEBUI.md` |
| DoSql / DoWeb 等统一工作台 skill | `docs/UNIFIED_OPERATIONS_WORKBENCH.md` |

权威源是 `backend/skills/SKILL.md`；`.agent/skills/doops/` 为安装副本，只由 `scripts/install.sh`
从 Release CLI 内嵌 commit 同步，勿直改。`docs/` 源码在 `backend/docs/`。

## 红线

- 只用 `~/.local/bin/doops`。访问 Gateway 或 Agent 的调用显式带 `-session <name>`；
  自行取合法名字，不需预创建或先 `push`。Agent 执行命令省略时会生成 session；控制面 HTTP
  不消费该参数，带上也不代表审计或隔离；同一工作区复用原名，`pull` 必须使用已有 session。
- CLI 只接受 Gateway target；没有直连 target、SSH target 或按 target 保存的 token。
  Gateway 登录态按 Gateway URL 共享，target 配置只存路由。Agent 注册默认不用 token。
- 凭据 payload 只走 stdin，不进入命令参数、prompt、日志、文档、提交或工作区。
  运行时只传 `--credential <resource-name>`（DoSql 自动绑定，见下文），只要求该 target 的 grant。
  不要用 `exec` 输出密码再让用户粘贴；已有目标 Secret 用 `credential configure --from-secret` 导入。
- 每次调用都等待 RPC 实际终态。取消 ACK、超时或断连缺少终态时报告 `outcome_unknown`，
  不自动重放 mutation，不宣称成功。
- `ask` 的文字回答是模型写的摘要，不是状态证据，可能写错 revision 或答非所问。发布前后的
  Helm release、Deployment、Pod 状态用 `doops exec`（`helm history`、`kubectl get -o json`）
  或 `doops k8s` 只读入口回读，以回读结果为准。
- 诊断/方案的 `ask` 写明只读；用户明确要求的操作直接执行，`--apply` 传递本次变更意图。
  已明确的目标和任务不再索要确认，不按 Chart 资源种类或工具调用次数重复授权。
- Helm 发布可用 `ask --apply`/`mcp_doops_helm`，也可用 `doops exec` 执行明确授权的
  原生命令。参数和终态契约见 `docs/NATURAL_LANGUAGE_DEPLOYMENT.md`。
- 发布出错后主动回读回执与目标状态，在既有授权内定位并修正；不把可自行取得的参数、
  配置或检查交给用户重复操作。缺失终态先只读核对并保留原始结果，恢复边界见
  `docs/NATURAL_LANGUAGE_DEPLOYMENT.md`。
- 运维类变更（重启、扩缩、cordon、删 Pod）用 `doops k8s` 的对应子命令直接执行；文本不算证据。
- 进入 Kubernetes Agent 所在节点只走 `doops exec` 里的 `nsenter`，并且该 Deployment 的
  `hostAccess.enabled` 必须为 `true`。这是 Gateway 的普通 `exec`：目标 grant 决定能否执行，
  审计记下用户、target、session 和有界命令摘要（最多 512 字节尾部）。不要用 `kubectl exec`、`docker exec` 或直连
  `42222` 绕过。`hostNetwork` 保持关闭，`/host` 只读。细节见 `docs/PHYSICAL_HOST_ACCESS.md`。
- 嵌套 SSH 只用 `doops ssh` 的结构化入口，不拼进 `exec --cmd`。
- 小文本用 `read/write`，目录和大文件用 `push/pull`；不用 base64、tar 或分片替代。
- `pull` 用新目录；已有 Git 目标会被 `reset --hard`，仅复用本任务无本地改动的下载目录，不能用业务仓库。
- 长任务用 `exec --bg`，不在 `exec` 里用 `&`、`nohup`、`setsid`。
- DoOps 镜像构建、Agent 独立控制器升级、Gateway 宿主机发布见 `docs/BUILD_DEPLOY_SOP.md`；
  不作为普通业务 Helm 发布准入条件。Agent 不能从自身进程替换承载它的 workload。
- 签发 token、建用户会输出秘密；按使用指南 “Gateway Admin Tokens” 存入 `0600` 私密文件，
  不读回对话。`--save-as` 不隐藏输出；无私密交付位置不签发，不 SSH 改 Gateway DB。

## 命令选择

| 需求 | 命令 |
| :--- | :--- |
| 路由 / 在线 / 准入 | `doops -session <s> list`（本地）；`doops -session <s> sync --target <t>`（补路由）；`doops -session <s> targets --target <t> [--admission]` |
| 协议能力 | `doops -session <s> capabilities --target <t> --require structuredSSH\|executionLifecycle`（调用前暴露能力缺失） |
| 发布身份 | `doops -session <s> version`（release tag 与提交；无 `--version` 参数） |
| 确定性命令 | `doops -session <s> exec --target <t> --cmd '<cmd>'` |
| 进入 K8s Agent 所在节点 | `doops -session <s> exec --target <t> --cmd 'nsenter -t 1 -m -u -i -n -- hostname'`（须 `hostAccess.enabled=true`；Gateway 按 target grant 授权并审计） |
| 长任务 | `doops -session <s> exec --bg --target <t> --cmd '<cmd>'` |
| 嵌套 SSH | `doops -session <s> ssh --target <t> --host <user@host> -- <cmd> [args...]` |
| 诊断分析 | `doops -session <s> ask --target <t> --msg '只读检查 <对象与问题>，禁止变更；返回证据与结论'` |
| 业务发布 | `doops -session <s> ask --target <t> --apply [--credential <name>...] --msg '<facts>'` |
| DoSql 自然语言 | `doops -session <s> sql --target <t> --mode query\|plan\|discover [--database <name>] --request '<req>'` |
| DoSql 原样查询 | `doops -session <s> sql --target <t> --mode query (--sql '<sql>'\|--file <f>) [--credential <name>]` |
| DoSql 原样变更 | `doops -session <s> sql --target <t> --mode apply (--sql '<sql>'\|--file <f>) [--credential <name>] [--verify '<select>'] [--dry-run]` |
| DoSql 跨库复制表 | `doops -session <s> sql --target <t> --mode apply --copy-from <源库> --database <目标库> --tables a,b [--dry-run]`（细节见 `docs/DOOPS_USAGE_GUIDE.md`） |
| K8s 只读 | `doops -session <s> k8s --target <t> get\|describe\|logs\|top\|events\|rollout status ... [-n <ns>\|-A] [-l <sel>] [-o jsonpath=...]` |
| Helm 只读 | `doops -session <s> k8s --target <t> helm list\|history\|status\|values <release> -n <ns>`（values 只给 key） |
| K8s 运维变更 | `doops -session <s> k8s --target <t> rollout restart\|scale\|node cordon\|delete pod ...` |
| DoWeb | `doops -session <s> web --target <t> --mode check\|plan\|apply --request '<req>'` |
| 小文本读写 | `doops -session <s> read --target <t> --path <file>`；`doops -session <s> write --target <t> --path <file> --file <local>` |
| 工作区推送 / 下载 | `doops -session <s> push --target <t> --src <dir>`；`doops -session <existing-session> pull --target <t> --dest <new-download-dir>` |
| OCI 镜像 | `doops -session <s> image build\|copy --target <t> ...`；`doops -session <s> registry sync --target <t> ...`（按任务选择构建目标；推送/复制需授权） |
| 一次配置凭据 | `doops -session <s> credential configure --target <t> --id <id> ...` |
| 开发凭据预检 | `doops -session <s> credential preflight --manifest .agent/doops-credentials.yaml` |
| 用户与授权 | `doops -session <s> admin user create\|list --target <t> ...`；`doops -session <s> admin grant list\|add\|delete --target <t> ...` |
| 网站代理路由 | `doops -session <s> admin proxy list\|set\|delete --target <t> ...`（`set --id` 只改传入字段） |
| token | `doops -session <s> admin token create\|list\|revoke --target <t> ...`（私密签发见使用指南；普通 ask 不提供） |
| Helm Agent 升级 / 回滚 | `doops -session <s> upgrade --target <t> --image <ref>`（`--dry-run` 只预览参数，不校验活体 release、挂载或权限）；`--rollback` 恢复上一已验证 revision。旧 Agent 无 `upgradeAudit v1` 时失败（`upgrade_audit_unsupported`），首升须经 Gateway exec 提交独立 Job，见 `docs/BUILD_DEPLOY_SOP.md` |
| CI 非交互登录 | `printf '%s' "$TOKEN" \| doops -session <s> login --gateway <u> --token-stdin` |
| 首次登录 | `doops -session <s> login --gateway <url>`（自动发现target） |

DoSql 必须从 `doops sql` 进入；普通 `ask` 不保证注入 DoSql 工具。Gateway 默认绑定当前 target
上全部 active `use=database` 凭据，有多个数据库凭据时用 `--database <name>` 选库；同一库绑了
应用账号和迁移账号时，`--sql/--file` 加 `--credential <name>` 选账号。MongoDB 使用 JSON 操作文档，
不含用户管理。完整参数和 `dosql_*` 错误类别见
`docs/DOOPS_USAGE_GUIDE.md`，凭据分步命令见 `docs/CREDENTIAL_RESOURCES.md`。
没有通用 `update` 命令；target、用户、grant、token、凭据、job 等元信息怎么改（原地改还是替换）
见 `docs/DOOPS_USAGE_GUIDE.md` 的 “Updating Metadata”。

## 场景链路

场景链路由 CLI 给出，和已安装二进制一致：`doops guide` 列出场景，
`doops guide <onboard|diagnose|release|credential|database|upgrade|user|files>` 打印顺序命令。
命令失败时核对 `next:` 的前提与授权再执行；它不扩大授权，也不允许重放结果未知的变更。

## 安装与升级

- 安装 CLI/Skill：`bash scripts/install.sh --version <tag> --project <absolute-path>`。
  它不部署 Agent；Agent 接入见 `docs/CLUSTER_INSTALL_GUIDE.md`。
- 配置用 `~/.agent/skills/doops/config.json`；`auth.json` 存令牌与元数据。
- 创建普通用户用 `admin user create`，交付前确认 grant 只含预期 target，并用隔离的
  `DOOPS_CONFIG` 验证允许与拒绝路径，流程见 `docs/DOOPS_USAGE_GUIDE.md`。
- Agent / Gateway 升级的参数、验收和回滚见 `docs/BUILD_DEPLOY_SOP.md`。

## 故障入口

- `401` 且 `code=gateway_auth_*`：读取具体分类（missing/invalid/expired/revoked/disabled/expiry）。
  `*_enrollment_auth_failed` 是 Agent/Edge enrollment token 问题。裸 `401` 不归因于受管凭据。
- `403`：已认证但权限不足。
- `credential_*` 错误与 `credential preflight failed`：见 `docs/CREDENTIAL_RESOURCES.md`。
- 报错都是 `错误码: 原因`，原因就是要修的东西，不要只看错误码去猜：
  `helm_mutation_failed|helm_mutation_invalid_result|helm_verification_failed|helm_interrupted`
  （含义见 `docs/NATURAL_LANGUAGE_DEPLOYMENT.md`）；`doagent_apply_tool_failed: <工具>: <工具错误>` 是工具自己的失败，
  模型文字说"成功"不算数；`execution_outcome_unknown: <原因>` 不重放 mutation，按原因去 target 上核对；
  `blocked_command(delete_root_filesystem)` 是 exec 唯一拦截的命令。
- `ask --apply` 没有可信 mutation 证据：检查 Agent 是否注册 `mcp_doops_helm` 及 attestation，
  见 `docs/NATURAL_LANGUAGE_DEPLOYMENT.md`。
- target 不可见：空结果可能是离线，也可能是缺少精确 grant，不能直接断言集群宕机。
- `busy=true` 只表示 Agent 自升级占用整个 target；普通操作按 session/资源互斥，语义见 `docs/DOOPS_GATEWAY_TUNNEL.md`。
- `push/pull` 失败：核对 target 在线及 session 工作区；`push` 返回已同步才确认文件就绪。
  不换用 base64、tar 或分片重试。
