# 业务项目如何编写和插拔 Skill 指南

Doops 的 AI 调度能力完全是由 `Skills` 驱动的。
业务项目（如 `exam-lab-api`、`training-hub` 等）如果希望将他们的构建、发布、故障排查经验自动化，**不需要修改 Doops 核心代码**，只需要提供 `.agent/skills/` 配置文件即可。

本文档将指导你如何从零编写并为一个业务项目插拔一项专属 Skill。

---

## 核心机制：Skill 的自动发现

当在含有 `.agent/skills/` 的工程目录（例如 `webide-gpu-workspace/` 的根目录）唤起 上位 Agent 时，它会自动扫描所有子目录并组装 Skill。
如果你希望为你的特定业务项目加一个部署能力，你只需要在现有的 `.agent/skills/` 下新建一个文件夹。

---

## 实例：为一个项目编写专有的发布 SOP

假设你要为 `labelU` 项目编写一个从代码合并到上线的标准化运维动作。

### 1. 创建目录
在工作区根目录执行：
```bash
mkdir -p .agent/skills/deploy-labelu
```

### 2. 编写 `SKILL.md`
在这个目录内创建 `SKILL.md`，它由**YAML Frontmatter**（元数据）和**Markdown Content**（行动指令）两部分组成。

```yaml
---
name: LabelU 专用发布流程
description: 处理 labelU 标注系统的自动化代码同步和 K8s 发布
triggers:
  keywords: ["发布 labelu", "更新标注系统", "deploy labelU"]
phase: translate
priority: 50
---

# LabelU 发布规范

当用户要求部署或更新 `labelU` 服务时，必须严格遵守以下动作序列执行。

## 第一性原理
1. 代码推流：必须使用 `doops push` 将本地源码送到沙盒 `/root/ws/$SESSION/`
2. 环境隔绝：禁止在宿主机使用 docker，所有的编译构建必须通过 `doops exec` 在 Agent 容器内部搞定。
3. 镜像构建：Agent 内部统一使用 `buildctl + buildkitd`，不要再用 `nerdctl build`。

## 标准工作流
```bash
# 1. 确保在正确的项目目录
cd platform-tools/labelU

# 2. 推送代码至目标计算节点 (默认 gpu-master 节点)
doops -session prod push --target gpu-master --src ./ --dest /root/ws/prod/labelu

# 3. 在 Agent 沙盒内用 BuildKit 执行远程构建
doops -session prod exec --target gpu-master --cmd \
  "cd /root/ws/prod/labelu && buildctl --addr unix:///run/buildkit/buildkitd.sock build \
     --progress=plain \
     --frontend dockerfile.v0 \
     --local context=. \
     --local dockerfile=. \
     --opt filename=k8s/Dockerfile \
     --opt platform=linux/amd64 \
     --output type=image,name=registry.example.com/library/labelu:latest,push=true,registry.insecure=true"

# 4. 执行远端重启
doops -session prod exec --target gpu-master --cmd \
  "kubectl rollout restart deployment/labelu -n ai"
```

## 异常处理红线（绝对不可修改的防线）
如果远端 K8s 响应超时、或者镜像构建发生依赖错误，**立即停止一切动作并使用 notify_user 寻求人类审核**。禁止 AI 擅自主张修改底层 Docker 配置。
```

*(注意在 markdown 文件的最开头必须闭合 yaml 围栏 `---`)*

---

## 3. 验证加载生效

只要这个 `.agent/skills/deploy-labelu/SKILL.md` 文件存在于项目目录的规则树内，下次在对话框里对 Agent 喊出：
> "帮我部署 LabelU 标注系统"

Agent 的智能核心就会匹配到关键词 `deploy labelU`，加载该 Skill 的全部内容作为系统提示词的上下文，然后根据你定义的工作流按部就班安全地执行构建逻辑。

---

## 最佳实践与建议

1. **内聚原则**：一个子业务的特性 SOP 就写在一个独立的 Skill 目录里，不要混杂。
2. **避免重复造轮子**：如果你只是希望用到现成的通用部署能力，优先依靠通用的 `deploy/SKILL.md`，没必要为每个项目写专有指引，除非它的部署逻辑很特殊。
3. **安全第一**：Skill 编写务必遵循 Doops 的沙盒纪律，所有远端操作都要在 `/root/ws/$SESSION/` 下收敛，绝对禁止引用旧的 SCP / SSH 手工交互手段。
