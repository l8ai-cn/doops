# Doops WebUI

`doops-webui` 是一个轻量 Web 控制台：在浏览器里连接 `doops-gateway`，列出在线
`cluster/instance`，并对选中的实例发起 `ask` 自然语言任务，实时看到 doagent 的
流式执行过程。

## 为什么需要一个后端

浏览器原生 `WebSocket` 无法设置 `Authorization` 请求头，而 gateway 的 `/v1/rpc`
只认 `Bearer token`。`doops-webui` 在服务端注入 token 并把浏览器 WS 双向桥接到
gateway，因此不需要改动 gateway 任何代码。

```
浏览器 SPA ──HTTP/WS──▶ doops-webui ──HTTP/WS(+Bearer)──▶ doops-gateway ──▶ doops-agent
```

## 端点

- `GET /` 内置静态前端（go:embed）
- `POST /api/login` 透传到 gateway `POST /v1/auth/login`，返回 user token
- `GET /api/targets?gateway=<url>`（带 `Authorization: Bearer`）透传 `GET /v1/targets`
- `GET /api/rpc?gateway=<url>&cluster=&instance=&token=`（WS）桥接到 gateway `/v1/rpc`

token 只保存在浏览器内存里；后端不落盘、不缓存密码。

## 构建

```bash
bash scripts/build-webui.sh
# 或本机直接跑：
cd agent && go run ./cmd/webui -port 8088
```

交叉编译同 gateway，用 `GOOS`/`GOARCH` 环境变量控制。

## 运行

```bash
./bin/doops-webui -port 8088 -gateway http://203.0.113.10:42222
```

参数：

- `-port`：监听端口，默认 `8088`
- `-gateway`：可选，预填到前端输入框的默认网关地址（仍可在页面修改）
- `-static`：可选，从指定目录加载前端资源（前端开发时用），默认用内置 embed

打开 `http://localhost:8088`：

1. 填 Gateway 地址，用账号密码登录或直接粘贴 user token，点「连接」。
2. 在左侧选择一个在线实例（绿点空闲 / 黄点活动 / 红点忙）。
3. 在右侧输入运维意图，点「发送 ask」（也可 Cmd/Ctrl+Enter），实时查看
   step / tool / AI 文本 / 完成等事件流。

## 部署建议

- 生产环境建议把 `doops-webui` 与 gateway 放在同机或内网，并在前面用 TLS 反代。
- WS 升级放开了 `CheckOrigin`，公网暴露时应由反向代理/防火墙限制来源。
- 每个浏览器会话使用独立 `session_id`（`webui-<rand>`），与 gateway 的串行/排队
  语义一致。
