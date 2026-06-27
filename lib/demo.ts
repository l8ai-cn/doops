"use client"

import type { Target, AuditEvent } from "./gateway"
import type { RpcEvent } from "./client"

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
    action: "doops_shell",
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
    action: "doops_agent_prompt",
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
    action: "doops_file_write",
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
export const DEMO_SETTINGS_PATH = "/root/.agent/settings.json"

const DEMO_SETTINGS = `{
  "provider": "openai-compatible",
  "model": "openai/gpt-5.4",
  "base_url": "https://api.example.com/v1",
  "api_key": "sk-demo-3f9a2b7c8d1e4f5061728394a5b6c7d8",
  "temperature": 0.2,
  "max_tokens": 4096,
  "models": [
    "openai/gpt-5.4",
    "anthropic/claude-opus-4.6",
    "google/gemini-3-flash"
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
  "/root": [
    { name: "ws", dir: true },
    { name: ".agent", dir: true },
  ],
}

function demoFileContent(path: string): string {
  if (path.includes("settings.json")) return DEMO_SETTINGS
  if (path.endsWith("deploy.sh"))
    return "#!/usr/bin/env bash\nset -euo pipefail\nIMAGE=doops/app:${1:-latest}\nbuildctl build --frontend dockerfile.v0 --local context=. --output type=image,name=$IMAGE\nkubectl set image deployment/app app=$IMAGE\nkubectl rollout status deployment/app\n"
  if (path.endsWith(".md"))
    return "# 部署报告\n\n- 镜像: doops/app:1.8.3\n- 滚动更新: 成功\n- 健康检查: /healthz 200\n- 耗时: 48s\n"
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

async function emitChunks(
  chunks: string[],
  onEvent: (ev: RpcEvent) => void,
  signal?: AbortSignal,
  delay = 220,
) {
  onEvent({ type: "open" })
  for (const c of chunks) {
    if (signal?.aborted) {
      onEvent({ type: "error", error: "已中断" })
      onEvent({ type: "done" })
      return
    }
    await sleep(delay)
    onEvent({ type: "output", data: c })
  }
}

// 根据 shell 命令产出模拟输出
function shellOutput(cmd: string): string[] {
  const c = cmd.trim()
  // 文件浏览器目录列举：ls -1Ap <path>
  const lsMatch = c.match(/^ls\s+-1Ap\s+(?:"([^"]+)"|'([^']+)'|(\S+))/)
  if (lsMatch) {
    const dir = lsMatch[1] || lsMatch[2] || lsMatch[3] || "/"
    return demoListing(dir)
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
      "● doops-app.service - Doops Application\n",
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

export async function demoCallTool(
  opts: { tool: string; arguments: Record<string, unknown>; signal?: AbortSignal },
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

  if (tool === "doops_agent_prompt") {
    const prompt = String(args.instruction || args.prompt || args.task || "")
    const p = prompt.toLowerCase()
    let chunks: string[]
    let summary: string

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
      summary = "巡检结果：负载正常、服务全部 active、磁盘 37%、无错误日志。"
    } else if (/回滚|rollback|上一个版本|恢复/.test(p)) {
      chunks = [
        `[plan] 收到任务：${prompt}\n`,
        "[step 1/2] 定位上一个稳定版本 releases/1.8.2…\n",
        "[tool] doops_shell: ln -sfn releases/1.8.2 current && systemctl reload doops-app\n",
        "[step 2/2] 健康检查 /healthz…\n",
        "[done] 已回滚至 1.8.2，服务恢复正常。\n",
      ]
      summary = "已回滚到 1.8.2，/healthz 返回 200。"
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
      summary = "任务完成：部署成功，服务健康。"
    }

    await emitChunks(chunks, onEvent, signal, 340)
    onEvent({ type: "result", result: { content: [{ type: "text", text: summary }] } })
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

  // 兜底
  onEvent({ type: "open" })
  await sleep(150)
  onEvent({ type: "result", result: { content: [{ type: "text", text: "(演示模式) 已执行" }] } })
  onEvent({ type: "done" })
}
