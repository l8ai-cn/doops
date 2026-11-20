"use client"

import type { Target, AuditEvent } from "./gateway"
import type { RpcEvent } from "./client"
import type {
  AdminUser,
  AdminGrant,
  AdminToken,
  AdminInstance,
  ProxyRoute,
  ProxyRouteBody,
} from "./admin"

// 演示模式：内置 mock 数据 + 模拟流式输出，无需连接真实 gateway

export const DEMO_TOKEN = "demo-local-token"

export const DEMO_TARGETS: Target[] = [
  {
    cluster: "prod-cn",
    instance: "web-01",
    key: "prod-cn/web-01",
    remote: "10.0.1.11:51820",
    status: "active",
    busy: false,
    active_ops: 1,
    queued_ops: 0,
    connected_at: new Date(Date.now() - 3600_000 * 6).toISOString(),
    last_seen: new Date().toISOString(),
    resources: ["nginx", "doops-app", "redis"],
    sessions: ["console-a1b2c3d4"],
  },
  {
    cluster: "prod-cn",
    instance: "web-02",
    key: "prod-cn/web-02",
    remote: "10.0.1.12:51820",
    status: "busy",
    busy: true,
    active_ops: 2,
    queued_ops: 1,
    connected_at: new Date(Date.now() - 3600_000 * 6).toISOString(),
    last_seen: new Date().toISOString(),
    resources: ["nginx", "doops-app"],
    sessions: ["console-aa11bb22", "deploy-zz99"],
  },
  {
    cluster: "staging",
    instance: "db-01",
    key: "staging/db-01",
    remote: "10.0.2.21:51820",
    status: "idle",
    busy: false,
    active_ops: 0,
    queued_ops: 0,
    connected_at: new Date(Date.now() - 3600_000 * 24).toISOString(),
    last_seen: new Date().toISOString(),
    resources: ["postgres-16", "pgbouncer"],
    sessions: [],
  },
]

export const DEMO_AUDIT: AuditEvent[] = [
  {
    id: 1042,
    user_id: "gaojiaqi",
    cluster: "prod-cn",
    instance: "web-01",
    action: "exec",
    session: "console-a1b2c3d4",
    command_summary: "systemctl status doops-app",
    status: "success",
    bytes_in: 28,
    bytes_out: 642,
    started_at: new Date(Date.now() - 600_000).toISOString(),
    ended_at: new Date(Date.now() - 599_000).toISOString(),
  },
  {
    id: 1041,
    user_id: "gaojiaqi",
    cluster: "prod-cn",
    instance: "web-02",
    action: "ask",
    session: "deploy-zz99",
    command_summary: "部署最新版本到 web-02 并滚动重启",
    status: "success",
    bytes_in: 64,
    bytes_out: 2048,
    started_at: new Date(Date.now() - 5_400_000).toISOString(),
    ended_at: new Date(Date.now() - 5_280_000).toISOString(),
  },
  {
    id: 1039,
    user_id: "ops-bot",
    cluster: "staging",
    instance: "db-01",
    action: "write",
    session: "console-77aa",
    command_summary: "写入 /etc/pgbouncer/pgbouncer.ini",
    status: "success",
    bytes_in: 512,
    bytes_out: 12,
    started_at: new Date(Date.now() - 86_400_000).toISOString(),
    ended_at: new Date(Date.now() - 86_399_000).toISOString(),
  },
]

// 演示文件系统：目录 -> 条目(目录以 / 结尾)；文件 -> 内容
const DEMO_SETTINGS = `{
  "provider": "openai-compatible",
  "model": "demo/ops-assistant",
  "base_url": "https://api.example.com/v1",
  "api_key": "sk-demo-3f9a2b7c8d1e4f5061728394a5b6c7d8",
  "temperature": 0.2,
  "max_tokens": 4096,
    "models": [
    "demo/ops-assistant"
  ]
}
`

const DEMO_DIRS: Record<string, { name: string; dir: boolean }[]> = {
  "/root/ws": [{ name: "console-a1b2c3d4", dir: true }],
  "/root/ws/console-a1b2c3d4": [
    { name: "releases", dir: true },
    { name: "logs", dir: true },
    { name: "deploy.sh", dir: false },
    { name: "config.yaml", dir: false },
    { name: "doops-report.md", dir: false },
  ],
  "/root/ws/console-a1b2c3d4/releases": [
    { name: "1.8.2", dir: true },
    { name: "1.8.3", dir: true },
  ],
  "/root/ws/console-a1b2c3d4/logs": [
    { name: "app.log", dir: false },
    { name: "access.log", dir: false },
  ],
  "/root/.agent": [
    { name: "skills", dir: true },
    { name: "settings.json", dir: false },
  ],
  "/root/kb": [
    { name: "运维手册.md", dir: false },
    { name: "部署流程.md", dir: false },
    { name: "故障排查.md", dir: false },
  ],
  "/root": [
    { name: "ws", dir: true },
    { name: ".agent", dir: true },
    { name: "kb", dir: true },
  ],
}

// 知识库演示文档内容
const DEMO_KB: Record<string, string> = {
  "/root/kb/运维手册.md": `# 运维手册

> 本手册由团队共同维护，记录节点的日常运维约定。

## 服务清单

| 服务 | 说明 | 端口 |
| --- | --- | --- |
| \`doops-app\` | 主应用 | 8080 |
| \`nginx\` | 反向代理 | 80 / 443 |
| \`redis\` | 缓存 | 6379 |

## 常用命令

\`\`\`bash
systemctl status doops-app
journalctl -u doops-app -n 100 --no-pager
\`\`\`

## 值班约定

1. 告警优先在群里 **认领**，避免重复处理。
2. 生产变更必须先在 staging 验证。
3. 任何回滚都要在本知识库登记。
`,
  "/root/kb/部署流程.md": `# 标准部署流程

## 1. 准备

- 确认目标分支已合并并通过 CI
- 核对发布版本号

## 2. 构建与发布

\`\`\`bash
ln -sfn releases/1.8.3 current
nginx -s reload
\`\`\`

## 3. 验证

- 健康检查 \`/healthz\` 返回 **200**
- 观察 5 分钟错误日志

> 如有异常，立即按《故障排查》回滚。
`,
  "/root/kb/故障排查.md": `# 故障排查

## 健康检查失败

1. 查看应用日志：\`journalctl -u doops-app -p err\`
2. 确认依赖服务（redis / 数据库）可用
3. 必要时回滚到上一个稳定版本

## 磁盘告警

- 清理历史发布包，保留最近 3 个版本
- 压缩并归档旧日志
`,
}

function demoFileContent(path: string): string {
  if (DEMO_KB[path]) return DEMO_KB[path]
  if (path.includes("settings.json")) return DEMO_SETTINGS
  if (path.endsWith("deploy.sh"))
    return "#!/usr/bin/env bash\nset -euo pipefail\nIMAGE=doops/app:${1:-latest}\nbuildctl build --frontend dockerfile.v0 --local context=. --output type=image,name=$IMAGE\nkubectl set image deployment/app app=$IMAGE\nkubectl rollout status deployment/app\n"
  if (path.endsWith("doops-report.md"))
    return "# 部署报告\n\n- 镜像: doops/app:1.8.3\n- 滚动更新: 成功\n- 健康检查: /healthz 200\n- 耗时: 48s\n"
  if (path.endsWith(".md"))
    return "# 工作区记录\n\n这是演示数据，没有连接真实节点。\n"
  if (path.endsWith(".yaml"))
    return "server:\n  port: 8080\n  workers: 4\nlog:\n  level: info\n"
  if (path.endsWith(".log"))
    return "2026-06-27T09:14:32Z INFO  server started on :8080\n2026-06-27T09:14:33Z INFO  connected to redis\n2026-06-27T09:15:01Z INFO  GET /healthz 200 1ms\n"
  return `# ${path}\n(演示文件内容)\n`
}

function demoListing(dir: string): string[] {
  const key = dir.replace(/\/+$/, "") || "/"
  let entries = DEMO_DIRS[key]
  // 任意 /root/ws/<session> 工作区都返回标准结构
  if (!entries) {
    if (/^\/root\/ws\/[^/]+$/.test(key)) entries = DEMO_DIRS["/root/ws/console-a1b2c3d4"]
    else if (/^\/root\/ws\/[^/]+\/releases$/.test(key))
      entries = DEMO_DIRS["/root/ws/console-a1b2c3d4/releases"]
    else if (/^\/root\/ws\/[^/]+\/logs$/.test(key))
      entries = DEMO_DIRS["/root/ws/console-a1b2c3d4/logs"]
  }
  if (!entries) return [`ls: 无法访问 '${dir}': 没有那个文件或目录\n`]
  return entries.map((e) => `${e.name}${e.dir ? "/" : ""}\n`)
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

// 演示集群里的 Deployment 及其主容器镜像，供「检查部署」比对。
const DEMO_DEPLOYMENTS: Record<string, string> = {
  "apps/api": "registry.example.com/doops/api:1.8.2",
  "apps/web": "registry.example.com/doops/web:1.8.2",
}

// 与 Agent 的 imageRefMatches 一致：完全相同，或一方是另一方去掉 :tag / @digest 的前缀。
function demoImageMatches(want: string, got: string) {
  return (
    want === got ||
    got.startsWith(`${want}:`) ||
    got.startsWith(`${want}@`) ||
    want.startsWith(`${got}:`) ||
    want.startsWith(`${got}@`)
  )
}

function demoToolError(onEvent: (ev: RpcEvent) => void, message: string): never {
  onEvent({ type: "error", error: message })
  onEvent({ type: "done" })
  throw new Error(message)
}

async function emitChunks(
  chunks: string[],
  onEvent: (ev: RpcEvent) => void,
  signal?: AbortSignal,
  delay = 220,
) {
  onEvent({ type: "open" })
  for (const c of chunks) {
    signal?.throwIfAborted()
    await sleep(delay)
    signal?.throwIfAborted()
    onEvent({ type: "output", data: c })
  }
}

// 根据 shell 命令产出模拟输出
function shellOutput(cmd: string): string[] {
  const c = cmd.trim()
  // 文件浏览器目录列举：ls -1Ap <path>（允许前置 mkdir -p ...; 等复合命令）
  const lsMatch = c.match(/(?:^|;\s*)ls\s+-1Ap\s+(?:"([^"]+)"|'([^']+)'|(\S+))/)
  if (lsMatch) {
    const dir = lsMatch[1] || lsMatch[2] || lsMatch[3] || "/"
    return demoListing(dir)
  }
  if (/^set -eu\ncommand -v buildctl/.test(c)) {
    return [
      "#1 [internal] load build definition from Dockerfile\n",
      "#5 exporting to image\n",
      "#5 exporting manifest sha256:" + "4f".repeat(32) + " done\n",
      "(演示模式) 未在真实目标上构建镜像。\n",
    ]
  }
  if (/^set -eu\ncommand -v timeout/.test(c) && /skopeo/.test(c)) {
    const synced = [...c.matchAll(/echo "([^"\s]+) manifest_digest=/g)].map((m) => m[1])
    const lines = ["Getting image source signatures\n", "Copying blob sha256:" + "1a".repeat(32) + " done\n", "Writing manifest to image destination\n"]
    if (synced.length) for (const ref of synced) lines.push(`${ref} manifest_digest=sha256:${"9e".repeat(32)}\n`)
    else if (/manifest_digest=/.test(c)) lines.push(`manifest_digest=sha256:${"9e".repeat(32)}\n`)
    lines.push("(演示模式) 未在真实 Registry 上复制镜像。\n")
    return lines
  }
  if (/find .* -printf/.test(c)) {
    return ["d\tartifacts\n", "f\tREADME.md\n", "f\toutput.md\n"]
  }
  if (/git -C .* rev-parse --is-inside-work-tree/.test(c)) {
    return ["git\tready\n", " M output.md\n", "?? artifacts/result.md\n"]
  }
  if (/git -C .* show HEAD:/.test(c)) {
    return ["# Previous result\n\nThe previous workspace result is available here.\n"]
  }
  if (/git -C .* rev-parse HEAD/.test(c)) {
    return ["branch\tmain\n", "head\t0123456789abcdef0123456789abcdef01234567\n"]
  }
  if (/git -C .* log -n/.test(c)) {
    return ["0123456789abcdef0123456789abcdef01234567\t2026-10-11T08:00:00Z\tdoops\tCreate workspace result\n"]
  }
  if (/grep -RFil/.test(c)) {
    const root = c.match(/\/root\/ws\/[A-Za-z0-9._-]+/)?.[0] || "/root/ws/demo-session"
    return [`${root}/output.md\n`, `${root}/artifacts/result.md\n`]
  }
  if (/uptime|^top|load/.test(c))
    return [" 09:14:32 up 6 days,  3:21,  2 users,  load average: 0.18, 0.12, 0.09\n"]
  if (/df(\s|$)/.test(c))
    return [
      "Filesystem      Size  Used Avail Use% Mounted on\n",
      "/dev/vda1        80G   28G   49G  37% /\n",
      "/dev/vdb1       200G   96G  104G  48% /data\n",
    ]
  if (/free|mem/.test(c))
    return [
      "               total        used        free      shared  buff/cache   available\n",
      "Mem:           7.8Gi       2.1Gi       1.2Gi       180Mi       4.5Gi       5.3Gi\n",
    ]
  if (/systemctl status|service/.test(c))
    return [
      "● doops-app.service - DoOps Application\n",
      "   Loaded: loaded (/etc/systemd/system/doops-app.service; enabled)\n",
      "   Active: active (running) since Mon 2026-06-22 03:11:40 UTC; 5 days ago\n",
      " Main PID: 1287 (doops-app)\n",
      "   Memory: 184.2M\n",
    ]
  if (/docker ps|docker container/.test(c))
    return [
      "CONTAINER ID   IMAGE                 STATUS         PORTS                  NAMES\n",
      "a1b2c3d4e5f6   doops/app:1.8.2       Up 5 days      0.0.0.0:8080->8080/tcp app\n",
      "f6e5d4c3b2a1   nginx:1.27            Up 5 days      0.0.0.0:80->80/tcp     nginx\n",
    ]
  if (/ls(\s|$)/.test(c))
    return ["bin  conf  data  logs  releases  current -> releases/1.8.2\n"]
  if (/whoami/.test(c)) return ["doops\n"]
  if (/uname/.test(c)) return ["Linux web-01 6.1.0-21-amd64 #1 SMP x86_64 GNU/Linux\n"]
  return [`$ ${c}\n`, "(演示模式) 命令已在目标节点执行，返回示例输出。\n"]
}

// 演示 doops_k8s：按 operation 返回示例输出，不触达真实集群。
function k8sOutput(args: Record<string, unknown>): string[] {
  const ns = String(args.namespace || "apps")
  const resource = String(args.resource || "deployment/api")
  const name = String(args.name || "")
  switch (args.operation) {
    case "get":
      if (args.output === "json") return [demoK8sResourceJson(String(args.kind), String(args.namespace || ""))]
      if (/^nodes?$/.test(String(args.kind)))
        return [
          "NAME          STATUS   ROLES           AGE   VERSION\n",
          "node-a       Ready    control-plane   92d   v1.30.4\n",
          "node-b       Ready    <none>          92d   v1.30.4\n",
        ]
      return [
        "NAME                   READY   STATUS    RESTARTS   AGE\n",
        "api-7c9d8f6b5d-2xk4p   1/1     Running   0          3d\n",
        "api-7c9d8f6b5d-q8mzn   1/1     Running   1          3d\n",
        "worker-0               1/1     Running   0          5d\n",
      ]
    case "describe":
      return [`Name:         ${name}\n`, `Namespace:    ${ns}\n`, "Status:       Running\n", "Events:       <none>\n"]
    case "logs":
      return [
        `[演示] ${resource} 最近日志\n`,
        "2026-11-01T08:00:01Z INFO  server listening on :8080\n",
        "2026-11-01T08:00:05Z INFO  GET /healthz 200 1ms\n",
      ]
    case "events":
      return [
        "LAST SEEN   TYPE     REASON              OBJECT                     MESSAGE\n",
        "2m          Normal   ScalingReplicaSet   deployment/api             Scaled up replica set api-7c9d8f6b5d to 2\n",
      ]
    case "top":
      return args.kind === "nodes"
        ? ["NAME     CPU(cores)   CPU%   MEMORY(bytes)   MEMORY%\n", "node-a   412m         10%    5120Mi          32%\n"]
        : ["NAME                   CPU(cores)   MEMORY(bytes)\n", "api-7c9d8f6b5d-2xk4p   12m          96Mi\n"]
    case "rollout-status":
      return [`deployment "${resource.split("/")[1]}" successfully rolled out\n`]
    case "configmap-keys":
      return [
        "NAMESPACE   NAME            KEYS\n",
        `${ns}        api-config      LOG_LEVEL,app.yaml\n`,
        `${ns}        orders-config   <none>\n`,
      ]
    case "helm-list":
      return [JSON.stringify([{ name: "api", namespace: ns, revision: "7", status: "deployed", chart: "api-1.8.2", app_version: "1.8.2" }]) + "\n"]
    case "helm-history":
      return [JSON.stringify([{ revision: 6, status: "superseded", chart: "api-1.8.1" }, { revision: 7, status: "deployed", chart: "api-1.8.2" }]) + "\n"]
    case "helm-status":
      return [JSON.stringify({ name, namespace: ns, info: { status: "deployed" }, version: 7 }) + "\n"]
    case "helm-values":
      return ["image.repository\n", "image.tag\n", "replicaCount\n", "resources.limits.cpu\n"]
    case "rollout-restart":
      return [`[演示] ${resource} restarted（未操作真实集群）\n`]
    case "scale":
      return [`[演示] ${resource} scaled to ${args.replicas}（未操作真实集群）\n`]
    case "cordon":
    case "uncordon":
      return [`[演示] node/${name} ${args.operation}ed（未操作真实集群）\n`]
    case "delete-pod":
      return [`[演示] pod "${name}" deleted（未操作真实集群）\n`]
    default:
      return ["(演示模式) 已执行\n"]
  }
}

function demoK8sResourceJson(kind: string, namespace: string): string {
  type DemoResource = { metadata: { name: string; namespace?: string; creationTimestamp: string }; spec?: object; status: object }
  const resources: Record<string, DemoResource[]> = {
    nodes: [
      { metadata: { name: "node-a", creationTimestamp: "2026-08-19T08:00:00Z" }, status: { capacity: { cpu: "8", memory: "32Gi" }, conditions: [{ type: "Ready", status: "True" }] } },
      { metadata: { name: "node-b", creationTimestamp: "2026-08-19T08:00:00Z" }, status: { conditions: [{ type: "Ready", status: "True" }] } },
    ],
    namespaces: [
      { metadata: { name: "apps", creationTimestamp: "2026-09-01T08:00:00Z" }, status: { phase: "Active" } },
      { metadata: { name: "agents", creationTimestamp: "2026-09-01T08:00:00Z" }, status: { phase: "Active" } },
      { metadata: { name: "monitoring", creationTimestamp: "2026-09-01T08:00:00Z" }, status: { phase: "Active" } },
    ],
    deployments: [
      { metadata: { name: "api", namespace: "apps", creationTimestamp: "2026-10-29T08:00:00Z" }, spec: { replicas: 2 }, status: { readyReplicas: 2, availableReplicas: 2 } },
      { metadata: { name: "worker", namespace: "apps", creationTimestamp: "2026-10-27T08:00:00Z" }, spec: { replicas: 1 }, status: { readyReplicas: 1, availableReplicas: 1 } },
      { metadata: { name: "doops-agent", namespace: "agents", creationTimestamp: "2026-10-25T08:00:00Z" }, spec: { replicas: 2 }, status: { readyReplicas: 1, availableReplicas: 1 } },
    ],
    pods: [
      { metadata: { name: "api-7c9d8f6b5d-2xk4p", namespace: "apps", creationTimestamp: "2026-11-16T08:00:00Z" }, status: { phase: "Running", containerStatuses: [{ ready: true }] } },
      { metadata: { name: "api-7c9d8f6b5d-q8mzn", namespace: "apps", creationTimestamp: "2026-11-16T08:00:00Z" }, status: { phase: "Running", containerStatuses: [{ ready: true }] } },
      { metadata: { name: "worker-0", namespace: "apps", creationTimestamp: "2026-11-14T08:00:00Z" }, status: { phase: "Running", containerStatuses: [{ ready: true }] } },
      { metadata: { name: "doops-agent-0", namespace: "agents", creationTimestamp: "2026-11-18T08:00:00Z" }, status: { phase: "Pending", containerStatuses: [{ ready: false }] } },
    ],
  }
  const items = resources[kind] || []
  const scoped = namespace && (kind === "pods" || kind === "deployments")
  return JSON.stringify({ apiVersion: "v1", kind: "List", items: scoped ? items.filter((item) => item.metadata.namespace === namespace) : items }) + "\n"
}

export function demoAssistantReply(prompt: string): string {
  const p = prompt.toLowerCase()
  if (/状态|检查|巡检|status|健康|检查一下/.test(p)) {
    return `## 巡检结果：节点健康 ✅

**总体状态**：负载正常、核心服务全部存活、无错误日志。以下是演示数据，没有连接真实节点。

| 检查项 | 结果 | 说明 |
| --- | --- | --- |
| 系统负载 | 正常 | load 0.18，低于告警阈值 |
| 服务存活 | 正常 | \`doops-app\` / \`nginx\` / \`redis\` 均 active |
| 磁盘使用 | 正常 | 根分区 37% |
| 错误日志 | 无 | 近 1 小时无 error 级日志 |

**建议**：无需处理，保持当前监控频率即可。`
  }
  if (/回滚|rollback|上一个版本|恢复/.test(p)) {
    return `## 回滚完成 ✅

已将 \`current\` 切回上一个稳定版本 **releases/1.8.2**。这是演示数据，没有改动真实服务。

**执行步骤**

1. 定位上一个稳定版本 \`releases/1.8.2\`
2. 切换软链并重载服务：

\`\`\`bash
ln -sfn releases/1.8.2 current
systemctl reload doops-app
\`\`\`

3. 健康检查 \`/healthz\` 返回 **200 OK**`
  }
  return `## 部署完成 ✅

已将 **doops-app** 滚动升级到 \`1.8.3\`。这是演示数据，没有改动真实集群。

**变更摘要**

- 新增发布：\`releases/1.8.3\`
- \`current\` 软链已指向新版本
- \`nginx\` 平滑重载，连接无中断
- 健康检查 \`/healthz\` → **200 OK**

完整报告已写入 \`doops-report.md\`。`
}

export function demoApplyReply(): string {
  return "演示发布完成。这是本地假数据，没有改动真实集群。真实发布以 Helm release 回执为准，业务验收按任务执行。"
}

export async function demoCallTool(
  opts: { cluster?: string; instance?: string; tool: string; arguments: Record<string, unknown>; signal?: AbortSignal },
  onEvent: (ev: RpcEvent) => void,
): Promise<void> {
  const { tool, arguments: args, signal } = opts

  if (tool === "doops_shell") {
    const cmd = String(args.command || args.cmd || "")
    await emitChunks(shellOutput(cmd), onEvent, signal, 160)
    onEvent({ type: "result", result: { content: [{ type: "text", text: "exit code: 0" }] } })
    onEvent({ type: "done" })
    return
  }

  if (tool === "doops_web" && args.operation === "site-list") {
    const sites = [
      { site_id: "portal", domain: "portal.example.test", current_release: "demo-2", previous_release: "demo-1", updated_at: "2026-11-18T09:45:00Z" },
      { site_id: "docs", domain: "docs.example.test", current_release: "demo-1", previous_release: null, updated_at: "2026-11-17T08:10:00Z" },
    ]
    onEvent({ type: "result", result: { content: [{ type: "text", text: JSON.stringify({ sites }) }] } })
    onEvent({ type: "done" })
    return
  }

  if (tool === "doops_database_inventory") {
    const { cluster, instance } = opts
    if (!cluster || !instance) throw new Error("数据库发现需要指定目标")
    const candidates = [
      { service: "orders-postgres", engine: "postgres", port: 5432, database_name: "orders" },
      { service: "billing-mysql", engine: "mysql", port: 3306, database_name: "billing" },
      { service: "events-mongodb", engine: "mongodb", port: 27017 },
    ].map((row) => ({ ...row, cluster, instance, namespace: "data", source: "StatefulSet", host: `${row.service}.data.svc`,
      database_asset_id: `dbscan_${cluster}_${instance}_data_${row.service}_${row.engine}`,
      status: row.database_name ? "candidate" : "needs_database_name_or_credential" }))
    onEvent({ type: "result", result: { content: [{ type: "text", text: JSON.stringify({ status: "discovered", candidates }) }] } })
    onEvent({ type: "done" })
    return
  }

  if (tool === "doops_k8s") {
    await emitChunks(k8sOutput(args), onEvent, signal, 120)
    onEvent({ type: "result", result: { content: [{ type: "text", text: "Kubernetes operation complete." }] } })
    onEvent({ type: "done" })
    return
  }

  if (tool === "doops_agent_prompt") {
    if (args.mode === "metadata") {
      onEvent({ type: "result", result: { content: [{ type: "text", text: JSON.stringify({ status: "connected", model: JSON.parse(DEMO_SETTINGS).model }) }] } })
      onEvent({ type: "done" })
      return
    }
    if (args.operation === "apply") {
      await emitChunks(["[演示] 收到发布授权；未操作真实目标。\n", "[演示工具] mcp_doops_helm upgrade-install\n"], onEvent, signal, 200)
      onEvent({ type: "output", kind: "assistant_delta", data: demoApplyReply() })
      onEvent({ type: "result", result: { content: [{ type: "text", text: "Operation complete." }] } })
      onEvent({ type: "done" })
      return
    }
    const prompt = String(args.instruction || args.prompt || args.task || "")
    const p = prompt.toLowerCase()
    let chunks: string[]
    const summary = demoAssistantReply(prompt)

    if (/状态|检查|巡检|status|健康|检查一下/.test(p)) {
      chunks = [
        `[plan] 收到任务：${prompt}\n`,
        "[step 1/3] 采集节点负载与服务状态…\n",
        "[tool] doops_shell: uptime && systemctl is-active doops-app nginx redis\n",
        "[observe] load 0.18 / 服务全部 active\n",
        "[step 2/3] 检查磁盘与近一小时错误日志…\n",
        "[tool] doops_shell: df -h / && journalctl -u doops-app --since '1 hour ago' -p err\n",
        "[observe] 磁盘 37%，无 error 级日志\n",
        "[done] 巡检完成，节点健康。\n",
      ]
    } else if (/回滚|rollback|上一个版本|恢复/.test(p)) {
      chunks = [
        `[plan] 收到任务：${prompt}\n`,
        "[step 1/2] 定位上一个稳定版本 releases/1.8.2…\n",
        "[tool] doops_shell: ln -sfn releases/1.8.2 current && systemctl reload doops-app\n",
        "[step 2/2] 健康检查 /healthz…\n",
        "[done] 已回滚至 1.8.2，服务恢复正常。\n",
      ]
    } else {
      chunks = [
        `[plan] 收到任务：${prompt}\n`,
        "[step 1/4] 检查当前部署状态与磁盘空间…\n",
        "[tool] doops_shell: df -h / && systemctl is-active doops-app\n",
        "[step 2/4] 拉取最新发布包到 releases/1.8.3…\n",
        "[step 3/4] 切换 current 软链并平滑重载 nginx…\n",
        "[tool] doops_shell: ln -sfn releases/1.8.3 current && nginx -s reload\n",
        "[step 4/4] 健康检查 /healthz 返回 200，部署完成。\n",
        "[done] 已将 doops-app 滚动升级到 1.8.3，无停机。\n",
      ]
    }

    await emitChunks(chunks, onEvent, signal, 340)
    onEvent({ type: "output", kind: "assistant_delta", data: summary })
    onEvent({ type: "result", result: { content: [{ type: "text", text: "Operation complete." }] } })
    onEvent({ type: "done" })
    return
  }

  if (tool === "doops_file_read") {
    const path = String(args.path || "")
    onEvent({ type: "open" })
    await sleep(200)
    onEvent({
      type: "result",
      result: { content: [{ type: "text", text: demoFileContent(path) }] },
    })
    onEvent({ type: "done" })
    return
  }

  if (tool === "doops_workspace_read") {
    const operation = String(args.operation || "")
    const path = String(args.path || "")
    const result = operation === "list"
      ? { entries: [{ name: "artifacts", path: "artifacts", type: "directory" }, { name: "README.md", path: "README.md", type: "file" }, { name: "output.md", path: "output.md", type: "file" }] }
      : operation === "changes"
        ? { gitStatus: "ready", gitAvailable: true, changes: [{ name: "output.md", path: "output.md", status: "modified" }, { name: "result.md", path: "artifacts/result.md", status: "created" }] }
        : operation === "search"
          ? { entries: [{ name: "output.md", path: "output.md", type: "file" }, { name: "result.md", path: "artifacts/result.md", type: "file" }], truncated: false }
          : operation === "diff"
            ? { path, baseline: "0123456789abcdef0123456789abcdef01234567", gitStatus: "ready", stale: false, before: { present: true, content: "# Previous result\n\nThe previous workspace result is available here.\n", encoding: "utf-8", truncated: false }, after: { present: true, content: demoFileContent(path), encoding: "utf-8", truncated: false } }
            : operation === "git-summary"
              ? { branch: "main", clean: false, detached: false, head: "0123456789abcdef0123456789abcdef01234567", refs: [], status: "ready" }
              : operation === "git-log"
                ? { commits: [{ oid: "0123456789abcdef0123456789abcdef01234567", authoredAt: "2026-10-11T08:00:00Z", author: "doops", subject: "Create workspace result", parents: [] }], cursor: null, ref: String(args.ref || "HEAD"), truncated: false }
                : { path, content: demoFileContent(path), encoding: "utf-8", contentType: "text/markdown", bytes: demoFileContent(path).length, truncated: false, binary: false }
    onEvent({ type: "result", result: { content: [{ type: "text", text: JSON.stringify(result) }] } })
    onEvent({ type: "done" })
    return
  }

  if (tool === "doops_file_write") {
    onEvent({ type: "open" })
    await sleep(260)
    onEvent({
      type: "result",
      result: { content: [{ type: "text", text: "写入成功 (演示模式，未持久化)" }] },
    })
    onEvent({ type: "done" })
    return
  }

  // 以下两个工具的参数、成功文本和报错文本与 Agent 一致
  // （backend/agent/internal/server/workspace_upload.go）；失败时和真实 callTool
  // 一样先推 error 事件再抛出同一错误。
  if (tool === "doops_check_deployment") {
    onEvent({ type: "open" })
    await sleep(260)
    signal?.throwIfAborted()
    const namespace = String(args.namespace || "").trim() || "default"
    const deployment = String(args.deployment || "").trim()
    const image = String(args.image || "").trim()
    const actual = DEMO_DEPLOYMENTS[`${namespace}/${deployment}`]
    if (!deployment || !image) demoToolError(onEvent, "namespace, deployment and image are required")
    else if (!actual)
      demoToolError(
        onEvent,
        `kubectl get deploy ${namespace}/${deployment} failed: exit status 1: Error from server (NotFound): deployments.apps "${deployment}" not found`,
      )
    else if (!demoImageMatches(image, actual)) demoToolError(onEvent, `image mismatch: want "${image}" got "${actual}"`)
    onEvent({ type: "result", result: { content: [{ type: "text", text: `check ok: ${namespace}/${deployment} -> ${actual}` }] } })
    onEvent({ type: "done" })
    return
  }

  if (tool === "doops_clean_workspace") {
    onEvent({ type: "open" })
    await sleep(260)
    signal?.throwIfAborted()
    const workspace = String(args.workspace || "").trim()
    if (!workspace || workspace === "." || workspace.includes("..") || /[\\/]/.test(workspace)) {
      demoToolError(onEvent, `unsafe session id: ${JSON.stringify(workspace)}`)
    }
    onEvent({ type: "result", result: { content: [{ type: "text", text: `Cleaned workspace and bare repo: ${workspace}` }] } })
    onEvent({ type: "done" })
    return
  }

  // 兜底
  onEvent({ type: "open" })
  await sleep(150)
  onEvent({ type: "result", result: { content: [{ type: "text", text: "(演示模式) 已执行" }] } })
  onEvent({ type: "done" })
}

// ===================== 管理后台演示数据（可变，便于体验增删） =====================

const nowISO = () => new Date().toISOString()
const ago = (h: number) => new Date(Date.now() - h * 3600_000).toISOString()

const demoUsers: AdminUser[] = [
  { id: "usr_admin01", name: "admin", disabled: false, has_password: true, grant_count: 1, is_admin: true, created_at: ago(720) },
  { id: "usr_ops01", name: "ops-zhang", disabled: false, has_password: true, grant_count: 2, is_admin: false, created_at: ago(240) },
  { id: "usr_dev01", name: "dev-li", disabled: false, has_password: true, grant_count: 1, is_admin: false, created_at: ago(72) },
  { id: "usr_bot01", name: "ci-bot", disabled: true, has_password: false, grant_count: 1, is_admin: false, created_at: ago(48) },
]

const demoGrants: AdminGrant[] = [
  { id: 1, user_id: "usr_admin01", user_name: "admin", cluster: "*", instance: "*", created_at: ago(720) },
  { id: 2, user_id: "usr_ops01", user_name: "ops-zhang", cluster: "prod-cn", instance: "*", created_at: ago(240) },
  { id: 3, user_id: "usr_ops01", user_name: "ops-zhang", cluster: "staging", instance: "*", created_at: ago(120) },
  { id: 4, user_id: "usr_dev01", user_name: "dev-li", cluster: "staging", instance: "db-01", created_at: ago(72) },
  { id: 5, user_id: "usr_bot01", user_name: "ci-bot", cluster: "prod-cn", instance: "*", created_at: ago(48) },
]

const demoTokens: AdminToken[] = [
  { id: "tok_a1", kind: "user", user_id: "usr_ops01", user_name: "ops-zhang", name: "笔记本", prefix: "dgw_user_3f2a", revoked: false, created_at: ago(240), expires_at: ago(-480) },
  { id: "tok_a2", kind: "user", user_id: "usr_dev01", user_name: "dev-li", name: "CLI", prefix: "dgw_user_88c1", revoked: false, created_at: ago(72) },
]

const demoInstances: AdminInstance[] = [
  { cluster: "prod-cn", instance: "web-01", status: "online", remote: "10.0.1.11:51820", busy: false, active_ops: 1, queued_ops: 0, connected_at: ago(6), last_seen: nowISO() },
  { cluster: "prod-cn", instance: "web-02", status: "online", remote: "10.0.1.12:51820", busy: true, active_ops: 2, queued_ops: 1, connected_at: ago(6), last_seen: nowISO() },
  { cluster: "staging", instance: "db-01", status: "offline", remote: "10.0.2.21:51820", busy: false, active_ops: 0, queued_ops: 0, connected_at: ago(48), last_seen: ago(2) },
]

let grantSeq = 100
let userSeq = 100

export function demoListUsers(): AdminUser[] {
  return demoUsers.map((u) => ({ ...u }))
}
export function demoCreateUser(body: { name: string; admin?: boolean }): void {
  demoUsers.push({
    id: `usr_demo${userSeq++}`,
    name: body.name,
    disabled: false,
    has_password: true,
    grant_count: 1,
    is_admin: !!body.admin,
    created_at: nowISO(),
  })
}
export function demoSetUserDisabled(body: { user_id: string; disabled: boolean }): void {
  const u = demoUsers.find((x) => x.id === body.user_id)
  if (u) u.disabled = body.disabled
}
export function demoListGrants(user?: string): AdminGrant[] {
  const list = user ? demoGrants.filter((g) => g.user_id === user) : demoGrants
  return list.map((g) => ({ ...g }))
}
export function demoCreateGrant(body: { user_id: string; cluster: string; instance: string }): void {
  const u = demoUsers.find((x) => x.id === body.user_id)
  demoGrants.push({
    id: grantSeq++,
    user_id: body.user_id,
    user_name: u?.name,
    cluster: body.cluster || "*",
    instance: body.instance || "*",
    created_at: nowISO(),
  })
  if (u) u.grant_count += 1
}
export function demoDeleteGrant(id: number): void {
  const idx = demoGrants.findIndex((g) => g.id === id)
  if (idx >= 0) {
    const [g] = demoGrants.splice(idx, 1)
    const u = demoUsers.find((x) => x.id === g.user_id)
    if (u && u.grant_count > 0) u.grant_count -= 1
  }
}
export function demoListTokens(): AdminToken[] {
  return demoTokens.map((t) => ({ ...t }))
}
export function demoCreateToken(body: { user: string; name?: string }): {
  token: string
  token_id: string
} {
  const id = `tok_demo${randomToken(4)}`
  const user = demoUsers.find((item) => item.name === body.user)
  demoTokens.unshift({
    id,
    kind: "user",
    user_id: user?.id,
    user_name: user?.name || body.user,
    name: body.name || "新令牌",
    prefix: `dgw_user_${randomToken(4)}`,
    revoked: false,
    created_at: nowISO(),
  })
  return { token: `dgw_user_demo_${id}_${randomToken(32)}`, token_id: id }
}
export function demoRevokeToken(id: string): void {
  const t = demoTokens.find((x) => x.id === id)
  if (t) t.revoked = true
}
export function demoListInstances(): AdminInstance[] {
  return demoInstances.map((i) => ({ ...i }))
}
export function demoArchiveInstance(body: { cluster: string; instance: string }): AdminInstance {
  const instance = demoInstances.find((item) => item.cluster === body.cluster && item.instance === body.instance)
  if (!instance) throw new Error("实例不存在")
  if (instance.status === "online") throw new Error("在线实例不能归档")
  instance.archived = true
  instance.archived_at = nowISO()
  instance.archived_by = "usr_admin01"
  return { ...instance }
}
export function demoUnarchiveInstance(body: { cluster: string; instance: string }): AdminInstance {
  const instance = demoInstances.find((item) => item.cluster === body.cluster && item.instance === body.instance)
  if (!instance) throw new Error("实例不存在")
  instance.archived = false
  delete instance.archived_at
  delete instance.archived_by
  return { ...instance }
}

const demoProxyRoutes: ProxyRoute[] = [
  { id: "route_demo01", host: "app.demo.example", cluster: "prod-cn", instance: "web-01", upstream: "http://127.0.0.1:8080", enabled: true, created_at: ago(96), updated_at: ago(24) },
  { id: "route_demo02", host: "status.demo.example", cluster: "staging", instance: "db-01", upstream: "http://127.0.0.1:9090", enabled: false, created_at: ago(48), updated_at: ago(48) },
]

// 与 Gateway 一致：按 host 排序，host 唯一，id 为空时新建。
export function demoListProxyRoutes(): ProxyRoute[] {
  return demoProxyRoutes.map((r) => ({ ...r })).sort((a, b) => a.host.localeCompare(b.host))
}
export function demoSetProxyRoute(body: ProxyRouteBody): ProxyRoute {
  const clash = demoProxyRoutes.find((r) => r.host === body.host && r.id !== body.id)
  if (clash) throw new Error(`域名 ${body.host} 已被路由 ${clash.id} 使用`)
  const now = nowISO()
  const existing = body.id ? demoProxyRoutes.find((r) => r.id === body.id) : undefined
  if (existing) {
    Object.assign(existing, { ...body, id: existing.id, updated_at: now })
    return { ...existing }
  }
  const route: ProxyRoute = { ...body, id: body.id || `route_demo${randomToken(6)}`, created_at: now, updated_at: now }
  demoProxyRoutes.push(route)
  return { ...route }
}
export function demoDeleteProxyRoute(id: string): void {
  const idx = demoProxyRoutes.findIndex((r) => r.id === id)
  if (idx < 0) throw new Error("proxy route not found")
  demoProxyRoutes.splice(idx, 1)
}
function randomToken(n: number): string {
  const chars = "0123456789abcdef"
  let out = ""
  for (let i = 0; i < n; i++) out += chars[Math.floor(Math.random() * chars.length)]
  return out
}
