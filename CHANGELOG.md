# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与语义化版本。

本文件是控制台「更新内容」的数据源：Web 镜像构建时从仓库根打包 `CHANGELOG.md`，控制台读取其中最新带版本号的条目作为当前版本。`[Unreleased]` 收敛尚未发布的变更，发布时整段改成 `## [版本号] — YYYY-MM-DD`，已发布条目不再改写；0.1.0–0.2.2 为回填的历史记录。

分类标题只使用 `### Added` / `### Changed` / `### Deprecated` / `### Removed` / `### Fixed` / `### Security`（可选 `### Performance`）；条目以 `- ` 开头，续行缩进；允许行内 `code`、**粗体** 与 Markdown 链接，不要使用表格或 HTML。

## [Unreleased]

### Fixed

- 受管 Helm 的 `set_strings` 接受字符串、数字与布尔值，统一按 `--set-string` 的字符串形式传给原生 Helm。`loki.useTestSchema: true` 这类 Chart 原生写法不再因参数类型在请求校验阶段被拒绝；对象与数组仍会被拒绝

## [0.3.3] — 2026-11-15

### Fixed

- 固定新版 DoAgent 运行时：完成复核的修正建议会进入后续执行，诊断工具调用不再消耗最终答复的修正次数；取消、时间与 token 预算覆盖复核调用
- Helm 的普通 `data` values 不再仅因键名被拒绝；内容中的凭据仍执行原有隔离检查
- Helm 错误保留多行诊断，避免丢失 schema 报错中的缺失配置项；失败与修复后的回执继续完整保留
- 发布诊断使用独立的进度通知，模型说明不会替代最终工具证据；将剩余执行期限传入 doagent 的本轮运行配置
- `ask --apply` 显式启用 doagent 的完成复核；模型过早给出结论时在原授权和同一轮次内继续处理未完成事项，工具失败回执与未知终态仍完整保留
- 工作区同步遵循 `.gitignore` 的有序反向规则，提交中明确放行的无密钥构建契约不会被快照阶段误删
- `pull` 捕获命令输出时排除独立的进度心跳，避免大型工作区已成功生成快照却报缺失 commit
- 清理 skill 与运维文档中遇错立即停止、重复要求用户操作及历史未知结果永久阻塞 target 的旧指导
- 开发 Skill 备份迁出加载目录，避免 `docs.release` 等历史副本继续向 Agent 提供已删除的 Helm 门禁

## [0.3.2] — 2026-11-15

### Changed

- 密码登录 token 的默认有效期从 24 小时改为 30 天，并在使用过程中自动顺延，但距签发时间最长不超过 90 天。可用 `-login-token-ttl` 和 `-login-token-max-lifetime` 调整。管理员签发的服务 token 不受影响
- `doops login` 以 `--gateway <name|url>` 指定 Gateway（登录属于 Gateway，不是 target）；`--target` 仍可作为别名解析到对应 Gateway
- Helm 发布直接执行原生 `upgrade --install --wait`，以实际 release 的 deployed 状态及本次 revision/description 判定成功；取消自设 server dry-run、manifest 类型白名单和工作负载/Ingress 二次验收，无 workload 的 NetworkPolicy 等 Chart 可正常发布
- Helm 调用移除 `expected_image` 与重复 `confirm` 参数，授权沿用 `--apply`；不再强制 `--reset-values`，values 使用 Helm 原生语义，`atomic` 与 `take_ownership` 仍由用户选择。迁移时删除旧参数并复核 values 合并结果，详见 `backend/docs/NATURAL_LANGUAGE_DEPLOYMENT.md`
- Helm 文档与 skill 不再要求唯一发布入口、CI 或镜像证明；明确授权的原生 Helm 命令也可经 `doops exec` 执行。保留身份与受管凭据隔离、可信结果及失败/未知终态，Helm 发布成功与业务健康验收分别报告
- 发布规则统一归入业务发布契约，清除重复的 Helm 工作区文档、未使用的固定 target 配置和已退役的安装参数示例；DoOps 自身升级要求不再混入普通业务发布说明

### Security

- 可为单个 target 签发 enrollment token。已在线 Agent 保持连接；该 Agent 用 token 连上一次之后，这个 target 的后续注册才需要它。未签发的 target 仍按原来的 cluster/instance 注册。不要使用 user token
- 控制台的变更请求要求 `Origin` 与控制台自身来源一致
- 密码登录对连续失败限速；未知用户名和错误密码走同一条口令校验
- 登录限速按客户端地址和「地址 + 用户名」分别计数，不会单凭用户名把别人锁在外面；经本机或内网反向代理时按代理传来的客户端地址计数；计数表有上限，被限速的请求不再做口令校验
- 某个 target 确认 enrollment 之后，未带 token 的连接即使已经通过握手，也不能顶替已在线的 Agent
- Agent 注册被 Gateway 以 401/403 拒绝时按指数退避重试（最长约 60 秒），连上后复位，并在日志里提示检查 `DOOPS_AGENT_ENROLL_TOKEN`
- 控制台同源校验在 TLS 反向代理后按 `X-Forwarded-Proto`/`X-Forwarded-Host` 推断来源，也可用 `DOOPS_CONSOLE_ORIGIN` 显式指定
- `adopt` 接管 Agent 时保留通过 Secret 注入的 enrollment token，无法保留的注入方式会直接拒绝接管

### Fixed

- 已绑定私有 Helm 仓库的 target 可以直接下载其他来源的公共 Chart；GitHub Release 的 HTTPS CDN 跳转经过逐跳校验，私有凭据不会发送到公共下载地址
- Helm 参数拒绝保留结构化失败回执和可信证明，避免一次空参数调用把整轮错误误记为 `raw_output_missing`；真正缺失证明的结果仍保持未知
- Helm 失败后可在原任务内定位、修复并继续验证；同一意图的后续可信成功关联原失败，完整保留每次工具回执。模型摘要不再替代执行结果，未知变更不自动重放
- target 绑定多个 Helm 仓库时按 Chart 的实际来源选择唯一凭据，不再因存在无关仓库而阻断公共 Chart 或要求用户重复选择
- Helm 的 `compat` 等普通 values 键不再因结尾包含 `pat` 被误判为凭据字段；实际 PAT 字段与凭据内容仍保持隔离
- 控制台连接成功后不再因 45 秒无输出中断任务；执行采用总期限，取消后继续等待原调用终态，进度通知不会取消总期限
- `k8s` 和 `image-build` 的 apply 会话可以调用 Helm，避免因选择 skill 而丢失已授权的发布工具
- 密码登录会话在持续使用时不再于 24 小时后被强制退出；控制台会刷新登录 Cookie，过期后直接回到登录页
- 同一用户未过期的密码登录 token 超过 10 个时，新登录会撤销最早的旧 token
- Gateway HTTP 服务设置请求头读取超时和空闲连接超时
- CLI 访问 COS 预签名地址时，连接、TLS、响应头以及读写停滞都有时限
- CI 运行 DoSql 的 Node 测试，并在 Go 测试前安装 PyYAML；Go 测试超时放宽到 20 分钟

## [0.3.1] — 2026-11-12

### Added

- 新 DoOps 标识：透明底 mark、横向与竖版 lockup、favicon、应用图标与 Apple 触控图标全部由同一份标识导出；README 提供亮/暗两套 lockup
- 落地页、案例页、博客、部署文档与登录页统一品牌视觉：氛围光主视觉、品牌渐变标题、卡片化内容、手风琴 FAQ 与两行式页脚

### Changed

- 主题色对齐标识：品牌蓝作为主色（填充一律使用浅色文字，禁止蓝底黑字），标识青作为选中与强调色，深色表面改为标识藏青；补充焦点环、选区色与减少动效支持
- 控制台表面统一：仪表盘指标卡、集群与活动面板、Kubernetes 操作胶囊、输入框与进度条改用共享的高架表面与品牌渐变
- 产品名统一写作 DoOps；预览构建可用 `NEXT_PUBLIC_DOOPS_DEMO=1` 打开演示模式，便于无 Gateway 的界面验收
- 控制台复用按钮、表单、分段选择与页面标签，统一主题和键盘/触摸反馈；工作区使用完整宽度与 16px 边距，保留运行中导航保护、权限判断和升级审计恢复
- 固定版本 Agent UI 使用可复现展示补丁，工具记录只显示一份，独立交互回执保持可见

### Fixed

- COS 快照从 Git tree 保存执行位，下载及 Agent 同步在新建和覆盖时恢复 `0755/0644`；缺失元数据的旧快照在写入前明确拒绝并提示重新 push，旧对象保留；按实际连接和选定 Agent 校验协议能力，避免混用旧端静默丢失执行位
- CLI 总帮助明确默认 `pull` 下载已上传的 COS 快照；获取远端当前工作区及生成产物需使用 `pull --storage gateway`
- Agent 最终验收审计写入失败保留实际结果与操作关联，CLI 不再因此回滚健康版本；控制台在业务验收完成后释放操作控件，保留审计告警和恢复记录
- 控制台最终协议或 commit 验收明确失败时自动回滚并核验历史版本；断连、审计故障和停止观察不触发推测性回滚
- 本机 Gateway 审计清理与操作者回执原子提交，回执失败保留原记录；HTTP 管理员权限与未知结果保护规则保持生效

### Removed

- 删除 Gateway 未参与实际准入或公开状态计算的旧 Busy 副本，以及 CLI 无生产调用的会话缓存写入方法；历史会话读取和目标状态接口保持原有行为

## [0.3.0] — 2026-11-11

### Changed

- 构建、发布与安装统一收敛到 GitHub：GitHub Actions 承担 CI 与 tag Release 发布，仓库不再携带镜像发布流水线配置；生产镜像由操作方以显式 registry/repository/tag 和完整 40 位源提交构建推送，安装只有 GitHub Releases 一条路径

### Fixed

- 终端命令执行期间保留输出和停止入口，锁定目标、会话及控制台切换；停止请求失败继续等待终态，退出中禁止启动新命令
- COS push 在创建传输连接前统一解析自动或目的路径推导的 session；低层工作区导出始终打包当前快照，支持 main、旧 master 并存和 detached HEAD
- Edge 隧道正确保留双向半关闭连接，终态释放 socket 与配额；全局连接上限的检查与占位改为同一临界区
- 手工发布历史 tag 时 CI 门禁和产物构建使用同一完整提交 SHA；文档明确远端生成产物需使用 `pull --storage gateway`
- Agent 升级失败不再记为成功：Gateway 持久化结构化结果、失败类别与脱敏证据，区分 Job 受理、运行和阶段终态；操作编号贯通 CLI、控制台、预检、升级与回滚
- 升级或回滚断连后保留未知结果；审计收尾失败保留 Job 信息，客户端继续验收并报告审计失败，不重复变更；手动和自动回滚核对历史回执中的实际 release commit
- 控制台审计支持操作编号、时间范围、游标分页和轮询隐藏；升级来源信息进入审计，最近一次操作可恢复观察
- 审计清理与清理回执原子提交；删除没有写入者的升级孤儿表创建及维护代码，保留现有库历史数据

### Removed

- 删除已退役的 ConfigPanel 及 16 个无调用的内部辅助函数、导出和脚本函数，保留现行配置中心及共享模块

### Security

- CLI 升级历史与调试输出脱敏，并在记录前拒绝带凭据的来源 URL；审计清理保留尚未取得 Job 终态的恢复证据
- K8s 升级脚本统一经 Gateway，不再接受本地 kubeconfig 变更入口；升级派发前检查 `upgradeAudit` v1，缺少结构化审计协议时明确拒绝

## [0.2.6] — 2026-11-11

### Changed

- 安装引导收口：公开线示例更新到当前发布；明确 CNB 默认分支是公开线分发器、CNB `v1.3.x` 必须固定 tag 并使用 `backend/scripts/install.sh`；新机器统一用 `doops login --gateway` 首次发现 target

## [0.2.5] — 2026-11-11

### Changed

- 公开仓库历史清理后重建发布资产：重新生成 CLI、Gateway、Skill 包与 build provenance，恢复 tag/commit 绑定和安装校验链；本次不包含运行时行为变更

## [0.2.4] — 2026-11-11

### Added

- 公开博客 `/blog`：中文 / English / Русский 三语言文章预渲染，含「DoOps 能做什么（业务功能、管理底座与 AI 原生）」与「再见 SSH 之后」两篇长文及 18 张配图；控制台顶栏与落地页可直达
- GitHub Release 现在同时发布 `doops-skill.tar.gz`：按 CNB 资产布局打包 `skills/SKILL.md`、`scripts/install.sh` 与 `docs/*.md`，校验和一并写入 `checksums.txt`，发布后复验包内条目
- DoSQL 在模型运行前于服务端解析唯一数据库：把解析结果作为提示并覆盖模型给出的 credential/database 参数，避免发现循环与超时
- 控制台新增「更新内容」对话框：按 `Added` / `Changed` / `Performance` / `Fixed` / `Security` / `Deprecated` / `Removed` 顺序分组（未知分类归「其他」）聚合多个历史版本，条目带版本徽标与展示范围，手动打开时可切换「自上次查看以来 / 最近 10/20/50/100 个版本」范围，支持行内 `code`、**粗体** 与 http(s) 链接
- 控制台顶栏新增「更新内容」入口与升级后自动弹出：首次访问静默记录当前版本、不弹窗；「今天不再弹出」只抑制当天自动弹出，手动入口始终可用；无数据时入口禁用，有未读版本时显示小圆点。内容来自仓库根 `CHANGELOG.md`，随 Web 镜像构建打包

### Changed

- 公开仓库结构收口：内部设计归档与评审草稿不再随仓库发布，产品文档统一由 `backend/docs/README.md` 索引，对外文章移入 `blog/`；三个 README 的文档入口同步更新
- 离线实例记录不再按 24 小时定时删除：Gateway 在启动时清理一次、之后每小时按 90 天保留窗口清理；7 天活跃窗口只决定默认列表
- 安装文档固定到发布 tag，并区分两条发布线的资产来源：公开发布线（`v0.2.x`，GitHub Releases）支持 `doops login --gateway` 首登发现，CNB 生产线（`v1.3.x`，`install.sh` 默认的 CNB Release Assets）不支持，需四项显式路由；CNB 仓库默认分支 `master` 仍是旧扁平布局，克隆时必须 checkout tag

### Fixed

- 配套文档纠正与实现不符的描述：普通 Agent 反向注册只声明 `cluster/instance`（不存在通用 enrollment token，只有 Edge 隧道使用 `-edge-enroll-token`）；非回环 Gateway 强制 https/wss；`read`/`write`/`push`/`pull` 的 COS 与 workspace 路径限制；`check`、`k8s`、凭据与审计接口的实际参数、过滤维度和错误类别

## [0.2.3] — 2026-11-11

### Added

- 统一 CLI：`login --gateway` 首次登录并自动发现 target，`sync` 补路由、`targets --admission` 回读 do-agent 名额与队列、`check` 比对线上镜像
- `credential` 命令组（create / configure / put / grant / grant-revoke / verify / promote / revoke / preflight / bundle）：凭据元数据存 Gateway，payload 只经 stdin 与私有通道，grant 在 target 上验证通过后才激活
- `sql`、`web` 命令组：经 Gateway/Agent 调用 DoSql、DoWeb skill；`image build|copy` 与 `registry sync` 承接镜像构建与复制
- `k8s` 受限运维入口：get / describe / logs / top / events / rollout status / helm 只读视图，restart / scale / cordon / delete 必须带 `--confirm`，Secret 与 ServiceAccount 不开放
- `ask --apply` 成为业务发布入口，任务内声明 target、范围、原因、Chart 与镜像资产及验收条件；支持 `--credential`、`--repo-ref`、`--database-ref`
- Web 控制台（/console）：Gateway 登录与目标切换，概览 / AI 助手 / 数据库 / 网站 / Kubernetes / 镜像 / 文件 / 知识库 / 凭据 / 审计 / 终端视图，另有部署、CI/CD、管理三个顶栏入口
- Helm Agent 自升级：`upgrade` 由目标集群内 Job 执行，升级前备份、升级后校验并在失败时自动回滚；`--dry-run` 只预览参数
- 发布与安装链：GitHub Actions 由 tag 触发构建不可变 CLI/Gateway 资产，`scripts/install.sh` 校验 sha256、release/commit 与 CLI contract 后安装，并在 receipt 记录 release、commit、skill_commit、contract 与每个 skill 路径
- Agent 镜像内置 shell / k8s / image-build / dosql / doweb 等 skill；`backend/skills/SKILL.md` 与 `backend/docs/*.md` 是项目 Skill 的唯一下发源

### Changed

- 授权单位收敛为 (user, cluster, instance)：一条 target grant 覆盖该 target 上的全部操作与已绑定凭据，旧的 per-action grant 迁移为 target grant
- `exec --bg` 承接原 `bg` 后台任务；`read` / `write` / `push` / `pull` 默认使用 Gateway COS 存储，`--storage gateway` 走本地中继
- 源码树重组为 `backend/`（gateway / agent / cli / docs / deploy / scripts / skills / test），Web 控制台位于仓库根 `app/`、`components/`、`lib/`
- 文档重组到 `backend/docs/` 并按主题单一归属，Skill 只保留红线、命令选择与链接

### Removed

- `doops audit` 命令：审计查询由 Gateway 审计记录与控制台审计视图提供
- Gateway `/metrics` 端点与 `doops-gateway token list|revoke`：token 管理改为 `doops admin token create|list|revoke`
- 命令护栏及 `DOOPS_BLOCK_EXTRA` / `DOOPS_BLOCK_DISABLE` / `DOOPS_BLOCK_OFF` 配置
- `examples/skills/` 示例库与 `install.sh --from-source`：安装只使用校验过的 Release 资产
- 顶层 `doops install`、`doops bg`、`doops build`：分别由 Helm/托管脚本接入、`exec --bg`、`image build` 取代

## [0.2.2] — 2026-11-08

### Added

- gateway `/metrics` Prometheus 端点：在线 agent 数、WS 客户端连接数、活跃操作数、工具调用计数（按 action/status）、护栏拦截计数；回环地址直接放行，非回环需 `Authorization: Bearer $DOOPS_METRICS_TOKEN`
- token 轮换管理：`doops-gateway token list` / `token revoke -id <tok_...>`，吊销后立即失效
- `examples/skills/` 示例库：nginx 巡检、证书到期巡检、日志收集打包（各含 README + 测试说明）

### Changed

- `scripts/install.sh` 默认从 GitHub Release 下载 CLI 二进制与 Skill 包（`doops-skill.tar.gz`），sha256 校验；`--version` 锁定版本，`--from-source` 保留仓库内预构建包路径
- E2E 冒烟新增 `/metrics` 断言（agents_online、exec/success 工具调用计数）

## [0.2.1] — 2026-11-08

### Added

- `doops audit`：审计查询命令，支持 `--blocked`/`--bypassed` 过滤、`--session` 聚合与 `--json` JSONL 导出；agent 侧新增 `doops_audit_query` 工具（只读授权）
- 演示站健康检查 workflow（`/health` + 登录拨测，每 30 分钟）

## [0.2.0] — 2026-11-08

### Added

- agent 命令护栏：`exec` 前拦截 10 类高危命令（递归删根/系统目录、写块设备、mkfs、关机重启、fork 炸弹、远程脚本管道执行等），命中即拒绝并写入审计（`blocked=1 rule=<id>`）；支持 `DOOPS_BLOCK_EXTRA` / `DOOPS_BLOCK_DISABLE` / `DOOPS_BLOCK_OFF` 配置
- CLI 错误分层可操作化：每类失败附下一步建议（401→重新登录、403→授权路径、护栏→规则 ID 与豁免、agent 离线→targets 核查、超时→bg 异步）
- 英文与俄文 README，三语言互链
- 微信公众号图文指南（单文件 standalone HTML）
- 护栏 scope 豁免通道：gateway 转发 exec 时注入调用方 scope（`caller_scopes`，覆盖防伪造），持 `admin`/`exec:privileged` scope 的调用命中护栏规则可放行，审计标记 `bypassed_by_scope=1`；旧 gateway/新 agent 向后兼容
- 本 Release 工作流：tag 触发自动构建 CLI（4 平台）+ agent/gateway（linux amd64/arm64）并发布附件

### Fixed

- gateway-only 校验错位：`exec`/`ask`/`write`/`read`/`info` 对 gateway target 被误拒，已将该校验移回 `bash` 子命令
- 旧 dispatcher 的误伤与混淆绕过：空白/大小写规范化，`rm -rf /tmp/x` 不再被 `rm -rf /` 规则误拦

### Security

- 护栏拦截事件纳入审计（`blocked=1`），可检索、可导出

## [0.1.0] — 2026-11-07

首个公开版本（MIT）：

### Added

- 公网 gateway + 内网 agent 反向注册架构，无需 SSH
- CLI 统一入口：exec / ask / write / read / bg / push / pull / build / upgrade / admin
- token 鉴权与 cluster/instance/action 粒度授权
- 会话工作区隔离与 `.doops-audit-log` 审计
- BuildKit 远端构建、Skills 沙盒、doagent ACP 接入
