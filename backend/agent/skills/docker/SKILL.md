---
name: docker
description: Docker 容器运维领域知识
triggers:
  keywords: [docker, container, 容器, 镜像, registry, harbor]
  regex: "docker\\s+(build|run|push|pull|compose|logs|inspect|ps|stop|start|rm|exec)"
phase: [analyze, translate, cleanup]
priority: 10
max_tokens: 500
requires: [shell]
conflicts: []
---

你具备深厚的 Docker 运维知识。执行容器相关任务时请遵循以下规范。

## 操作规范

### 状态检查（操作前必做）
1. `docker ps -a | grep {name}` — 检查容器是否存在
2. `docker images | grep {name}` — 检查镜像是否已拉取
3. `docker inspect {container} --format '{{.State.Status}}'` — 获取运行状态

### 容器生命周期
- **启动**：优先 `docker run -d --name {name} --restart=unless-stopped`
- **更新**：`docker rm -f {name} 2>/dev/null || true` → `docker run ...`（先删后建，幂等）
- **日志**：`docker logs --tail 50 {name}` 或 `docker logs -f --since 5m {name}`
- **进入**：`docker exec -it {name} /bin/sh`（alpine 用 sh，debian 用 bash）

### 镜像管理
- **清理悬空镜像**：`docker image prune -f`
- **清理全部无用**：`docker system prune -f`（不包括 volume）
- **磁盘占用**：`docker system df` 查看各类资源占用

### 私有仓库
- 仓库地址：`registry.example.com`
- 命名规范：`registry.example.com/example-system/{service}:{tag}`
- 无需 `docker login`，仓库已配置 IP 白名单

### Dockerfile 最佳实践
1. 多阶段构建减小镜像体积
2. 合并 RUN 指令减少层数
3. 使用 `COPY` 而非 `ADD`（除非解压 tar）
4. `EXPOSE` 声明端口
5. 使用非 root 用户：`RUN adduser -D appuser && USER appuser`

### 网络
- 容器间通信：使用 `docker network create` + `--network` 替代 `--link`
- 端口映射：`-p 主机端口:容器端口`
- 如在 K8s 环境，使用 Service 而非 Docker 端口映射
