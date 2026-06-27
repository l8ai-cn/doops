// 共享的 gateway 协议类型与工具函数

export interface Target {
  cluster: string
  instance: string
  key: string
  remote?: string
  connected_at?: string
  last_seen?: string
  busy?: boolean
  status?: string
  active_ops?: number
  queued_ops?: number
  resources?: string[]
  sessions?: string[]
}

export interface AuditEvent {
  id: number
  user_id?: string
  token_id?: string
  cluster?: string
  instance?: string
  action?: string
  session?: string
  command_summary?: string
  status?: string
  tail?: string
  bytes_in?: number
  bytes_out?: number
  started_at?: string
  ended_at?: string
}

// gateway 目标动作工具名
export const TOOLS = {
  shell: "doops_shell",
  prompt: "doops_agent_prompt",
  fileRead: "doops_file_read",
  fileWrite: "doops_file_write",
  nodeInfo: "doops_node_info",
  checkDeployment: "doops_check_deployment",
  cleanWorkspace: "doops_clean_workspace",
} as const

// 把 http(s):// 规整成 ws(s):// 基地址，去掉尾部斜杠
export function toWsBase(gateway: string): string {
  const trimmed = gateway.trim().replace(/\/+$/, "")
  if (trimmed.startsWith("https://")) return "wss://" + trimmed.slice("https://".length)
  if (trimmed.startsWith("http://")) return "ws://" + trimmed.slice("http://".length)
  if (trimmed.startsWith("wss://") || trimmed.startsWith("ws://")) return trimmed
  return "ws://" + trimmed
}

// 规整 http 基地址
export function toHttpBase(gateway: string): string {
  const trimmed = gateway.trim().replace(/\/+$/, "")
  if (trimmed.startsWith("ws://")) return "http://" + trimmed.slice("ws://".length)
  if (trimmed.startsWith("wss://")) return "https://" + trimmed.slice("wss://".length)
  if (trimmed.startsWith("http://") || trimmed.startsWith("https://")) return trimmed
  return "http://" + trimmed
}

// 从请求头读取 gateway 地址与 token
export function readAuth(req: Request): { gateway: string; token: string } {
  const gateway = req.headers.get("x-doops-gateway") || ""
  const auth = req.headers.get("authorization") || ""
  const token = auth.toLowerCase().startsWith("bearer ") ? auth.slice(7).trim() : auth.trim()
  return { gateway, token }
}

// 生成一个随机 session id
export function randomSession(prefix = "console"): string {
  return `${prefix}-${Math.random().toString(36).slice(2, 10)}`
}
