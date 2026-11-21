# Doops 构建与部署强制规范 (SOP)

本 SOP 规定 DoOps 自身 CLI、Gateway、Agent 和 Web 的构建与升级。
普通业务 Helm 发布统一见 [业务发布契约](NATURAL_LANGUAGE_DEPLOYMENT.md)；
受管凭据生命周期见 [凭据文档](CREDENTIAL_RESOURCES.md)。

## 发布前判定

安装、正常升级和旧 Agent 恢复的入口统一见
[集群接入指南](CLUSTER_INSTALL_GUIDE.md#先选部署对象)。`install.sh` 只更新客户端和
Skill。K8S 升级脚本的当前支持范围和退役参数以本 SOP 为准：
`scripts/release-managed-agent.sh` 只委托已授权 Gateway 上的 `doops upgrade`。
本机 Helm/kubectl 发布、`--values` 首次安装及离线恢复入口已从该脚本移除；
旧文档中的 kubeconfig 调用示例会直接失败，不能作为可用发布入口或已验证部署证据。

执行生命周期变更必须先验证 Gateway 的统一终态状态机，再切换 Gateway。候选
Agent 不再需要声明独立的生命周期 capability；必须验证前台和后台 RPC 的原始终态、
进程组终止、`202 cancel_requested`、持久化 `outcome_unknown` 以及按
`request_id` 路由流式通知。结构化 SSH 单独验证 `structuredSSH`。发布者仍须逐项
核对 Gateway 在线 Agent 基线，不能只以一个 target 的上线验收代替实际使用场景的
终态证据。任务没有给出升级范围时只完成源码与 CI 制品，不做生产切换。

先用 `doops version`、`doops list` 和
`doops -session <s> targets --target <t>` 核对已安装 CLI、Gateway 路由及精确
`cluster/instance`。升级前分别记录 CLI 和 Agent 的完整 commit，允许旧 Agent 与新 CLI
版本不同；不能要求升级前已经同版。升级后按选定 CI 制品验收。`doops upgrade` 以 CLI
commit 校验候选 Agent，因此直接调用或经脚本调用时，CLI 都必须与候选制品同版。源码已修改不代表 CLI、Gateway 或 Agent 已更新；
`cli-contract/v3` 只证明协议兼容性，`structuredSSH` 只证明 SSH 协议能力，
两者都不能证明 Agent 具有自升级控制器。

| 发布对象 | 入口 | 控制上下文 |
| :--- | :--- | :--- |
| 普通业务 Helm release | `doops -session <s> ask --target <t> --apply`，或 `doops -session <s> exec --target <t> --cmd '<已授权的 helm 命令>'` | 目标 Agent 的运行环境，不读取本机 kubeconfig |
| 已具备控制器的 Helm Agent 镜像 | `doops -session <s> upgrade --target <t> --image <repository:tag 或 repository@sha256:<digest>` | 集群内 Job 使用 Agent 的 Kubernetes 身份，不读取本机 kubeconfig |
| 已具备控制器的 K8S Agent 脚本入口 | `scripts/release-managed-agent.sh` | 仅转发 Gateway `doops upgrade`，只接受已核验 digest；不读取或转发本机 kubeconfig |
| 不支持自升级的旧 Agent 首升、显式 Helm 配置变更 | 经 Gateway `doops -session <s> exec --target <t>` 提交独立 Job | 需另行审查完整控制器；当前脚本未实现通用首升或配置变更 |
| 尚无 Agent、release 或恢复对象的新身份首次安装 | 手工 GitOps 初装 | 经审查的仓库 Chart/values，通过集群已有管理流程实施；当前没有自动 installer |
| 无在线授权管理通道的已有 K8S Agent 恢复 | 停止 | 不能使用本机 kubeconfig 或凭据绕过 Gateway，也不能改称初装 |
| 已有独立 Docker Agent | `scripts/release-host-agent.sh` | 外部宿主 SSH；不依赖旧 Agent、K8S 或 Helm |
| 空 Ubuntu/Debian 服务器 | `scripts/bootstrap-host-agent.sh` | 外部 root；安装 Docker/Compose，不要求预置模型 settings，Agent 连接后由 Gateway 下发模型；不覆盖已有 Agent |
| Gateway 控制面 | `scripts/deploy-gateway.sh --no-build` | 已核验 CI 产物，通过宿主机 SSH 发布，不通过 Agent target |

`-session` 用于操作上下文和审计关联，不是要求创建 Git worktree 或先 `doops push`。
CLI 安装必须把 Release Asset 内嵌 commit 对应的 skill/docs 一起安装；不得从当前
未发布工作区复制规则去搭配另一个版本的 CLI。安装记录中的 `commit` 与 `skill_commit`
必须一致。

> **核心纪律：绝对禁止在本地开发机或业务宿主机直接执行容器镜像构建。**
>
> 本地开发机的架构（如 Apple Silicon ARM64）、网络环境（代理/污染）以及未清理的缓存，极易导致打出的 OCI 镜像体积过大、架构错配或携带不安全的凭据。
> 所有线上与测试环境的 `doops-agent` 镜像，**必须且只能由受控构建流程（CI 或经指定的构建主机）按标准流程构建、校验并推送**；
> 推送目标由操作方显式给出 registry/repository/tag 与完整 40 位源提交，本仓库不含镜像发布流水线。

---

## 一、 Gateway 发布边界

`doops-gateway` 是公网控制面服务，必须作为独立发布包处理。它不是普通
`doops-agent` target，也不登记为 doops target。

当前 Gateway 是单活服务。`serve` 在打开数据库和恢复执行状态之前，对规范化的
数据库路径持有进程独占锁；第二实例会立即失败，不修改运行中实例的状态。进程退出
自动释放锁，不能删除 `.lock` 文件强行接管。首次升级到此版本必须先停止未实现该锁
的旧服务；这不是跨主机分布式租约。

固定发布入口：

```bash
bash scripts/fetch-gateway-release.sh \
  --release-tag <verified-release-tag> \
  --source-commit <verified-release-commit>

bash scripts/deploy-gateway.sh \
  --host <gateway-ssh-host> \
  --user <gateway-ssh-user> \
  --binary bin/doops-gateway \
  --no-build \
  --source-commit <verified-main-commit> \
  --verify-target gw-edu-coder \
  --verify-session gateway-release-<release>
```

当前 gateway 维护主机：

- SSH 地址与用户：由运维密钥库或维护人保管，文档、示例和提交信息只保留占位符。
- Gateway URL：`https://doops.l8ai.cn`
- SSH 凭据、生产 IP 和登录用户禁止写入仓库、日志、提交信息和文档正文。

这条脚本只走 SSH/SCP，在宿主机真实 root 下完成：

1. 从 GitHub Release 下载同 tag 的 `doops-gateway-linux-amd64`，按
   `checksums.txt` 校验，并确认二进制内嵌 release tag 与 commit；禁止在发布控制端重新编译。
2. SSH 校验远端真实 DB `/var/lib/doops-gateway/gateway.db` 存在且非空。
3. 上传二进制到远端 `/tmp`。
4. 备份 `/usr/local/bin/doops-gateway`。
5. 替换二进制并重启 gateway。
6. 校验 `/proc/<pid>/exe` hash 与下载的 Release Asset 一致。
7. 校验 `/v1/targets` 未认证访问返回 `401`，并通过 `--verify-target` 复用 DoOps 已配置认证，
   确认发布前的全部 target 均已重新连接；不得为发布导出或新建明文 token 文件。
8. 校验候选二进制内嵌的 `source-commit` 与本次构建 commit 完全一致；
   旧的本地 `bin/doops-gateway` 或没有 provenance 的二进制会在上传前拒绝。

禁止事项：

- 禁止用 `doops exec` 启动或重启 `doops-gateway`。
- 禁止用 `doops push/write/read` 给 gateway 控制面发版。
- 禁止让 `deploy-gateway.sh` 在发布控制端执行默认本地构建；线上发布必须显式
  使用 `--no-build` 和由 `fetch-gateway-release.sh` 下载并校验的 CI Release Asset。
- 禁止用 base64/tar/分片临时传输替代 gateway 发布脚本。
- 不保留 gateway host agent 作为默认 target；需要主机巡检时走独立 SSH/监控流程。

排查 `doops push` 时先确认存储模式。默认模式检查 COS 配置、快照能力和工作区同步结果。指定 `--storage gateway` 时检查 Git HTTP 反向隧道。

---

## 二、普通业务 Helm 发布

业务发布由用户及项目确定，DoOps 提供已授权 target 上的执行通道。
自然语言入口为 `doops -session <s> ask --target <t> --apply`，
明确命令入口为 `doops -session <s> exec --target <t> --cmd '<helm 命令>'`。
参数、工具模式、资产、凭据和终态只在
[业务发布契约](NATURAL_LANGUAGE_DEPLOYMENT.md) 定义。
下文的 DoOps 自身镜像来源、独立控制器及版本验收要求不扩展为普通业务发布门禁。

## 三、Agent 镜像构建工作流 (Server-Side Build)

本节只负责构建、校验和发布 `doops-agent` 运行时镜像，不负责普通业务 workload。
进行任何 Agent 运行时修改后，由受控构建流程（CI 或经指定的构建主机）从完整 40 位源提交
构建并推送候选镜像，推送时显式给出 registry/repository/tag；本仓库不含镜像发布流水线，
GitHub Actions 只负责测试与 CLI/Gateway/Skill 发布。操作方应把同一构建推成
`<repository>:<40-hex-commit>` 和明确的版本 tag，`latest` 只作为最近一次推送的指针：

```bash
docker pull registry.example.com/doops-agent
docker pull registry.example.com/doops-agent:latest
```

Web 控制台镜像 `registry.example.com/doops-web:<tag>` 按同样规则构建推送。
`latest` 会移动，不是版本证据：生产升级仍用 `vX.Y.Z` 或 digest。该镜像携带同一 revision 的 `/app/doops-agent` 和锁定版本的
`/usr/local/bin/do-agent`，不包含 Gateway；Gateway 只作为同 tag 的 Release Asset
`doops-gateway-linux-amd64` 发布，不得在发布控制端重新编译。生产候选镜像必须解析为 registry digest；`doops upgrade` 也接受明确的
`repository:tag`，并在运行时校验实际 `imageID`。发布者必须自行保留精确 source commit、
registry digest 和构建成功证据；Agent、CLI 和
发布脚本不会替发布者证明构建 provenance。

### 双镜像构建原则

发布时维护两类镜像：

```bash
registry.example.com/doops-agent/base-light:<release>
registry.example.com/doops-agent:<release>
```

`Dockerfile.base.light` 只放基础运行时：doagent、BuildKit、`buildctl`、`skopeo`、kubectl、Python+PyYAML、Helm、SSH 客户端和系统包。`buildctl` 是标准镜像构建入口，`skopeo` 是标准 OCI 复制/检查入口。它必须保留 `ssh` 客户端但删除 SSH 服务端。`Dockerfile` 只放轻更新：doops-agent、skills、docs、entrypoint。禁止把基础 rootfs 压平成每个 release 都变化的单个多 GiB layer。

基础镜像必须作为独立 repository 发布（例如 `<registry>/<repository>/base-light`）；
不要把它做成 Agent repository 的某种别名。

正式发布构建先从同一提交构建并校验基础镜像，把其不可变 digest 作为 Agent
构建输入。基础运行时变更（例如 DoWeb 所需 Node）不能只进入候选构建、却在正式
发布构建中继续引用旧 base digest；没有本轮基础镜像产物时直接失败，不回退。

发布后必须验证：

1. `/app/doops-agent -help` 可运行。
2. `/usr/local/bin/do-agent --version` 与发布声明的 doAgent 版本完全一致。
3. `buildctl --version` 和 `skopeo --version` 可运行。
4. `python3 -c 'import yaml'` 可运行。
5. `helm version --short` 可运行。
6. `ssh -V` 可运行，且镜像中不存在 `sshd`。
7. `doops exec` 能读取 `hostname/date/df/free`；K8S 节点另验 `kubectl get nodes`，独立 Docker 不要求 K8S。
8. `doops ssh` 能代理用户明确给出的结构化 SSH 客户端命令；不得把多层 SSH
   拼进 `doops exec` 字符串、内联凭据或启动 SSH 服务端。
9. `doops ask` 能通过 doagent ACP HTTP 调用工具并返回结论。

镜像构建必须完成 base label 与前 6 项镜像内自检；第 7 至 9 项必须在 Agent 滚动更新后对真实 target 执行。

---

## 四、 Agent 部署与热更新机制

普通业务 Helm release 的资产、授权和终态契约统一见
[`NATURAL_LANGUAGE_DEPLOYMENT.md`](NATURAL_LANGUAGE_DEPLOYMENT.md)；本节只规定 Agent
自身升级，不重复定义业务发布门禁或验收。

已经运行该控制器的 Helm Agent 用 `doops upgrade` 自升级，不需要操作机 kubeconfig。
该路径要求 Agent 位于自身 namespace、运行标准 Helm-managed Deployment，并已有
`gateway.cluster/instance` 匹配的 Helm release；未提供 `--release` 时必须只有一个匹配
release，提供 `--release` 时按名称选择并校验该 release。Job 复用 Agent 已挂载的
`/root/.kube` 或 `/root/.kube/config`；无该挂载时，使用 Agent 已配置的专用
ServiceAccount，不能用空值或 namespace 的 `default` 账号替代。
两种部署方式均须具备实际所需的集群权限，不读取操作机 kubeconfig。
Agent 只创建 Job，在 Agent 进程外执行 Helm upgrade，并在
新 Pod 未持续就绪时回滚。镜像可以是 `repository:tag` 或 `repository@sha256:<64 hex>`；
tag 使用 `Always` 拉取，回滚只接受历史已验证的 digest。
真实升级前 CLI 先对每个 target 提交一个预检 Job（`preflight=true`）：Job 以目标镜像启动，
所以镜像必须先被节点成功拉取；随后重复升级的全部改动前检查（Helm release 唯一、
当前 Pod 基线；现有 Deployment 的 `maxSurge`/`maxUnavailable` 不是准入条件，由 Chart 在
同一次升级里改成 `maxSurge=1`、`maxUnavailable=0`），并对同一 values 做 server-side
Helm dry-run。预检不修改 release；它写独立操作回执，不把目标版本写成已验收的 Helm revision。
预检失败（含镜像拉取不到，预检 Job 600 秒
超时）时 CLI 报 `preflight failed, nothing was changed`，不提交真实升级 Job。
未纳入 Helm 的收编路径没有 release 可 dry-run，预检只证明镜像可拉取。
新 Gateway 在派发升级调用前要求 Agent 声明 `upgradeAudit v1`；旧协议 Agent 返回
`upgrade_audit_protocol_required` 并记录 blocked，不会先创建 Job 再因缺少结构化结果失败。
这是协议升级边界：没有该能力的旧 Agent 不能直接使用本命令完成首升，也不接受文本成功响应代替协议。
CLI 在 Job 成功后执行 `capabilities --require structuredSSH`，探针失败会创建回滚 Job。
升级后校验 `release_commit` 时，CLI 期望的是**自己这次发布**的 commit，即发起 `doops upgrade`
的 CLI 必须与目标 Agent 镜像出自同一次发布（同一 commit）。CLI 比 Agent 新或旧都会导致
`target Agent release commit mismatch`，并按上面第 3 条触发自动回滚；升级前允许 CLI 与 Agent
不同版本，但升级后的 `release_commit` 校验要求同版，因此必须先用与候选 Agent 制品同 commit
的 CLI 发起升级。
操作编号贯通预检、apply、observe、最终协议/commit 探针和 rollback，并写入 Gateway 的
`correlation_id`。Job 注解、独立操作回执与历史 revision 回执记录请求者、session、audit ID、
镜像、实际 commit、Pod UID、runtime digest 和时间。各阶段 Job 独立命名，保留 7 天；
`doops-upgrade-receipt-<operation-hash>-<phase>` 在 Job 回收后仍可用于核验，观察结果进入
Gateway 的持久审计。缺失或冲突的回执不能作为成功证据。
手动回滚使用当前 Agent 的控制器镜像执行验证，恢复历史已验证的镜像与 commit；不运行
旧版本镜像里的验证逻辑。无历史 commit 回执时拒绝回滚，须按受审计的恢复方案处理。
收编备份改为带操作编号的不可变 Secret，保留完整 Deployment 供恢复，不在 ConfigMap
或日志内长期保存其明文环境变量。Secret 的保密性仍取决于集群 RBAC 和静态加密配置。

2026-11-16 fleet 的旧 Helm Agent 首升控制器和进度保存在
[`deploy/fleet-20261116/`](../deploy/fleet-20261116/PROGRESS.md)。该一次性控制器的
`render/run/accept` 只支持独立挂载 kubeconfig、单副本且可保持现有权限和挂载的实例，
由 Gateway exec 提交独立 Job；它不是通用安装器。真实集群试点通过前不得宣称已支持
其它运行方式，不能用本地事务测试代替逐 target 的发布验收。

`PREVIEW` 仅表示参数预览，不检查 Helm release、挂载、权限或回滚可行性；`ACCEPTED`
仅表示 Job 已提交。Job 完成和协议声明不是完整发布验收：还须通过同一 target 的
`exec hostname` 确认 Gateway 当前实际服务的 Pod，并核对该 Pod 的 UID、Ready、
imageID 与候选 digest、`do-agent --version` 和实际功能路径。
审计收尾失败时保留 Job 恢复信息并继续验收，CLI/控制台最终仍报告审计失败；不得重发 apply。

第三条保证（校验失败自动恢复备份）只有在回滚会恢复的那个镜像还在 registry 里时才成立。
因此改动前检查还必须解析回滚目标：从 `helm history` 取唯一 deployed revision，
用 `helm get manifest --revision` 读出该 revision 的 doops-agent 镜像，再用
`skopeo inspect --raw` 读这个镜像的清单。认证用 Agent Pod 自己的 imagePullSecrets，
也就是 kubelet 拉回滚 Pod 时用的凭据；Agent Pod 没有 pull secret 时匿名读取，
和 kubelet 一致。Secret 内容只写进一个 0600 临时文件给 skopeo 读，检查结束即删除，
不进参数也不进 Job 日志。不新增凭据维度、挂载或权限：升级 Job 本来就要读写
同 namespace 里存放 Helm release 的 Secret。

检查直接问 registry 而不是让 kubelet 真拉一遍：kubelet 的拉取要排在节点上其它拉取
后面，繁忙节点上要等几分钟，却证明不了更多。探针失败按稳定类别上报，预检不做任何改动：

- `rollback_baseline_image_unavailable`：registry 回答没有这个清单
  （`manifest unknown`、`name unknown`、`artifact … not found`）。
- `rollback_baseline_image_unresolved`：历史 revision 不含唯一 doops-agent 镜像，
  或 release 没有唯一 deployed revision。
- `rollback_baseline_probe_failed`：其它失败，原因写在 `reason=` 里：registry 拒绝凭据
  （`unauthorized`/`denied`/`forbidden`，先于"缺失"判断，因为有的 registry 会把拒绝说成
  not found）、registry 连不上或超时、pull secret 读不到或不是 registry 凭据。
  这些都不是镜像已丢，不能用 `--allow-missing-rollback-baseline` 放行。

默认 fail closed。`rollback_baseline_image_unavailable` 的标准处置是下面的手工发新停旧
（仓库里没有对应的自动化实现），不使用 `--allow-missing-rollback-baseline`。该开关只在运维明确拒绝更换
`cluster/instance`、并接受原地升级失败后无法拉回旧镜像时使用；它只作为参数传给
Agent（`allow_missing_rollback_baseline`），Job 日志会留下可见的 `warning:`。

### 回滚镜像缺失时发新停旧

这一节是手工 runbook：0-1 并存切换（临时 release 名、验收后卸载旧 release）没有自动化
实现，下面的步骤由发布者从控制端经 Gateway 执行，候选镜像内的一次性 Job 按实例单独准备，
所有 mutation 均通过 `doops -session <s> exec --target <t>` 提交独立 Job，
不在本机或旧 Agent 进程里执行 Helm 替换
（已核验实例见[已核验实例的发新停旧](#已核验实例的发新停旧)）；`doops upgrade` 只做同名
release 的原地升级。

回滚镜像已从 registry 消失时，原地 `doops upgrade` 不能在失败后拉回旧 Pod。手工切换按 0-1 进行：先人工匹配到唯一的旧 release，装上唯一的新 release，验收通过后卸载旧 release。这是长期通道，不是临时 target。回滚镜像仍在仓库、并且不更换 workload 身份时，继续用 `doops upgrade`。

候选镜像先解析成 digest 再写入 Deployment。`latest` 只用于解析，不留在运行中的 tag 上。多架构索引用索引 digest，验收时的 `imageID` 必须等于这次解析出的 digest。候选镜像里的 `doops-agent -version` 给出 release 和 40 位 `release_commit`。这条路径不要求操作机 CLI 事先与候选镜像同 commit；切过去之后，下一次原地升级前再安装同 commit 的 CLI。

旧 release 在安装新 release 之前用模糊匹配选出，结果必须正好一个，并记下它的 namespace 和 release 名。候选的 chart 是 `doops-agent`，并且 release 名、workload 名或 `gateway.instance` 包含这次要切换的身份。同集群里对不上这个身份的其他 Agent 不在范围内；匹配到 0 个或多于 1 个就停止，不猜测。新 release 沿用原来的 `gateway.cluster` 和 `gateway.instance`。最终名字只有一套：Helm release `doops-agent`，`workload.name` 为 `doops-agent`，`selectorLabels` 为 `app: doops-agent`，本机路由为 `gw-<instance>`。旧 release 如果已经占用这个名字，并存期间只用临时 release `doops-agent-next`，验收并卸载旧 release 后，再安装正式的 `doops-agent` 并卸载 `doops-agent-next`。临时名字不留在线上。agent home 使用单独目录，不和并存的旧 Pod 共用 `/root`；正式 Pod 的 home 目录用 `/var/lib/doops-agent`。`hostNetwork` 保持关闭，`nodeSelector` 沿用当前 Pod 所在节点。Chart 用候选镜像里的 `/app/deploy/helm/doops-agent`。

安装由候选镜像中的一次性 Job 执行。旧 Agent 只负责提交这个 Job，不在自己的进程里对旧 release 做 Helm mutation。宿主机 kubeconfig 指向 `127.0.0.1` 时，Pod 网络访问不到 API；Job 用镜像里的 `configure_kubeconfig.py` 把 server 改写到 `KUBERNETES_SERVICE_HOST`，凭据仍来自原来的 kubeconfig。先做 server-side `helm upgrade --install --dry-run=server`，再正式安装。正式安装失败时只卸载新 release，旧 Deployment 继续服务。

新通道全部通过后才删旧：新 Pod Ready 且 `imageID` 等于钉住的 digest；Gateway 上原来的 `cluster/instance` 在线，且 `release_commit` 等于候选镜像的 commit；`capabilities --require structuredSSH` 为 ready；`exec hostname` 等于新 Pod 名，而不是旧 Pod。然后 `helm uninstall` 安装前记下的那一个旧 release。Deployment、ReplicaSet 和 Pod 随 release 删除，不把副本缩到 0 后留着。同集群的其他 Agent release 不动。

`scripts/release-managed-agent.sh` 现在只调用
`doops -session <s> upgrade --target <t> --mode k8s`，由现有 CLI 和集群内 Job
负责预检、备份、验收及失败自动回滚。脚本不再持有另一套本机 Helm 事务，
也不会在 Gateway 离线、权限不足、审计不可用、旧 CLI 或旧 Agent 不支持时改走其它入口。
CLI 的输出、失败退出码和进程信号直接保留，不将“请求已提交”改写为“升级成功”。

所有 Agent 升级路径必须满足同一组保证，与由谁发起无关（`doops upgrade`、通过 `doops exec`
在集群内起的独立 Job，或委托这些入口的脚本）：

1. 改动前备份：Helm revision，或尚未由 Helm 管理时导出的 Deployment/ConfigMap。
2. 改动后校验：新 Pod Ready 且 imageID 与固定 digest 一致、Gateway 重连、`release_commit` 正确。
3. 校验失败自动恢复备份。备份可恢复的前提是回滚镜像仍可拉取，该前提由上面的
   改动前检查保证。发新停旧在删除旧 release 之前满足这四条，失败时只卸载新 release。旧 release 卸载后，这条通道只剩新 Pod，不再保留一个副本数为 0 的旧 Deployment。
4. 全程走 Gateway 审计路径，参数和输出不含凭据 payload。

Agent 不能在自己进程里替换自己的 workload，升级始终由独立 controller 执行；
外部脚本也必须经 Gateway 发起 mutation，事后查询 target 或补写记录不满足这条审计约束。

**旧 Agent 首升的实际支持范围：**只有已支持 `upgradeAudit v1` 和新控制器的目标可直接走
`doops upgrade`。旧协议或缺少控制器的 Agent 被新 Gateway 在派发前 blocked，
只能通过仍在线、已授权且具备集群权限的
Gateway `exec` 通道提交独立 Job；旧 Agent 本身只负责提交，不能在其进程或 `exec --bg`
进程里运行替换自身的 Helm 命令。该 Job 必须先完成上面四项保证的实现和验证，
包含备份可恢复性、候选 digest/commit、Gateway 接管及验收失败自动回滚。
仓库当前没有可供该脚本复用的通用首升 Job 提交器，本次入口不实现或自动选择这条路径。
需要首升、收编或配置迁移而现有控制器不支持时，先设计并审查该实例的独立 Job。
Gateway 不可用或没有在线授权管理 target 时停止，不新增离线授权例外。

尚未由 Helm 管理的 Agent Deployment 可以在满足上述保证的前提下收编进 Helm：
Deployment selector 与 Chart 的 `workload.selectorLabels` 一致即可原地接管；
没有 `/root/.kube` 卷的 Agent 设置 `kubeconfig.source=serviceAccount` 和
`serviceAccount.name`，由 Pod 自己的 ServiceAccount 提供集群权限。收编完成后
按已有 Helm release 路径升级。

### Agent RBAC 纳管与回滚

身份与权限配置契约见 [CLUSTER_INSTALL_GUIDE.md](CLUSTER_INSTALL_GUIDE.md#agentgateway-target-与集群身份)。
`rbac.create=true` 是显式集群管理授权，不能只为绕过 Forbidden 而给未授权 target 开启。
第一次把外管 SA/CRB 加入 release 时，必须由另一个已授权管理通道执行 Helm 收敛，
不能依赖正在收编的 SA 做控制器：回滚到旧 revision 会删除新纳管的 SA/CRB，
控制器会同时失去自己的身份或恢复权限。

1. 经 Gateway 审计通道核对 target 对应的集群、release/namespace、当前 revision、SA UID、
   CRB roleRef/subjects 和现有 owner。备份 Helm history、完整 values/manifest、SA/CRB、
   该 SA 的现有 RoleBinding 与 Agent Pod 状态；备份仅用户可读，不包含 token/Secret payload。
2. 用明确版本的 Chart 和完整 values，显式设置 `kubeconfig.source=serviceAccount`、
   `serviceAccount.name` 和 `rbac.create=true`。同名对象归属另一个 release 时停止。
   对已核验的无 owner 对象使用 Helm 的 `--take-ownership` 收编，不全局修改其他对象标签。
   去掉旧 CRB 的 `helm.sh/resource-policy: keep` 前保存原注解，避免残留永久集群权限。
3. 独立控制器先执行 server-side dry-run，再执行真实 Helm mutation。不要在 test Agent
   进程里替换自身。没有 Pod template 差异时，不为增加 RBAC 强行重启 Pod。
4. 回读 manifest 必须同时包含 Deployment、SA、CRB；live CRB 的唯一主体必须等于
   Pod SA。经目标 Agent 验证 `auth whoami`、跨 namespace 读 Pod、列节点和 API server
   dry-run 创建 Namespace/Deployment；验证 `auth can-i` 原有四项，以及 Ready、Gateway
   回连和固定镜像。不得用模型摘要或单独的 Helm ownership 标签当成功证据。
5. 失败由独立管理通道恢复旧 Helm revision，重新应用备份的 SA/CRB 并恢复其原 owner
   和 `keep` 状态，再检查身份、权限和健康。普通应用 `apply` 不会删除新增的 ownership
   注解，恢复时需明确移除备份中没有的 Helm 标签/注解。恢复后的 SA UID 若变化，
   现有 Pod 的投射 token 会失效，需要通过独立通道重新创建 Pod 后验证 Gateway 回连。

迁移后的普通镜像升级仍用 `doops upgrade`。回滚范围必须是已包含托管 RBAC 的 revision；
需要跨越首次纳管点回滚时，按上面的独立控制器流程恢复，不能让当前 SA 删除自己的权限。
预检、升级和显式回滚先用 Kubernetes `auth whoami` 取得控制器真实身份；
实际使用 SA 的控制器会比较前后 manifest 中自己的 SA 及相关绑定（包括挂载 SA kubeconfig 的情况）。
新增纳管、删除、改 roleRef 或 subjects 会在 Helm mutation 前报
`rbac_controller_identity_changed`，要求独立控制器处理，不绕过检查。
卸载 Agent 会撤销 release 管理的集群级绑定；外部维护的历史 RoleBinding 不在此自动删除。

Agent 已离线、Deployment 卡在 ImagePullBackOff 或收编中途失败时，必须经另一个
已授权且能访问同一集群的 Gateway target 提交经审查的独立恢复 Job，保持备份、
验收和自动回滚。没有此通道时停止，不能在本机使用 kubeconfig 代替 Gateway 授权与审计。
`scripts/reinstall-k8s-agent.py` 已收敛为只读规划器：`run`/`restore` 在选择 kubeconfig、
访问 registry 或调用集群工具前，以 `local_reinstall_disabled` 和退出码 2 失败。
本地拉取测试 Pod、备份写入、删除、Helm 安装和恢复函数已移除；脚本不会自动调用
`doops upgrade`，镜像升级也不等于删旧重装或恢复历史 Deployment。
现存备份不删除、不改写。真正恢复须由上述经审查的独立 Job 完成完整备份、验收和自动回滚。

保留的只读检查从 `backend/` 执行：

```bash
python3 scripts/reinstall-k8s-agent.py plan \
  --cluster <gateway-cluster> --instance <gateway-instance> \
  --namespace <namespace> --workload <workload> --release <helm-release> \
  --image <repository>@sha256:<verified-ci-digest> \
  --kubeconfig <read-only-inspection-kubeconfig>
```

`plan` 只执行 registry 查询、`kubectl get` 和 `helm list`，不拉镜像、不启动 Pod、
不写备份；`--backup-dir` 已退役。结果的 `inventory` 列出现有对象，替代旧 `delete` 字段，
并明确 `readOnly=true`、`mutationSupported=false`。它读取现存 Deployment 或历史
重装/收编 ConfigMap；新控制器的 operation 级 Secret 备份由受审计控制器处理，此工具不读取。
没有现存对象或历史备份时不能规划，也不把空集群当成初装成功。
本机只读检查不构成 Gateway 审计、发布授权或实际恢复验收证据。

已有 Helm release 的路径以 `maxSurge=1`、`maxUnavailable=0` 先拉起新 Pod，Gateway
`Register` 用新会话替换旧 WebSocket 后再删旧副本。Job 成功后 CLI 执行
`capabilities --require structuredSSH`，失败时请求回滚 Job；Gateway 完全离线时
无法自动恢复。自升级 Job 固定到 Agent 节点并保留 runtime class。新 Pod 与 Job 在同一节点时，
imageID 必须等于 Job 自己的运行时 digest。镜像按 `repository@sha256:<digest>` 固定时，调度器
把新 Pod 放到其他节点也可以，此时 imageID 必须等于固定 digest 或 Job 的运行时 digest，其它
digest 视为不同内容并回滚。按 tag 升级时只能在同一节点证明身份，新 Pod 必须与 Job 同节点。
自动回滚恢复的 Pod 按同一规则核对。验证证据保存在 `doops-upgrade-verified-<release>`
ConfigMap，记录实际 Pod 的节点和 digest；手动回滚只使用有验收证据的历史 revision。
旧 Agent 生成的是旧版 Job 脚本，新控制器要等 Agent 升级后才生效。

Helm 只删除它上一个 revision 渲染过的内容。上一 revision 渲染过、新 Chart 不再渲染的卷
会被删除，但 Helm 之外（例如首次 `kubectl apply` 建 Agent 时）加上的同名挂载不归 Helm 管，
会留下来指向已删除的卷，API server 因此拒绝整个 Deployment
（`volumeMounts[N].name: Not found`）。改动前检查用 `helm template` 渲染新 Chart，
与 `helm get manifest --revision <previous>` 对比，列出 live Deployment 里挂载了这些
被删卷的挂载（`stale_volume_mounts=...`）；Helm 从未渲染过的卷不在其列，挂载保持不动。
预检只列出不改。apply Job 在第一次改动前先把这些挂载备份到
`doops-upgrade-mount-backup-<release>` ConfigMap，再用带 `test` 守卫的 JSON patch 删除
（Deployment 在快照之后变化时整个 patch 被拒绝），然后执行 Helm 升级。任一步失败时
自动回滚：Helm rollback 恢复卷后，再把缺少的挂载加回去，最后照常校验回滚后的
Deployment；挂载恢复失败按回滚失败报告，需要人工处理。新 Chart 渲染不出唯一的 Agent
Deployment 时以 `stale_volume_mounts_unresolved` 失败，不做任何改动。

```bash
bash scripts/release-managed-agent.sh \
  --target <configured-target> \
  --session agent-release-<commit> \
  --release <helm-release> \
  --namespace <namespace> \
  --image <repository>@sha256:<verified-ci-digest>
```

上述命令从 `backend/` 执行，五个参数均必填。target 使用已配置的 Gateway
`cluster/instance` 授权路由，namespace/release 由升级控制器校验。
镜像必须是发布者已核验 CI 来源的 `repository@sha256:<64 hex>`。
脚本拒绝 `--version`，不再在本机运行 Docker 拉取或解析 tag。
`--dry-run` 透传给 `doops upgrade`，仅是经 Gateway 的参数预览，不是原外部
脚本的 server-side Helm dry-run，不证明 live release、挂载、权限或回滚基线有效。
正式执行仍由 CLI 先提交预检 Job，通过后再提交实际升级 Job。

**退役参数与迁移：**`--kubeconfig`、`--values`、`--chart-dir`、`--cluster`、
`--instance`、`--timeout`、`--handoff-timeout`、`--proxy-allowed-hosts`、
`--host-alias`、`--version` 均在调用任何外部工具前报错；不会忽略它们继续升级。
环境中的 `KUBECONFIG` 不读取也不转发。旧的显式 Helm 配置变更和首次安装路径
不再由该脚本支持。配置变更须进入仓库 Chart/values，由满足四项保证的
Gateway 独立 Job 收敛；不能改用本机 Helm 或在旧 Agent 进程里直接替换自身。
首次安装不由这两个脚本执行：只有尚无同身份 Agent、release 或恢复对象的新身份，
才可按[集群接入指南](CLUSTER_INSTALL_GUIDE.md#安装)使用手工 GitOps 初装。
集群已有管理流程实施经审查的 Chart/values，完成 Gateway 注册和验收后，
后续升级统一走受审计的 `doops upgrade`。已有对象损坏、离线或只有备份均属于恢复，
不能通过更换入口、重跑初装或本机 Helm 来绕过审计。

### 已核验实例的发新停旧

旧镜像已从 registry 删除但旧实例仍健康时，先保留旧实例，独立部署固定镜像的
候选，完成 Gateway、Pod 身份和真实模型调用验收后才卸载旧 release。
2026-11-08 `unicom-k8s` 的单次 Job、配置、输入校验和恢复路径见
[该实例操作记录](../deploy/instances/unicom-k8s/README.md)。
Job 明确绑定旧 release/revision，不能直接复用于其他实例或重放未知结果。

### 独立 DockerHost 发布

Compose 管理的容器不能交给下述独立 Docker 控制器。2026-11-08 的
`unicom-1` 升级使用已核验该实例身份的独立 systemd 事务，保留 Compose、
固定镜像和相同的既有 kubeconfig；配置备份、缓存旧镜像、失败自动恢复及
Gateway/真实模型验收见
[该实例操作记录](../deploy/instances/unicom-1/README.md)。
这是绑定该次容器基线的单次控制器，不能套用于其他 Compose 项目。

已有独立 Docker Agent 使用 `backend/scripts/release-host-agent.sh` 和同目录
`release-host-agent.py`，由外部宿主控制器执行；不由当前 Agent 替换自己，
不自动从 `doops upgrade` 降级，也不要求先迁 K8S。旧 Agent 的 `doops_agent_upgrade`
或 `managedHelm` 缺失不能成为恢复前置条件，否则会形成“先升级才能升级”的依赖环。

以下从仓库 `backend/` 执行。全部参数必须显式提供；`--port` 没有默认 22，
`--release` 是 DoOps 发布版本而非 Helm release。先核对 CI 成功证据、完整 40 位
source commit 和不可变平台镜像 digest；脚本不替发布者证明 CI 来源：

候选镜像须先由外部控制器在目标 Docker 宿主执行
`docker pull <repository>@sha256:<verified-ci-digest>` 准备；这是独立于只读预检的制品准备
mutation。`--dry-run` 只检查已有本地候选制品，缺失时明确报告预检制品未准备，不自动 pull。

```bash
bash scripts/release-host-agent.sh \
  --target <configured-target> \
  --session host-agent-release-<release> \
  --host <host> \
  --user <ssh-user> \
  --port <verified-ssh-port> \
  --container <existing-agent-container> \
  --cluster <gateway-cluster> \
  --instance <gateway-instance> \
  --image <repository>@sha256:<verified-ci-digest> \
  --source-commit <verified-40-hex-ci-commit> \
  --release <vX.Y.Z> \
  --dry-run
```

只读预检通过后，使用相同参数去掉 `--dry-run`。可选
`--ssh-control-path <private-authenticated-socket>` 仅复用本机已认证的私有 SSH socket，
不接收 password/token；严格核验已知主机密钥，禁止关闭校验。密码、私钥或 token
不得写入参数、文件、示例或日志；不输出完整 `docker inspect` 或敏感环境值。
宿主密码只用于外部 bootstrap 的 stdin，以建立外部管理 SSH 认证，
不传给 Agent。成功访问目的地主机需要 Agent 内已有授权 SSH 认证材料，发布不新增该能力。

1. `--dry-run` 必须实际只读检查 SSH、宿主身份、Docker 权限、原容器运行配置、
   精确 Gateway route 与本地候选制品元数据；不拉取镜像、不创建/停止/重命名/重启容器，
   不运行候选二进制。不是仅打印参数；制品或关键证据缺失、配置无法保留时明确失败。
2. 正式发布持有宿主侧互斥锁，保留原 command/args、环境、挂载、网络、特权、
   restart policy 等配置；不得把 `network=host` 改成 bridge、增减权限或隐式迁 Compose。
   停止旧容器前，以静态 `--version` 模式校验候选二进制的 release/commit；仅输出版本，
   不启动 Agent、不监听或注册新 target，失败时不停止旧容器。
   停止后保留旧容器供回滚，同一 `cluster/instance` 不同时运行新旧 Agent。
3. 验证实际替代容器 ID、运行状态、镜像 ID/digest、Agent 与 do-agent 版本；Gateway
   必须是同一 `cluster/instance`，`release` 和完整 `release_commit` 分别匹配本次
   `--release` 与 `--source-commit`。真实 RPC 流须覆盖 foreground `exec` 和 `exec --bg`，
   核对终态及流式 `params.request_id` 与每次调用的 Root RequestID 一致。
   structuredSSH 验证协议握手、`host + options[] + command[]` argv 派发及预期失败终态，
   同样核对 Root RequestID；不以能力声明或普通 exec 代替。该验收不宣称目的地主机
   成功认证；外部管理 SSH 的成功访问另记入口、身份和结果，不作为 Agent SSH 认证证据。
   外层脚本自动检查前台双流输出、后台真实输出与零退出终态、结构化 SSH argv 的
   预期连接失败和随后命令可执行。CLI 按每次 Root RequestID 接收通知；能力声明和
   version 返回不能代替这些探针。探针失败立即回滚，不反复重放未知执行。
4. 任一替换或验收失败，由外部控制器停止新容器并恢复旧容器，复核原镜像、配置和
   Gateway 接管；不调用旧 Agent 自升级，不删除旧容器。回滚失败必须明确报告并保留现场。
   外层回滚保护必须在首次宿主替换调用前安装，覆盖 SSH 返回状态丢失；部分回滚
   失败后可从持久阶段重试，不能因已删除新容器而永久卡死，也不能放宽容器所有权校验。
   每次调用复用其唯一 SSH staging ID 绑定 receipt；重复 session 被拒绝后，自动回滚
   不得操作上一次发布的 receipt。该 ID 由脚本内部生成，不新增用户配置或版本门禁。

宿主入口和只读授权边界见 [宿主访问说明](PHYSICAL_HOST_ACCESS.md)。

### 环境依赖注射规范 (Secret)
严禁在 `agent.yaml` 中明文出现 `LLM_API_KEY=sk-xxxx` 之类的配置。此类密钥应由 Secret 提取并通过 `envFrom` 下放。

### 容器运行时挂载规范

容器化 `doops-agent` 需要在 agent 内部继续调用宿主机容器运行时，例如
`nerdctl pull/run`、BuildKit、或一次性 helper 容器。只挂
`/run/containerd/containerd.sock` 不够：`pull` 可能成功，但 `run` 会因为
overlay snapshot、FIFO、`/etc/hosts` 或 nerdctl name-store 位于宿主机路径而失败。

标准 Helm Deployment 的 Agent 容器必须同时挂载：

```text
/run/containerd      -> /run/containerd
/var/lib/containerd  -> /var/lib/containerd
/var/lib/nerdctl     -> /var/lib/nerdctl
```

遇到 containerd overlay/FIFO/name-store 错误时，保留现有 Agent 并读取挂载、节点和
运行时错误证据。挂载修正必须进入仓库 Chart/values，再由上述 Agent 升级路径收敛。
K8S Agent 完全不可用时，通过另一个已授权且可访问同集群的 Gateway target
提交独立恢复 Job，恢复后仍须完成 Gateway、版本和功能验收；没有该通道时停止，
不能通过本机 kubeconfig 或 SSH 绕过升级审计。

### 其它非 Helm 节点

已有独立 Docker Agent 按上述 DockerHost 入口保持运行方式发布。非 Helm 的 K8S
`agent.yaml`、二进制 bind-mount 或裸机 tar 不在该入口范围；需先明确其受支持的发布或
迁移方案。

## 五、GitHub Actions 标签发布（开放仓库）

公开 GitHub 仓库的 `vX.Y.Z` tag 由 [`.github/workflows/release.yml`](../../.github/workflows/release.yml)
自动构建并发布；生产镜像由操作方按第三节、第四节显式构建推送，两条发布路径互不替代。

1. **触发与准入**：只接受 `push` 的 `vX.Y.Z` tag（可带 `-rc.N` 预发布后缀）和手工
   `workflow_dispatch`（默认 dry-run，不创建 Release）。tag 必须已存在且指向 `main` 上的提交；
   已存在的 Release 拒绝重建，已发布资产不可变。
2. **CI 门禁**：发布前直接复用 `.github/workflows/ci.yml`（`workflow_call`），tag 不会绕过
   检查；prepare 解析出的完整 tag commit 通过 `source_commit` 传入每个 CI checkout，并与构建阶段使用同一 SHA，手工发布历史 tag 也不测试触发分支的最新代码。独立 push/PR 检查使用各自事件的 SHA。复用的门禁覆盖
   main 的检查（Go/Web 测试、`backend/test` 契约测试）；secret scan 只在 branch push/PR 上运行——tag 和手工发布没有增量 commit 范围，
   全历史扫描会把历史上已存在的测试 fixture 当成泄漏，tag 指向的提交在合入 main 时已扫描。
3. **构建**：`backend/scripts/build-cli.sh --all` 在固定 `go1.25.14` builder 身份下产出
   `doops-{darwin,linux}-{amd64,arm64}`、`doops-gateway-linux-amd64` 和 `checksums.txt`；
   发布前校验 sha256、`release=<tag> commit=<commit>` 身份和 `doops.sh/cli-contract/v3`。
4. **发布**：`gh release create --verify-tag --generate-notes`（预发布加 `--prerelease`），
   只有 publish job 拥有 `contents: write`；二进制同时生成 GitHub build provenance attestation。
5. **发布后复验**：从 GitHub Release URL 重新下载资产，运行
   `backend/scripts/verify-published-cli-release.sh` 复核 checksums 与身份，`gh attestation verify`
   校验来源，并用 `backend/scripts/install.sh`（`DOOPS_RELEASE_ASSET_BASE` 指向该 Release）
   完成一次端到端安装。
6. **安装来源**：`install.sh` 默认从 GitHub Releases
   （`https://github.com/symansun/doops/releases/download`）下载；`DOOPS_RELEASE_ASSET_BASE`
   只用于指向镜像且只接受 https（回环地址用于测试），不会放宽 sha256、身份或 contract 校验。
7. **契约测试**：`backend/test/test_github_release_contract.py` 锁定触发条件、CI 门禁、
   不可变发布、资产集合、builder 身份和复验步骤。

手工 dry-run（只构建与复验，不创建 Release）：

```bash
gh workflow run release.yml -f tag=<release-tag> -f publish=false
```
