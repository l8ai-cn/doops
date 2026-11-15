---
name: k8s
description: 用 doops 结构化工具读 Kubernetes/Helm 状态并管理 Helm release
triggers:
  keywords: [k8s, kubernetes, kubectl, pod, deployment, namespace, daemonset, ingress, helm, rollout]
  regex: "kubectl\\s+|k8s|kubernetes|helm\\s+|rollout"
phase: [analyze, translate, cleanup]
priority: 10
max_tokens: 800
requires: []
conflicts: []
---

只在上下文已明确 Kubernetes 时使用；“重启服务”不自动指向集群。
沿用本次 Gateway target 对应的集群和运行时配置，资源名、namespace、release 从任务
或指定资产核对，不从示例补默认值。只读/方案任务到证据或方案结束，不进入发布。

## 读状态：`mcp_doops_k8s_read`

| 操作 | 必填 | 例子 |
|---|---|---|
| `get` | `kind` | `kind=pods namespace=apps output=wide`；单个对象加 `name`；`selector=app=api`；`all_namespaces=true`；`output=jsonpath={.items[*].spec.containers[0].image}` |
| `describe` | `kind`、`name` | `kind=deployment name=api namespace=apps` |
| `logs` | `resource` | `resource=deployment/api namespace=apps tail=200`；崩溃前日志加 `previous=true`；`since=10m` |
| `events` | — | 整个 namespace 省略 `resource`；只看一个对象用 `resource=pod/<name>` |
| `top` | `kind` | `kind=pods namespace=apps` |
| `rollout-status` | `resource` | `resource=deployment/api namespace=apps` |
| `helm-history` / `helm-status` | `name`、`namespace` | `name=<release>`；可读 history 记录回滚目标。status 只给修订、状态和时间，不给 values/manifest/notes |
| `helm-list` | — | `namespace=<ns>` 或 `all_namespaces=true` |
| `helm-values` | `name`、`namespace` | 只返回 key 路径，看 chart 支持哪些配置 |
| `configmap-keys` | — | `namespace=apps` 或 `all_namespaces=true`；可加 `name`、`selector`。只返回 ConfigMap 的键名，不返回值；不要对 ConfigMap 用 `describe` 或 `output=json` |

namespaced 资源显式带 `namespace`，授权的跨 namespace 查询用 `all_namespaces=true`；
`nodes` 这类集群级资源不带虚假 namespace。kind 用 kubectl 小写名。
Secret、ServiceAccount 不开放读取。

## 发布：`mcp_doops_helm`

- `ask --apply` 已传递变更授权，沿用目标和范围，不增加 Helm 字段或逐对象重复确认。
  用户明确给出的原生 Helm 命令也可由上层经 `doops exec` 执行，保留其独立终态和审计。
- `upgrade-install`：`release`、`namespace`、`chart`（HTTPS `.tgz`，或 `asset_source=workspace`
  时的 session 相对路径）；可选 `value_files`、`set_strings`、`timeout`、`atomic`、
  `take_ownership`。values 合并沿用 Helm 原生语义；`set_strings` 的字符串使用
  `--set-string`，数字和布尔保留原生类型并使用 `--set-json` 传给 Helm。
- `rollback`：`release`、`namespace`、`revision`；`uninstall`：`release`、`namespace`。
- upgrade-install 直接执行原生 Helm `--wait`，随后核对 release status=deployed，
  且 revision/description 属于本次执行。没有额外 server dry-run、manifest 类型白名单、
  镜像策略或工作负载/Ingress 二次验收。
- 仅含 NetworkPolicy、ConfigMap、Ingress 或其它合法资源的 Chart 照常发布，
  不要求存在 workload、镜像、CI 成功记录或业务健康证明。
- `atomic=true` 和 `take_ownership=true` 由用户选择，默认不启用；Helm 的 ownership、
  schema、Kubernetes API、等待超时和下载/认证错误如实返回，不通过改标签或关闭校验掩盖。
- 失败返回 `错误码: 原因`：
  - `helm_mutation_failed`：原生 Helm 执行失败，保留脱敏后的实际原因。
  - `helm_mutation_invalid_result`：缺少可归属本次执行的结果。
  - `helm_verification_failed`：release 状态、revision 或 description 不符合本次执行。
  - `helm_interrupted`：调用预算耗尽，保留实际 release 状态和中断原因。
- 按任务另行做适用的 rollout、日志、健康与功能检查；分别报告 Helm 发布和业务验收结果，
  不把 release 成功说成全业务健康，也不把额外验收作为所有 Chart 的发布门禁。
- 保留错误回执和可信证明；RPC 断连、取消 ACK 或缺少终态按 `outcome_unknown` 处理，
  不自动重放变更，不吞退出码，不借用其它调用的成功。
- 失败后先读相关状态和配置，在已有授权内修复根因并继续验证；同一目标的明确失败可
  由后续同意图成功纠正，保留两次回执并反馈修复过程。不能只报告错误或要求用户重做检查。

## 运维变更

重启、扩缩、cordon、删 Pod 交回上层用 `doops -session <s> k8s --target <t> ... --confirm`
在已有具体授权内执行；你在 ask 里只读不改。
