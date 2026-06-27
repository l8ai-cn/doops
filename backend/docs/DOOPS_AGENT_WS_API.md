# doops-agent WebSocket API 协议指南

本文档说明如何通过 WebSocket API 直接与 `doops-agent` 进行交互。

## 1. 基础概念

doops-agent 采用 **MCP (Model Context Protocol)** 风格的 JSON-RPC 消息，通过 **WebSocket** 实现双向通信。

-   **鉴权**: WebSocket 握手必须包含 `X-Doops-Key` Header，其值为 `agent` 启动时配置的 token。
-   **通信模型**: 客户端连接 `/ws` 后，在同一条 WebSocket 连接上发送 JSON-RPC 请求并接收流式响应。

## 2. 交互流程

### 步骤 1: 建立 WebSocket 连接
连接到 `ws://[node-ip]:42222/ws`。

**请求示例**:
```bash
websocat -H 'X-Doops-Key: your-token' ws://127.0.0.1:42222/ws
```

### 步骤 2: 发送 JSON-RPC 请求
向 WebSocket 连接发送 JSON-RPC 请求。

**通用格式**:
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "工具名称",
    "arguments": { ... }
  }
}
```

### 步骤 3: 接收响应
响应会通过同一条 **WebSocket** 连接回传。

-   **中间状态**: `method: "notifications/message"` (用于 PTY 增量输出流)。
-   **最终结果**: `id: [请求ID]` (包含执行结果文本)。

## 3. 核心 API 工具

### 3.1 提交自然语言任务 (AI 自动运维)
这是最强大的接口，会自动进行：分析 -> 翻译命令 -> 执行 -> 验证结果。

-   **工具名**: `doops_agent_prompt`
-   **参数**:
    -   `instruction`: (string) 自然语言指令。
    -   `session_id`: (string, 可选) 维持 PTY 状态的 ID。

**示例**:
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "doops_agent_prompt",
    "arguments": {
      "instruction": "检查 /var/log/nginx 下是否有 error 关键字，并统计行数"
    }
  }
}
```

### 3.2 直接执行 Shell 命令
-   **工具名**: `doops_shell`
-   **参数**:
    -   `command`: (string) 要执行的命令。
    -   `session_id`: (string, 可选) 默认 "default"。

### 3.3 文件操作
-   **写入**: `doops_file_write` (path, content, session_id)
-   **查看小文本**: `doops_file_read` (path, session_id)
-   **拉取工作区**: `doops pull`

`doops_file_read` 只用于查看配置、脚本、日志片段等小文本文件。大文件、压缩包、图片、数据库导出等二进制资产不能通过 `read` 回收，避免把 WebSocket 文本响应误用成下载通道。
大文件和二进制资产应使用 `doops pull`，其底层基于远端工作区 Git 快照和 Git HTTP。

### 3.4 仅翻译指令 (不执行)
-   **工具名**: `doops_shell_gen`
-   **参数**: `instruction`

## 4. 示例代码 (Node.js WebSocket)

```javascript
import WebSocket from 'ws';

const ws = new WebSocket('ws://ip:42222/ws', {
  headers: { 'X-Doops-Key': 'your-token' },
});

ws.on('open', () => {
  ws.send(JSON.stringify({
    jsonrpc: '2.0',
    id: 'task-1',
    method: 'tools/call',
    params: {
      name: 'doops_agent_prompt',
      arguments: { instruction: 'uptime' },
    },
  }));
});

ws.on('message', (data) => {
  const msg = JSON.parse(data.toString());
  console.log(msg);
});
```
