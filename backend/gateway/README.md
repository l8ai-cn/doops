# doops-gateway

`gateway/` 只是 `doops-gateway` 二进制的构建入口：`main.go` 共 11 行，调用
`agent/pkg/gatewaycmd`。实现代码在 `agent/internal/server/`，与 Agent 运行时共用一个包，
按文件前缀区分 Gateway 侧与 Agent 侧，见 [server/README.md](../agent/internal/server/README.md)。Gateway 接收用户 CLI / API 请求，维护内网 `doops-agent` 主动连上的 WebSocket，执行 RBAC、审计、排队和 `cluster/instance` 路由。

## 构建

```bash
cd gateway
go build -o ../bin/doops-gateway .
```

## 启动

```bash
./bin/doops-gateway serve \
  -db /var/lib/doops-gateway/gateway.db \
  -port 42222
```

## 本地反向代理验收

在发布 Gateway 前，可以启动真实 Gateway 和 Agent 进程，并用本地网站验证
`Gateway -> Agent 主动 WebSocket -> 内网站点`：

```bash
bash backend/scripts/smoke-proxy-tunnel.sh
```

脚本只使用临时 SQLite、临时端口和合成管理员数据；它会验证 GET 的 Host 路由、
查询参数、响应头、POST 请求体，以及 Agent 的 `/health/ready` 状态。测试结束后会
清理进程和临时目录。

## 发布

Gateway 是独立控制面包，必须从宿主机 SSH 上下文发布：

```bash
bash scripts/deploy-gateway.sh \
  --host <gateway-host> \
  --user <ssh-user> \
  --source-commit <verified-main-commit> \
  --verify-target <configured-doops-target> \
  --verify-session <session-name>
```

Gateway 本体不登记为 doops target，也不通过 doops target 发布。target 是
`doops-agent` 执行通道，可能处在容器/agent root 中，不能代表 gateway 服务的宿主机
root。也不要用 base64/tar/分片临时传输替代发布脚本；`doops push` 的故障排查按实际存储模式进行，见[构建与发布 SOP](../docs/BUILD_DEPLOY_SOP.md)。

`--verify-target` 只复用本机 DoOps CLI 已配置的认证来读取发布前后 target 基线，不导出
或落盘 user token。发布脚本不接受独立 token 文件。

### TLS reverse proxy

Production clients use `https://doops.l8ai.cn`. The versioned Nginx config is
`gateway/nginx/doops-proxy.conf`; it terminates the `*.l8ai.cn` certificate and
proxies HTTP, WebSocket, and Git HTTP traffic to `127.0.0.1:42222`.

Deploy from the gateway host SSH boundary:

```bash
bash scripts/deploy-gateway-tls-proxy.sh \
  --host <gateway-host> --user <ssh-user> --dry-run
bash scripts/deploy-gateway-tls-proxy.sh \
  --host <gateway-host> --user <ssh-user>
```

The script backs up `/opt/doops/nginx/default.conf`, validates the candidate
inside `doops-web-proxy`, reloads Nginx, and restores the backup if validation
or the HTTPS `401` probe fails. After deployment, verify authenticated target
listing returns `200` and run an active `exec` smoke through the TLS URL.

## 代码同步

`push/pull` 在 gateway 模式下也基于 Git HTTP：

```text
CLI Git -> /v1/git/<cluster>/<instance>/<session>.git -> gateway -> agent WS -> agent /git/<session>.git
```

Gateway 不保存源码包，也不再用 tar/base64 分块作为 `push/pull` 正式路径；它只做鉴权、审计和 Git HTTP 反向透传。

## 源码入口

构建入口是本目录；实现在 `agent/internal/server/`（文件索引见
[server/README.md](../agent/internal/server/README.md)）。

## 文档

- 用户侧 API 对接指南：[docs/DOOPS_API_GUIDE.md](../docs/DOOPS_API_GUIDE.md)
- Gateway 隧道和运维指南：[docs/DOOPS_GATEWAY_TUNNEL.md](../docs/DOOPS_GATEWAY_TUNNEL.md)
