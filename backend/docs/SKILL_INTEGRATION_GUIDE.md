# 业务 Skill 编写与接入

Agent 原生职责边界见
[`AGENT_NATIVE_CICD.md`](AGENT_NATIVE_CICD.md)。

Skill 是交给 doagent 原生 Agent 引擎的领域契约。Skill 不执行命令，也不实现 Agent
框架；doagent 负责上下文、规划、多 Agent 委派、Skill 组合和工具调用。

## 何时创建 Skill

只有以下内容稳定且会重复使用时才创建：

- 业务目标和术语；
- 不可变约束和安全边界；
- 输入对象与权威配置来源；
- 成功、阻塞和失败所需的实际证据；
- 允许使用的能力范围和禁止行为；
- 结构化输出契约。

一次性命令、环境临时值、凭据、完整日志和罕见边角案例不应写进 Skill。

## 文件结构

Skill 使用带 frontmatter 的 `SKILL.md`。安装位置由 doagent runtime 的 Skill root
配置决定；不要在 DoOps Gateway 中增加自定义扫描、匹配或拼装逻辑。

```yaml
---
name: labelu-release
description: 验证并协调 LabelU 发布目标
triggers:
  keywords: [LabelU, 发布, DeploymentPlan]
  regex: "LabelU.*发布|DeploymentPlan"
phase: [analyze, translate, cleanup]
priority: 100
max_tokens: 1200
requires: []
conflicts: []
---

# LabelU 发布

## 目标

将输入计划声明的 LabelU release 协调到目标环境，并返回可复查的运行状态证据。

## 输入边界

- 只接受计划中已经解析的 target、artifact、desired state 和 acceptance。
- 不根据项目名、域名、目录或历史环境补全目标。
- 未声明的凭据、环境和 mutation 授权都视为阻塞事实。

## 不变量

- 工作区与不可变 release identity 一致。
- 运行制品与计划引用一致。
- 未授权时不得改变真实环境。

## Agent 原生执行

- doagent 可以组合其他已安装 Skill、工具和多 Agent。
- 只能使用运行时实际暴露且当前权限允许的能力。
- 本 Skill 不规定固定命令序列、工具名称、子 Agent 数量或重试拓扑。

## 验收

- 只有全部 required evidence 都来自实际观察时才能报告 converged。
- 每条 evidence 必须引用产生该观察事实的真实 `toolCallId`；bridge 只接受匹配且已完成的观察调用，并注入对应工具摘要。
- 缺能力或权限时报告 blocked，并记录具体缺失事实。
- mutation 后验收失败时，只能使用环境明确声明的可逆恢复能力。

## 输出

返回调用契约要求的单个结构化对象，不返回 Markdown、命令清单或文本成功声明。
```

## 编写规则

1. 写目标和事实，不写“第一步、第二步”的固定命令序列。
2. 写允许和禁止的能力边界，不假设某个二进制或工具一定存在。
3. 写完成证据，不把 Agent 自报完成当证据。
4. 写 mutation 授权，不在 Skill 中自动批准权限。
5. 写恢复条件，不写死某个部署技术或历史版本。
6. 允许 doagent 自主使用多 Agent 和其他 Skill，不在 Skill 中设计子 Agent 框架。
7. 遇到真实问题直接 `blocked` 或 `failed`，不添加 fallback、静默降级或兼容分支。

## 脚本边界

Skill 可以引用运行时已经暴露的原子工具，但不应内嵌大段 shell。业务确实需要脚本时：

- 脚本只完成一个原子能力；
- 输入、输出、退出码和副作用必须明确；
- mutation 必须有显式授权，尽量支持 dry run 和幂等执行；
- 失败必须非零退出并保留根因；
- 不包含凭据、环境猜测、Skill 路由、多 Agent 调度或自动修复 loop；
- 是否调用该脚本仍由 doagent 根据现场能力和任务决定。

## 验证

- 检查 frontmatter 可被 doagent 原生 Skill 机制读取。
- 用一个成功场景和至少一个缺权限或缺能力场景验证行为。
- 确认输出证据来自真实工具观察。
- 确认 Skill 中没有具体环境坐标、凭据、固定命令序列和隐藏 fallback。
- CI/CD Skill 还必须通过 `test/test_semantic_cicd_contract.py`。
