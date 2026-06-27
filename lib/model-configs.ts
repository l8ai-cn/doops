"use client"

import { callTool, type Session, type Target } from "./client"
import { TOOLS } from "./gateway"

export const SETTINGS_PATH = "/root/.agent/settings.json"

// 一个可复用的模型配置预设，保存在浏览器本地，可一键应用到任意节点。
export interface ModelConfig {
  id: string
  name: string
  provider?: string
  model?: string
  base_url?: string
  api_key?: string
  temperature?: number
  max_tokens?: number
  models?: string[]
  updated_at: string
}

const STORE_KEY = "doops.modelConfigs.v1"

const SEED: ModelConfig[] = [
  {
    id: "cfg_gpt",
    name: "OpenAI 兼容 · GPT",
    provider: "openai-compatible",
    model: "openai/gpt-5.4",
    base_url: "https://api.openai.com/v1",
    api_key: "",
    temperature: 0.2,
    max_tokens: 8192,
    models: ["openai/gpt-5.4", "openai/gpt-5-mini"],
    updated_at: new Date().toISOString(),
  },
  {
    id: "cfg_claude",
    name: "Anthropic · Claude",
    provider: "anthropic",
    model: "anthropic/claude-opus-4.6",
    base_url: "https://api.anthropic.com",
    api_key: "",
    temperature: 0.3,
    max_tokens: 16384,
    models: ["anthropic/claude-opus-4.6", "anthropic/claude-haiku-4"],
    updated_at: new Date().toISOString(),
  },
]

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

export function newConfigId(): string {
  return "cfg_" + Math.random().toString(36).slice(2, 10)
}

// 把预设里的模型字段合并进节点现有 settings.json，并写回。
// 返回写入后的完整内容字符串。
export async function applyConfigToNode(
  session: Session,
  target: Target,
  sessionId: string,
  cfg: ModelConfig,
): Promise<void> {
  // 1) 读取现有配置（容错：读不到或非法 JSON 则以空对象为基底）
  let base: Record<string, unknown> = {}
  let buf = ""
  await callTool(
    session,
    {
      cluster: target.cluster,
      instance: target.instance,
      tool: TOOLS.fileRead,
      arguments: { session_id: sessionId, path: SETTINGS_PATH },
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
  try {
    if (buf.trim()) base = JSON.parse(buf)
  } catch {
    base = {}
  }

  // 2) 合并预设字段（仅覆盖有值的字段）
  const merged: Record<string, unknown> = { ...base }
  if (cfg.provider !== undefined) merged.provider = cfg.provider
  if (cfg.model !== undefined) merged.model = cfg.model
  if (cfg.base_url !== undefined) merged.base_url = cfg.base_url
  if (cfg.api_key) merged.api_key = cfg.api_key
  if (cfg.temperature !== undefined) merged.temperature = cfg.temperature
  if (cfg.max_tokens !== undefined) merged.max_tokens = cfg.max_tokens
  if (cfg.models && cfg.models.length) merged.models = cfg.models

  const content = JSON.stringify(merged, null, 2) + "\n"

  // 3) 写回节点
  let writeErr: string | null = null
  await callTool(
    session,
    {
      cluster: target.cluster,
      instance: target.instance,
      tool: TOOLS.fileWrite,
      arguments: { session_id: sessionId, path: SETTINGS_PATH, content },
    },
    (ev) => {
      if (ev.type === "error") writeErr = ev.error
    },
  )
  if (writeErr) throw new Error(writeErr)
}
