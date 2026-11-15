# Natural-Language Deployment

本文件是普通业务 Helm 发布的权威契约。发布决定及业务准入由用户和项目负责；
DoOps 负责认证、target 授权、执行、审计和如实返回结果。

DoOps 的 Helm 发布支持 `doops ask --apply` 自然语言任务，也支持通过 `doops exec`
执行用户明确授权的原生 Helm 命令。自然语言任务适合由 Agent 解析目标与资产；
明确命令按原生命令执行，两种入口都保留执行终态和审计。
Gateway 控制台的部署表单调用同一 `doops_agent_prompt operation=apply`，通过已登录的
Gateway target grant 提交；通道不额外判断用户是否具备发布资格。公开调用只携带
credential resource names，不携带 payload。
工具失败、连接中断和缺失终态不能成为成功结果，诊断模式不会授权 mutation。
控制台会话、身份与页面行为见 [DOOPS_WEBUI.md](DOOPS_WEBUI.md)，
表单也遵循本文件的发布契约。
apply 不要求先把代码同步到 target 的隔离工作区。Chart 可以只包含 NetworkPolicy、
ConfigMap、Ingress 或其它合法资源，不要求存在 workload、镜像或可供二次验收的 Pod。
需要设置镜像时，从任务或任务指定的仓库、Chart、values、现有 release 读取事实，
不猜测，也不要求把已有事实再粘贴到 prompt。镜像键和版本由 Chart 定义，
不固定 `image.repository/tag/digest` 或日期 tag。CI 记录、镜像证明、CORS Origin、
仓库业务名、项目目录和业务健康检查都不是 Helm 发布的全局前置门禁。

执行前从任务及其指定资产确定：

- target；
- release、namespace 和变更范围；
- Chart，以及用户选择的 values 和 Helm 选项。

建议在任务中记录变更原因及业务验收条件；这些建议不新增 Helm 准入条件。

调用示例：

```bash
doops -session release-example ask --target <target> --apply \
  --msg "在 namespace <namespace> 安装或升级 release <release>，使用 chart https://charts.example/<name>.tgz。"

doops -session network-policy-release ask --target <target> --apply \
  --msg "在 namespace <namespace> 安装或升级 release <release>，使用 chart https://charts.example/network-policy.tgz；该 Chart 仅含 NetworkPolicy。"

doops -session native-release exec --target <target> \
  --cmd 'helm upgrade --install <release> https://charts.example/<name>.tgz --namespace <namespace> --wait'
```

`--apply` 是调用端的 mutation 授权，Helm 不再要求额外确认字段或逐资源重复授权。
缺少执行必需的目标或参数时报告具体缺项，可先读取任务指定的资产，不猜测目标或凭据引用。
运行时 Helm 资产仅使用任务指定的远程 Chart/values 或本 session workspace 资产。
首次配置用 `doops -session <s> credential configure` 复用受管凭据系统完成写入、目标绑定、
验证和激活；已有资源用 ID 复用，不把配置步骤或秘密塞入发布 prompt。
`--credential` 只传 resource name，完整生命周期见
[`CREDENTIAL_RESOURCES.md`](CREDENTIAL_RESOURCES.md)。

凭据运行时只要求该 target 的 grant；已有目标绑定不需逐操作员重授权。
数据库绑定直接读取保存的 metadata，凭据 namespace 由配置决定，不依赖模型规划。

CI 可以调用同一命令。对需要新构建镜像的任务，建议记录 source commit、registry digest
和构建终态；CLI、Agent 和 Helm 工具不自动证明构建来源。使用已有 Chart 不要求先提供
CI 或镜像证据。CI 调用只负责准备临时 Gateway 登录并提交这条自然语言任务；它不
同步当前仓库、不创建 target 隔离 workspace，也不是独立部署协议，不读取环境注册表。

Agent 必须把 doagent 会话创建为 `auto`。普通 `ask` 暴露内置读取工具、
`Bash`、`mcp_doops_k8s_read` 和 `mcp_doops_web_read`；K8s 读取工具明确拒绝 Secret、ServiceAccount 和其它凭据载体。
`Bash` 由 doagent 的统一 PermissionEngine 决策，运行在目标 Agent 执行环境中，
用于结构化读取工具未覆盖的诊断；普通 `ask` 不暴露业务 workload mutation MCP。
DoOps 不提供交互式 `permission/reply`，Auto 会话的合法工具不应触发 `permission.updated`。
显式 `--apply` 才切换到 doagent 原生 `build` 模式。该会话中的 Helm mutation 使用
`mcp_doops_helm`；目标本地 Nginx 站点变更使用 `mcp_doops_web`，详细契约见
[DOWEB_INTEGRATION.md](DOWEB_INTEGRATION.md)。结构化 Helm 按任务指定的 Chart 管理 release。
工具缺失、启动失败或没有本轮完成的可信 mutation 证据时，不能宣称该 apply 成功。
独立 exec 命令保留自己的终态，不能用来补造或替代缺失的 MCP 证明。

内置 `doops` MCP 必须设置 `trusted_result_attestation=true`。doagent 只根据实际
stdio 请求和响应在本地生成结果证明，完成验证后才向 ACP 暴露 `rawOutput`。Agent 必须
逐项验证 attestation schema、tool、scope、input/result digest、运行时 session 绑定、
authority binding、`mutationAuthorized` 和 `executionMode`，再判定工具结果。
成功结果还须有合法 mutation operation；仅有 completed 状态或工具名不构成成功证据。
一次性 mutation authority 同时绑定精确 allowedTools 与自然语言任务的 context digest，并由
doagent 原子消费；可信 attestation 必须返回同一 authority
digest。同一 target 上的 apply、shell、Kubernetes mutation 和凭据物化在同一 session
内共享 mutation 锁，
并排斥同 session 的结构化 workspace 操作。不同 session/workspace 可以并行；session
不是资源沙箱，不得用不同 session 同时修改同一 Helm release 或数据库。
结构化诊断查询不受发布锁阻挡。`ask --apply`
本身不要求先执行 `push`，也不要求发布输入存在于 session workspace。

普通前台 `exec` 可以执行明确给出的 Helm `upgrade/install/rollback/uninstall`。
命令受 Gateway target grant、session 隔离、同 session 的目标操作串行化和完整退出审计约束；
该路径可用于明确命令的 Helm 发布；exec 不解析 managed credential reference，
不能读取或内联受管凭据 payload 来补齐认证。`exec --bg` 同样按原样
执行；其原始 RPC 和审计保留到进程终止，取消请求不等于终态。
Agent 只拒绝删除根目录（`rm -rf /`、`rm -rf /*`），报错 `blocked_command(<rule>): <做法>`。
只有 `exec --bg` 派发前要求完整 `executionLifecycle` 能力；普通前台 `exec`、
结构化 `ssh`、`ask` 和 `ask --apply` 不要求该声明。缺少后台能力不能阻断前台发布，
也不能静默改走另一条执行路径。

取消 Shell/SSH/bg、apply 或 Kubernetes 写操作只请求停止；本地进程/模型轮次终止不等于
远端变更已停止，Gateway 如实保留 `outcome_unknown`，仍报告失败，不自动重放 mutation。
历史未知审计不参与新任务准入，不改为 success，也不删除；活动任务仍由实际资源锁、
排队和取消机制串行，执行结束及清理完成后释放 lease；审计落盘失败返回错误并独立重试原始证据，
不再保留执行锁。人工声明不能替代真实终态或强制释放活动 lease。
已验签的 Helm 明确执行失败记为失败，不归为未知结果；Agent 继续读取相关状态与配置，
在已有授权内修复根因并验证原任务。失败不是要求用户手工排查或再次授权的理由。
已知 Helm 工具的参数拒绝也必须返回 `isError=true` 的结构化工具结果并生成证明；
已验证的失败不要求无效输入满足成功变更的 operation 条件。
可信回执确认 `phase=validation|preparation`、`executionStarted=false`，且输入已明确
release 和 namespace 时，后续针对同一 release/namespace 的可信成功可记为参数或准备修正；
原操作有效时还须保持同一 operation。实际执行后的明确失败只有在后续串行成功完成同一
release/namespace、同一 operation 和原意图时才可记为修复：upgrade-install 保持 Chart
与资产来源，rollback 保持目标 revision。不同对象或意图的成功不能覆盖失败。
每次尝试按发生顺序返回完整可信工具结果，修复通过 `resolved_by` 关联原失败与成功调用；
模型说明单列为 `summary`，不作为终态。缺失目标不得从后续调用倒推。
缺失结果或证明、传输中断和取消仍保持未知结果，不能用失败文本替代证据。
Agent 应主动反馈定位、修正和验证进度；等待工具时通道持续报告最后观察到的事件及等待时间。
doagent 将带工具调用的模型说明发送为 `agent_progress`；DoOps 脱敏后作为进度通知转发，
不计为最终回答或成功证据。DoOps 通过 `wallClockTimeoutSecs` 把剩余调用期限传入 doagent，
复核与模型调用共用本轮期限，并响应同一个取消信号。
`ask --apply` 显式传递 `completionReview=true`，由 doagent 的现有执行循环复核最终
回答与本轮工具证据；任务尚未完成时在同一轮次中继续诊断和修正，不重新提交整个发布任务。
完成复核使用 doagent 的有界 continuation 计数，整个调用仍受 30 分钟总期限约束；
复核只是继续执行的提示，最终成功仍以 DoOps 验证的工具回执为准。
同一问题连续三次没有新证据或修正时应停止该尝试，报告已完成的排查、实际阻因与下一步；
不能原样反复调用或无限等待。未知变更不自动重放。
DoOps 在发送 prompt 前检查
`initialize.agentCapabilities.promptCapabilities.rejectWhenBusy=true`，然后固定使用
`params.mode=reject`。session 忙或全局 turn 名额耗尽时返回 `doagent_prompt_busy`，
该请求未进入队列、未创建 turn，可在容量恢复后重新提交，不会在 CLI 报错后后台执行。
能力缺失返回 `doagent_prompt_admission_unsupported`；初始化失败返回
`doagent_prompt_admission_unavailable`。两者都发生在发送 prompt 前，必须修复或升级目标
运行时，不能改用旧排队路径继续执行。DoOps 与支持该能力的 do-agent 必须成套交付。
Agent 镜像构建会调用被 pin 运行时的 `initialize` 校验同一能力；版本和 revision 标签匹配
但能力缺失同样不能发布。

如果已声明该能力的运行时仍返回 `queued`，属于协议违约。只有携带 `queueRequestId`
且请求级取消明确返回 `queued_cancelled`，才能返回可重试的 `doagent_prompt_queued`。
无 token、`queued_not_found`、取消失败均返回 `doagent_prompt_queue_cleanup_unknown`；
排队项可能已执行，禁止自动重试，先核对内部准入状态及变更事实。session 级取消 ACK
不能证明某一排队项已移除，不再用它推断可重试。
Gateway 将清理未知记为 `outcome_unknown`，保留原始类别，也不按确定失败撤回准备好的凭据；
取消/超时不能将已观察到的未知终态改写成普通取消。

空 prompt 返回 `doagent_prompt_no_prompt`，不发送取消。缺失或畸形的 prompt 应答、
transport 失败仍保留 `execution_outcome_unknown`；未知准入不能取消可能共享的流式 session。
Gateway 在 deadline/取消竞态中保留已经收到的明确未执行终态，清理未知或已开始的 mutation
仍按未知结果处理。凭据准备回滚失败仍按凭据不确定处理，不套用准入前类别。
HTTP 准入超时只有同时带 `failureClass=admission_timeout`、`executionStarted=false`、
`retryable=true` 才表示请求已在准备/准入前原子撤回，映射为
`doagent_prompt_admission_timeout`。准备已经开始、应答尚未确认时保留未知结果，不能重放。
Agent 在发送 prompt 前收到取消则返回 `doagent_prompt_cancelled_before_admission`；
这个明确未执行类别与已发送之后的普通取消分开处理。
非流式 ACP 调用在排队期限内未获得准入时返回 `doagent_prompt_queue_timeout`，这是准入前的明确失败，
不代表 mutation 已开始，也不应被重写成 `execution_outcome_unknown`。
当前锁按 target 路由
隔离，没有跨 target 的 release/数据库统一锁或执行器 fencing，不支持安全强制抢占。

## Helm 资产与迁移

结构化发布调用 `mcp_doops_helm upgrade-install`，提供 release、namespace 和 chart。
`asset_source` 默认是 `remote`，
当前支持任务指定的 HTTPS `.tgz` Chart 下载和可选 HTTP(S) `value_files` URL；
结构化 remote 下载尚不支持 `oci://`，不能自动替换引用或新增 fallback。
明确原生 Helm 命令的资产形式由目标 Helm 自身处理。
`value_files` 可为零或多个，按给定顺序传给 Helm；`set_strings` 是非敏感的
Chart-specific 覆盖。每个 `set_strings` 值可以是字符串、数字或布尔；字符串按
`--set-string` 传递，数字和布尔按 `--set-json` 保留原生 Helm 类型（例如
`useTestSchema: true`）；对象和数组没有单一字符串形式，会被拒绝。values 合并及历史值
继承由 Helm 原生语义决定，DoOps 不强制重置，
也不自行读取、改写或合并历史 values。
普通 `data` 内容可以通过 `set_strings` 传入，不因键名本身被当作 Secret；
凭据字段、内联认证材料以及 `data` 中的凭据内容仍不能进入模型参数。

upgrade-install 直接执行 `helm upgrade --install --wait`，不附加 server dry-run、
manifest 类型白名单、镜像策略或工作负载/Ingress 二次验收。Helm 自身的 schema、
ownership、Kubernetes API 和等待错误仍完整返回。用户要求只读验证或预演时按该请求执行，
不把验证请求变成实际安装。

受管 Chart 通过任务指定的 Helm HTTPS 下载接口认证读取。凭据权限继承目标操作权限，
不逐项匹配 namespace、workload、release、Chart 版本和仓库路径；当前包校验后一次
staging，实际 Helm 使用该已校验文件。允许任务指定的 `value_files` 和 `set_strings`。
这些覆盖不扩大凭据授权；认证或摘要失败不重试匿名、旧登录态或其它资产。
绑定私有仓库凭据不会限制公共 Chart：下载前按仓库与 Chart 的 HTTPS origin 选择认证，
不匹配时匿名下载公共包，支持经逐跳目标校验的 HTTPS CDN 重定向，不携带私有仓库凭据。
凭据选择与重定向边界见 [受管凭据](CREDENTIAL_RESOURCES.md#helm-repository)。

需要使用已绑定 session workspace 中的 Helm 资产时，任务必须显式传
`asset_source=workspace`。此模式接受任意 session 相对 Chart 目录，必须包含 regular
`Chart.yaml`；`value_files` 可为零或多个任意 session 相对 regular 文件，不要求在 Chart
目录内，也不限定文件名。绝对路径、`..` 和 symlink escape 在 mutation 前拒绝。
与 remote 一样允许非敏感 Chart-specific `set_strings`。Helm 在精确 `/root/ws/<session>`
cwd 中执行，使用任务指定的资产与原生 values 语义；
`session_id` 不属于模型参数，由 doagent 按会话工作目录启动 MCP 子进程并由 DoOps
运行时绑定。Gateway target 只负责路由，不能作为 session 或工作区路径。
按任务另行做适用的 rollout 和功能验收；exec 命令使用自己的退出状态和回读证据，
不能将其输出冒充 `ask --apply` 的可信工具结果。

**接管已存在的对象。** 渲染出的对象已经存在、但没有 Helm 所有权元数据（例如曾被手工
apply）时，Helm 的 `invalid ownership metadata` 错误按真实失败返回。用户选择
`take_ownership: true` 时传 `--take-ownership`，接管随本次 mutation 进入审计。
该选项默认不开启，不自动补 `managed-by` / `meta.helm.sh/*` 元数据来掩盖原生错误。

### 发布终态与错误

`upgrade-install` 执行原生 Helm 后回读实际 release：status 必须为 `deployed`，
revision 和 description 必须属于本次执行。没有工作负载不影响这个判断；
仅退出码为零、revision 增加、历史 release 成功或其它调用成功都不足以证明本次成功。
失败返回稳定错误码与具体原因；工具失败时 `ask --apply` 返回
`doagent_apply_tool_failed: <工具>: <工具错误码与原因>`。

| 错误码 | 含义 |
|---|---|
| `helm_mutation_failed` | 真实 Helm 命令失败，保留脱敏后的多行 stderr；超过 4096 字符时显式标记截断 |
| `helm_mutation_invalid_result` | Helm 输出缺少可归属本次 mutation 的 revision 等必要结果 |
| `helm_verification_failed` | 实际 release 未 deployed，或 revision/description 不属于本次执行 |
| `helm_interrupted` | 本次调用用完时间预算；原因写明本次 revision 和 release 当前所处状态 |

TLS、网络、Chart 下载、包校验、认证、Helm/Kubernetes 原生错误均保留真实原因，
不改写为业务策略拒绝，也不通过关闭 TLS、换凭据或更换资产掩盖。
结构化工具不向模型返回可能携带秘密的 Chart stdout（NOTES、渲染 manifest），
错误回执保留脱敏后的原因与可信 attestation。凭据隔离不因删除发布策略而放宽。

一次 `mcp_doops_helm` 调用必须在 doagent 的 MCP 工具超时（900 秒）之前结束并留下终态。
Helm `--wait` 和 release 状态回读共用一个调用预算（810 秒），`timeout` 最大 `10m`，超出时
构建阶段直接拒绝，不开始升级。预算用完时 Agent 先给 helm 发 SIGTERM，让 Helm 自己把
revision 记为 failed 并释放锁，30 秒后仍未退出才强制结束。

升级失败后（含 Helm 自己的 `--wait` 超时，helm 非零退出）Agent 读取 release history，
按本次 mutation 的 description 找到自己的 revision，并处理它留下的状态：

| 本次 revision 状态 | Agent 的处理 |
|---|---|
| 未创建 | release 未改动，只返回 helm 的失败原因 |
| `failed` | 不回滚，报告失败 revision 和仍在生效的 deployed revision |
| `deployed`（本次结果尚未确认） | 不回滚，先核对本次 revision/description 与实际 release 状态 |
| `pending-*` | 回滚到最近的 deployed revision 以解锁 release，报告两个 revision |
| `pending-*` 且没有 deployed revision | 不处理，提示卸载或重新安装 |

预算用完时以 `helm_interrupted: <原因>` 返回；Helm 自己失败但状态已收敛时以
`helm_mutation_failed: <revision 状态>; helm reported: <原因>` 返回。未能恢复的 pending
状态如实报告，不把错误回执或恢复动作宣称为发布成功。

不同 session 的 Helm 调用不额外获取 DoOps release 独占锁。每次 upgrade-install 写入唯一
description，从本次 Helm JSON 结果取得 revision；
最终回读要求当前 release 为 deployed，且 revision/description 与本次 mutation 一致。被并发发布覆盖时
明确返回 `helm_verification_failed`，不借用另一任务的结果，也不自动重放。
upgrade-install 默认不加 `--atomic`：升级失败时失败的 revision 保留在集群里，由操作者查看后自行决定是否回滚、回滚到哪个 revision。只有调用方显式传 `atomic: true` 时才加 `--atomic`，让 Helm 在失败时自动回滚。结构化 rollback 使用 Helm 支持的 wait/timeout 参数，不传 `--atomic`。

Helm 发布成功只证明本次 release 达到上述终态，不证明所有业务路径健康。
任务要求的健康检查、镜像核对或功能验收单独执行并分别报告；检查失败时明确写出
“Helm 发布成功，业务验收失败”，未执行时说明未验证，不把它们恢复为所有 Chart 的二次门禁。

DoOps 自身 Agent/Gateway 的构建、首次安装、独立控制器升级及回滚统一见
[构建与部署 SOP](BUILD_DEPLOY_SOP.md)。这些运行时维护要求不改变本文件的业务发布契约。
