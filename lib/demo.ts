"use client"

import type { Target, AuditEvent } from "./gateway"
import type { RpcEvent } from "./client"
import type {
  AdminUser,
  AdminGrant,
  AdminToken,
  AdminInstance,
  AdminOperation,
  SchedulerJob,
  SchedulerIssue,
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
      summary = `## 巡检结果：节点健康 ✅

**总体状态**：负载正常、核心服务全部存活、无错误日志。

| 检查项 | 结果 | 说明 |
| --- | --- | --- |
| 系统负载 | 正常 | load 0.18，低于告警阈值 |
| 服务存活 | 正常 | \`doops-app\` / \`nginx\` / \`redis\` 均 active |
| 磁盘使用 | 正常 | 根分区 37% |
| 错误日志 | 无 | 近 1 小时无 error 级日志 |

**建议**：无需处理，保持当前监控频率即可。`
    } else if (/回滚|rollback|上一个版本|恢复/.test(p)) {
      chunks = [
        `[plan] 收到任务：${prompt}\n`,
        "[step 1/2] 定位上一个稳定版本 releases/1.8.2…\n",
        "[tool] doops_shell: ln -sfn releases/1.8.2 current && systemctl reload doops-app\n",
        "[step 2/2] 健康检查 /healthz…\n",
        "[done] 已回滚至 1.8.2，服务恢复正常。\n",
      ]
      summary = `## 回滚完成 ✅

已将 \`current\` 切回上一个稳定版本 **releases/1.8.2**，服务平滑重载，未中断请求。

**执行步骤**

1. 定位上一个稳定版本 \`releases/1.8.2\`
2. 切换软链并重载服务：

\`\`\`bash
ln -sfn releases/1.8.2 current
systemctl reload doops-app
\`\`\`

3. 健康检查 \`/healthz\` 返回 **200 OK**

> 如需进一步排查 1.8.3 的问题，可在测试环境复现后再重新发布。`
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
      summary = `## 部署完成 ✅

已将 **doops-app** 滚动升级到 \`1.8.3\`，全程无停机。

**变更摘要**

- 新增发布：\`releases/1.8.3\`
- \`current\` 软链已指向新版本
- \`nginx\` 平滑重载，连接无中断
- 健康检查 \`/healthz\` → **200 OK**

**关键命令**

\`\`\`bash
ln -sfn releases/1.8.3 current
nginx -s reload
\`\`\`

完整报告已写入 \`doops-report.md\`。`
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

  if (tool === "doops_check_deployment") {
    await emitChunks(
      [
        "[check] 探测当前发布版本与软链接…\n",
        "current -> releases/1.8.2 (3 天前发布)\n",
        "[check] 服务存活：doops-app active · nginx active · redis active\n",
        "[check] 健康检查 GET /healthz → 200 OK (12ms)\n",
        "[check] 磁盘 /data 使用率 48% · 内存可用 5.3Gi\n",
      ],
      onEvent,
      signal,
      200,
    )
    onEvent({
      type: "result",
      result: {
        content: [{ type: "text", text: "部署正常：版本 1.8.2，三项服务全部存活，健康检查通过。" }],
      },
    })
    onEvent({ type: "done" })
    return
  }

  if (tool === "doops_clean_workspace") {
    await emitChunks(
      [
        "[clean] 扫描工作区临时文件与历史发布包…\n",
        "[clean] 保留最近 3 个版本，移除 releases/1.7.9、releases/1.7.8\n",
        "[clean] 清理 logs/*.gz 与 tmp/ 缓存\n",
        "[clean] 释放磁盘空间 2.4 GiB\n",
      ],
      onEvent,
      signal,
      200,
    )
    onEvent({
      type: "result",
      result: { content: [{ type: "text", text: "工作区清理完成，释放 2.4 GiB，保留最近 3 个版本。" }] },
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
  { id: 1, user_id: "usr_admin01", user_name: "admin", cluster: "*", instance: "*", actions: ["admin"], created_at: ago(720) },
  { id: 2, user_id: "usr_ops01", user_name: "ops-zhang", cluster: "prod-cn", instance: "*", actions: ["exec", "ask", "read", "write", "info", "check"], created_at: ago(240) },
  { id: 3, user_id: "usr_ops01", user_name: "ops-zhang", cluster: "staging", instance: "*", actions: ["exec", "ask", "read"], created_at: ago(120) },
  { id: 4, user_id: "usr_dev01", user_name: "dev-li", cluster: "staging", instance: "db-01", actions: ["read", "info"], created_at: ago(72) },
  { id: 5, user_id: "usr_bot01", user_name: "ci-bot", cluster: "prod-cn", instance: "*", actions: ["push", "pull"], created_at: ago(48) },
]

const demoTokens: AdminToken[] = [
  { id: "tok_a1", kind: "user", user_id: "usr_ops01", user_name: "ops-zhang", name: "笔记本", prefix: "dgw_user_3f2a", revoked: false, created_at: ago(240), expires_at: ago(-480) },
  { id: "tok_a2", kind: "user", user_id: "usr_dev01", user_name: "dev-li", name: "CLI", prefix: "dgw_user_88c1", revoked: false, created_at: ago(72) },
  { id: "tok_g1", kind: "agent", name: "prod-cn/web-01", prefix: "dgw_agent_aa01", cluster: "prod-cn", instance: "web-01", revoked: false, created_at: ago(700) },
  { id: "tok_g2", kind: "agent", name: "prod-cn/web-02", prefix: "dgw_agent_aa02", cluster: "prod-cn", instance: "web-02", revoked: false, created_at: ago(700) },
  { id: "tok_g3", kind: "agent", name: "staging/db-01", prefix: "dgw_agent_bb01", cluster: "staging", instance: "db-01", revoked: true, created_at: ago(400) },
]

const demoInstances: AdminInstance[] = [
  { cluster: "prod-cn", instance: "web-01", status: "online", remote: "10.0.1.11:51820", busy: false, active_ops: 1, queued_ops: 0, connected_at: ago(6), last_seen: nowISO() },
  { cluster: "prod-cn", instance: "web-02", status: "online", remote: "10.0.1.12:51820", busy: true, active_ops: 2, queued_ops: 1, connected_at: ago(6), last_seen: nowISO() },
  { cluster: "staging", instance: "db-01", status: "offline", remote: "10.0.2.21:51820", busy: false, active_ops: 0, queued_ops: 0, connected_at: ago(48), last_seen: ago(2) },
]

const demoOps: AdminOperation[] = [
  { id: "op_1", user_id: "usr_ops01", cluster: "prod-cn", instance: "web-01", action: "ask", kind: "doops_agent_prompt", command_summary: "巡检节点状态", started_at: ago(0.02), age_seconds: 72 },
  { id: "op_2", user_id: "usr_ops01", cluster: "prod-cn", instance: "web-02", action: "exec", kind: "doops_shell", command_summary: "systemctl restart nginx", started_at: ago(0.01), age_seconds: 36 },
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
export function demoCreateGrant(body: { user_id: string; cluster: string; instance: string; actions: string[] }): void {
  const u = demoUsers.find((x) => x.id === body.user_id)
  demoGrants.push({
    id: grantSeq++,
    user_id: body.user_id,
    user_name: u?.name,
    cluster: body.cluster || "*",
    instance: body.instance || "*",
    actions: body.actions.length ? body.actions : ["exec", "ask", "read"],
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
export function demoListTokens(kind?: string): AdminToken[] {
  const list = kind ? demoTokens.filter((t) => t.kind === kind) : demoTokens
  return list.map((t) => ({ ...t }))
}
export function demoCreateToken(body: { kind?: string; cluster?: string; instance?: string; name?: string }): {
  token: string
  token_id: string
} {
  const id = `tok_demo${randomToken(4)}`
  const kind = body.kind === "agent" ? "agent" : "user"
  demoTokens.unshift({
    id,
    kind,
    name: body.name || (kind === "agent" ? `${body.cluster}/${body.instance}` : "新令牌"),
    prefix: `dgw_${kind}_${randomToken(4)}`,
    cluster: body.cluster,
    instance: body.instance,
    revoked: false,
    created_at: nowISO(),
  })
  return { token: `dgw_${kind}_demo_${id}_${randomToken(32)}`, token_id: id }
}
export function demoRevokeToken(id: string): void {
  const t = demoTokens.find((x) => x.id === id)
  if (t) t.revoked = true
}
export function demoListInstances(): AdminInstance[] {
  return demoInstances.map((i) => ({ ...i }))
}
export function demoListOperations(): AdminOperation[] {
  return demoOps.map((o) => ({ ...o }))
}
export function demoCancelOperation(id: string): void {
  const idx = demoOps.findIndex((o) => o.id === id)
  if (idx >= 0) demoOps.splice(idx, 1)
}

function randomToken(n: number): string {
  const chars = "0123456789abcdef"
  let out = ""
  for (let i = 0; i < n; i++) out += chars[Math.floor(Math.random() * chars.length)]
  return out
}

// ===================== 定时巡检演示数据 =====================

const demoJobs: SchedulerJob[] = [
  {
    id: "job_disk",
    name: "磁盘水位巡检",
    cluster_glob: "prod-cn",
    instance_glob: "*",
    interval_sec: 600,
    scan_mode: "ask",
    scan_config: JSON.stringify({ prompt: "检查磁盘使用率，超过 85% 时报告分区与占用 top5" }),
    platform: "github",
    repo_slug: "l8ai-cn/ops-issues",
    labels: "auto,disk",
    token_env: "GITHUB_TOKEN",
    api_base: "",
    dedup_window_sec: 86400,
    enabled: true,
    last_run_at: ago(0.2),
    created_at: ago(360),
  },
  {
    id: "job_svc",
    name: "服务存活巡检",
    cluster_glob: "*",
    instance_glob: "*",
    interval_sec: 300,
    scan_mode: "exec",
    scan_config: JSON.stringify({ command: "systemctl is-active doops-app || echo DOWN" }),
    platform: "cnb",
    repo_slug: "l8ai/ops",
    labels: "auto,service",
    token_env: "CNB_TOKEN",
    api_base: "https://api.cnb.cool",
    dedup_window_sec: 3600,
    enabled: false,
    last_run_at: ago(26),
    created_at: ago(200),
  },
]

const demoIssues: SchedulerIssue[] = [
  {
    id: 1,
    job_id: "job_disk",
    fingerprint: "a1b2c3",
    repo_slug: "l8ai-cn/ops-issues",
    cluster: "prod-cn",
    instance: "web-02",
    issue_url: "https://github.com/l8ai-cn/ops-issues/issues/142",
    title: "[巡检] web-02 /data 分区使用率 91%",
    status: "open",
    created_at: ago(3),
  },
  {
    id: 2,
    job_id: "job_disk",
    fingerprint: "d4e5f6",
    repo_slug: "l8ai-cn/ops-issues",
    cluster: "prod-cn",
    instance: "web-01",
    issue_url: "https://github.com/l8ai-cn/ops-issues/issues/138",
    title: "[巡检] web-01 / 分区使用率 88%",
    status: "closed",
    created_at: ago(28),
  },
  {
    id: 3,
    job_id: "job_svc",
    fingerprint: "99aa88",
    repo_slug: "l8ai/ops",
    cluster: "staging",
    instance: "db-01",
    issue_url: "https://cnb.cool/l8ai/ops/-/issues/7",
    title: "[巡检] db-01 doops-app 未运行",
    status: "open",
    created_at: ago(26),
  },
]

let jobSeq = 100

export function demoListJobs(): SchedulerJob[] {
  return demoJobs.map((j) => ({ ...j }))
}
export function demoCreateJob(body: Partial<SchedulerJob>): SchedulerJob {
  const job: SchedulerJob = {
    id: `job_demo${jobSeq++}`,
    name: body.name || "新巡检任务",
    cluster_glob: body.cluster_glob || "*",
    instance_glob: body.instance_glob || "*",
    interval_sec: body.interval_sec || 600,
    scan_mode: body.scan_mode || "ask",
    scan_config: body.scan_config || "{}",
    platform: body.platform || "github",
    repo_slug: body.repo_slug || "",
    labels: body.labels || "auto",
    token_env: body.token_env || "GITHUB_TOKEN",
    api_base: body.api_base || "",
    dedup_window_sec: body.dedup_window_sec || 86400,
    enabled: body.enabled ?? true,
    created_at: nowISO(),
  }
  demoJobs.unshift(job)
  return { ...job }
}
export function demoDeleteJob(id: string): void {
  const idx = demoJobs.findIndex((j) => j.id === id)
  if (idx >= 0) demoJobs.splice(idx, 1)
}
export function demoSetJobEnabled(id: string, enabled: boolean): void {
  const j = demoJobs.find((x) => x.id === id)
  if (j) j.enabled = enabled
}
export function demoRunJobNow(id: string): string {
  const j = demoJobs.find((x) => x.id === id)
  if (j) j.last_run_at = nowISO()
  return "(演示) 已扫描 3 台机器，命中 1 项异常，去重后提交 1 个 issue"
}
export function demoListJobIssues(jobId?: string): SchedulerIssue[] {
  const list = jobId ? demoIssues.filter((i) => i.job_id === jobId) : demoIssues
  return list.map((i) => ({ ...i }))
}
