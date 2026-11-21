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
| Web 同源检查 | 默认使用请求源，反代场景可显式配置可信 origin | [同源测试](../../../lib/server/same-origin.test.mjs) | 已发布 |
| Web 升级观察 | 提交前持久化可从操作编号推导的 Job，关联证据补齐命名空间 | [升级面板](../../../components/agent-upgrade-panel.tsx) | 已发布 |
| CLI 流式终态 | 拒绝缺失或空终态，解析失败以错误返回 | [CLI 流式测试](../../cli/cli/doops/client_test.go) | 已发布 |
| CLI 历史 | 新历史省略 ask 和 exec 正文，收紧旧文件权限 | [CLI 历史测试](../../cli/cli/doops/auth_test.go) | 已发布，旧文件内容保留 |
| Operator Skill 示例 | SSH、Kubernetes 和凭证配置示例经过真实 CLI 的参数校验 | [合同测试](../../test/test_sql_skill_contract.py) | 已发布 |
| DoWeb 安装挂载 | 将自定义目录作为完整挂载参数传递 | [DoWeb 脚本](../../agent/skills/doweb/scripts/doweb.mjs) | 已发布 |

[主分支 CI](https://github.com/symansun/doops/actions/runs/44874825409) 和 [发布验证](https://github.com/symansun/doops/actions/runs/44874825429) 已通过。隔离容器通过官方安装器下载并校验 CLI 和 Skill，安装回执与 CLI 版本确认同一提交。[正式发布](https://github.com/symansun/doops/releases/tag/v0.3.19)

`v0.3.20` 的主分支 CI 和发布验证均通过，隔离 Linux 容器通过正式安装器校验 CLI 和 Skill 的提交身份。该标签的本地合同测试通过 484 项及 8 个子测试。[CI](https://github.com/symansun/doops/actions/runs/44878405583) · [发布验证](https://github.com/symansun/doops/actions/runs/44878405915) · [发布资产](https://github.com/symansun/doops/releases/tag/v0.3.20)

本地浏览器使用合成账号覆盖登录、会话恢复、目标空列表、平台管理导航和凭证接口允许及拒绝响应。随后在本地合成 Gateway 中验证了凭据列表从加载到显示、表单提交暂存版本且页面未回显载荷，以及 Gateway 停止后接口返回 502、页面显示会话恢复错误。

使用 `v0.3.20` 同提交构建的本机 Gateway 与 Agent，在隔离 Kubernetes 集群上通过 HTTP 验证合成凭证创建、暂存、绑定、用户列表隔离、Gateway 重启后持久化、验证、提升及撤销。验证后临时 Secret 已清理，提升后读到一个 Secret，撤销后 Secret 数量恢复为零。HTTP 响应与运行日志未回显合成值。正式 Gateway 和 CLI 资产的 SHA-256 另已校验。

实际服务的 T2 验收仍需获得部署范围，核对 Gateway、Agent、Web 的不可变产物身份，并在该环境重复合成凭证生命周期。[交付规程](../../../AGENTS.md)
