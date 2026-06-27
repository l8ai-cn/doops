"use client"

import type { Session } from "./client"

export interface AdminUser {
  id: string
  name: string
  disabled: boolean
  has_password: boolean
  grant_count: number
  is_admin: boolean
  created_at?: string
}

export interface AdminGrant {
  id: number
  user_id: string
  user_name?: string
  cluster: string
  instance: string
  actions: string[]
  created_at?: string
}

export interface AdminToken {
  id: string
  kind: string
  user_id?: string
  user_name?: string
  name: string
  prefix: string
  cluster?: string
  instance?: string
  revoked: boolean
  expires_at?: string
  created_at?: string
}

export interface AdminInstance {
  cluster: string
  instance: string
  status: string
  remote?: string
  busy: boolean
  active_ops: number
  queued_ops: number
  connected_at?: string
  last_seen?: string
}

export interface AdminOperation {
  id: string
  user_id: string
  cluster: string
  instance: string
  action: string
  kind: string
  command_summary?: string
  started_at: string
  age_seconds: number
}

export const ALL_ACTIONS = [
  "exec",
  "ask",
  "read",
  "write",
  "push",
  "pull",
  "info",
  "check",
  "clean",
  "agent:upgrade",
  "targets:list",
  "admin",
] as const

function headers(s: Session): HeadersInit {
  return {
    "Content-Type": "application/json",
    Authorization: `Bearer ${s.token}`,
    "x-doops-gateway": s.gateway,
  }
}

async function req<T>(s: Session, path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api/admin/${path}`, { ...init, headers: headers(s) })
  const text = await res.text()
  const data = text ? JSON.parse(text) : {}
  if (!res.ok) throw new Error(data.error || `请求失败 (${res.status})`)
  return data as T
}

// ---------- 用户 ----------
export async function listUsers(s: Session): Promise<AdminUser[]> {
  if (s.demo) return (await import("./demo")).demoListUsers()
  return (await req<{ users: AdminUser[] }>(s, "users")).users || []
}

export async function createUser(
  s: Session,
  body: { name: string; password: string; admin?: boolean },
): Promise<void> {
  if (s.demo) return (await import("./demo")).demoCreateUser(body)
  await req(s, "users", { method: "POST", body: JSON.stringify(body) })
}

export async function setUserPassword(
  s: Session,
  body: { user_id: string; password: string },
): Promise<void> {
  if (s.demo) return
  await req(s, "users/password", { method: "POST", body: JSON.stringify(body) })
}

export async function setUserDisabled(
  s: Session,
  body: { user_id: string; disabled: boolean },
): Promise<void> {
  if (s.demo) return (await import("./demo")).demoSetUserDisabled(body)
  await req(s, "users/disable", { method: "POST", body: JSON.stringify(body) })
}

// ---------- 授权 ----------
export async function listGrants(s: Session, user?: string): Promise<AdminGrant[]> {
  if (s.demo) return (await import("./demo")).demoListGrants(user)
  const qs = user ? `?user=${encodeURIComponent(user)}` : ""
  return (await req<{ grants: AdminGrant[] }>(s, `grants${qs}`)).grants || []
}

export async function createGrant(
  s: Session,
  body: { user_id: string; cluster: string; instance: string; actions: string[] },
): Promise<void> {
  if (s.demo) return (await import("./demo")).demoCreateGrant(body)
  await req(s, "grants", { method: "POST", body: JSON.stringify(body) })
}

export async function deleteGrant(s: Session, id: number): Promise<void> {
  if (s.demo) return (await import("./demo")).demoDeleteGrant(id)
  await req(s, `grants?id=${id}`, { method: "DELETE" })
}

// ---------- 令牌 ----------
export async function listTokens(s: Session, kind?: string): Promise<AdminToken[]> {
  if (s.demo) return (await import("./demo")).demoListTokens(kind)
  const qs = kind ? `?kind=${encodeURIComponent(kind)}` : ""
  return (await req<{ tokens: AdminToken[] }>(s, `tokens${qs}`)).tokens || []
}

export async function createToken(
  s: Session,
  body: {
    kind?: string
    user?: string
    name?: string
    cluster?: string
    instance?: string
    expires?: string
  },
): Promise<{ token: string; token_id: string }> {
  if (s.demo) return (await import("./demo")).demoCreateToken(body)
  return req<{ token: string; token_id: string }>(s, "tokens", {
    method: "POST",
    body: JSON.stringify(body),
  })
}

export async function revokeToken(s: Session, id: string): Promise<void> {
  if (s.demo) return (await import("./demo")).demoRevokeToken(id)
  await req(s, `tokens?id=${encodeURIComponent(id)}`, { method: "DELETE" })
}

// ---------- 实例 ----------
export async function listInstances(s: Session): Promise<AdminInstance[]> {
  if (s.demo) return (await import("./demo")).demoListInstances()
  return (await req<{ instances: AdminInstance[] }>(s, "instances")).instances || []
}

// ---------- 运行中操作 ----------
export async function listOperations(s: Session): Promise<AdminOperation[]> {
  if (s.demo) return (await import("./demo")).demoListOperations()
  return (await req<{ operations: AdminOperation[] }>(s, "operations")).operations || []
}

export async function cancelOperation(s: Session, id: string): Promise<void> {
  if (s.demo) return (await import("./demo")).demoCancelOperation(id)
  await req(s, `operations?id=${encodeURIComponent(id)}`, { method: "DELETE" })
}
