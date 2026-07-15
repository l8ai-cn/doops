# Doops 构建与部署强制规范 (SOP)

> **核心纪律：绝对禁止在本地开发机或业务宿主机直接执行容器镜像构建。**
>
> 本地开发机的架构（如 Apple Silicon ARM64）、网络环境（代理/污染）以及未清理的缓存，极易导致打出的 OCI 镜像体积过大、架构错配或携带不安全的凭据。
> 所有线上与测试环境的 `doops-agent` 镜像，**必须且只能由 `main` 分支的 CNB CI 按标准流程构建、校验并推送。**

---

## 一、 Gateway 发布边界

`doops-gateway` 是公网控制面服务，必须作为独立发布包处理。它不是普通
`doops-agent` target，也不登记为 doops target。

固定发布入口：

```bash
bash scripts/deploy-gateway.sh --host 203.0.113.10 --user ubuntu
```

当前 gateway 维护主机：

- 公网地址：`203.0.113.10`
- SSH 用户：`ubuntu`
- Gateway URL：`http://203.0.113.10:42222`
- SSH 凭据由运维密钥库或维护人保管，禁止写入仓库、日志、提交信息和文档正文。

这条脚本只走 SSH/SCP，在宿主机真实 root 下完成：

1. 本地构建 `bin/doops-gateway`。
2. SSH 校验远端真实 DB `/var/lib/doops-gateway/gateway.db` 存在且非空。
3. 上传二进制到远端 `/tmp`。
4. 备份 `/usr/local/bin/doops-gateway`。
5. 替换二进制并重启 gateway。
6. 校验 `/proc/<pid>/exe` hash 与本地构建一致。
7. 校验 `/v1/targets` 未认证访问返回 `401`；可选用 `--verify-token` 校验认证访问返回 `200`。

禁止事项：

- 禁止用 `doops exec` 启动或重启 `doops-gateway`。
- 禁止用 `doops push/write/read` 给 gateway 控制面发版。
- 禁止用 base64/tar/分片临时传输替代 gateway 发布脚本。
- 不保留 gateway host agent 作为默认 target；需要主机巡检时走独立 SSH/监控流程。

如果 `doops push` 卡住，应修复 Git HTTP 反向隧道或目标 agent 版本，不允许用分片绕过。

---

## 二、 Agent 标准构建工作流 (Server-Side Build)

在进行任何修改后，若需要更新大模型管控组件 `doops-agent`，先由 `main` 的 CNB CI 构建并推送以完整 40 位 commit SHA 为标签的候选镜像；候选镜像必须在下一步被解析为 registry digest。已注册的 DoOps Agent 只能通过版本化 DoOps `DeploymentTemplate` 消费该 digest 并完成环境同步、Helm 收敛与验证。CNB 不负责环境部署、回滚或 Helm/Kubernetes 操作；`deploy.sh` 与任何 SSH/rsync 手工发布入口均已移除。

### 1. 触发受控发布 (在控制端执行)
发布者必须先将待发布提交推送到 `main`，确认 CNB CI 的候选镜像已经生成，并在无未提交文件的精确 commit checkout 中，以该完整 commit SHA 作为 `releaseId` 执行环境 `DeploymentTemplate`。CLI 会拒绝 HEAD 不匹配或工作区不干净的 source release。运行期必须使用该候选镜像的 registry digest，不得以可变 tag 作为实际部署镜像。Oilan 的入口为 `deploy/workflows/oilan-agent-bootstrap.yaml`，运行期目标清单由 `deploy/environments.yaml`、Helm Chart 和环境 values 定义。

```bash
doops -session oilan-agent-bootstrap cicd run \
  -f backend/deploy/workflows/oilan-agent-bootstrap.yaml \
  -target <configured-doops-target> \
  --dry-run \
  --set releaseId=<main-commit-sha>
```

The command synchronizes the repository, then sends one ordinary Ask to doagent
for the `$doops-cicd` Skill. The Skill writes the Gateway-designated JSON
`DeploymentRun` artifact;
text such as `admitted` is not evidence, and dry-run mutation count must be zero.

**DoOps Agent 执行顺序：**
1. 校验工作区与 `main` 上的精确 source commit 一致。
2. 解析 CI 候选镜像的 registry digest，并按环境声明同步到目标 registry。
3. 在候选镜像内校验 `doops-agent`、`do-agent`、BuildKit、Python/PyYAML、Helm 与嵌入的 Helm Chart。
4. 通过版本化 bootstrap Job 将现有 Deployment 纳入 Helm 管理，并执行 `helm upgrade --install`。
5. 等待 Job、Helm release 和 Kubernetes rollout 均成功。

### 双镜像构建原则

发布时维护两类镜像：

```bash
docker.cnb.cool/l8ai/ai/doops.sh/base-light:<release>
docker.cnb.cool/l8ai/ai/doops.sh:<release>
```

`Dockerfile.base.light` 只放基础运行时：doagent、BuildKit、kubectl、Python+PyYAML、Helm、系统包。`Dockerfile` 和 `agent/Dockerfile` 只放轻更新：doops-agent、skills、docs、entrypoint。禁止把基础 rootfs 压平成每个 release 都变化的单个多 GiB layer。

CNB 非同名制品路径使用 `docker.cnb.cool/<owner>/<repo>/base-light:<release>` 这种仓库下级路径；不要使用 `<repo>-base-light:<release>`。

发布后必须验证：

1. `/app/doops-agent -help` 可运行。
2. `/usr/local/bin/do-agent --version` 与发布声明的 doAgent 版本完全一致。
3. `buildctl --version` 可运行。
4. `python3 -c 'import yaml'` 可运行。
5. `helm version --short` 可运行。
6. `doops exec` 能读取 `hostname/date/kubectl get nodes/df/free`。
7. `doops ask` 能通过 doagent ACP HTTP 调用工具并返回结论。

镜像构建必须完成 base label 与前 5 项镜像内自检；第 6、7 项必须在 Agent 滚动更新后对真实 target 执行。

---

## 三、 Agent 部署与热更新机制

`doops-agent` 由 Helm 管理的 `Deployment` 托管，Chart 位于 `deploy/helm/doops-agent`；每个环境通过独立 values 文件声明镜像、目标节点、挂载、Secret 与 gateway TLS 地址。禁止直接 `kubectl set image`、`kubectl rollout restart` 或从节点 SSH 更新运行中的 Agent。

`hostNetwork` 更新必须等待旧实例释放 DoAgent 和 DoOps 监听端口。容器入口使用同一个 120 秒窗口等待两个端口；Chart 的 `startupProbe` 必须覆盖至少 200 秒，并同时验证当前容器内的 DoOps 进程和本地 health，避免误连宿主网络中的旧实例。DoAgent 未启动或健康检查失败时容器必须直接失败，不能只打印警告后继续。发布验收必须确认稳定窗口后的容器重启数仍为 0。

### 环境依赖注射规范 (Secret)
严禁在 `agent.yaml` 中明文出现 `LLM_API_KEY=sk-xxxx` 之类的配置。此类密钥应由 Secret 提取并通过 `envFrom` 下放。

### 容器运行时挂载规范

容器化 `doops-agent` 需要在 agent 内部继续调用宿主机容器运行时，例如
`nerdctl pull/run`、BuildKit、或一次性 helper 容器。只挂
`/run/containerd/containerd.sock` 不够：`pull` 可能成功，但 `run` 会因为
overlay snapshot、FIFO、`/etc/hosts` 或 nerdctl name-store 位于宿主机路径而失败。

标准容器/DaemonSet 必须同时挂载：

```text
/run/containerd      -> /run/containerd
/var/lib/containerd  -> /var/lib/containerd
/var/lib/nerdctl     -> /var/lib/nerdctl
```

升级顺序必须是：

1. 先在目标节点成功 `pull` 新镜像。
2. 再启动新 agent 或触发 DaemonSet 滚动更新。
3. 新 agent 注册 gateway 并通过 `exec` 验证后，再清理旧容器。

遇到 containerd overlay/FIFO/name-store 错误时，不要强切旧 agent。优先保留旧
agent 在线，用宿主机 namespace 或 Kubernetes 一次性特权 Pod 从宿主机视角修复：

```bash
kubectl run <node>-doops-deployer \
  --restart=Never \
  --overrides '<hostPID/hostNetwork/privileged + hostPath / 的 PodSpec>' \
  --image docker.cnb.cool/l8ai/ai/doops.sh:<release> -- \
  /bin/sh -lc 'chroot /host /usr/bin/nsenter -t 1 -m -n -- nerdctl run ...'
```

修复后必须验证：

```bash
doops -session smoke exec --target <target> --cmd 'sha256sum /app/doops-agent; nerdctl ps | grep doops-agent'
doops -session smoke exec --target <target> --cmd 'nerdctl run --rm --net none --entrypoint /bin/sh docker.cnb.cool/l8ai/ai/doops.sh:<release> -lc "echo direct-ok"'
doops -session smoke push --target <target> --src <small-dir>
```

### 宿主 Docker 直装升级规则

如果某个节点上的 `doops-agent` 由宿主 Docker 管理，而且容器里 `/app/doops-agent`
被宿主二进制 bind mount 覆盖，那么镜像升级不会生效。此时必须先做架构优化：

1. 保留旧 agent 在线。
2. 拉取新镜像。
3. 启动一个不绑定宿主 `/app/doops-agent` 的新容器，或迁移成 K3s/DaemonSet。
4. 确认 gateway 看到新 hash。
5. 再停旧容器。

不要把“换镜像 tag”误当成“换二进制”。如果宿主文件覆盖了 `/app/doops-agent`，
这两件事不是一回事。

### Unicom 过渡节点

Unicom 当前的部署形态是过渡态，不是最终标准态：它同时有 gateway 容器和
旧宿主 Docker 容器。这个形态下，升级应该先把新标准容器/K3s 路径跑起来，
再下线旧容器。不要继续在宿主二进制 bind mount 的容器上堆镜像升级。

---

## 四、 特殊情况：无容器引擎的物理裸机部署

如果需要将其部署到一台没有 K8s 且容器平台残缺的物理机上，**杜绝**使用本地 `docker` 打成 tar 传过去的远古做法。

提供原生高效解法（零外部依赖，极速发版）：

```bash
# 1. 在本地交叉编译目标平台的干净二进制文件 (耗时 < 5秒)
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/doops-agent-new ./cmd/agent

# 2. 利用 OCI 生成器直出镜像 tar
python3 scripts/build_oci_image.py /tmp/doops-agent-new /tmp/doops-agent-amd64.tar

# 3. 在目标机器用 ctr (containerd CLI) 直接导入
ctr -n k8s.io images import /tmp/doops-agent-amd64.tar
```
