# doops-gateway

`gateway/` 是 doops 反向隧道模式的一等公网控制面入口。

它构建 `doops-gateway` 二进制。Gateway 接收用户 CLI / API 请求，维护内网 `doops-agent` 主动连上的 WebSocket，执行 RBAC、审计、排队和 `cluster/instance` 路由。

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

## 发布

Gateway 是独立控制面包，必须从宿主机 SSH 上下文发布：

```bash
bash scripts/deploy-gateway.sh --host <gateway-host> --user <ssh-user>
```

Gateway 本体不登记为 doops target，也不通过 doops target 发布。target 是
`doops-agent` 执行通道，可能处在容器/agent root 中，不能代表 gateway 服务的宿主机
root。也不要用 base64/tar/分片临时传输替代发布脚本；如果 `doops push` 卡住，应修复
Git HTTP 反向隧道或目标 agent 版本。

## 代码同步

`push/pull` 在 gateway 模式下也基于 Git HTTP：

```text
CLI Git -> /v1/git/<cluster>/<instance>/<session>.git -> gateway -> agent WS -> agent /git/<session>.git
```

Gateway 不保存源码包，也不再用 tar/base64 分块作为 `push/pull` 正式路径；它只做鉴权、审计和 Git HTTP 反向透传。

## 源码入口

旧路径 `agent/cmd/gateway` 仅保留为兼容 wrapper。新的部署、流水线和文档都应把本目录作为 gateway 权威入口。

## 文档

- 用户侧 API 对接指南：[docs/DOOPS_API_GUIDE.md](../docs/DOOPS_API_GUIDE.md)
- Gateway 隧道和运维指南：[docs/DOOPS_GATEWAY_TUNNEL.md](../docs/DOOPS_GATEWAY_TUNNEL.md)
