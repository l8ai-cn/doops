# Doops WebUI

Agent 目录配置按 Gateway 和目标读取，不随 session 名称输入重新加载。文件页面的 session 输入框在名称为空、目录配置加载中或加载失败时仍可使用，错误显示在输入框旁的面板中。

session 输入保留独立草稿，按 Enter 或移开焦点时只有有效名称才更新实际操作会话；清空或输入无效名称不会创建无效代码工作区。AI 助手及发布页面的代码工作区同样使用目标 Agent 报告的工作区目录。

文件页面使用目标 Agent 的 `DOOPS_WORKSPACE_ROOT/<session>`，知识库页面使用目标 Agent 的 `DOOPS_KB_ROOT`。控制台通过 `doops_node_info` 的 `paths` section 读取实际目录；未配置时分别为 `/root/ws` 和 `/root/kb`。旧版 Agent 返回 `-32602: unsupported node info section` 时沿用这两个默认目录；其他读取配置失败均显示错误，不尝试访问默认目录。

从本机 3000 端口打开控制台时，登录页默认填写 Gateway 地址 `http://127.0.0.1:42222`。手动连接其他 Gateway 后，登录页会记住该地址。

登录页使用 14 px 文字，表单标签和输入提示采用浅色，博客链接使用浅蓝色并带下划线。

登录后的顶栏以 34 px 高显示 DoOps 标志与字标，右上角是目标 Agent 选择器：显示当前目标的集群/实例和在线状态，可切换目标或返回全部 Agent。登录后默认进入「首页」，可查看在线连接、活跃会话、机器和集群规模、正在执行、近期执行，以及集群概览和近期活动。未连接 Agent 时仍可查看首页。首页提供 AI 助手、文件管理、知识库、终端和 CI/CD 接入快捷入口。

主导航是一行拉平的功能入口，不出现「Agent 工作区」「平台控制面」这类内部工作台名称，也没有展开出来的第二行。窗口变窄时导航不换行、不堆叠：放不下的入口收进行尾的「更多」下拉菜单，当前页面对应的入口仍保持高亮。下拉菜单基于 Radix Primitives（shadcn/ui 同款底层），支持方向键、Enter、Esc 和焦点回填。目标类入口为：首页、概览、AI 助手、终端、数据库、网站、Kubernetes、镜像、文件、Agent 资产、Agent 监控、凭据、审计记录、知识库；平台类入口紧随其后，以细分隔线相隔：平台监控、机器台账、集群台账、业务系统、Agent 资产分配、发布工作负载、升级 Agent、首次接入、CI/CD 接入、实例、配置中心、用户、权限、令牌、代理路由。其中「Agent 资产分配」和「实例」及其后的管理项仅对管理员显示。目标相关入口共享右上角选择的目标；尚未选定 Agent 时先显示 Agent 目录，选定后直达所点工具。「发布工作负载」和「升级 Agent」需要目标，未选目标时置灰。刷新或断线后，仍获授权的目标会保留。离线时导航仍可使用，可查看登记资产、历史指标、凭据和审计记录；需要 Agent 执行的工具会提示等待连接，当前目标重新上线后自动恢复。

「平台监控」汇总当前可见范围的资产、异常和采集中断，页面内的看板视图（概览、拓扑、监控、巡检、覆盖率）切换的是同一份数据，不属于导航层级。「机器台账」「集群台账」「业务系统」「Agent 资产分配」各自直接打开对应台账分区，页面内不再重复一组页签。发布与管理的各个分区同样直接出现在顶部导航，页面内不再重复页签。

## 前端组件层

控制台的界面由 shadcn/ui 风格的基础组件组成，底层是 Radix Primitives，样式仍用 Tailwind v4 与仓库既有主题令牌，不引入自带设计语言的整套 UI 框架。

- 布局：`components/ui/layout.tsx` 提供 `Stack`、`Inline`、`Grid`、`Container`、`Spacer`，页面用它们表达纵向节奏、工具行和卡片墙，不再手写 `grid-cols-*` 字符串
- 容器：`Card` 系列（`components/ui/card.tsx`）、`Badge`、`Separator`
- 交互：`DropdownMenu`（`更多` 菜单）、`Tabs`、`Tooltip`、`ScrollArea`，以及基于 `react-resizable-panels` 的 `ResizablePanelGroup` 分栏
- 类名合并统一走 `lib/cn.ts` 的 `cn()`，变体矩阵用 `class-variance-authority`

新增页面应优先复用这些组件；需要新原语时在同一目录按同样方式封装 Radix，不另起一套样式体系。

控制台的下拉选项使用跟随主题的菜单，支持方向键选择、Enter 确认和 Esc 收起。资产看板仅有全平台可见范围时，直接显示范围名称。

控制台导航、目标选择器、AI 助手的状态与模型标签以及消息输入框均使用 14 px 字号。

目标选择器的在线状态表示 Agent 已连接 Gateway。「AI 助手会话」中的连接状态表示控制台能否读取该会话并接收事件。AI 助手的模型标签显示目标已下发的模型。旧版 Agent 在启动后收到凭据时，控制台会核对目标上报的模型列表和连接状态。读取会话状态失败时，可在「目标运维 → 审计记录」查看该目标的请求记录。

## 资产台账

登录 Gateway 后打开顶部导航的「机器台账」。即使机器尚未接入 Agent，管理员也可以先登记配置。普通用户看到自己获授权目标关联的资产。

- **机器**支持手工新增、搜索和编辑。物理机登记物理核心数，虚拟机登记 vCPU 数，内存与磁盘容量单位为 GiB。虚拟机可引用已登记的宿主。详情页可反查虚拟机、宿主和手工关联的 K8s Node。修改规格会更新资产记录。
- **文本导入**支持在机器台账或手工新增页粘贴多行清单。机器行格式为 `管理IPv4 账号 密码 [内网IPv4] [SSH端口]`，也支持 `-p 端口`；仅登记资产时可只填写管理 IP。分组名单独占一行（有空格的名称可用方括号包围），作用到下一分组。新机器以管理 IP 生成编号，可选物理机或虚拟机，分组、内网地址和显式端口保存在显示名称中。已有机器按唯一管理 IP 匹配。默认只导入资产，勾选「同时保存登录账号和密码」才会将登录资料送交 Gateway。核对右侧字段后点「预览导入」，再确认写入。
- **CSV 导入**先选择导入内容。选「机器台账」后，使用 UTF-8 模板填写 `asset_id,name,kind,address,cpu_count,memory_gib,disk_gib,host_asset_id`，上传后点「预览 CSV」。必填列为编号、名称和类型。`physical` 表示物理机，`vm` 表示虚拟机。宿主可以出现在同一批次的任意位置。确认前核对可新增、重复和错误行数，只导入可新增记录。每批最多 2,000 行、1 MiB。按批次号回查能看到台账写入结果和本批保存的机器密码数。
- **机器登录凭据**可从已有机器详情点击「添加登录凭据」，填写账号、密码，预览后确认。批量处理时选择「机器和登录凭据」：已有机器可用 `asset_id,username,password` 补录，新增机器在 CSV 中一并填写 `name,kind`。XLSX 可以按资产编号或台账中唯一匹配的管理 IP 关联，预览时请核对跳过的源行。每个账号单独计数。相同凭据再次上传会显示「已有」，密码冲突会阻止确认。管理员可以查看已保存的账号与本批回执。密码由 Gateway 加密保存。
- **集群台账**中点「从 Agent 导入」，填写资产编号、名称和来源 Agent。指定命名空间时仅验证该空间的 Pod 列举权限；留空时读取 Namespace 对象。先预览再确认，Gateway 在确认时重新读取。集群列表保存来源、验证时间和命名空间，断线后仍可查询。展开命名空间可进入对应范围的实时工作负载。
- **凭据**沿用 Gateway 受管凭据。在已关联在线目标的资产详情点凭据按钮，进入「当前目标可用」。具备管理权限的用户可从「我管理的」操作绑定、验证、轮换和吊销。适用类型、从已有 Secret 导入的操作见 [凭据说明](CREDENTIAL_RESOURCES.md)。资产文件只填写元数据。

迁移顺序、接口和权限见 [API 指南](DOOPS_API_GUIDE.md#资产台账)。

打开顶部导航的「Agent 资产分配」，选择已登记的 Agent，按机器、集群或业务系统逐项分配。每项资产可以供多个 Agent 查看。撤销仅影响所选目标的显式分配，台账直接关联由对应资产详情维护。登录凭据及目标授权沿用各自的管理流程。

### 在资产看板切换范围

「平台监控」用一组页签查看概览、拓扑、监控、巡检和覆盖率。「看板范围」默认显示全平台可见资产，选择集群可在当前页签查看该集群。集群对比按稳定资产编号统计多个 Agent 来源，列出可见机器、在役系统、命名空间和最后核验时间。点集群名称进入该集群，点未关联机器可回到它的台账详情。集群拓扑支持按关系层或命名空间筛选，从数据库反查依赖应用，以及查看关系证据。

从集群台账点「打开集群看板」进入同一个资产看板，范围自动选中该集群。点「集群台账」可回到原集群列表，恢复搜索和展开位置。

从机器详情点「进入目标运维」，或从集群命名空间点工作负载入口，页面会切换到该 Agent 的操作区。目标选择器旁的返回按钮回到原资产，恢复筛选和展开位置。主动切换 Agent 后，操作区采用新目标。

在「业务系统」登记系统编号、负责人和服役状态，再添加来源目标、命名空间、应用与可选数据库。保存表示操作员已核对所填关系。管理员维护记录，其他用户仅能查询自己目标内的部署。

在机器详情中可通过已领取的 Agent 核验主机指纹、获取宿主采样。主机侧按分钟生成带身份的 JSON；页面按采样时间展示系统、内核、进程和资源使用情况。[宿主采样与首次接入](MACHINE_SAMPLING.md)

从集群概览进入「拓扑」，选择已纳管的命名空间获取运行快照。管理员将已登记的应用绑定到实际工作负载 UID；有对应的 Node 与机器台账关系后，就可以从宿主机反查受影响的业务系统。Agent 掉线时请核对最后一次成功观测时间。

资产看板的「覆盖率」分别展示在役服务器、业务系统的分母、已覆盖数和距 80% 的差额。首次进入可选择「当前台账预览」核查范围，管理员可确认宿主关系并冻结基线。从已有基线导出的 CSV 使用当前用户可见的证据。分母为空时界面显示无适用资产。

「平台监控」汇总当前可见范围的资产、异常和采集中断。也可在「按采集目标查看指标」选择该范围内的来源。从主导航进入「Agent 监控」可按资产和时间范围读取当前目标的已采集样本。登记机器后仍需由对应的采集源上报指标。原生 Pod 采集依赖目标的 Kubernetes 指标读取权限。主机、设备和业务计数器需要对应的采集源。可按序列查看单位、数据中断、告警及阈值规则。

平台和集群概览均可直接查看未恢复的异常、采集中断和本周巡检。点异常明细可打开对应来源和时窗内的曲线；点周检记录可进入同一范围的报告与整改。本周无应检资产、证据不足、漏检和已检分别显示。

「巡检」用于预览周检范围、指定时区与执行账号、运行计划、查看缺口、下载报告以及登记整改和复检。手动执行后可以立即查看运行记录，检查结果在收到相应证据后更新。自动执行需要为绑定账号配置独立时钟与证据来源。[周检执行器](../inspection/README.md)

## Current Gateway Console

The production application is the Next.js console at `/console`. Users sign in
with their existing Gateway account or user token, select an explicit target,
and execute operations under that identity. The console stores the Gateway
session in an HttpOnly cookie, restores and clears it through `/api/session`,
and does not expose the token to React state, localStorage or copied examples.
When the Gateway returns a token expiry, the cookie lifetime is capped at that
expiry and never outlives the token. A token with no expiry keeps the console's
local 90-day session cap; that cap is only browser storage policy, not token
validity. `GET /api/session` refreshes the cookie `Max-Age` after a successful
identity check, so a long-lived or sliding token stays in the browser. An
authentication 401 (expired, revoked, invalid) clears the cookie and returns
HTTP 200 with `session: null`, so the console returns to the login page instead
of showing a restore failure. A temporary Gateway 5xx keeps the local session
cookie so a transient outage does not force a new login. A 403 means permission
denial and keeps the login cookie.

Password login, token import and session restore all validate against
`GET /v1/auth/me`. The Gateway supplies the real username, platform-admin flag,
current token ID, issue time and optional expiry. The console does not infer
identity from target visibility or probe an admin-only endpoint. An absent or
malformed identity endpoint is an explicit error and does not create a cookie.
Token login therefore displays the real username and restores only that user's
target/session selection. The token page marks the current login token; revoking
it removes access for the console, CLI and any other clients using that same
token. Disconnect only clears the browser cookie;
revocation remains a separate explicit action. Password-login tokens are renewed
by the Gateway on authenticated use, and the console cookie is refreshed when
`GET /api/session` succeeds; administrator-issued service tokens are not renewed.

- AI conversation uses the shared Agent Cloud `AgentWorkspace`, v2 runtime,
  queue and store. The host supplies only Gateway transport, target/session
  routing and deployment forms. The session API requires the Agent's
  explicit structured ACP event capability and never downgrades to the old chat
  engine. This source change needs a new shared package publication and matching
  Agent/Web rollout; local fixture evidence is not production acceptance.
- AI diagnostics run on the Agent and query runtime metadata instead of reading
  settings or API keys. The console sends them, and the database and website pages
  use the same prompt RPC with `operation=ask` (the website page sends
  `operation=apply` when the user submits a change). The deployment form sends
  `operation=apply` with target, release, namespace, Chart, optional values, task
  text and managed credential references.
- After Gateway initialization succeeds, the console clears the 45-second
  connection timer. Gateway owns the normal prompt/apply execution deadline
  (currently 60 minutes); the console keeps a 65-minute watchdog margin so it
  does not cancel an authorized repair before Gateway returns its terminal receipt;
  waiting for its first output or a quiet tool does not cause an earlier timeout,
  and progress events do not reset the deadline. Metadata requests retain their
  separate 45-second limit. At the execution deadline, the console requests
  cancellation and waits up to 15 seconds for the original RPC's terminal result.
  A cancellation ACK is not completion; if no terminal result arrives, the outcome
  remains unknown and the mutation is not automatically replayed. See the
  [execution lifecycle](DOOPS_API_GUIDE.md#执行生命周期).
- Tool start/end status is progress only. Apply success or failure comes from the
  trusted structured tool receipt, not the model's final wording or an unrelated
  successful tool call. An explicitly unexecuted Helm validation/preparation
  failure can be followed by a corrected call for the same target, release and
  namespace; a real execution failure or unknown outcome remains unresolved.
- Helm success is the actual release terminal state: the release must be
  `deployed` and the revision and description must belong to that mutation.
  Business health and functional acceptance remain task-level checks. Native MCP
  `isError`, protocol errors and interrupted streams remain failures. Helm
  parameters, values behavior, credential origin selection and failure handling
  are defined in [NATURAL_LANGUAGE_DEPLOYMENT.md](NATURAL_LANGUAGE_DEPLOYMENT.md).
- While a terminal command is running, target/session and in-console navigation
  remain locked so the original output and Stop control stay available. A failed
  stop request keeps the lock until a remote terminal result arrives. Background
  target refreshes do not replace a running terminal's target. Disconnecting an
  idle session displays a pending screen so a new command cannot start mid-logout.
- Open **Kubernetes** in the console to browse Pods, Deployments, nodes and namespaces on the selected target. The initial view lists Pods across namespaces. Select a kind or filter by name or namespace to find a resource. Click its name to run a read-only `describe` in the operations panel. Refresh reloads the lists through `doops_k8s` and shows read errors for the affected kind.
- To query workloads in a single namespace, type its name in **查询命名空间** and select **应用范围**, or select **查看工作负载** on a namespace row. In this scope the console requests Pods and Deployments directly in that namespace. Use **返回整个集群** to restore the cluster view. The node list shows CPU and memory as reported by the Kubernetes Node capacity fields.
- Kubernetes Execute stays in the same position and is disabled during a call.
  Stop is a separate control, so a double-click cannot cancel the submitted
  operation. Later clicks in the same double-click gesture cannot resubmit even
  after a fast result. Errors wrap below the controls without compressing their labels.
- Agents advertising `upgradeAudit` v1 have an image-reference upgrade/rollback form.
  The console runs preflight before apply, sends the source commit and CI URL, and
  joins all stages with one operation ID. Accepted is not completed: the page
  observes that Job, verifies the Gateway protocol and the expected release commit.
  A rollback uses the commit recorded in its historical receipt. Audit completion
  errors retain the accepted Job for verification and remain visible failures,
  including failures from the final initialize probe. Completed version verification
  releases the execution controls while the audit warning stays visible; the page
  does not wait for audit storage to recover. An unresolved Job still requires
  observation before another mutation. A definite final protocol or commit rejection
  starts one rollback and verifies the historical commit. Audit-only failures,
  network uncertainty and stopped observation do not start a rollback.
  The latest operation is kept in sessionStorage, scoped to Gateway, user and
  target, for resuming observation without resubmitting a mutation. The operation's
  audit records are available from the upgrade panel. First-upgrade and installation
  limits are defined in [BUILD_DEPLOY_SOP.md](BUILD_DEPLOY_SOP.md).
- Credential pages manage create, staged payload, target bindings, persisted version
  metadata, verify, promote, grant revocation and resource revocation. Payload
  input uses the credential endpoint only and never enters an AI task.
  The `凭据组` button creates a credential bundle and looks one up by name; a
  bundle holds credential IDs only. Steps and limits are in
  [CREDENTIAL_RESOURCES.md](CREDENTIAL_RESOURCES.md#bundle).
- The audit page exposes target/session/self/global scopes, action/status, operation
  ID and time filters, and cursor pagination. Routine `observing` upgrade polls are
  hidden by default; terminal observations remain visible. Each row shows its audit
  ID, operation ID, elapsed time, byte counts and available evidence.
  Users can read their own history; global audit requires platform admin.
  Credential owners also see events that reference their credentials. Audit
  query indexes are created when the Gateway starts.
- Open **数据库** and select an instance. Choose its managed connection to read the database directory. Open a database for its tables and schemas, then select a table to preview rows. The breadcrumb returns to the previous level. Access to each database uses the binding for the selected credential.
- Inventory groups a StatefulSet with Pods whose controller reference matches its UID, kind, name and namespace. Its governing Service supplies the address only when the selector matches its Pods exclusively. Other candidates remain separate. A missing service endpoint is shown as **未确认服务地址**. Previously discovered member IDs can still be used with their existing credential bindings.
- From a database or table choose **执行 SQL**. The editor uses the selected database and credential. From a table it prefills a quoted preview query. Enter a read-only statement and click **执行只读 SQL** for result columns, rows, and the DoSql operation ID. **停止 SQL** requests cancellation and shows the remote outcome.
- The database operations form routes through the same `doops_agent_prompt` Gateway RPC and
  invokes the bundled DoSql skill. It reuses the selected target, session and
  Gateway audit; database connections and credentials are never stored in the
  browser. The page is read-only (query, discover, plan); changes go through
  `doops sql --mode apply --sql/--file`.
- The **网站** tab loads DoWeb-published sites using `doops_web` with `operation=site-list`. Filter by site ID or domain. Select a site to prefill a read-only inspection with its ID and domain. The listing reads release records from the target on the first visit, before any files have been uploaded to the session workspace. It requires an Agent with `site-list` support.
- The website operations form routes through the same `doops_agent_prompt` Gateway RPC and
  invokes the bundled DoWeb skill. It reuses the selected target and Gateway
  audit for SiteSpec, Nginx validation, reload, health and rollback evidence.
- Administrator mutations are audited with actor/token IDs, resource metadata,
  HTTP result and stable errors. Credential/configuration bodies are excluded.
  Unavailable audit storage blocks a mutation.
- 管理员实例页将在线、近期离线、历史和已归档记录分开计数。历史和已归档记录默认折叠；
  只有离线记录可归档或恢复，点击对应操作直接提交。归档不会删除 target 授权、令牌、凭据绑定或审计历史，
  Agent 重新连接时会自动恢复该记录。离线超过 90 天的实例记录由 Gateway 自动删除，授权仍保留。

Explicit execution and management buttons submit their operation directly,
without an extra confirmation dialog or checkbox. Invalid inputs, unavailable
targets and operations still running retain their actual state restrictions.
Creating a new session does not replay a previous command or resolve an unknown
outcome. The original receipt stays visible.

The complete Web/CLI/skill boundary and authorization matrix is documented in
`UNIFIED_OPERATIONS_WORKBENCH.md`.

Deploy Gateway and Web from matching published artifacts — GitHub Release assets for
Gateway and explicit operator-built image digests for Web — with the canonical host
scripts. Record the binary source commit, Web digest, health, reconnects and
rollback artifact. Local fixtures do not establish live Helm or production-user acceptance.

## What's New in the Console

The console answers "what changed in this release" from the repository's
`CHANGELOG.md`, not from a Gateway feed. `Dockerfile.web` copies that file into
the Web image; `lib/server/changelog.ts` reads it while the page is rendered and
`lib/changelog.ts` parses it, so `app/console/page.tsx` hands plain release
data to the shell. The dialog needs no Gateway RPC and no runtime HTTP request,
so it keeps working with an air-gapped Gateway.

- The current version is the newest versioned entry in `CHANGELOG.md`.
  `[Unreleased]` is parsed but never shown: it belongs to a release that does
  not exist yet. Empty releases, repeated version sections and headings without
  a version are ignored. A pending change becomes visible by renaming the
  heading to `## [<version>] — YYYY-MM-DD`; the date stays optional.
- Content is merged across the releases in range: sections are ordered as
  Added, Changed, Performance, Fixed, Security, Deprecated and Removed, and an
  unknown `###` heading falls into 其他. Every item keeps the version it
  arrived in, and an item repeated across releases is shown once.
- Inline formatting is limited to `code`, **bold** and http(s) links. Other
  Markdown, including raw HTML, is rendered as text; a non-http(s) link target
  stays plain text.
- The header entry is always available when releases exist, and disabled when
  the image carries no changelog. It shows a dot while the browser's last seen
  version is older than the current one; a first visit has no remembered
  version and therefore no dot.
- The dialog shows one merged view: a "v<current> 更新内容" title, a subtitle with
  the version count and range, one heading per non-empty category with an item
  count, and every item tagged with the version it arrived in. The automatic
  popup shows the releases since the browser's last visit. The manual entry can
  switch between "since last visit" and the latest 10, 20, 50 or 100 releases;
  when nothing is new since the recorded version it falls back to the latest 10
  releases and says so. It closes through the Close button, Escape or the
  overlay.
- After an upgrade the dialog opens once by itself, unless the browser asked not
  to be popped up again today ("今天不再弹出"). A first visit only records the
  current version, so a new user is not shown history. The automatic popup waits
  until the session is restored and the onboarding guide is closed, and opens at
  most once per page load. Opening the dialog, automatic or manual, records the
  current version and clears the dot, so a reload does not open it again. The
  quiet choice suppresses automatic popups for the current local day only; it
  never disables the header entry.
- State lives in browser localStorage only, under `LAST_VERSION_KEY` and
  `QUIET_DAY_KEY` (defined in `lib/whats-new.ts`). It stores version strings and
  a local day, never a token or identity, and nothing is reported to the
  Gateway. A browser that blocks storage behaves like a first visit and can
  still open the dialog manually.
- A missing, unreadable or empty `CHANGELOG.md` is not an error: the console
  renders, the dialog has no notes and its entry is disabled.
- A user-visible change updates `CHANGELOG.md` in the same change and moves its
  `[Unreleased]` entries under the release heading when the version is cut.

## 部署

- 生产控制台是仓库根目录的 Next.js 应用，由 `Dockerfile.web` 构建，由操作方以显式
  registry/repository/tag 推送，例如 `registry.example.com/doops-web:<tag>`。
- Gateway 主机上的正式发布使用 `backend/scripts/deploy-web-console.sh`，只接受
  immutable image digest，先在 `127.0.0.1:3001` 启动候选容器并验证，再替换
  `doops-web`；旧容器保留为 rollback。随后用
  `backend/scripts/deploy-gateway-tls-proxy.sh` 发布版本化 Nginx 配置。
- 生产容器必须设置 `DOOPS_CONSOLE_ALLOWED_GATEWAYS`，值为允许代理的 Gateway
  origin，多个值用英文逗号分隔，例如
  `DOOPS_CONSOLE_ALLOWED_GATEWAYS=https://doops.example.com`。控制台只接受精确
  origin，未配置时生产代理接口拒绝请求；除本机 loopback 外，生产 Gateway 必须使用
  HTTPS。
- 控制台的变更请求（POST/PUT/PATCH/DELETE）要求浏览器 `Origin` 与控制台同源，并拒绝
  `Sec-Fetch-Site` 为跨站的请求。同一可注册域下的兄弟站点也会被拒绝。
  默认根据 Next.js 接收到的 URL 和 Host 校验来源。TLS 在反向代理终止、公开地址与服务地址不同或有多个公开地址时，
  设置 `DOOPS_CONSOLE_ORIGIN=https://doops.example.com`（多个用英文逗号分隔）。设置后只接受这些 origin。使用正式部署脚本发布单一 HTTPS 入口时，传入 `--console-origin https://doops.example.com`。脚本会验证候选容器及切换后的容器接受该来源并拒绝跨源登录请求。
- 模型密钥在凭证管理中录入、验证并启用。配置中心记录模型配置与凭证引用，打开时会清除旧浏览器配置里的 API 密钥字段。

## Console layout and shared controls

[`DESIGN.md`](../../DESIGN.md) defines console spacing, semantic tokens and control states.
Console views fill the available width with a 16px gutter; only public pages, dialogs
and document previews use bounded reading widths. Desktop and mobile share the same
components. Use `components/ui.tsx` for buttons, inputs, selects, textareas, segmented
controls and page tabs; keep business behavior and permission checks in the host panel.

The pinned `@agent-cloud/agent-ui@0.3.52` display patch is tracked in `patches/`
and applied by pnpm in local and Docker builds through `pnpm.patchedDependencies`
in `package.json`. Tool activity remains in the conversation list, and 0.3 keeps
execution evidence in the "Execution trace" drawer whose trigger counts interactions
and runs, so tool rows are not rendered a second time inline. Titles that repeat the
workspace path or translated tool label are omitted by the SDK; the patch keeps
textual progress stages/messages on the tool row, including progress without a
fraction.
