"use client"

import { useCallback, useEffect, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import {
  KeyIcon,
  SaveIcon,
  RefreshIcon,
  EyeIcon,
  EyeOffIcon,
  PlusIcon,
  TrashIcon,
} from "./icons"

const SETTINGS_PATH = "/root/.agent/settings.json"

interface Settings {
  provider?: string
  model?: string
  base_url?: string
  api_key?: string
  temperature?: number
  max_tokens?: number
  models?: string[]
  [k: string]: unknown
}

export function ConfigPanel({
  session,
  target,
  sessionId,
}: {
  session: Session
  target: Target
  sessionId: string
}) {
  const [obj, setObj] = useState<Settings>({})
  const [rawText, setRawText] = useState("")
  const [mode, setMode] = useState<"form" | "raw">("form")
  const [showKey, setShowKey] = useState(false)
  const [busy, setBusy] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [dirty, setDirty] = useState(false)
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null)

  const load = useCallback(async () => {
    setBusy(true)
    setStatus(null)
    let buf = ""
    try {
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
          else if (ev.type === "result") buf += extractText(ev.result)
          else if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
        },
      )
      setRawText(buf)
      try {
        setObj(JSON.parse(buf))
      } catch {
        setStatus({ kind: "err", text: "settings.json 不是合法 JSON，已切换到原始编辑模式" })
        setMode("raw")
      }
      setLoaded(true)
      setDirty(false)
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }, [session, target, sessionId])

  useEffect(() => {
    load()
  }, [load])

  function setField<K extends keyof Settings>(key: K, value: Settings[K]) {
    setObj((p) => ({ ...p, [key]: value }))
    setDirty(true)
  }

  function switchMode(next: "form" | "raw") {
    if (next === mode) return
    if (next === "raw") {
      setRawText(JSON.stringify(obj, null, 2))
      setMode("raw")
    } else {
      try {
        setObj(JSON.parse(rawText))
        setMode("form")
        setStatus(null)
      } catch {
        setStatus({ kind: "err", text: "JSON 解析失败，无法切换到表单模式" })
      }
    }
  }

  async function save() {
    if (busy) return
    let content: string
    if (mode === "raw") {
      try {
        JSON.parse(rawText)
      } catch {
        setStatus({ kind: "err", text: "保存失败：JSON 格式不合法" })
        return
      }
      content = rawText
    } else {
      content = JSON.stringify(obj, null, 2) + "\n"
    }
    setBusy(true)
    setStatus(null)
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.fileWrite,
          arguments: { session_id: sessionId, path: SETTINGS_PATH, content },
        },
        (ev) => {
          if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
          else if (ev.type === "result") {
            setStatus({ kind: "ok", text: "配置已写入 settings.json" })
            setDirty(false)
          }
        },
      )
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }

  const models = obj.models || []

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b bg-card px-4 py-2.5">
        <div className="flex items-center gap-2">
          <KeyIcon width={16} height={16} className="text-primary" />
          <span className="text-sm font-medium text-foreground">doagent 配置</span>
          <span className="font-mono text-xs text-muted-foreground">{SETTINGS_PATH}</span>
          {dirty && <span className="text-warning">●</span>}
        </div>
        <div className="flex items-center gap-2">
          <div className="flex rounded-lg border p-0.5">
            <button
              onClick={() => switchMode("form")}
              className={`rounded-md px-2.5 py-1 text-xs transition-colors ${
                mode === "form" ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:text-foreground"
              }`}
            >
              表单
            </button>
            <button
              onClick={() => switchMode("raw")}
              className={`rounded-md px-2.5 py-1 text-xs transition-colors ${
                mode === "raw" ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:text-foreground"
              }`}
            >
              原始 JSON
            </button>
          </div>
          <button
            onClick={load}
            disabled={busy}
            title="重新加载"
            className="rounded-lg border p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:opacity-50"
          >
            <RefreshIcon width={15} height={15} className={busy ? "animate-spin" : ""} />
          </button>
          <button
            onClick={save}
            disabled={busy || !loaded}
            className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
          >
            <SaveIcon width={15} height={15} /> 保存
          </button>
        </div>
      </div>

      {status && (
        <p
          className={`mx-4 mt-3 rounded-lg px-3 py-2 text-sm ${
            status.kind === "ok" ? "bg-success/15 text-success" : "bg-destructive/15 text-destructive"
          }`}
        >
          {status.text}
        </p>
      )}

      <div className="min-h-0 flex-1 overflow-y-auto p-4">
        {!loaded ? (
          <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
            {busy ? "加载配置中…" : "未能加载配置"}
          </div>
        ) : mode === "raw" ? (
          <textarea
            value={rawText}
            onChange={(e) => {
              setRawText(e.target.value)
              setDirty(true)
            }}
            spellCheck={false}
            className="h-full min-h-[20rem] w-full resize-none rounded-lg border bg-background p-3 font-mono text-sm leading-relaxed text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
          />
        ) : (
          <div className="mx-auto flex max-w-2xl flex-col gap-4">
            <Field label="Provider" hint="模型提供方，如 openai-compatible / anthropic">
              <input
                value={obj.provider ?? ""}
                onChange={(e) => setField("provider", e.target.value)}
                className="input"
                placeholder="openai-compatible"
              />
            </Field>

            <Field label="默认模型 (model)">
              <input
                value={obj.model ?? ""}
                onChange={(e) => setField("model", e.target.value)}
                className="input font-mono"
                placeholder="openai/gpt-5.4"
              />
            </Field>

            <Field label="API 网关 (base_url)">
              <input
                value={obj.base_url ?? ""}
                onChange={(e) => setField("base_url", e.target.value)}
                className="input font-mono"
                placeholder="https://api.example.com/v1"
              />
            </Field>

            <Field label="API 密钥 (api_key)" hint="生产环境建议用 Secret 注入，避免明文写入 ConfigMap">
              <div className="flex items-center gap-2">
                <input
                  type={showKey ? "text" : "password"}
                  value={(obj.api_key as string) ?? ""}
                  onChange={(e) => setField("api_key", e.target.value)}
                  className="input flex-1 font-mono"
                  placeholder="sk-..."
                />
                <button
                  onClick={() => setShowKey((s) => !s)}
                  className="rounded-lg border p-2 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
                  title={showKey ? "隐藏" : "显示"}
                >
                  {showKey ? <EyeOffIcon width={16} height={16} /> : <EyeIcon width={16} height={16} />}
                </button>
              </div>
            </Field>

            <div className="grid grid-cols-2 gap-4">
              <Field label="temperature">
                <input
                  type="number"
                  step="0.1"
                  value={obj.temperature ?? ""}
                  onChange={(e) => setField("temperature", e.target.value === "" ? undefined : Number(e.target.value))}
                  className="input font-mono"
                />
              </Field>
              <Field label="max_tokens">
                <input
                  type="number"
                  value={obj.max_tokens ?? ""}
                  onChange={(e) => setField("max_tokens", e.target.value === "" ? undefined : Number(e.target.value))}
                  className="input font-mono"
                />
              </Field>
            </div>

            <Field label="可用模型列表 (models)">
              <div className="flex flex-col gap-2">
                {models.map((m, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <input
                      value={m}
                      onChange={(e) => {
                        const next = models.slice()
                        next[i] = e.target.value
                        setField("models", next)
                      }}
                      className="input flex-1 font-mono"
                    />
                    <button
                      onClick={() => setField("models", models.filter((_, j) => j !== i))}
                      className="rounded-lg border p-2 text-muted-foreground transition-colors hover:bg-destructive/15 hover:text-destructive"
                      title="删除"
                    >
                      <TrashIcon width={15} height={15} />
                    </button>
                  </div>
                ))}
                <button
                  onClick={() => setField("models", [...models, ""])}
                  className="flex w-fit items-center gap-1.5 rounded-lg border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
                >
                  <PlusIcon width={14} height={14} /> 添加模型
                </button>
              </div>
            </Field>

            <p className="rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning-foreground">
              安全提示：API 密钥保存在边缘节点的 settings.json。生产环境请通过 Kubernetes Secret 或节点本地文件注入，不要写入 ConfigMap 模板。
            </p>
          </div>
        )}
      </div>
    </div>
  )
}

function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: React.ReactNode
}) {
  return (
    <label className="flex flex-col gap-1.5">
      <span className="text-sm font-medium text-foreground">{label}</span>
      {children}
      {hint && <span className="text-xs text-muted-foreground">{hint}</span>}
    </label>
  )
}
