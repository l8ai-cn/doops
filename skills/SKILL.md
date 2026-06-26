---
name: doops-server
description: |
  用 doops 连接和操作 doops-agent 节点。doops 是 SSH 的替代入口，默认通过
  doops-gateway 进入内网 agent；直连 agent 只作为遗留自举/排障路径。
---

## Skill 定位

本 skill 是 doops 的入口导航和操作纪律，不承载所有功能细节。

- 安装与接入：先看本文，再按 `docs/CLUSTER_INSTALL_GUIDE.md` 执行。
- 日常使用：先看本文命令选择，再按 `docs/DOOPS_USAGE_GUIDE.md` 执行。
- Gateway/API 细节：看 `docs/DOOPS_GATEWAY_TUNNEL.md` 和 `docs/DOOPS_API_GUIDE.md`。
- 构建、发布、升级：看 `docs/BUILD_DEPLOY_SOP.md` 和 `docs/DOOPS_MAINTENANCE_LOG.md`。
- 模型与 ask 配置：看 `docs/MODEL_STANDARDS.md`。

权威源码在 doops.sh 仓库 `skills/SKILL.md`。项目里的 `.agent/skills/doops/SKILL.md` 是安装副本；修改 skill 时必须先改 doops.sh 源码，再通过 `scripts/install.sh` 同步。

## 核心纪律

- 优先使用 PATH 中的 `doops`；标准安装路径是 `~/.local/bin/doops`。
- 所有远端调用必须带 `-session <name>`。
- 默认使用 Gateway 模式；日常 target 必须是 gateway target。
- SSH 只用于首次安装、自恢复 agent，以及发布 `doops-gateway` 控制面；不是日常 target 操作入口。
- `doops-gateway` 是独立发布包，必须用 `scripts/deploy-gateway.sh --host <gateway-host>` 从 SSH 宿主机上下文发布。
- gateway 控制面不登记为 doops target，不通过任何 doops target 发布 gateway 本体。
- 不要用 base64、tar 或手写分片传输绕过 `push`。`doops_workspace_*` 是低层兼容 JSON-RPC 工具，不是 CLI、发布脚本和 skill 的标准路径；如果看到 512KB 分块日志，说明调用方绕过了当前 `doops push/pull`。应修复 Git HTTP 反向隧道、目标 agent 版本或发布脚本。
- 不要混用 token：
  - gateway CLI 操作用 gateway user token。
  - agent 注册 gateway 不需要 agent token；只需要 gateway URL、cluster 和 instance。
  - 直连 agent token 只属于遗留直连路径，不写入标准 target 配置。
- 不要在日志、终端输出、文档、提交记录里暴露完整 token。
- 如果用户是在发账号、发 token、授权、查审计、清理审计、查看/取消 gateway active operations，要按 gateway 管理动作处理，不要混成普通 target 运维。
- 遇到 `user operation limit exceeded` 时，先用 `doops admin operations list --target <gateway-target>` 查占槽操作；确认 stale 后再用 `doops admin operations cancel --target <gateway-target> --id <operation-id>` 取消。升级后的 agent 会按进程组终止 shell 类命令；`ask` 需要额外核查 doagent session 是否仍在工作。
- 给用户签发 gateway user token 时，优先使用 `doops admin token create`；不要 SSH 到 gateway 主机手工运行内部 DB 命令。

## 配置与凭据

Doops CLI 默认只有一个配置源：

```text
~/.agent/skills/doops/config.json
```

同目录运行态文件：

- `auth.json`：`doops login/logout` 管理的 token 缓存，结构必须是 `{"tokens":{"target-name":"..."}}`。
- `history.jsonl`：本机命令历史。
- `sessions.json`：本机会话缓存。

旧路径和运行痕迹不能作为当前配置依据：

- `~/.config/doops/config.json`
- `~/.doops/auth.json`
- 项目 `.doops/`

`DOOPS_CONFIG=/path/to/config.json` 只用于测试和应急显式覆盖。

标准配置文件只放 gateway target：

```json
{
  "servers": [
    {
      "name": "jm",
      "aliases": ["jy"],
      "gateway": "https://gateway.example.com",
      "cluster": "doops-jm",
      "instance": "jm-228",
      "use": "JM via gateway",
      "token": "<GATEWAY_USER_TOKEN>"
    }
  ]
}
```

## 安装引导

安装 CLI 和 skill：

```bash
git clone https://cnb.cool/l8ai/ai/doops.sh.git
cd doops.sh
bash scripts/install.sh --project /absolute/path/to/project
```

安装结果：

- CLI：`~/.local/bin/doops`
- Skill 副本：`<project>/.agent/skills/doops/SKILL.md`
- Canonical config：`~/.agent/skills/doops/config.json`

接入 gateway target：

```bash
doops add \
  --name jm \
  --gateway https://gateway.example.com \
  --cluster doops-jm \
  --instance jm-228 \
  --aliases jy \
  --token '<GATEWAY_USER_TOKEN>' \
  --use 'JM via gateway'
```

首次安装 agent 到新机器、K8s/DaemonSet 接入、裸二进制迁移等细节见 `docs/CLUSTER_INSTALL_GUIDE.md`。如果必须临时排查遗留直连节点，要在问题结论里明确标注它不是标准 gateway target。

## 使用引导

先确认本地配置和 gateway 在线状态：

```bash
doops list
doops targets --target unicom
```

常用命令：

| 需求 | 命令 |
| :--- | :--- |
| 执行确定性命令 | `doops -session <s> exec --target <t> --cmd '<cmd>'` |
| AI 自主分析 | `doops -session <s> ask --target <t> --msg '<task>'` |
| 推送本地目录 | `doops -session <s> push --target <t> --src <dir>` |
| 拉取远端工作区 | `doops -session <s> pull --target <t> --dest <dir>` |
| 小文本读写 | `doops read/write ...` |
| 工作区清理 | `doops -session <s> clean --target <t>` |
| 升级在线 agent | `doops -session <s> upgrade ... --dry-run` |
| 管理员签发 token | `doops admin token create --target <t> --user <u> --name <label> --expires 720h` |

大文件、压缩包、数据库导出、图片、视频、课程资源包等资产，用 `pull` 拉取整个 session 工作区。不要用 `read`，也不要让 `exec` 输出 base64。

Gateway 管理员签发 token 示例：

```bash
doops admin token create \
  --target jm \
  --user gaojiaqi \
  --name maintenance \
  --expires 720h
```

`--target` 负责选择 gateway 地址和当前管理员 token。新 token 只打印一次；如需写入本机 token 缓存，显式加 `--save-as <target-name>`。

完整命令说明和示例见 `docs/DOOPS_USAGE_GUIDE.md`。

## 升级引导

升级 doops CLI、gateway、agent 或 skill 前，先更新维护记录：

```text
docs/DOOPS_MAINTENANCE_LOG.md
```

升级前检查：

- 唯一配置源仍是 `~/.agent/skills/doops/config.json`。
- `auth.json` 使用 `tokens` 字段，没有写到旧的 `~/.doops/auth.json` 或 `passwords` 字段。
- `doops targets --target <gateway-mode-target>` 只展示当前有效 gateway targets。
- 旧 target、旧 alias、旧直连节点、短 token 测试配置已经淘汰。
- `.agents/`、`.doops/`、`auth.json`、`config.json`、`history.jsonl`、`sessions.json` 没有混进仓库。
- 文档里的镜像 tag 和发布策略一致；日常只更新轻镜像，不应反复拉大 base。
- gateway 控制面发布走 `scripts/deploy-gateway.sh`；不要走 `doops exec/push/write`。
- 至少对一个真实 gateway target 回归 `exec`、`push`、`pull`、`ask`、`targets` 中受影响的路径。

发布和构建细节见 `docs/BUILD_DEPLOY_SOP.md`。

## 故障入口

- `401 Unauthorized`：认证失败。先确认 CLI 读的是 `~/.agent/skills/doops/config.json`，标准 target 的 token 必须是 gateway user token。
- `403 Forbidden`：认证通过但权限不足，或用户被显式收窄。
- target 不在线：先跑 `doops targets --target <gateway-target>`。
- `busy=true`：表示 target-wide 阻塞；`status=active` 且 `busy=false` 表示有 session 在跑，但其它 session 仍可使用。
- gateway `push/pull` 失败：确认 `/v1/git/<cluster>/<instance>/<session>.git` 能路由到在线 agent，并检查 agent 本地 `/git/` handler/token。

更完整的 API、隧道和故障细节见 `docs/DOOPS_GATEWAY_TUNNEL.md`、`docs/DOOPS_API_GUIDE.md`。
