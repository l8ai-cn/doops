---
name: k8s
description: Kubernetes 集群运维与部署领域知识
triggers:
  keywords: [k8s, kubernetes, kubectl, pod, deployment, service, namespace, daemonset, ingress, helm, rollout, 部署, 重启, 扩容, 缩容]
  regex: "kubectl\\s+|k8s|rollout|部署.*服务|重启.*服务"
phase: [analyze, translate, cleanup]
priority: 10
max_tokens: 800
requires: [shell]
conflicts: []
---

你具备深厚的 Kubernetes 运维知识。执行 K8s 任务时请遵循以下规范。

## 集群环境

| 项目 | 值 |
|------|-----|
| 发行版 | K3s |
| kubeconfig | `/etc/rancher/k3s/k3s.yaml`（需要 sudo） |
| 业务命名空间 | `oilan-system` |
| 实验命名空间 | `labs` |
| 镜像仓库 | `registry.example.com` |
| imagePullPolicy | `Always`（确保每次拉最新）|

## 核心原则

1. **Namespace 显式**：所有命令必须带 `-n {namespace}`，永远不依赖 default。
2. **诊断优先**：
   - Pod 状态异常 → `kubectl describe pod` + `kubectl logs --tail=50`
   - CrashLoopBackOff → 查看上一次日志 `kubectl logs --previous`
   - Pending → 检查 `Events` 中的调度失败原因
3. **补丁式更新**：优先 `kubectl apply` / `kubectl patch`，避免 delete+create 导致中断。
4. **滚动更新**：使用 `kubectl rollout restart deployment/{name}` + `kubectl rollout status` 确认完成。
5. **资源限制**：Requests 设小（如 100m CPU / 128Mi），Limits 合理（如 1 CPU / 1Gi）。

## 常用服务清单

| 服务名 | Deployment | 端口 | 镜像 |
|--------|-----------|------|------|
| Java 后端 | exam-training-api | 8081 | `registry.example.com/example-system/exam-training-api:latest` |
| Go 实验服务 | exam-lab-api | 23456 | `registry.example.com/example-system/exam-lab-api:latest` |
| Python 课程服务 | exam-course-api | 8083 | `registry.example.com/example-system/exam-course-api:latest` |
| 前端 | exam-frontend | 80 | `registry.example.com/example-system/exam-frontend:latest` |

## 部署流程模板

```
1. buildctl ... --output type=image,name={image},push=true,registry.insecure=true   # 确保镜像已推到仓库
2. sudo kubectl rollout restart deployment/{name} -n oilan-system --kubeconfig=/etc/rancher/k3s/k3s.yaml
3. sudo kubectl rollout status deployment/{name} -n oilan-system --kubeconfig=/etc/rancher/k3s/k3s.yaml --timeout=120s
4. sudo kubectl get pods -n oilan-system -l app={name} --kubeconfig=/etc/rancher/k3s/k3s.yaml   # 验证 Running
```

## 安全合规
- 不要在命令行明文传递 Secret
- 使用 `kubectl create secret` 管理敏感配置
- 生产环境禁止 `kubectl delete pod --force --grace-period=0`
