# DoOps 整改交付记录

基线为 `v0.3.16`。初次交付为 `v0.3.19`。Helm 中断观测补丁随 `v0.3.20` 发布，提交为 `5b36d1aa9cbda9f11b822060b770bc82cd85e3d8`。下表按当前可核对的代码行为列项。

| 检查项 | 当前行为 | 证据 | 状态 |
| --- | --- | --- | --- |
| 凭证绑定变动 | 修改绑定、撤销和重新启用会作废旧验证 | [credential_store_test.go](../../agent/internal/server/credential_store_test.go) | 已发布 |
| 模型物化中断 | 持久化待处理记录，恢复时清理未确认的设置 | [model_provider_test.go](../../agent/internal/server/model_provider_test.go) | 已发布，待线上凭证验收 |
| Registry 授权文件 | 归属记录或授权文件落盘失败时恢复已有内容，并核对 Secret 版本 | [registry_authfile_test.go](../../agent/internal/server/registry_authfile_test.go) | 已发布，待线上凭证验收 |
| Linux 凭证清理 | 按凭证类型路由，避免普通凭证受模型目录权限影响 | [credential_materializer_test.go](../../agent/internal/server/credential_materializer_test.go) | 已发布，非 root Linux 定向通过 |
| DoSQL 直接查询与验证 | 收紧只读语句准入并保留引号中的字面量 | [remediation_regression_test.go](../../agent/internal/server/remediation_regression_test.go) | 已发布，采用词法规则 |
| MySQL 查询模式 | 拒绝 INTO 和可执行注释 | [remediation_regression_test.go](../../agent/internal/server/remediation_regression_test.go) | 已发布，Skill 测试随 CI 通过 |
| MongoDB 文档校验 | 在解码后区分键和字面值 | [remediation_regression_test.go](../../agent/internal/server/remediation_regression_test.go) | 已发布 |
| DoSQL 部分提交 | 结果未知时保留已知的部分提交证据 | [handler_ws.go](../../agent/internal/server/handler_ws.go) | 已发布，Agent CI 通过 |
| Helm 补偿回滚 | 等待回滚并回读 live release 状态 | [helm_interrupt_test.go](../../agent/internal/server/helm_interrupt_test.go) | 已发布 |
| Helm 原生回滚和卸载中断 | 保留结果未知，补充有界的只读状态观测 | [Helm 中断测试](../../agent/internal/server/helm_interrupt_test.go) | 已随 v0.3.20 发布，macOS 和 Linux 非 root 定向通过 |
| Agent 回滚候选 | 要求匹配的成功持久化回执 | [self_upgrade_test.go](../../agent/internal/server/self_upgrade_test.go) | 已发布 |
| Edge 会话注册 | 落盘后发布，锁内复查容量并保留其他活动连接 | [remediation_regression_test.go](../../agent/internal/server/remediation_regression_test.go) | 已发布 |
| Edge ID 重用原判 | 分配器递增并隔离已释放 ID | [edge_registry.go](../../agent/internal/server/edge_registry.go) | 原问题撤回 |
| HTTP 代理流 | 响应已经开始时遇到转发失败则中止 | [proxy_outcome_test.go](../../agent/internal/server/proxy_outcome_test.go) | 已发布 |
| Git 隧道流和取消 | 失败时中止流，取消回执按 RPC ID 匹配 | [handler_ws_test.go](../../agent/internal/server/handler_ws_test.go) | 已发布，macOS 和 Linux 各重复通过 100 次 |
| 会话文本文件 | 限制相对路径访问，限制读取大小，Linux 相对路径已修复 | [remediation_regression_test.go](../../agent/internal/server/remediation_regression_test.go) | 已发布，工作区模板仍支持任意文本 |
| 非空工作区替换 | 原子交换，交换不可用时保留现有工作区 | [remediation_regression_test.go](../../agent/internal/server/remediation_regression_test.go) | 已发布 |
| Web 模型配置 | 本地存储清理旧 API key，发布入口采用凭证引用 | [Web 指南](../DOOPS_WEBUI.md) | 已发布，待线上验收 |
| Web 同源检查 | 默认使用请求源。正式部署脚本可传入公开 HTTPS origin，并检查候选与切换后容器的同源登录行为 | [部署规程](../DOOPS_WEBUI.md) · [PR #27](https://github.com/symansun/doops/pull/27) | 部署脚本已随 v0.3.21 发布，待实际服务验证 |
| Web 升级观察 | 提交前持久化可从操作编号推导的 Job，关联证据补齐命名空间 | [升级面板](../../../components/agent-upgrade-panel.tsx) | 已发布 |
| CLI 流式终态 | 拒绝缺失或空终态，解析失败以错误返回 | [CLI 流式测试](../../cli/cli/doops/client_test.go) | 已发布 |
| CLI 历史 | 新历史省略 ask 和 exec 正文，收紧旧文件权限 | [CLI 历史测试](../../cli/cli/doops/auth_test.go) | 已发布，旧文件内容保留 |
| Operator Skill 示例 | SSH、Kubernetes 和凭证配置示例经过真实 CLI 的参数校验 | [合同测试](../../test/test_sql_skill_contract.py) | 已发布 |
| DoWeb 安装挂载 | 将自定义目录作为完整挂载参数传递 | [DoWeb 脚本](../../agent/skills/doweb/scripts/doweb.mjs) | 已发布 |

[主分支 CI](https://github.com/symansun/doops/actions/runs/44874825409) 和 [发布验证](https://github.com/symansun/doops/actions/runs/44874825429) 已通过。隔离容器通过官方安装器下载并校验 CLI 和 Skill，安装回执与 CLI 版本确认同一提交。[正式发布](https://github.com/symansun/doops/releases/tag/v0.3.19)

`v0.3.20` 的主分支 CI 和发布验证均通过，隔离 Linux 容器通过正式安装器校验 CLI 和 Skill 的提交身份。该标签的本地合同测试通过 484 项及 8 个子测试。[CI](https://github.com/symansun/doops/actions/runs/44878405583) · [发布验证](https://github.com/symansun/doops/actions/runs/44878405915) · [发布资产](https://github.com/symansun/doops/releases/tag/v0.3.20)

本地浏览器使用合成账号覆盖登录、会话恢复、目标空列表、平台管理导航和凭证接口允许及拒绝响应。随后在本地合成 Gateway 中验证了凭据列表从加载到显示、表单提交暂存版本且页面未回显载荷，以及 Gateway 停止后接口返回 502、页面显示会话恢复错误。

使用 `v0.3.20` 同提交构建的本机 Gateway 与 Agent，在隔离 Kubernetes 集群上通过 HTTP 验证合成凭证创建、暂存、绑定、用户列表隔离、Gateway 重启后持久化、验证、提升及撤销。验证后临时 Secret 已清理，提升后读到一个 Secret，撤销后 Secret 数量恢复为零。HTTP 响应与运行日志未回显合成值。正式 Gateway 和 CLI 资产的 SHA-256 另已校验。随后用该正式 Linux AMD64 Gateway 二进制在容器中运行，并连接由同一发布提交构建的 Agent，在隔离 Kubernetes 集群复测上述流程。创建、列表隔离、Gateway 重启后持久化、验证、提升、撤销、Secret 清理和响应及日志脱敏均通过。

从 `v0.3.20` 发布提交构建的 Linux AMD64 Web 镜像使用了镜像源中相同 digest 的 Node 基础镜像。本地按镜像 ID `sha256:5c4aa4f057b29ccd4e45654c83e8ecbb7c59cef85c9ab455460a0363dad32892` 启动容器，校验提交标签和首页。配置的 HTTPS 来源通过同源检查并进入参数校验（400），跨源登录请求返回 403。

使用同一发布提交构建 Linux AMD64 Agent 镜像 `sha256:2754b45122a82fbe6a8a6d9a7db29ff13aa350356107a51f143dfb800ce72a22`。构建使用锁定 digest 的运行时和基础组件，Go 依赖从校验过的本地模块缓存读取。隔离容器按镜像 ID 运行 Agent 二进制并挂载临时集群 kubeconfig。与正式 Gateway 联调时，Web 容器完成会话建立与恢复并拒绝跨源登录。随后 Gateway 与 Agent 容器复跑凭证创建、列表隔离、重启后持久化、验证、提升和撤销，Secret 清理和载荷脱敏通过。

`v0.3.21` 纳入 Web 部署脚本修复，发布提交为 `e6d3d225cd7f5416583ad7d646dbf977173a9228`。主分支 CI 和正式发布复验通过，发布流程从公开资产完成 CLI 与 Skill 安装。该提交构建的 Linux AMD64 Web 镜像 `sha256:89141a437d2eb2ca5f82d735339920b728111c6108d168886ba1be8519c4bdd2` 已通过首页、配置来源登录参数校验（400）及跨源拒绝（403）检查。[CI](https://github.com/symansun/doops/actions/runs/44888902665) · [发布验证](https://github.com/symansun/doops/actions/runs/44888939631) · [发布资产](https://github.com/symansun/doops/releases/tag/v0.3.21)

## 凭证回归矩阵

| 给定 | 执行 | 预期与回归证据 |
| --- | --- | --- |
| 有效绑定和已暂存版本 | 重启 Gateway、提交验证并提升 | 绑定和版本持久化，只有当前有效绑定的验证参与提升。[存储回归](../../agent/internal/server/credential_store_test.go#L49) · [提升回归](../../agent/internal/server/credential_http_test.go#L153) |
| 非管理员请求平台凭证，或请求目标缺少绑定 | 创建或验证 | 请求被拒绝。[权限回归](../../agent/internal/server/credential_http_test.go#L13) · [目标范围回归](../../agent/internal/server/credential_verify_stored_scope_test.go#L14) |
| 导入目标离线 | 从目标导入 | 返回 503 且没有暂存版本。[离线回归](../../agent/internal/server/credential_import_failure_test.go#L53) |
| 审计不可写或外部主密钥不可用 | 创建凭证或写入版本 | 变更失败关闭。[审计回归](../../agent/internal/server/credential_http_test.go#L215) · [主密钥回归](../../agent/internal/server/credential_store_test.go#L12) |
| 验证失败或结果未知 | 验证 | 返回失败类别并完成对应目标的审计。[失败回归](../../agent/internal/server/credential_audit_target_test.go#L61) |
| 撤销时部分目标尚未清理 | 撤销、恢复目标后重试 | 在清理通过前保留撤销中状态。[撤销回归](../../agent/internal/server/credential_resource_revocation_test.go#L113) |
| 写入合成载荷 | 读取 HTTP 响应和审计 | 可读元数据不含载荷。[脱敏回归](../../agent/internal/server/credential_http_test.go#L13) |

## 实际服务验收

只读回读确认，`gw-test` 在线，Agent 报告的提交为 `e1d2ba7c24b4283efe7a1d2c0c1fa19bba960358`。部署与合成凭证验收待确认环境范围，测试目标与其他实例共用 Gateway。

| 验收项 | 需记录的证据 |
| --- | --- |
| Gateway | 发布资产 SHA-256、运行中进程的可执行文件 SHA-256、发布提交及目标重连结果。见 [Gateway 发布规程](../BUILD_DEPLOY_SOP.md#一-gateway-发布边界)。 |
| Agent | 测试目标、候选镜像 digest、实际 Pod imageID、Ready 状态、Gateway 回读的 release_commit。见 [Agent 发布规程](../BUILD_DEPLOY_SOP.md)。 |
| Web | 发布提交、实际运行的 Web 镜像 digest、公开 HTTPS origin、允许来源和跨源登录响应。见 [Web 部署规程](../DOOPS_WEBUI.md#部署)。 |
| 合成凭证 | 创建与暂存回执、两个用户的列表隔离结果、持久化回读、验证与提升回执、撤销终态和 Secret 清理结果。保留元数据和审计结果。见 [凭据生命周期](../CREDENTIAL_RESOURCES.md#分步创建和首次激活) 与 [交付规程](../../../AGENTS.md)。 |
