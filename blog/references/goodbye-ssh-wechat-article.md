# 再见，SSH。

| 项目 | 内容 |
| --- | --- |
| 公众号 | 智涌君 |
| 作者 | Syman（原创） |
| 发布时间 | 2026年11月8日 08:33（浙江） |
| 原文链接 | https://mp.weixin.qq.com/s/KZPcJhiFTv9hakWGVnjaUg |
| 采集时间 | 2026-11-10（Asia/Shanghai） |
| 采集方式 | ego-lite 浏览器打开微信网页版文章，提取渲染后的正文 |

---

**开源 · MIT · 运维新范式**

> *一条命令直达任何一台内网机器*

DoOps 上手指南与二次开发实战：公网 Gateway + 内网 Agent，把排障、巡检、部署、文件同步收进同一个可审计入口。

项目地址：**github.com/symansun/doops** ｜ 核心贡献者：**孙勇 · 高家祺 · 宋晨宇**

## 01 每个运维人，都被 SSH 坑过

先别急着学新工具，看看这些场景你中了几个。

凌晨两点，告警响了。你要登录一台内网 GPU 机器看日志，于是开始了一套熟悉的仪式：

连 VPN → 登跳板机 → 翻出记录在某个 Excel 里的端口和密码 → 再跳一次 → 终于到了。十分钟过去，你还没看到第一行日志。

这只是日常。**SSH 的真正成本，从来不在那一次登录**，而在它身后一整条隐形账单：

🔑 **密钥与密码的泥潭**

几十台机器、几十套账号密码或密钥，散落在 Excel、备忘录和聊天记录里，换人即失控。

🕳️ **NAT 与内网隔离**

机器没有公网 IP，就得养跳板机、堡垒机、VPN——为一个"能连上"付出一整套基础设施。

👻 **操作无痕**

谁在什么时候对哪台机器执行了什么？SSH 不管。出了事故，复盘靠回忆。

🤖 **AI 帮不上忙**

大模型很会排障，但它拿不到你的机器。你只能人肉复制粘贴，做 AI 和服务器之间的"传话筒"。

**如果一个工具能让你彻底忘掉 SSH——不用记 IP、不用管端口、不用发密钥，还天然支持 AI 操作——你愿不愿意试五分钟？**

这就是**DoOps**要干的事。它已经以 MIT 协议完整开源。

## 02 DoOps 是什么？

一句话：SSH 的替代入口。日常使用只走公网 Gateway，再由 Gateway 路由到主动注册的内网 Agent。

DoOps 由三个角色组成，理解它们就理解了全部：

- **doops CLI / Skill**（客户端）：你本地唯一的操作入口，也是 AI Agent 的调用入口。
- **doops-gateway**（公网控制面）：负责认证、授权、审计、排队和内网 Agent 路由。
- **doops-agent**（边缘执行端）：跑在目标机器上，**主动向外发起长连接注册到 Gateway**，等待任务。

关键设计在于：**连接方向是反的**。传统 SSH 是你从外面敲门进来；DoOps 是里面的 Agent 主动出来报到。所以目标机器不需要公网 IP、不需要开端口、不需要 VPN，只要能出网，就能被纳管。

*图 1 · DoOps 架构：用户侧 → 公网 Gateway → 内网 Agent（NAT 之后）*

在这个链路上，DoOps 提供两种执行模式：

| 模式 | 命令 | 适合什么 |
| --- | --- | --- |
| 快路径 | `exec / read / write / push / pull / info / check / clean` | 确定性操作：跑命令、传文件、同步代码，秒级返回 |
| 慢路径 | `ask`/`chat` | 把自然语言任务交给远端 doagent 智能体，自主分析和执行 |

**安全默认收口**

统一认证、按 target/action 授权、全链路审计，高危操作默认拒绝。Agent 容器默认不启动 SSH、不启动 WebIDE，只保留`doops-agent`、`do-agent`、`buildkitd`三个进程。

## 03 先睹为快：它长什么样

CLI 之外，DoOps Console 提供一屏总览：连接、会话、集群、任务，尽收眼底。

![DoOps 智能运维工具首页](https://mmbiz.qpic.cn/sz_mmbiz_jpg/ia1IOjI1CzFlfSwc6aofTOFUcO7jT62Zgap5kd1hQg5IML1ib16N3wSeNCgKhet30Ut3P203j6FUg6b1gicVeLv9ZFaibibT5EliaVsE3Uff7TpJs/640?wx_fmt=jpeg&from=appmsg&watermark=1#imgIndex=0)

*图 2 · DoOps 统一入口：智能体、控制台、CLI 与多领域 Skill*

![DoOps Console 控制台总览](https://mmbiz.qpic.cn/sz_mmbiz_png/ia1IOjI1CzFnAUqWHwfYpYiazmp4AJ110iaHjV7ZbpMC68B6IxvIjUBYlA2tvknNKSiaxbmEzzA0kX3BWGZqHx60rHicQiamssxrEs9cPxc4awicbw/640?wx_fmt=png&from=appmsg&watermark=1#imgIndex=1)

*图 3 · Console 总览：在线连接、活跃会话、纳管集群、AI 助手运维入口*

控制台能做的事和 CLI 完全一致——因为它们走的是**同一条可审计执行链路**。网页上点一下，和终端里敲一行，审计里留下的是同一种记录。

## 04 五分钟快速上手

从 clone 到在第一台内网机器上执行命令，一共四步。

**1**

**安装 CLI 和 Skill**

安装脚本会把对应平台的预构建 CLI 放到`~/.local/bin/doops`，并把官方 Skill 装进你的项目：

```bash
# 克隆仓库并安装。--version 必须是已发布的 tag，--project 指向业务项目
git clone https://github.com/symansun/doops.git
cd doops
bash scripts/install.sh \
  --version <tag> \
  --project /absolute/path/to/your/project
# 如果 ~/.local/bin 不在 PATH 里
export PATH="$HOME/.local/bin:$PATH"
```

**2**

**登录 Gateway**

登录属于 Gateway，不属于某一个 target。向管理员要 Gateway 地址，用用户名密码登录，或从标准输入导入已有 user token。首次登录会发现该账号可见的在线 target，并写回本机路由。token 不要放进命令参数，也不要写进 `doops add`。

```bash
doops login --gateway https://gateway.example.com
printf '%s' "$TOKEN" | doops login --gateway https://gateway.example.com --token-stdin
doops list
```

只有 Gateway 发现不到的 Agent（例如还离线）才手工添加路由。这条命令只保存路由：

```bash
doops add \
  --name prod-master \
  --gateway https://gateway.example.com \
  --cluster prod \
  --instance master-1 \
  --aliases prod,master \
  --use 'prod cluster via gateway'
```

`cluster/instance`是路由键，名字由部署决定；别名`prod`、`master`之后可以直接当操作命令的`--target`用。

**3**

**验证链路**

```bash
doops list

# 列出已配置节点
doops targets --target prod-master

# 看到 cluster/instance 在线才算通
doops -session smoke exec --target prod-master --cmd 'hostname'
```

`targets`能看到在线，说明内网 Agent 已注册到 Gateway——**此后不再需要 SSH**。

**4**

**把任务交给 AI**

```bash
doops -session smoke ask --target prod-master \
  --msg '检查磁盘和内存压力，给出结论'
```

这一条命令之后，远端 doagent 会自己规划、自己执行、自己汇总结论。你要做的只是看结果。

**关于服务端 Agent**

如果目标机器还没有 Agent：K8s 集群用**DaemonSet 标准安装**（方式 A），单机用**Docker/nerdctl 容器**（方式 B），首次自举才用一次性 SSH（方式 C）。Agent 启动只需三个参数：`-gateway-url`、`-cluster`、`-instance`，注册不需要 token。详细命令见仓库 README。

## 05 六个高频实战场景

所有命令都遵循同一个直觉：`doops -session <会话> <动作> --target <目标>`。

### 场景一：批量巡检，一条命令

```bash
doops -session demo exec --target prod-node --cmd 'uptime && df -h'
```

没有交互式登录，没有密码提示，输出直接回到你的终端——也直接可以被脚本和 AI 消费。

### 场景二：让 AI 排障

```bash
doops -session demo ask --target prod-node \
  --msg '最近一小时 502 增多，查一下原因并给出处置建议'
```

AI 在远端沙盒里自主排查，所有动作落在审计里。它干活，你监督。

### 场景三：代码同步（基于 Git，不是 scp）

```bash
doops -session demo push --target prod-node --src .

# 本地 → /root/ws/demo
doops -session demo pull --target prod-node --dest ./demo-output
# 远端产物拉回本地
```

`push`尊重`.gitignore`/`.doopsignore`；底层走 Git HTTP 反向隧道，不再是 tar 分块上传。每个会话有独立工作区`/root/ws/<session>`，互不污染。

### 场景四：写个配置，立刻生效

```bash
doops -session demo write --target prod-node \
  --path /root/ws/demo/pod.yaml --file ./pod.yaml
doops -session demo exec --target prod-node --cmd 'ls -la /root/ws/demo'
```

### 场景五：远端构建镜像（BuildKit）

```bash
doops -session prod exec --target gpu-master --cmd \
  "cd /root/ws/prod/labelu && buildctl --addr unix:///run/buildkit/buildkitd.sock build \
--frontend dockerfile.v0 --local context=. --local dockerfile=. \
--opt platform=linux/amd64 \
--output type=image,name=registry.example.com/library/labelu:latest,push=true"
```

构建在 Agent 容器内完成，宿主机不沾构建垃圾。

### 场景六：批量升级所有 Agent

```bash
doops -session upgrade_demo upgrade \
  --target prod --cluster prod \
  --image <your-registry>/doops:<release> \
  --mode k8s --namespace doops-system \
  --workload daemonset/doops-agent --container doops-agent \
  --dry-run

# 先演练，确认无误后去掉再执行
```

K8s 模式自动执行`kubectl set image`和`rollout status`；裸二进制 Agent 会被明确拒绝，不会误伤。

## 06 二次开发（一）：写一个自己的 Skill

DoOps 的 AI 调度能力完全由 Skill 驱动——不改核心代码，就能沉淀团队的运维 SOP。

这是 DoOps 最被低估的能力。假设你要把「发布 labelU 标注系统」这套流程固化下来，让 AI 以后自动执行：

**1**

**建目录**

```bash
mkdir -p .agent/skills/deploy-labelu
```

**2**

**写 SKILL.md（元数据 + 行动指令）**

```markdown
---
name: LabelU 专用发布流程
description: 处理 labelU 标注系统的自动化代码同步和 K8s 发布
triggers:
  keywords: ["发布 labelu", "更新标注系统", "deploy labelU"]
phase: translate
priority: 50
---

# LabelU 发布规范

## 标准工作流

cd platform-tools/labelU
doops -session prod push --target gpu-master --src ./ --dest /root/ws/prod/labelu
doops -session prod exec --target gpu-master --cmd "cd /root/ws/prod/labelu && buildctl ..."
doops -session prod exec --target gpu-master --cmd "kubectl rollout restart deployment/labelu -n ai"

## 异常处理红线

远端超时或构建依赖错误时，立即停止并请求人工审核，禁止 AI 擅自修改底层配置。
```

**3**

**喊一声，自动执行**

下次在对话框里说「帮我部署 LabelU 标注系统」，Agent 命中关键词`deploy labelU`，就会加载这份 SOP 按部就班执行。你的经验，从此是团队的资产。

**编写纪律**

一个子业务一个 Skill 目录；远端操作一律收敛到`/root/ws/$SESSION/`沙盒；绝对禁止在 Skill 里引用旧的 SCP / SSH 手工手段。通用能力优先复用官方`deploy/SKILL.md`，别重复造轮子。

## 07 二次开发（二）：API 对接你的平台

CI 系统、内部平台、审批流——都可以直接调 Gateway 的 HTTP / WebSocket API。

调用链路：`你的平台 → doops-gateway → 目标 cluster/instance 的 doops-agent`。调用方永远不需要直连内网。

### 登录换取 Token

```http
POST /v1/auth/login
Content-Type: application/json

{
  "username": "alice",
  "password": "secret",
  "name": "ci deploy token"
}

# 响应
{
  "token": "dgw_user_xxx",
  "token_type": "user",
  "username": "alice"
}
```

之后的请求带上`Authorization: Bearer <token>`即可。长期自动化建议让管理员用`POST /v1/admin/tokens`签发服务 token。

### 核心端点一览

| 端点 | 用途 |
| --- | --- |
| `GET /health` | 健康检查，返回`ok` |
| `WS /v1/rpc?cluster=&instance=` | CLI 到 Gateway 的 JSON-RPC 通道（exec / push / ask …） |
| `WS /v1/agent/connect` | 内网 Agent 注册长连接 |
| `Git HTTP /v1/git/<cluster>/<instance>/<session>.git` | 代码同步反向隧道 |
| `POST /v1/admin/tokens` | 管理员签发 user token（明文只返回一次，库存哈希） |

### 故障码速查

- **401 Unauthorized**：token 无效或过期，认证失败。
- **403 Forbidden**：token 有效，但缺少该`cluster/instance/action`的授权，或缺少`admin`权限。

完整的登录、目标查询、WebSocket JSON-RPC、自然语言部署、进度流与审计查询，见仓库docs/DOOPS_API_GUIDE.md。

## 08 把 AI 放进生产环境，安全吗？

这是所有人第一反应。DoOps 的答案是把安全做在链路上，而不是贴在文档上。

- **统一认证**：用户用 gateway user token；Agent 注册不需要 token，天然少一份要保管的凭据。
- **细粒度授权**：默认全功能，需要隔离时按`cluster / instance / action`显式收窄；`agent:upgrade`、`admin`等高敏权限单独授予。
- **全链路审计**：每条记录可按`user_id、token_id、cluster、instance、action、session、status`追溯——AI 干的和人干的，同样可查。
- **沙盒纪律**：所有远端操作收敛在`/root/ws/$SESSION/`；Agent 容器默认不开 SSH、不开 WebIDE；高危操作默认拒绝。

**让 AI 替你干活的前提，是它干的每一步你都能复核。这正是 DoOps 和普通"AI 运维脚本"的分水岭。**

## 09 常见问题

### 完全不需要 SSH 吗？

日常操作完全不需要。SSH 只保留两个用途：首次把 Agent 装到机器上（一次性自举），以及 Agent 完全不可用时做自恢复。装好即忘。

### 目标机器没有公网 IP 可以吗？

这正是 DoOps 的主场。Agent 主动向公网 Gateway 发起长连接，只要能出网就能被纳管——NAT、防火墙、内网隔离都不是障碍。

### doops ask 报错怎么办？

先检查 Agent 侧`doagent-config`里的`settings.json`是否配置了有效的 API Key 和`API_BASE_URL`。注意本仓库不发布`do-agent`二进制，基础镜像需自行提供。

### 同一 cluster/instance 注册了多个 Agent 会怎样？

任一时刻只应有一个在线；重复注册会替换旧连接。命名遵循`prod/master-1`、`edge/gpu-node-1`这类约定即可。

### 支持哪些平台？

CLI 预构建二进制覆盖 darwin / linux 的 amd64 与 arm64；Agent 推荐 K8s/DaemonSet 或 Docker/nerdctl 容器部署，containerd 环境把 docker 命令换成 nerdctl 即可。

## 现在，轮到你把 SSH 关掉了

五分钟装完，第一条命令见效。好用的话，给个 Star，转给那个还在维护跳板机的同事。

github.com/symansun/doops

核心贡献者：**孙勇 · 高家祺 · 宋晨宇**｜ MIT License ｜ 欢迎 Issue 与 Pull Request

**参考来源**

1. DoOps 项目仓库（README、安装与命令说明）：https://github.com/symansun/doops
2. DoOps API 对接指南（登录、JSON-RPC、审计查询）：https://github.com/symansun/doops/blob/main/docs/DOOPS_API_GUIDE.md
3. 业务项目如何编写和插拔 Skill 指南：https://github.com/symansun/doops/blob/main/docs/SKILL_INTEGRATION_GUIDE.md

本文所有命令与配置均来自 DoOps 官方文档，可逐条复现。

---

## 留言 / 评论

> 网页版（mp.weixin.qq.com）不渲染留言列表：页面接口 `mp/appmsg_comment?action=getcomment` 返回 `no session`，点「写留言」提示需在微信中打开。以下留言按读者转发的评论区内容整理（转发有省略，仅收录可见部分）。

### 读者留言（摘录）

- **杰克**（广东 · 昨天）：不就是 AI 帮你做 SSH 了嘛？可以可以，但你自己也得会做 SSH，而且大部分情况最好还是自己 SSH，又不是啥麻烦事。
- **帷幄庸者**（浙江 · 12 小时前 · 👍2）：这个项目面向 OPC、非技术人群，Vibe Coding 让大多数人都能写代码，但是如何做部署还是最后一公里，通过黑盒让 AI 能够使用 SSH、K8s 部署自己的应用，而不需要了解 SSH、K8s 的技术细节。（作者赞过，1 条回复）
- **一一**（广东 · 57 分钟前）：我敢说，除了小白，基本不会用。
- **Faisal**（阿联酋 · 3 小时前）：AI 可以直接 SSH 帮忙运维啊。谁说不行了。
- **杨帅军**（山西 · 3 小时前）：不懂 SSH，乱写。
- **ElementAK**（美国 · 11 小时前）：第一次你怎么登进去部署这玩意了？不得有个通道么？别告诉我不是 SSH。
- **十**（广东 · 12 小时前）：agent 一旦暴露，一锅端。哪个没有安全思维的产品想出来的。

> 注：以上为用户提供的转发摘录，非完整评论区；昵称、地区、时间以转发内容为准。

## 采集说明

- 正文取自 ego-lite 浏览器中该文章网页渲染后的内容（`#js_content`），标题、作者、发布时间来自页面头部元信息。
- 微信网页版把正文代码块渲染成单行横向滚动，代码块的换行为按内容重排的近似还原。
- 文中 2 张配图使用原图链接（mmbiz.qpic.cn，微信图床，可能受防盗链影响）。
- 文章正文文字与页面一致，未做改写；仅补充了本文件的结构化标题、表格与说明。
