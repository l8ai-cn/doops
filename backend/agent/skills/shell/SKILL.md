---
name: shell
description: 进入当前 target 的宿主机，或把结构化工具未覆盖的操作交给受审计的 exec
triggers:
  keywords: [宿主机, 进入节点, nsenter]
phase: [analyze]
priority: 5
max_tokens: 2400
requires: []
conflicts: []
---

结构化工具没覆盖的文件、进程、网络探测或镜像诊断，优先使用当前 Auto ask 会话提供的
`Bash` 工具在目标 Agent 执行环境中完成。需要用户可复核的确定性命令、完整 Gateway
执行审计或后台任务时，交给上层通过 `doops -session <s> exec --target <t>` 执行。
不要因为工具名单或交互审批 UI 缺失而退回让用户手工执行。
只处理目标和范围已明确的任务；仅方案/诊断不变更，缺关键目标或授权时返回缺项，
不猜测对象。已有具体授权沿用，不逐命令重复确认。

## 进入宿主机

用户在已有 Kubernetes Agent target 上说“进入宿主机”，指该 Agent 所在节点。
沿用当前 target/session，先做只读身份验证；不要列 A/B/C 让用户选，也不转成 GPU
检查或新 Agent 安装。只有明确指向其它物理机或 hypervisor 且缺少映射时，才询问具体目标。

1. 使用本 Skill 下方的完整探测脚本，校验 `/host`、PID 1 根目录和进入前后的 mount
   namespace。`Skill` 的 `load_prompt` 已加载脚本，不再调用 `Read` 读取 Skill 或
   `/app/self-docs`：`Read` 只能访问当前工作区，无法读取这些全局文件。
2. 当前会话提供 `Bash` 工具时直接在其中执行；会话没有 `Bash`、或需要用户可复核的
   确定性命令与完整 Gateway 审计时，输出一个完整的原始 `bash` 代码块，
   不包 `doops exec`、不含占位符。Web 的“在此执行”会预填命令，用户提交后沿当前
   target/session 走 Gateway exec；上层有 CLI 时，将脚本作为
   `doops -session <s> exec --target <t> --cmd` 的参数，沿用已有授权执行。
3. 等待执行方的终态和身份输出。Web 结果留在执行栏，不自动回传 ask；没收到结果时
   只说明命令已准备好，不宣告成功或假装轮询。失败保留退出码和缺项，
   不尝试 K8s 只读工具的 exec、其它 Pod、SSH、直连端口或自行开启 `hostAccess`。
4. 每次后续宿主命令仍带 `nsenter`；单次 exec 不会永久切换后续会话。
   进入和只读检查不授权安装、重启、改挂载、改权限或修驱动。

```bash
set -eu
for tool in nsenter readlink stat; do
  command -v "$tool" >/dev/null || {
    printf 'host_access_unavailable: missing %s\n' "$tool" >&2
    exit 69
  }
done
test -d /host || { printf '%s\n' 'host_access_unavailable: /host not mounted' >&2; exit 69; }
host_root=$(stat -Lc '%d:%i' /host)
pid1_root=$(stat -Lc '%d:%i' /proc/1/root)
host_mnt=$(readlink /proc/1/ns/mnt)
agent_mnt=$(readlink /proc/self/ns/mnt)
if [ "$host_root" != "$pid1_root" ] || [ "$host_mnt" = "$agent_mnt" ]; then
  printf '%s\n' 'host_access_unavailable: host root or PID namespace not exposed; check hostAccess' >&2
  exit 69
fi
nsenter -t 1 -m -u -i -n -- sh -c '
  set -eu
  cd /
  test "$(readlink /proc/self/ns/mnt)" = "$1" || {
    printf "host_access_unavailable: entered mount namespace mismatch\n" >&2; exit 69;
  }
  test "$(stat -Lc "%d:%i" /)" = "$2" || {
    printf "host_access_unavailable: entered root mismatch\n" >&2; exit 69;
  }
  printf "host_hostname="; hostname
  printf "host_mount_namespace=%s\nhost_root=%s\n" "$1" "$2"
  uname -srm
  id
' sh "$host_mnt" "$host_root"
```

## 其它命令

- 按已核实的 OS、工具和身份生成命令；不固定包管理器，不因缺 sudo 自动安装或提权。
  代理、入口和工作目录取自项目及调用环境；每次 exec 需要工作区时显式 cd。
- Bash 脚本用 `set -euo pipefail`；其它 shell 保留等价的关键步骤和管道失败检查。
  不用 `|| true` 吞掉验证、权限或连接失败，不把删除重建当通用幂等手段。
- 长命令设有界超时，由上层使用 `exec --bg` 等待终态；不自行 nohup 或放到后台。
- 需要临时文件时用 `mktemp -d /tmp/doops-XXXXXX`；只清理本轮独占目录，不清理前缀通配符。
  依赖它的任务结束后才清理；失败保留必要诊断并报告位置。
- 使用受管凭据引用；不打印、展开或把秘密内联进命令、prompt、日志、工作区，禁用敏感链路的 xtrace。
- Helm 发布可用 `mcp_doops_helm`，用户明确授权的原生 Helm 命令也可交上层经
  `doops -session <s> exec --target <t> --cmd '<helm 命令>'` 执行；沿用已有授权，
  不新增 CI、镜像、资源种类或重复确认门禁。保留原生命令退出码与 release 终态，
  不以 exec 结果替代 MCP attestation，不把 Helm 成功说成全业务健康。
  exec 不解析受管凭据引用，不能读取或内联凭据 payload 来补齐认证。
- 运维变更交上层
  `doops -session <s> k8s --target <t> ...`，直接执行已明确的操作；数据库变更交上层
  `doops -session <s> sql --target <t> --mode apply --sql/--file`，不改用 shell。
