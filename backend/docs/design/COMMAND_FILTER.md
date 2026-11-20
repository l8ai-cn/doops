# Agent 命令过滤

Gateway 先按用户和 target 授权并记录操作。对已获授权的执行请求，目标 Agent 在启动命令前判断是否命中拒绝规则。CLI 和模型都不能通过请求自带规则，也不能在拒绝后自动改走其它入口。

| 入口 | 交给过滤器的输入 | 放行后的执行方式 |
| --- | --- | --- |
| `exec`、`exec --bg` | 用户的原始命令文本 | 现有 Bash 前台或后台执行器 |
| 普通 `ask` 的原生 Bash | doagent 权限引擎批准的命令文本 | 受管 shell 入口执行 Bash |
| 内置 K8s 诊断脚本 | 每一步 `kubectl` 或 `helm` 的原始参数 | `/app/doops-shell --argv` 检查后直接启动程序，保留退出状态 |
| `k8s`、`ask` 的 `k8s_read` | `buildK8SCommands` 生成的 kubectl 或 Helm 命令及参数 | 原始 argv 直接启动进程，保留输出裁剪和脱敏 |
| `ask --apply` 的结构化 Helm 工具 | 实际执行的 Helm 命令及参数 | 保留阶段回执、校验及已有的补偿语义 |

命令形状检查只观察交给执行器的文本或 argv，不解释脚本内容，不监控任意子进程，也不承诺识别所有语义等价的 shell 程序。它不替代 target 授权、Kubernetes RBAC 或凭据管理。数据库、文件传输、SSH 等专用协议和工具保留各自的输入契约。普通 `ask` 只读工具不因规则放行而获得写权限；`helm values` 仍只返回键路径。

上述 `ask` 路径以受管镜像启动的 doagent 为准。如果 `DO_AGENT_URL` 指向自行运行的服务，Agent 不会给那个已启动的进程注入环境。运维方必须在该服务安装匹配版本的工具、将 `DO_AGENT_SHELL` 设为 `/app/doops-shell` 并传入同一份规则；仅在本地 Agent 上配置规则不代表外部服务已经生效。

受管 doagent 通过 `DO_AGENT_BASH_WAIT_FOR_EXIT=1` 等待原生 Bash 命令退出，包括带 `run_in_background=true` 的请求。独立部署的 doagent 需要使用支持结构化退出状态的构建，并配置相同的等待模式。Linux 下取消或超时会终止受管命令的进程组。

普通 `ask` 按本轮工具事件生成终态。Bash 非零退出与 K8s 读取失败都会返回工具失败，后续成功调用和模型答复也会保留先前的失败。`toolFailures` 列出工具名、调用 ID、错误类别以及运行时提供的整数退出码。工具超时与用户取消分别记为 `timeout`、`canceled`。标准输出和标准错误均进入工具结果；打印错误文字的成功命令仍返回成功。需要把预期非零状态视作查询成功时，在命令中显式处理该状态。

## 配置和失败语义

在 Agent chart 中设置 `commandDenyRules`；非 Helm 安装在目标 Agent 进程环境中设置对应的 JSON 数组 `DOOPS_COMMAND_DENY_RULES`。更新配置后重新启动 Agent 及其受管 doagent。未配置时只使用内置规则；附加规则不能覆盖或关闭内置规则。例：在维护窗口暂停结构化和同形文本的 `kubectl drain` 请求：

```yaml
commandDenyRules:
  - name: maintenance_drain
    match: '\bkubectl\s+drain\b'
    hint: '节点维护窗口尚未开放；请联系值班员核对时间。'
```

规则从内置到配置顺序匹配，首个命中返回 `blocked_command(name): next: hint`。`match` 使用 Go 正则，默认区分大小写；结构化 argv 中含空格或特殊字符的参数使用单引号表示，仅用于匹配，执行时不重新解析。非法 JSON、未知字段、重名、空提示或非法正则导致启动失败。

- **启动前被拒：** 返回失败。节点巡检不会返回成功；同轮 `ask --apply` 中后续 Helm 调用成功也不会抵消这次拒绝。
- **写入后无法验证：** 返回“结果未知”。Gateway 将 Agent 的公开变更回执传给 CLI，其中保留规则名和处置提示。先回读实际状态，不自动重放。
- 结构化 kubectl 写操作启动后返回非零时，终态携带命令失败原因及经过脱敏的 stderr。进程退出码能说明 kubectl 执行失败，资源是否已变更还要结合 API 证据判断。取消时 Agent 等待进程组结束。

## 扩展原生命令

明确的单条操作可以直接通过 `exec` 运行原生 `kubectl` 或 `helm`。经常一起查询的对象可以编排成 K8s Skill 脚本，逐步经 `doops-shell --argv` 执行，并向 AI 返回精简的查询结果。工作负载和 Helm 修订诊断见镜像内 `/app/skills/k8s/SKILL.md`。

## 增加已有结构化工具的操作

在 Agent 接受的结构化字段中表达意图，验证参数后在 `buildK8SCommands` 映射为原始 argv；CLI 只翻译语法。只有新增的只读操作才加入 `k8sReadOperations`（供 Gateway 分类与 MCP 读取工具共用）。新增操作本身不需要新增拒绝规则。用替身可执行文件记录 argv：允许时核对参数和结果，命中策略时断言规则名、下一步提示和零次启动。
