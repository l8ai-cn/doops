---
name: semantic-deployment
description: 将 DeploymentPlan 协调为经证据验证的目标状态
triggers:
  keywords: [DeploymentPlan, ReconciliationResult, deployment plan, reconcile deployment, 声明式部署, 发布计划, 发布协调]
  regex: "DeploymentPlan|ReconciliationResult|声明式.*部署|部署.*协调"
phase: [analyze, translate, cleanup]
priority: 200
max_tokens: 1800
requires: []
conflicts: []
---

# 语义部署

你接收的唯一部署输入是一个完整的 `DeploymentPlan`。你的职责不是解释一串操作步骤，而是将计划中已经解析的目标状态协调到真实环境，并以 `ReconciliationResult` 记录可复查的观察事实。

## 输入边界

- 只接受 `apiVersion=doops.sh/v2`、`kind=DeploymentPlan` 的单个计划对象。
- 只使用计划中的 `digest`、`release`、`target.profile`、`artifactContract`、`desiredState` 和 `acceptance`。
- 当前 doagent 会话的工作目录是已同步的发布工作区。它必须与计划中的不可变 release identity 一致。
- 不得根据域名、目录名、历史环境名、默认命名空间或聊天上下文补全目标。
- `target.profile.executor` 是唯一执行适配器；不得把 Helm 配置解释为通用 CI/CD 字段。
- 当 executor 配置声明 `workload` 与 `container` 时，它们是唯一可协调的运行工作负载和容器身份。
- 计划未声明的目标、制品、配置或授权都是阻塞事实，不可自行猜测或替换。

## 目标状态

计划的目标状态由以下不可分割的事实组成：

- 不可变 release identity 已被确认，且工作区、制品与计划引用同一版本。
- 所有服务都具有符合 artifact contract 的不可变 image set。
- executor 配置所属的 release manifest 已表达该 image set 与期望配置。
- 目标环境的运行态、服务端点和公网检查满足 acceptance。
- 发布后的日志没有与本次变更相关的未处理错误。

## Agent 原生执行

- 规划、上下文管理、工具选择和多 Agent 委派由 doagent 引擎负责，本 Skill 不实现 Agent 框架或固定执行器。
- 引擎可以创建或委派多 Agent，也可以组合其他已安装 Skill 与工具完成独立观察或操作。
- 只能使用运行时实际暴露且当前权限允许的能力，不得根据文档示例推断工具存在。
- 本 Skill 只约束目标、授权、不可变事实和证据契约，不规定命令顺序、工具名称、子 Agent 数量、重试拓扑或部署脚本。
- 不得把内部计划、工具调用、脚本或阶段列表写回 `DeploymentPlan`，也不得把它们伪装成发布协议。

## 协调语义

- 对 source release，必须读取 `.doops/source.json`，确认其中的 source revision 与
  `DeploymentPlan` 一致。工作区 Git HEAD 是传输快照 commit，不是 source revision，
  不得将两者直接比较。
- 对 manifest release，只协调该 manifest 表达的目标状态，不得重新解释或替换其引用。
- 只在请求明确授权 mutation 时改变真实状态。dry run 只能收集事实并报告是否仍需 mutation。
- 每次协调后重新观察真实状态；`attempts` 与 `noProgressAttempts` 仅记录实际尝试，不代表宿主侧重试控制，不可虚构。
- 如果运行时缺少完成声明所需的能力、凭据或目标访问权，报告 `blocked`，并把缺失事实写入 failure evidence。

## 验收与恢复

- `converged` 只能在每个 `requiredEvidence` 都有实际观察证据时返回。
- 每条 evidence 都必须有 `kind`、`subject`、RFC3339 `observedAt` 和非空 `value`。
- 运行态、端点、公网检查和发布后日志属于观察事实，不能用“已完成”之类文本代替。
- 每条 evidence 必须填写 `toolRef`：`tool` 是运行时显示的精确工具名，`ordinal`
  是该工具在本轮终态 SSE 事件中的一基序号。不得用失败调用、写入/通用执行调用或
  无关调用为观察事实背书。
- doops-agent 会按原始 SSE 顺序解析 `toolRef`，注入真实 `toolCallId`、对应的
  `toolDigest`、整轮 `traceDigest` 和 `executionEvidence`。
- 不得自行生成、猜测或复制 `toolCallId`、`executionEvidence`、`traceDigest`、
  tool call digest 或 `turnId`；这些字段必须来自本轮实际工具事件。
- 在 mutation 前因能力、凭据或权限不足而阻塞时，报告实际 `failureEvidence`，不得伪造回滚。
- 仅在实际发生 mutation、验收失败且目标环境明确声明可逆恢复能力时，才可使用该能力；恢复动作及其观察结果必须写入 `failureEvidence`。
- 如果没有声明可逆恢复能力，或恢复能力不可用，不得编造兼容路径；按实际情况返回 `blocked` 或 `failed`。
- `rollback-state` 只能记录实际执行并观察到的恢复结果，不能作为固定必填项。
- `failed` 与 `blocked` 必须包含可复查的实际阻塞原因；无法取得的证据不能伪造。
- 不得通过缩容、删除工作负载、跳过验收或降级为旧入口来制造表面成功。

## 输出契约

当调用指令声明机器结果文件路径时，必须先把最终 JSON 对象写入同目录临时文件，
再通过原子 rename 写入该路径。机器结果文件必须只包含一个 JSON 对象，不能包含
Markdown 或解释性文本。最终回复必须与该文件表达同一个对象，且只能是一个 JSON
对象：

```json
{
  "apiVersion": "doops.sh/v2",
  "kind": "ReconciliationResult",
  "planDigest": "sha256:...",
  "status": "converged",
  "attempts": 1,
  "noProgressAttempts": 0,
  "evidence": [
    {
      "kind": "source-identity",
      "subject": "release",
      "observedAt": "2026-07-12T00:00:00Z",
      "value": "immutable revision confirmed",
      "toolRef": {
        "tool": "<exact-runtime-tool-name>",
        "ordinal": 1
      }
    }
  ],
  "failureEvidence": []
}
```

上面的对象是 Agent 输出。doops-agent 在权威 `turn_finished` 后按原始 SSE
终态顺序解析每条 `toolRef`，注入真实 `toolCallId`、`executionEvidence`、
对应 `toolDigest` 和整轮 `traceDigest`。CLI 会再次校验 source revision、
workspace commit、turn、逐条绑定和工具事件摘要。

不得返回 Markdown、解释性文本、命令清单、阶段列表或额外 JSON 对象。
