"use client"

import { callTool, type Session, type Target } from "./client"
import { TOOLS } from "./gateway"

export const SETTINGS_PATH = "/root/.agent/settings.json"

export type ConfigKind = "model" | "file"

// 一个可复用的配置项，保存在浏览器本地，可一键应用/发布到任意节点。
// - kind="model"：浏览器中的模型元数据和受管凭证引用
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
  credentialRef?: string
  temperature?: number
  max_tokens?: number
  models?: string[]
  // —— 状态 ——
  updated_at: string
  published_at?: string // 最近一次成功发布到任意节点的时间
}

const STORE_KEY = "doops.modelConfigs.v1"

const SEED: ModelConfig[] = [
  // 不预置任何厂商或模型：模型由 Gateway 系统设置按 target 下发
  // （modelProvider 凭据），技能与前端都不绑定具体模型。
  {
    id: "cfg_model",
    name: "模型配置（由 Gateway 下发）",
    kind: "model",
    provider: "openai-compatible",
    model: "",
    base_url: "",
    temperature: 0.2,
    max_tokens: 8192,
    models: [],
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
    if (!Array.isArray(parsed)) return []
    const clean = parsed.filter((item) => item && typeof item === "object").map(withoutLegacyKey)
    if (JSON.stringify(clean) !== raw) persist(clean)
    return clean
  } catch {
    return []
  }
}

function withoutLegacyKey(config: ModelConfig & { api_key?: string }): ModelConfig {
  const clean = { ...config }
  delete clean.api_key
  return clean
}

function persist(list: ModelConfig[]) {
  if (typeof window === "undefined") return
  window.localStorage.setItem(STORE_KEY, JSON.stringify(list.map(withoutLegacyKey)))
}

export function upsertConfig(cfg: ModelConfig): ModelConfig[] {
  const list = loadConfigs()
  const idx = list.findIndex((c) => c.id === cfg.id)
  // 编辑后内容变化视为有未发布改动：保留 published_at，由 UI 通过 updated_at>published_at 判断
  const next = { ...withoutLegacyKey(cfg), updated_at: new Date().toISOString() }
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
// - model：由凭证管理发布，配置中心不写入 settings.json
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

  throw new Error("请在凭证管理中验证并启用目标的模型凭证。配置中心不会向节点写入模型设置。")
}
