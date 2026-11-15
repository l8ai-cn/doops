# ZYJ test Agent 身份

本实例的身份、期望镜像与 Gateway 配置见 [values.yaml](values.yaml)。SA 与集群级绑定由
`deploy/helm/doops-agent` 渲染，实例目录不再维护第二份 RBAC YAML。

| 项目 | 约定 |
|---|---|
| Gateway target | `gw-test` → `doops-zyj/zyj-test` |
| Helm release / namespace | `doops-agent-test` / `agents-test` |
| Kubernetes 身份 | `system:serviceaccount:agents-test:doops-agent-test` |
| 集群级绑定 | `agents-test-doops-agent-test-cluster-admin` → `cluster-admin` |
| 权限管理 | `rbac.create=true`，由同一 Helm release 管理 SA/CRB |
| 节点入口 | `hostAccess.enabled=true`，Gateway `exec` 审计下进入 Pod 所在节点 |

此次运维授权明确要求与 `gw-zyj` 对齐 Kubernetes 集群级权限。`agents-test` 是 Agent
运行位置，不再是它的管理范围；用户仍须具备对应 Gateway target grant。
不复制 `gw-zyj` 的 admin.conf，不改变其他 Agent 或用户的 grant，不删除旧的 namespace
RoleBinding。持有本 target grant 意味着可经 Agent 管理整个集群，包括 Secret。

安装与身份标准见 [CLUSTER_INSTALL_GUIDE.md](../../../docs/CLUSTER_INSTALL_GUIDE.md#agentgateway-target-与集群身份)；
收编与恢复见 [BUILD_DEPLOY_SOP.md](../../../docs/BUILD_DEPLOY_SOP.md#agent-rbac-纳管与回滚)。
跨越首次 RBAC 纳管点回滚必须由独立管理通道恢复；不能让 SA 自己删除自己的绑定后继续验证。

2026-11-15 曾部署 `v0.3.6`（DoAgent `0.2.45`），Helm revision 24，镜像 digest
为 `sha256:3ee2887fd04bfd6dfca0c811174d6619489975054156c1105b9d6a9fb7cd57a9`。
Gateway 路由到的 Pod、镜像 digest、版本与 Ready 已独立核验。首次尝试发现旧 Helm
values 与实际 Deployment 的 Gateway URL 不一致，验收失败后自动回滚；修正配置后
再次升级成功。单次控制器及恢复过程见 [FIRST_UPGRADE.md](FIRST_UPGRADE.md)。
其余现有 release values 在升级中保留，不应使用本文件覆盖未列出的配置。

标量类型修复使用 `v0.3.6`，源提交
`efda13d09682a2fd2350f652bc201e10c8d5c4b1`。镜像 digest 已由受审计 BuildKit
构建回执及仓库回读核对，写入本目录配置。正式 `doops upgrade` 的预检及独立
升级 Job 已通过，保留原 release values。Gateway 路由到的 Pod、Ready、imageID、
版本和浏览器命令均核验成功。受管 Helm 在同一请求内以原生布尔、整数和小数
部署仅含 NetworkPolicy 的 Chart，独立回读 values 确认类型正确；文件摘要未变，
验收 release 已卸载并确认清理。

正式 `ask --apply` 已验证仅含 NetworkPolicy 的 Chart：原 values 执行失败后，AI 在同一
请求中修正参数并部署成功，完整返回失败与成功回执。独立回读确认 revision 1 为 deployed、
模板未修改，测试 release 随后已清理。原 Loki Chart 匿名下载成功；只读渲染确认默认值
缺少存储配置，未安装 `test/loki`。

当前版本为 `v0.3.7`（DoAgent `0.2.46`），源提交
`d9f15472b2384a9d65917ea40ec7f7bf35b098eb`，Helm revision 25。
镜像摘要见 [values.yaml](values.yaml)。Release 流水线 `43695713658` 完成测试、
发布和制品复验；受审计构建任务 `368111e58a629d90` 成功，仓库回读的摘要与标签一致。
升级操作 `upgrade_e58d843944506755087d37f57585cd21` 完成预检、升级及协议校验；
Gateway 实际路由到的新 Pod UID 为 `3068d82d-0765-4552-a0c1-19bcee344df4`，
Ready、imageID 和运行提交均已回读核对。CLI、项目与全局 Skill 已安装同一 Release。

会话 `helm-autorepair-v037-20261115` 在同一请求内修正模板参数，成功发布仅含
NetworkPolicy 的 `test/doops-recovery-v037-20261115`，返回 `verified=true`、
`outcome=succeeded`。回执包含 35 次路径校验失败、一次真实模板失败和一次成功，
失败通过 `resolved_by` 关联成功调用；独立回读确认 revision 1 为 deployed，
三个 Chart 文件摘要未变。路径反复猜测仍待定位，不因最终成功而视为已解决。

上述测试 release 尚未清理。后续部分完成负向验收在 RPC 建立前因 Gateway 登录
凭据过期而拒绝，没有创建 `doops-partial-v037-20261115`。恢复登录后须完成负向验收，
清理隔离测试 release，并回读确认清理结果。业务 `test/loki` 已由其他操作变为
revision 6 / failed；此次平台升级没有修改它。浏览器验证使用本地生产构建连接真实
Gateway，完成新 Agent 的版本与 Ready 查询；不代表公网 Web 控制台已发布本版本。
