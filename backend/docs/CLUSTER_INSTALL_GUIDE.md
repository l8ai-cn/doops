# Doops Agent 集群接入指南

本文档介绍如何将 `doops-agent` 部署到一个新的服务器或集群环境。Agent 被设计为**无状态执行通道**，只需要有网络连通性和合适的权限，即可连接并执行不同底层架构的资源。

---

## 先选部署对象

本文是 Agent 安装与升级的入口。不要把 CLI 安装、Agent 注册、操作员登录和
Agent 镜像升级当作同一件事。下列脚本都在 `backend/scripts/` 下：在仓库根目录执行时
写 `bash backend/scripts/...`，在 `backend/` 目录内执行时写 `bash scripts/...`；
根目录的 `scripts/install.sh` 是转发入口，两种写法等价。

| 当前状态 | 使用入口 | 前提 |
| :--- | :--- | :--- |
| 只安装客户端和 Skill | `bash scripts/install.sh --version <tag> --project <项目绝对路径>` | 不部署 Agent；不修改线上 Helm |
| 空 Ubuntu/Debian 服务器首次 Docker 接入 | `bash scripts/bootstrap-host-agent.sh ...` | root、唯一 cluster/instance、指定发布版本；模型由 Gateway 下发 |
| 已有标准 Helm Agent，支持新控制器和 `upgradeAudit v1` | `doops -session <s> upgrade --target <t> --image <已核验镜像>` | 已登录且获授权 Gateway；Agent 有共享 kubeconfig 或专用 ServiceAccount |
| 旧协议 Agent 首升、命令通道损坏或已有对象恢复 | 经 Gateway `doops -session <s> exec --target <管理target>` 提交经审查的独立 Job | 需在线授权管理通道；没有通用自动首升/恢复入口，不能用本机脚本替换自身 |
| 检查 K8S Agent 或历史恢复对象 | `python3 scripts/reinstall-k8s-agent.py plan --cluster <c> --instance <i>` | 仅只读检查；`run`/`restore` 已在代码中禁用，结果不构成恢复授权 |
| 尚无同身份 Agent、release 或恢复对象的首次安装 | 手工 GitOps 初装，见下文“安装” | 审查仓库 Chart/values，使用集群已有管理流程；当前没有自动 installer |
| 已有独立 Docker Agent | `release-host-agent.sh` | 见下文 DockerHost 流程 |

标准 Helm Agent 的日常镜像升级是一条命令。镜像由操作方以显式 registry/repository/tag 构建并推送，
本仓库不含镜像发布流水线；`latest` 只表示最近一次推送的构建，不能证明来源：

```bash
docker pull registry.example.com/doops-agent

doops -session agent-upgrade upgrade \
  --target <configured-target> \
  --image registry.example.com/doops-agent@sha256:<verified-digest>
```

`--image` 也接受其它发布 tag 或 `repository@sha256:<verified-digest>`。tag 不是不可变版本证据。`--dry-run` 只预览参数，
不能证明控制器、挂载或权限可用。没有命令通道的旧 Agent 不能用这条命令自恢复。
当前 CLI 会将升级后的 Agent commit 与自身 commit 比较，因此先安装候选镜像
同版 CLI；升级前旧 Agent 不需要与 CLI 同版。
新 Gateway 在派发前检查 `upgradeAudit v1`，旧协议目标会被 blocked 并报
`upgrade_audit_protocol_required`，不会先创建 Job 再检查响应格式。
版本不同不等于协议兼容；旧协议的文本成功不能替代升级审计能力。

操作员在一台管理机上登录一次，同一 Gateway 的 targets 共用登录态。尚无本地路由时按 Gateway 首次登录，
CLI 会自动发现并写回可见 target 路由：

```bash
doops -session setup login --gateway <gateway-url>
doops -session preflight list
doops -session preflight targets --target <自动发现的 target>
doops -session preflight exec --target <自动发现的 target> --cmd hostname
```

登录支持账号密码交互，不必另外找管理员签发 token。已有登录不要重建隔离配置：
`DOOPS_CONFIG` 会改变配置及相邻 `auth.json` 的位置，单独 `doops add` 只添加路由，
不会复制登录态。Agent 免 token 注册不等于操作员免登录访问。

### 操作员客户端安装（git、CLI、Skill）

新操作员机器按下面顺序执行。`install.sh` 从下载的 CLI 内嵌的发布提交读取 Skill 和文档，
所以必须先有 git 和一份包含该提交的仓库克隆：

1. 安装 git：macOS 用 `brew install git`，Debian/Ubuntu 用 `apt install git`。
2. 先确定要安装的发布 tag。唯一发布与安装源是 GitHub Releases
   （`https://github.com/symansun/doops/releases/download`）；默认分支是 `main`，
   克隆时**固定到该 tag**：

   ```bash
   git clone --branch <tag> --depth 1 https://github.com/symansun/doops.git
   cd doops
   ```

   已经克隆了 main 时，先 `git fetch --tags && git checkout <tag>` 再继续。
3. 安装 CLI 和 Skill。默认资产来源即 GitHub Releases；只给 `--target-gateway`，
   由首次登录发现 target：

   ```bash
   bash scripts/install.sh \
     --version <tag> \
     --project <项目绝对路径> \
     --target-gateway <gateway-url>
   ```

   `DOOPS_RELEASE_ASSET_BASE` 只用于把资产来源指向镜像；sha256、`release=... commit=...`
   身份和 CLI contract 校验不变。
4. 确认 `~/.local/bin` 在 `PATH` 中，再首次登录并验证：

   ```bash
   doops login --gateway <gateway-url>
   printf '%s' "$TOKEN" | doops login --gateway <gateway-url> --token-stdin
   doops list
   doops targets --target <自动发现的 target>
   ```

   首次登录会调用 `GET /v1/targets`，把该账号可见的在线 target 写回
   `~/.agent/skills/doops/config.json`；已有路由的名称、别名和描述不会被改写。
   因此操作员不需要事先知道 Agent 的 `cluster/instance`。

约束：

- 安装前确认 `<tag>` 已在 GitHub Release 发布 CLI 二进制和 `checksums.txt`，
  不要凭 tag 列表假定可用。
- 本地缺少发布提交时脚本报 `Release commit ... is unavailable in local git objects` 并退出，
  不会自动 fetch，也不会混装其它版本。先 `git fetch --tags` 或 `git pull` 再重试。
- 安装结果：CLI 在 `~/.local/bin/doops`；同一发布提交的 Skill/docs 写入
  `<项目>/.agent/skills/doops/` 和 `~/.agent/skills/doops/`，同时更新用户目录和
  当前项目中已有的 `.codex/skills/doops/`、`.agents/skills/doops/`。
  不创建未启用客户端的目录，也不扫描其它项目。
  `~/.local/share/doops/install.txt` 记录 `skill_commit` 和全部 `skill_path`；
  每个路径的 `SKILL.md` 与 `docs/*.md` 均来自该提交，不保留旧版 docs。
- 更新只替换 Skill/docs，保留各目录的 config、auth 和历史记录。
  开发链接的 `SKILL.md`/`docs` 会被正式发布内容替换，链接指向的源码保持不变。
  整个 `doops` 目录是符号链接或文件类型异常时，在替换 CLI 前明确失败；
  先确认该入口的归属，再修正为独立安装目录，不沿未知链接写入。
  检查 `doops` 目录归属后，按物理父路径规范化并去重；HOME 尾斜杠或父目录别名不会
  导致同一份 Skill 重复替换，回执中的 `skill_path` 使用去重后的物理路径。
  配置仍在 `~/.agent/skills/doops/config.json`。不要先改已安装的副本。
- CLI、全部 Skill/docs 和回执在替换前暂存新内容并保存原版本。任一替换、回执写入或
  回执替换失败，安装器按逆序恢复原版本并非零退出；首次安装则移除本次新增的文件。
  这是失败恢复，不会继续使用旧版执行安装任务。回执成功落盘后才完成这一批制品更新；
  显式 `--target-*` 的配置变更仍在其后独立执行，制品恢复不修改 config/auth。
  恢复失败时保留 `.doops-install.*` 备份目录，stderr 给出失败目标及原版本备份位置；
  先按该位置核对并恢复，未恢复前不要清理备份。全部恢复成功或安装成功后才清理暂存。
- 管理员创建账号后交付给操作员的密码和 API key 只显示一次，不写入文档、issue 或提交。
  `doops admin user create` 只创建非管理员；管理员账号需调用 `POST /v1/admin/users` 并带
  `"admin": true`，见 [DOOPS_API_GUIDE.md](DOOPS_API_GUIDE.md)。

### 开发者本地基线（可选，不替代上面的安装）

在仓库里改 CLI 源码时，不必每次都从 Release Assets 重装。在仓库根目录执行：

```bash
bash backend/scripts/dev-link.sh --project <项目绝对路径>
```

它把当前仓库构建成 `bin/doops`，并把 `~/.local/bin/doops` 和项目里的
`.agent/skills/doops` 做成指向仓库的软链接，所以改完源码重新执行一次就生效。
已有的正式安装产物会备份成 `~/.local/bin/doops.release`，不删除。

这是开发用的单一基线，不是受支持的安装入口：操作员和用户仍按上面一节用
`install.sh` 安装。开发链接版的 CLI 版本号是 `dev`、`release_commit` 是当前仓库
HEAD，而 Agent 升级后会校验 Agent 的 `release_commit` 是否等于 CLI 自己的 commit，
所以升级 Agent 必须用与候选 Agent 制品同 commit 的正式 CLI，不能用开发链接版。
`bash backend/scripts/dev-link.sh --unlink` 可以断开软链接；已有正式文件保留，
不会自动恢复历史备份。断链后用 `install.sh` 安装所选正式版本。
开发 Skill 的旧文件和链接备份存放在项目 `.local/doops-dev-link/`，不留在 Skill
加载目录。建链或断链时会迁出历史 `docs.release`、`SKILL.md.release` 和
对应的 `*.devlink-backup`，避免 Agent 读到已经移除的发布门禁。

### 脚本入口与协议边界

- `install.sh` 只更新 CLI/Skill，成功不代表 Agent 已升级。
- target 显示 idle 但 `RELEASE_COMMIT` 为 `<unknown>`、只读命令返回
  `execution_outcome_unknown` 时，不是完整健康状态，不能据此执行 `doops upgrade`。
- `release-managed-agent.sh` 只委托 Gateway `doops upgrade --mode k8s`，依赖同一
  target 授权、`upgradeAudit v1`、预检、独立 Job、备份、验收和自动回滚。
  `KUBECONFIG` 不读取也不转发；`--kubeconfig`、`--values`、`--version`、
  `--cluster`、`--instance` 和旧 Helm 配置变更参数均直接拒绝。
- 脚本只接受发布者已经核验的不可变 `--image` digest，不在本机用 Docker 解析 tag。
  CLI 须与候选 Agent 制品同 commit，完整镜像与实际功能验收见
  [BUILD_DEPLOY_SOP.md](BUILD_DEPLOY_SOP.md)。
- `reinstall-k8s-agent.py` 只保留 `plan`：通过本机只读访问检查现有 Deployment、
  历史 ConfigMap 备份来源和资源清单，不创建备份、不启动拉取测试 Pod。
  `run`/`restore` 已 fail closed，不能用作首装、升级或恢复的执行入口。

具备协议能力的在线 Agent，可使用以下升级委托入口：

```bash
bash scripts/release-managed-agent.sh \
  --target <target> --session <session> \
  --release <helm-release> --namespace <namespace> \
  --image <repository>@sha256:<verified-ci-digest> \
  --dry-run
```

这里的 `--dry-run` 仅预览参数，不是 server-side Helm dry-run，也不验证 live release、
挂载、权限或可恢复基线。正式升级使用相同参数去掉 `--dry-run`，由 CLI 和独立 Job
完成真实预检、备份、验收及失败自动回滚。旧协议首升必须另行审查完整独立 Job，
通过仍可用的授权 Gateway `exec` 通道提交；本仓库尚无通用提交配方。
没有在线授权管理通道时停止已有 Agent 的变更，不能切换本机 kubeconfig 或重跑初装。

## Kubernetes / K3s

### Agent、Gateway target 与集群身份

Gateway 的 `cluster/instance` 是路由及授权身份，不是 Kubernetes context 或 namespace。
一个 Agent 在哪个 namespace 运行，不决定它能管理哪些 namespace。Kubernetes API
按 Agent 实际使用的凭据和 RBAC 判断权限；给 Gateway 用户增加 target grant 不会创建 RBAC。

标准集群管理 Agent 使用专用 ServiceAccount。完整 values 显式包含：

```yaml
kubeconfig:
  source: serviceAccount
serviceAccount:
  name: doops-agent
rbac:
  create: true
```

`rbac.create=true` 由同一 Helm release 管理 ServiceAccount 和名为
`<namespace>-<serviceAccount.name>-cluster-admin` 的 ClusterRoleBinding；绑定只指向该
namespace 的这一个 SA。SA 和绑定都会出现在 `helm get manifest`，不使用 `keep`，
卸载 release 时一并删除。已有同名资源必须先核对主体和 owner，再显式收编；
只贴 Helm 标签不算完成收编。迁移和回滚见 [BUILD_DEPLOY_SOP.md](BUILD_DEPLOY_SOP.md#agent-rbac-纳管与回滚)。

这项设置赋予整个集群的管理权限，包括所有 namespace 的 Secret；获准操作该 target 的用户
通过 Agent 获得同一能力。Gateway 负责用户、target 授权及操作审计，Kubernetes 审计仍显示
`system:serviceaccount:<namespace>:<name>`，不用共享 `kubernetes-admin` 证书。
入口通过 `tokenFile` 引用 Pod 的短期投射 token，让 kubelet 轮换凭据；不复制 token 到 Git
或长期 kubeconfig。`serviceAccount` 模式拒绝空名称和 `default`。

`rbac.create` 默认 `false`，升级不会自动提升已有 Agent 权限。选择外部管理的 SA 时，
用 `kubeconfig.source=serviceAccount` 和其专用名称，RBAC 由外部声明负责；选择现有挂载
kubeconfig 时继续显式使用 `kubeconfig.source=mounted`，权限来自该 kubeconfig。
这两种既有输入不会在认证或授权失败后相互切换。新建集群管理 Agent 优先使用上述
Helm 管理的 SA 模式，保留可区分的身份和完整生命周期。

`hostAccess.enabled` 管的是 Pod 所在节点的操作系统入口，不能代替 Kubernetes RBAC；
Kubernetes `cluster-admin` 也不等于已取得其他节点或 hypervisor 的操作系统权限。

### 安装

当前没有 K8S Agent 自动 installer。首次安装支持**手工 GitOps 初装**：
使用本仓库 `deploy/helm/doops-agent` 的已审查版本和完整 values，
由集群已经建立的授权管理流程实施，不调用已退役的脚本参数。

1. 先核实目标集群和新 `gateway.cluster/instance`。同身份不能已有在线 Agent，
   也不能存在待升级、收编、恢复的 release、Deployment 或备份。离线不等于从未安装。
2. 将 Chart 版本、namespace/release/workload、固定镜像 digest、专用 ServiceAccount、
   受控凭据引用、完整 values 和初装清理方案放入目标集群的 GitOps 仓库，审查后实施。
   明确 `gateway.url/cluster/instance` 和集群权限；不将 token、kubeconfig payload
   或模型配置写入 values/Git。发布者保留 CI 来源、完整 commit 和 digest 证据。
3. 通过集群已有 GitOps 控制器或经审查的人工 GitOps 流程创建新资源。此步骤建立首个
   Agent 身份，不覆盖现有实例；失败按本次清理方案仅处理本次创建的资源。
4. 检查 Deployment 收敛、实际新 Pod Ready 和 imageID，登录 Gateway 并核对唯一
   `cluster/instance`、`release_commit`、`upgradeAudit v1`，再完成下文安装验收。
   未完成 Gateway 注册与验收前，不宣称 target 已可用。

初装完成后，Agent 生命周期切换统一通过授权、受审计的 Gateway：
在线且支持新协议的目标使用 `doops upgrade`；旧协议首升、收编、重装和恢复
按 [BUILD_DEPLOY_SOP.md](BUILD_DEPLOY_SOP.md) 审查独立 Job 后经 Gateway 提交。
不能重复套用“手工初装”来更新已有 Agent，或用其规避离线、权限、审计及协议错误。
`doops upgrade` 不修改 `proxy.allowedHosts` 或 `hostAliases`；
这类配置变更进入仓库 Chart/values，再由符合备份、验收和自动回滚要求的受审计独立 Job 收敛，
当前脚本不提供配置变更入口。普通业务仍通过明确 target 的 `ask --apply` 发布。

旧副本卡在 Terminating 时，先核对替代 Pod 已接管 Gateway，再检查事件、finalizer 和
节点运行时；不能仅凭一分钟超时就强制删除，也不要对同一身份再跑一次安装器。
进入 Pod 所在节点由 values `hostAccess.enabled` 控制，默认 `true`：Pod 与该节点
共享 PID namespace，并把节点根目录只读挂到 `/host`。显式 `false` 才把 Pod 留在
自己的 PID namespace。`hostNetwork` 保持关闭。进入该节点只通过 Gateway 的
`doops exec`，由 target grant 控制并写入审计，命令见
[PHYSICAL_HOST_ACCESS.md](PHYSICAL_HOST_ACCESS.md)。
这只到达 Pod 所在节点的操作系统，不跨越虚拟机的 hypervisor，也不能代替一份写明范围的 Helm values。

## 空服务器 Docker 首装

支持使用 systemd 的 Ubuntu/Debian，amd64/arm64（所选发布镜像必须包含对应架构）。
这是首次安装入口，不用于覆盖已有容器。以下命令从仓库根执行，Unicom 示例：

```bash
sudo bash backend/scripts/bootstrap-host-agent.sh \
  --version v1.3.48 \
  --gateway https://doops.l8ai.cn \
  --cluster doops-unicom --instance unicom-1
```

`v1.3.48` 是明确示例版本，不表示动态最新版。服务器上不放模型配置：Agent 连上
Gateway 后，Gateway 把绑定到该 target 的 `modelProvider` 凭据下发给它，配置方式见
[CREDENTIAL_RESOURCES.md](CREDENTIAL_RESOURCES.md#model-provider)。没有绑定时 Agent
照常上线，只有 `ask` 会报“模型未配置”。

脚本使用 Docker 官方 apt 仓库安装 Engine/Compose，已安装 Docker 时验证并复用；
不卸载其它运行时、不迁移已有环境。拉取明确版本后锁定 registry digest，检查镜像
内版本信息，将 Compose 部署文件写入 `/opt/doops-agent`，使用 Compose 启动。
容器使用标准镜像要求的 privileged 模式；不发布宿主端口，不挂载宿主 Docker socket。
Agent 管理的是容器执行环境；此安装不会自动获得宿主文件系统或 Kubernetes 权限。

工作区和 Agent 状态用 Docker volumes 持久化。健康检查要求 Gateway 隧道已连接；
240 秒内失败则停止容器、返回失败并保留配置供排查，不输出可能含密钥的原始日志。
已有 `/opt/doops-agent` 或同名容器时拒绝覆盖。修正首装配置后可显式重试：

```bash
sudo docker compose -f /opt/doops-agent/compose.yaml up -d --wait --wait-timeout 240
```

选择未占用的 cluster/instance，安装前在管理端核对已有节点。注册不需要 operator
token；安装后的完整命令验收在已登录的管理机执行：

```bash
doops add --name gw-unicom --gateway https://doops.l8ai.cn \
  --cluster doops-unicom --instance unicom-1
doops -session unicom-install targets --target gw-unicom
doops -session unicom-install exec --target gw-unicom --cmd hostname
```

尚未登录的管理机先执行 `doops -session setup login --gateway <gateway-url>`。
本地健康仅证明隧道连接；管理端 RPC 和 AI 请求还须真实验收，不能仅凭启动完成宣称
全部功能可用。已有 Docker Agent 后续发布仍走下面的外部升级入口。

## 已有独立 Docker 节点

已有独立 Docker Agent 保持现网运行方式，通过正式外部
`backend/scripts/release-host-agent.sh` 发布；不要求迁 K8S，不使用 `doops upgrade`
或旧 Agent `exec` 替换自己。脚本不是无 Agent 主机的安装配方，不自动迁 Compose/Helm。
显式 target/session、SSH 地址/用户/端口、原容器、cluster/instance 和 CI 制品参数、
真实只读预检、宿主锁、配置保留与外部回滚见
[主 SOP](BUILD_DEPLOY_SOP.md#独立-dockerhost-发布)。新接入方案须另行确认授权，不能为检查硬件擅自安装。

## 安装完成后的验收标准

CLI 不提供直连 Agent 安装入口。不要以“容器起来了”或“`ps` 里有进程”
作为安装成功标准；必须从 Gateway 和真实远程调用两侧验收：

- `doops -session install-verify targets --target <gateway-target>` 返回指定 `cluster/instance`，不是其它同
  Gateway 节点。
- `doops -session install-verify exec --target <gateway-target> --cmd 'do-agent --version'`
  返回当前发布版本。
- 容器运行时可见（`nerdctl` / `docker` / `podman`）。
- K8S 节点的 `kubectl` 和 Kubernetes 身份来源明确，不是查找开发机 kubeconfig；独立 Docker 不要求 K8S。
- 声明 BuildKit 能力的节点须验证 `buildctl` 与 `/run/buildkit/buildkitd.sock` 可见。
- 独立 Docker 发布还须完成主 SOP 的实际容器/镜像/版本、Gateway `release_commit`、structuredSSH 协议/argv/预期失败终态、foreground/bg RPC 流与 Root RequestID 验收；不宣称 SSH 目的地主机成功认证。

Gateway heartbeat、Agent 版本或容器运行时任一失败，安装都应直接视为失败；如果
节点所需的 `kubectl` 或 `buildkit` 为 `MISSING`，不能被当成具备相应能力的发布节点；
独立 Docker 未部署 K8S 本身不是升级失败。

---

## 连通性验证

新节点部署并启动容器后，需要确保该 agent 已主动注册到 gateway。

1. **添加 gateway 配置**:
   在 `~/.agent/skills/doops/config.json` 里追加或用 `doops add` 写入：
   ```json
   {
     "name": "new-cluster",
     "gateway": "https://gateway.example.com",
     "cluster": "doops-new",
     "instance": "master-1",
     "use": "新测试环境 via gateway"
   }
   ```

2. **登录 Gateway**:
   ```bash
   doops -session install-verify login --gateway <gateway-url>
   ```

3. **确认精确身份已注册**:
   ```bash
   doops -session install-verify targets --target new-cluster
   ```

4. **终端测试验证**:
   ```bash
   doops -session dev01 exec --target new-cluster --cmd "uname -a"
   ```

返回内核信息只证明远程命令通道可用；完成上节全部验收后才能宣布节点接入成功。
独立 Docker Agent 使用上述正式外部入口；非 Helm K8S 清单或 bind-mount 二进制不能
借此手工换镜像，也不能让当前 Agent 自行替换自己。
