"use client"

import { callTool, type Session, type Target } from "./client"
import { TOOLS } from "./gateway"

export const SETTINGS_PATH = "/root/.agent/settings.json"

export type ConfigKind = "model" | "file"

// 一个可复用的配置项，保存在浏览器本地，可一键应用/发布到任意节点。
// - kind="model"：大模型配置，按字段合并进节点 settings.json
// - kind="file" ：通用配置文件，把 content 原样写入目标 path
export interface ModelConfig {
  id: string
  name: string
  kind?: ConfigKind // 缺省视为 "model"，兼容历史数据
  // —— 通用配置文件字段 ——
  path?: string // kind="file" 时的目标路径；kind="model" 时缺省为 settings.json
  content?: string // kind="file" 的原始文件内容
  format?: string // 展示用：json / yaml / env / text
  // —— 大模型配置字段 ——
  provider?: string
  model?: string
  base_url?: string
  api_key?: string
  temperature?: number
  max_tokens?: number
  models?: string[]
  // —— 状态 ——
  updated_at: string
  published_at?: string // 最近一次成功发布到任意节点的时间
}

const STORE_KEY = "doops.modelConfigs.v1"

const SEED: ModelConfig[] = [
  {
    id: "cfg_gpt",
    name: "OpenAI 兼容 · GPT",
    kind: "model",
    provider: "openai-compatible",
    model: "openai/gpt-5.4",
    base_url: "https://api.openai.com/v1",
    api_key: "",
    temperature: 0.2,
    max_tokens: 8192,
    models: ["openai/gpt-5.4", "openai/gpt-5-mini"],
    updated_at: new Date().toISOString(),
    published_at: new Date(Date.now() - 86400_000).toISOString(),
  },
  {
    id: "cfg_claude",
    name: "Anthropic · Claude",
    kind: "model",
    provider: "anthropic",
    model: "anthropic/claude-opus-4.6",
    base_url: "https://api.anthropic.com",
    api_key: "",
    temperature: 0.3,
    max_tokens: 16384,
    models: ["anthropic/claude-opus-4.6", "anthropic/claude-haiku-4"],
    updated_at: new Date().toISOString(),
  },
  {
    id: "cfg_nginx",
    name: "Nginx 反代配置",
    kind: "file",
    path: "/etc/nginx/conf.d/doops.conf",
    format: "text",
    content:
      "server {\n  listen 80;\n  server_name _;\n  location / {\n    proxy_pass http://127.0.0.1:8080;\n    proxy_set_header Host $host;\n  }\n}\n",
    updated_at: new Date().toISOString(),
  },
]

export function configKind(c: ModelConfig): ConfigKind {
  return c.kind ?? "model"
}

export function configTargetPath(c: ModelConfig): string {
  return configKind(c) === "file" ? c.path || "" : c.path || SETTINGS_PATH
}

export function loadConfigs(): ModelConfig[] {
  if (typeof window === "undefined") return []
  try {
    const raw = window.localStorage.getItem(STORE_KEY)
    if (!raw) {
      window.localStorage.setItem(STORE_KEY, JSON.stringify(SEED))
      return SEED.map((c) => ({ ...c }))
    }
    const parsed = JSON.parse(raw) as ModelConfig[]
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
}

function persist(list: ModelConfig[]) {
  if (typeof window === "undefined") return
  window.localStorage.setItem(STORE_KEY, JSON.stringify(list))
}

export function upsertConfig(cfg: ModelConfig): ModelConfig[] {
  const list = loadConfigs()
  const idx = list.findIndex((c) => c.id === cfg.id)
  // 编辑后内容变化视为有未发布改动：保留 published_at，由 UI 通过 updated_at>published_at 判断
  const next = { ...cfg, updated_at: new Date().toISOString() }
  if (idx >= 0) list[idx] = next
  else list.push(next)
  persist(list)
  return list
}

export function removeConfig(id: string): ModelConfig[] {
  const list = loadConfigs().filter((c) => c.id !== id)
  persist(list)
  return list
}

// 标记某配置已发布（应用成功后调用），更新 published_at
export function markPublished(id: string): ModelConfig[] {
  const list = loadConfigs()
  const idx = list.findIndex((c) => c.id === id)
  if (idx >= 0) {
    list[idx] = { ...list[idx], published_at: new Date().toISOString() }
    persist(list)
  }
  return list
}

// 是否有未发布的改动
export function hasUnpublishedChanges(c: ModelConfig): boolean {
  if (!c.published_at) return true
  return new Date(c.updated_at).getTime() > new Date(c.published_at).getTime() + 1000
}

export function newConfigId(): string {
  return "cfg_" + Math.random().toString(36).slice(2, 10)
}

async function readRemote(session: Session, target: Target, sessionId: string, path: string): Promise<string> {
  let buf = ""
  await callTool(
    session,
    {
      cluster: target.cluster,
      instance: target.instance,
      tool: TOOLS.fileRead,
      arguments: { session_id: sessionId, path },
    },
    (ev) => {
      if (ev.type === "output") buf += ev.data
      else if (ev.type === "result") {
        const r = ev.result as any
        if (typeof r === "string") buf += r
        else if (Array.isArray(r?.content)) buf += r.content.map((c: any) => c?.text ?? "").join("")
      }
    },
  )
  return buf
}

async function writeRemote(session: Session, target: Target, sessionId: string, path: string, content: string) {
  let writeErr: string | null = null
  await callTool(
    session,
    {
      cluster: target.cluster,
      instance: target.instance,
      tool: TOOLS.fileWrite,
      arguments: { session_id: sessionId, path, content },
    },
    (ev) => {
      if (ev.type === "error") writeErr = ev.error
    },
  )
  if (writeErr) throw new Error(writeErr)
}

// 把配置应用（发布）到节点：
// - model：读取现有 settings.json，合并模型字段后写回
// - file ：把 content 原样写入目标 path
export async function applyConfigToNode(
  session: Session,
  target: Target,
  sessionId: string,
  cfg: ModelConfig,
): Promise<void> {
  if (configKind(cfg) === "file") {
    const path = configTargetPath(cfg)
    if (!path) throw new Error("配置文件缺少目标路径")
    await writeRemote(session, target, sessionId, path, cfg.content ?? "")
    return
  }

  // model 类型：合并进现有 settings.json
  const path = configTargetPath(cfg)
  let base: Record<string, unknown> = {}
  const buf = await readRemote(session, target, sessionId, path)
  try {
    if (buf.trim()) base = JSON.parse(buf)
  } catch {
    base = {}
  }

  const merged: Record<string, unknown> = { ...base }
  if (cfg.provider !== undefined) merged.provider = cfg.provider
  if (cfg.model !== undefined) merged.model = cfg.model
  if (cfg.base_url !== undefined) merged.base_url = cfg.base_url
  if (cfg.api_key) merged.api_key = cfg.api_key
  if (cfg.temperature !== undefined) merged.temperature = cfg.temperature
  if (cfg.max_tokens !== undefined) merged.max_tokens = cfg.max_tokens
  if (cfg.models && cfg.models.length) merged.models = cfg.models

  const content = JSON.stringify(merged, null, 2) + "\n"
  await writeRemote(session, target, sessionId, path, content)
}
