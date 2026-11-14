# 业务 Skill 编写与接入

Agent 原生发布职责边界见
[`NATURAL_LANGUAGE_DEPLOYMENT.md`](NATURAL_LANGUAGE_DEPLOYMENT.md)。

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
max_tokens: 1200
requires: []
conflicts: []
---

# LabelU 发布

## 目标

将调用方明确声明的 LabelU release 协调到指定 target，并返回可复查的运行状态证据。

## 输入边界

- 从当前任务及其指定、已授权的资产确定 target、artifact digest、desired state 和 acceptance。
- 输入缺项先读取当前任务指定且已授权的资产，复用已有事实，不要求用户重复粘贴。
- 不根据项目名、域名、目录或历史环境补全目标。
- 沿用已有凭据绑定、环境事实和 mutation 授权；经读取仍缺少必需事实或授权时，说明具体缺项。

## 不变量

- 工作区与不可变 release identity 一致。
- 运行制品与计划引用一致。
- 未授权时不得改变真实环境。

## Agent 原生执行

- doagent 可以组合其他已安装 Skill、工具和多 Agent。
- 只能使用运行时实际暴露且当前权限允许的能力。
- 失败先读取错误、相关配置和目标状态，定位根因；已有授权内可修复的问题继续修复并验证，
  反馈原因、处理和真实结果，不重复请求授权。
- 本 Skill 不规定固定命令序列、工具名称、子 Agent 数量或重试拓扑。

## 验收

- 只有全部 required evidence 都来自实际模块观察时才能报告 converged。
- 不把 admission 文本、通用 attestation 或 Agent 自报完成当作 evidence。
- 本层缺能力时，通过现有调用结果向上层交接 target/session、已查事实、错误、未完成事项及
  所需现有入口和非秘密参数；上层沿用已有授权执行并回传实际结果，不仅列命令让用户手工跑。
  必需事实、权限或执行能力确实无法取得时才报告 blocked，并记录具体缺项。
- mutation 后验收失败时，只能使用环境明确声明的可逆恢复能力。

## 输出

返回调用契约要求的单个结构化结果，不返回 Markdown、命令清单或文本成功声明。
```

## 编写规则

1. 写目标和事实，不写“第一步、第二步”的固定命令序列。
2. 写允许和禁止的能力边界，不假设某个二进制或工具一定存在。
3. 写完成证据，不把 Agent 自报完成当证据。
4. 写 mutation 授权，不在 Skill 中自动批准权限。
5. 写恢复条件，不写死某个部署技术或历史版本。
6. 允许 doagent 自主使用多 Agent 和其他 Skill，不在 Skill 中设计子 Agent 框架。
7. 遇错先诊断并在已有授权内修复、验证和反馈；能力缺口具体交接，不因首次错误直接
   `blocked` 或 `failed`，不添加 fallback、静默降级或兼容分支，不原样重试或掩盖原失败。

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
- 验证输入缺项能先读取已授权资产、可修复错误能继续处理，能力缺口能具体交接且不重复授权。
- 确认输出证据来自真实工具观察。
- 确认 Skill 中没有具体环境坐标、凭据、固定命令序列和隐藏 fallback。
