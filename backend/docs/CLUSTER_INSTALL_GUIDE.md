# Doops Agent 集群接入指南

本文档介绍如何将 `doops-agent` 部署到一个新的服务器或集群环境。Agent 被设计为**无状态执行通道**，只需要有网络连通性和合适的权限，即可接管不同底层架构的资源。

---

## 模式一：Kubernetes / K3s 集群部署 (推荐)

在完整的 K8s/K3s 体系下，推荐将 Agent 作为 **DaemonSet** 运行在特权模式下，以确保每个计算节点都被接管管控，并利用集群原生认证。

### 1. 配置 `agent.yaml`

使用项目根目录提供的 `agent/agent.yaml`：
- **网络**: 需要 `hostNetwork: true` 以暴露端口。
- **特权**: 需要 `securityContext.privileged: true` 以支持底层的沙盒逃逸和硬件访问。
- **默认最小暴露面**: `doops-agent` 默认不启动 SSH，不启动 WebIDE；只有在极少数兼容场景下，才允许显式打开 `DOOPS_ENABLE_SSHD=1` 或 `DOOPS_ENABLE_WEBIDE=1`。
- **运行时视图**: 只挂宿主运行时 socket 不够。容器内需要能调用宿主机运行时视图时，
  必须同时挂载 `/run/containerd`、`/var/lib/containerd`、`/var/lib/nerdctl`。
  否则会出现“`pull` 成功但 `run` 失败”的 overlay/FIFO/name-store 问题。
- **升级原则**: 镜像升级应替换容器内 `/app/doops-agent`，不要再把宿主二进制
  bind mount 到 `/app/doops-agent`。宿主二进制覆盖镜像会让换镜像失去意义。

> [!NOTE]
> `doops-agent` 启动时会自动检测容器引擎 (nerdctl > docker > podman) 供运行时巡检使用；镜像构建则统一走内置 BuildKit，不依赖宿主机 `nerdctl build`。

### 1.1 配置 doagent 的 ConfigMap

`doops ask` 依赖 `doagent-config` ConfigMap，里面放 `settings.json`。公开仓库里保留的是模板，生产环境应由私有仓库 `l8ai-secret` 生成后再 apply。

```bash
kubectl -n ai apply -f agent/agent-config.yaml
```

如果你已经在私有仓库里生成了正式配置，优先使用那个版本覆盖模板：

```bash
kubectl -n ai apply -f /path/to/doagent-config.yaml
```

### 2. 部署到集群

```bash
# 推荐创建命名空间并部署
kubectl create namespace doops-system
kubectl apply -f agent/agent.yaml -n doops-system
```

如果是单节点单实例接入，而不是整集群所有节点都跑 DaemonSet，可以改成 Deployment，并用 `nodeSelector` 固定到一个节点。

---

## 模式二：纯 Docker 单机环境 (快速拉起)

如果你有一台全新的没有任何编排系统的裸机服务器，可以通过原生 Docker 容器的方式拉起 Agent。

### 1. 环境准备
确保机器已安装 `docker`。

### 2. 极简启动命令

```bash
docker run -d \
  --name doops-agent \
  --net host \
  --pid host \
  --ipc host \
  --privileged \
  -v /run/containerd:/run/containerd \
  -v /var/lib/containerd:/var/lib/containerd \
  -v /var/lib/nerdctl:/var/lib/nerdctl \
  -v /root/.kube/config:/root/.kube/config:ro \
  -v /path/to/doagent-config.json:/opt/doagent_config/settings.json:ro \
  docker.cnb.cool/l8ai/ai/doops.sh:v1.1
```

> [!TIP]
> **原理解析**：
> - `--net host` 等效于开放 42222 监听端口。
> - 不要为了“图省事”再把 SSH 或 WebIDE 面暴露出来；`doops-agent` 当前安全默认值是关闭这两者。

## 安装完成后的验收标准

不要再以“容器起来了”或“`ps` 里有进程”作为安装成功标准。当前版本的 `doops install` 会在安装后主动做一次 agent 侧能力自检，建议至少满足下面四项：

- `agent process`: `OK`
- `container runtime`: `OK`
- `kubectl`: `OK` 或至少有明确的 kubeconfig 来源
- `buildkit`: `OK`，且 `buildkit-sock` 可见

如果 `agent process` 或 `container runtime` 失败，安装应直接视为失败；如果 `kubectl` 或 `buildkit` 为 `MISSING`，说明目标只能做部分能力，不能再被当成“完整可运维节点”。
> - `-v /root/.kube/config` 允许 Agent 继承宿主机的 Kube 凭证操作集群（如果不涉及 K8s 操作可略去）。

---

## 3. 常见连通性验证

新节点部署并启动容器后，需要确保该 agent 已主动注册到 gateway。

1. **确认 agent 注册 gateway**:
   ```bash
   doops targets --target <gateway-target>
   ```

2. **添加 gateway 配置**:
   在 `~/.agent/skills/doops/config.json` 里追加或用 `doops add` 写入：
   ```json
   {
     "name": "new-cluster",
     "gateway": "https://gateway.example.com",
     "cluster": "doops-new",
     "instance": "master-1",
     "use": "新测试环境 via gateway",
     "token": "<GATEWAY_USER_TOKEN>"
   }
   ```

3. **终端测试验证**:
   ```bash
   doops -session dev01 exec --target new-cluster --cmd "uname -a"
   ```

如果能返回新服务器的内核信息，恭喜你，新节点接入成功。

### 4. 升级到标准容器/K3s 形态

如果现有节点是宿主 Docker 直装，且 `/app/doops-agent` 被宿主二进制 bind mount 覆盖，
不要继续做镜像层级的“原地升级”。正确做法是：

1. 先让新镜像在节点上成功 `pull`。
2. 启动一个新的标准容器或 DaemonSet 版本，不再绑定宿主 `/app/doops-agent`。
3. 验证 `doops exec` 和 `doops push` 正常。
4. 再清理旧容器。

K3s / Kubernetes 场景优先走 `agent/agent.yaml` 的 DaemonSet 形态。

### 5. Unicom 过渡节点

Unicom 这类混合节点如果同时存在 `doops-agent-gateway` 和旧宿主 Docker `doops-agent`，
并且旧容器把宿主二进制 bind mount 到 `/app/doops-agent`，就不要继续把它当成镜像升级
目标。正确收口方式是：

1. 先在 K3s 里起标准 `doops-agent`。
2. 让 gateway 先看到新 `cluster/instance`。
3. 验证 `exec/push/ask`。
4. 再停掉旧宿主容器。

Gateway 隧道模式下，应先验证：

```bash
doops targets --target <gateway-target>
```

只有能看到新的 `cluster/instance` 在线，后续 `exec/ask` 才会成功。
