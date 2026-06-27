"use client"

import type { Target, AuditEvent } from "./gateway"

export type { Target, AuditEvent }

export interface Session {
  gateway: string
  token: string
  username?: string
  demo?: boolean
}

export type RpcEvent =
  | { type: "open" }
  | { type: "output"; data: string; session?: string }
  | { type: "result"; result: unknown }
  | { type: "error"; error: string; code?: number }
  | { type: "done" }

function authHeaders(s: Session): HeadersInit {
  return {
    "Content-Type": "application/json",
    Authorization: `Bearer ${s.token}`,
    "x-doops-gateway": s.gateway,
  }
}

export async function login(
  gateway: string,
  username: string,
  password: string,
): Promise<{ token: string; username: string }> {
  const res = await fetch("/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ gateway, username, password }),
  })
  const data = await res.json()
  if (!res.ok) throw new Error(data.error || "登录失败")
  return { token: data.token, username: data.username || username }
}

export async function fetchTargets(s: Session): Promise<Target[]> {
  if (s.demo) {
    const { DEMO_TARGETS } = await import("./demo")
    return DEMO_TARGETS
  }
  const res = await fetch("/api/targets", { headers: authHeaders(s) })
  const data = await res.json()
  if (!res.ok) throw new Error(data.error || "查询目标失败")
  return (data.targets || []) as Target[]
}

export async function fetchAudit(
  s: Session,
  params: Record<string, string>,
): Promise<{ events?: AuditEvent[]; error?: string; status?: number }> {
  if (s.demo) {
    const { DEMO_AUDIT } = await import("./demo")
    const inst = params.instance
    return {
      events: inst ? DEMO_AUDIT.filter((e) => !e.instance || e.instance === inst) : DEMO_AUDIT,
    }
  }
  const qs = new URLSearchParams(params)
  const res = await fetch(`/api/audit?${qs.toString()}`, { headers: authHeaders(s) })
  const data = await res.json()
  if (!res.ok) return { error: data.error || "查询审计失败", status: res.status }
  return { events: (data.events || []) as AuditEvent[] }
}

// 调用一个 gateway 工具并流式返回事件
export async function callTool(
  s: Session,
  opts: {
    cluster: string
    instance: string
    tool: string
    arguments: Record<string, unknown>
    signal?: AbortSignal
  },
  onEvent: (ev: RpcEvent) => void,
): Promise<void> {
  if (s.demo) {
    const { demoCallTool } = await import("./demo")
    await demoCallTool({ tool: opts.tool, arguments: opts.arguments, signal: opts.signal }, onEvent)
    return
  }
  const res = await fetch("/api/rpc", {
    method: "POST",
    headers: authHeaders(s),
    body: JSON.stringify({
      cluster: opts.cluster,
      instance: opts.instance,
      tool: opts.tool,
      arguments: opts.arguments,
    }),
    signal: opts.signal,
  })

  if (!res.ok || !res.body) {
    let msg = `请求失败 (${res.status})`
    try {
      const data = await res.json()
      msg = data.error || msg
    } catch {
      /* noop */
    }
    onEvent({ type: "error", error: msg })
    onEvent({ type: "done" })
    return
  }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""

  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    let idx: number
    while ((idx = buffer.indexOf("\n")) >= 0) {
      const line = buffer.slice(0, idx).trim()
      buffer = buffer.slice(idx + 1)
      if (!line) continue
      try {
        onEvent(JSON.parse(line) as RpcEvent)
      } catch {
        /* 忽略不完整行 */
      }
    }
  }
  if (buffer.trim()) {
    try {
      onEvent(JSON.parse(buffer.trim()) as RpcEvent)
    } catch {
      /* noop */
    }
  }
}

// 从工具结果里提取文本内容（MCP content 数组或字符串）
export function extractText(result: unknown): string {
  if (result == null) return ""
  if (typeof result === "string") return result
  const r = result as any
  if (Array.isArray(r.content)) {
    return r.content
      .map((c: any) => (typeof c === "string" ? c : c?.text ?? ""))
      .join("")
  }
  if (typeof r.text === "string") return r.text
  try {
    return JSON.stringify(r, null, 2)
  } catch {
    return String(result)
  }
}
