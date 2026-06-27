"use client"

import { useCallback, useEffect, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import {
  loadConfigs,
  upsertConfig,
  removeConfig,
  newConfigId,
  applyConfigToNode,
  markPublished,
  configKind,
  configTargetPath,
  hasUnpublishedChanges,
  type ModelConfig,
} from "@/lib/model-configs"
import {
  FileTextIcon,
  SaveIcon,
  RefreshIcon,
  PlusIcon,
  TrashIcon,
  RocketIcon,
  CheckIcon,
  LayersIcon,
} from "./icons"

// 通用配置文件管理：编辑节点上任意配置文件并应用/发布。
// 注意：大模型配置不在此处，请到「管理 → 配置中心」维护模型。
const QUICK_PATHS = [
  "/etc/nginx/conf.d/doops.conf",
  "/etc/hosts",
  "/etc/environment",
  "/root/.agent/settings.json",
]

function formatFromPath(path: string): string {
  const p = path.toLowerCase()
  if (p.endsWith(".json")) return "json"
  if (p.endsWith(".yaml") || p.endsWith(".yml")) return "yaml"
  if (p.endsWith(".env") || p.endsWith("environment")) return "env"
  if (p.endsWith(".conf") || p.endsWith(".ini") || p.endsWith(".cfg")) return "ini"
  return "text"
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
  const [path, setPath] = useState("")
  const [content, setContent] = useState("")
  const [busy, setBusy] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [dirty, setDirty] = useState(false)
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null)
  const [configs, setConfigs] = useState<ModelConfig[]>([])

  useEffect(() => {
    setConfigs(loadConfigs())
  }, [])

  const fileConfigs = configs.filter((c) => configKind(c) === "file")

  const load = useCallback(
    async (p: string) => {
      if (!p.trim()) {
        setStatus({ kind: "err", text: "请先填写要读取的文件路径" })
        return
      }
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
            arguments: { session_id: sessionId, path: p },
          },
          (ev) => {
            if (ev.type === "output") buf += ev.data
            else if (ev.type === "result") buf += extractText(ev.result)
            else if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
          },
        )
        setContent(buf)
        setLoaded(true)
        setDirty(false)
        setStatus({ kind: "ok", text: `已读取 ${p}` })
      } catch (err) {
        setStatus({ kind: "err", text: (err as Error).message })
      } finally {
        setBusy(false)
      }
    },
    [session, target, sessionId],
  )

  async function saveToNode() {
    if (busy || !path.trim()) return
    setBusy(true)
    setStatus(null)
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.fileWrite,
          arguments: { session_id: sessionId, path, content },
        },
        (ev) => {
          if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
          else if (ev.type === "result") {
            setStatus({ kind: "ok", text: `已应用并写入 ${path}` })
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

  function openPath(p: string) {
    setPath(p)
    load(p)
  }

  function saveToLibrary() {
    if (!path.trim()) {
      setStatus({ kind: "err", text: "请先填写目标路径，再另存为配置" })
      return
    }
    const name = window.prompt("为该配置文件命名（保存到配置库，可应用到其他机器）：")
    if (!name) return
    const list = upsertConfig({
      id: newConfigId(),
      name: name.trim(),
      kind: "file",
      path: path.trim(),
      format: formatFromPath(path),
      content,
      updated_at: new Date().toISOString(),
    })
    setConfigs(list)
    setStatus({ kind: "ok", text: `已保存到配置库：${name.trim()}` })
  }

  function editFromLibrary(cfg: ModelConfig) {
    setPath(configTargetPath(cfg))
    setContent(cfg.content ?? "")
    setLoaded(true)
    setDirty(true)
    setStatus({ kind: "ok", text: `已载入「${cfg.name}」到编辑器，确认后点击「应用并保存」` })
  }

  async function applyFromLibrary(cfg: ModelConfig) {
    setBusy(true)
    setStatus(null)
    try {
      await applyConfigToNode(session, target, sessionId, cfg)
      setConfigs(markPublished(cfg.id))
      setStatus({ kind: "ok", text: `已应用「${cfg.name}」到 ${target.instance}` })
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }

  function deleteFromLibrary(id: string) {
    if (!window.confirm("确定从配置库删除该配置？")) return
    setConfigs(removeConfig(id))
  }

  return (
    <div className="flex h-full min-h-0 flex-col lg:flex-row">
      {/* 主编辑区 */}
      <section className="flex min-h-0 flex-1 flex-col">
        <div className="flex shrink-0 flex-wrap items-center gap-2 border-b bg-card px-4 py-2.5">
          <FileTextIcon width={16} height={16} className="text-primary" />
          <span className="text-sm font-medium text-foreground">配置文件</span>
          {dirty && <span className="text-warning" title="有未保存改动">●</span>}
          <input
            value={path}
            onChange={(e) => setPath(e.target.value)}
            placeholder="/etc/nginx/conf.d/doops.conf"
            className="input ml-1 min-w-48 flex-1 font-mono text-xs"
            onKeyDown={(e) => {
              if (e.key === "Enter") load(path)
            }}
          />
          <button
            onClick={() => load(path)}
            disabled={busy}
            title="从节点读取"
            className="rounded-lg border p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:opacity-50"
          >
            <RefreshIcon width={15} height={15} className={busy ? "animate-spin" : ""} />
          </button>
          <button
            onClick={saveToLibrary}
            className="hidden rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted sm:block"
          >
            另存为配置
          </button>
          <button
            onClick={saveToNode}
            disabled={busy || !path.trim()}
            className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
          >
            <SaveIcon width={15} height={15} /> 应用并保存
          </button>
        </div>

        {/* 快捷路径 */}
        <div className="flex shrink-0 flex-wrap items-center gap-1.5 border-b bg-muted/30 px-4 py-2">
          <span className="text-xs text-muted-foreground">快捷打开：</span>
          {QUICK_PATHS.map((p) => (
            <button
              key={p}
              onClick={() => openPath(p)}
              className="rounded-md border bg-background px-2 py-0.5 font-mono text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            >
              {p}
            </button>
          ))}
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

        <div className="min-h-0 flex-1 p-4">
          {!loaded ? (
            <div className="flex h-full flex-col items-center justify-center gap-2 text-center text-muted-foreground">
              <FileTextIcon width={30} height={30} />
              <p className="text-sm">输入路径或点击上方快捷按钮打开配置文件</p>
              <p className="max-w-md text-xs">
                这里管理节点上的通用配置文件。AI 模型配置请前往「管理 → 配置中心」。
              </p>
            </div>
          ) : (
            <textarea
              value={content}
              onChange={(e) => {
                setContent(e.target.value)
                setDirty(true)
              }}
              spellCheck={false}
              className="h-full min-h-[20rem] w-full resize-none rounded-lg border bg-background p-3 font-mono text-sm leading-relaxed text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
            />
          )}
        </div>
      </section>

      {/* 配置库 */}
      <aside className="flex min-h-0 w-full flex-col border-t lg:w-80 lg:border-l lg:border-t-0">
        <div className="flex shrink-0 items-center justify-between gap-2 border-b bg-card px-4 py-2.5">
          <span className="flex items-center gap-1.5 text-sm font-medium text-foreground">
            <LayersIcon width={15} height={15} className="text-primary" /> 配置库
          </span>
          <button
            onClick={saveToLibrary}
            className="flex items-center gap-1 rounded-lg border px-2 py-1 text-xs text-foreground transition-colors hover:bg-muted"
          >
            <PlusIcon width={13} height={13} /> 存当前
          </button>
        </div>
        <div className="min-h-0 flex-1 overflow-auto p-3">
          {fileConfigs.length === 0 ? (
            <p className="px-1 py-8 text-center text-xs text-muted-foreground">
              暂无可复用的配置文件。编辑后点击「另存为配置」即可保存，方便应用到其他机器。
            </p>
          ) : (
            <ul className="flex flex-col gap-2">
              {fileConfigs.map((c) => {
                const unpublished = hasUnpublishedChanges(c)
                return (
                  <li key={c.id} className="rounded-lg border bg-card px-3 py-2.5">
                    <div className="flex items-center justify-between gap-2">
                      <p className="truncate text-sm font-medium text-foreground">{c.name}</p>
                      <span
                        className={`shrink-0 rounded px-1.5 py-0.5 text-[10px] font-medium ${
                          unpublished ? "bg-warning/15 text-warning" : "bg-success/15 text-success"
                        }`}
                        title={c.published_at ? `最近应用：${new Date(c.published_at).toLocaleString()}` : "尚未应用"}
                      >
                        {unpublished ? (c.published_at ? "有改动" : "未应用") : "已应用"}
                      </span>
                    </div>
                    <p className="mt-0.5 truncate font-mono text-xs text-muted-foreground">
                      {configTargetPath(c) || "未设置路径"}
                    </p>
                    <div className="mt-2 flex items-center gap-1.5">
                      <button
                        onClick={() => applyFromLibrary(c)}
                        disabled={busy}
                        className="flex items-center gap-1 rounded-md bg-primary px-2 py-1 text-xs font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
                      >
                        <RocketIcon width={12} height={12} /> 应用到本机
                      </button>
                      <button
                        onClick={() => editFromLibrary(c)}
                        className="rounded-md border px-2 py-1 text-xs text-foreground transition-colors hover:bg-muted"
                      >
                        载入编辑
                      </button>
                      <button
                        onClick={() => deleteFromLibrary(c.id)}
                        className="ml-auto rounded-md border border-destructive/40 px-1.5 py-1 text-destructive transition-colors hover:bg-destructive/10"
                        aria-label="删除配置"
                      >
                        <TrashIcon width={13} height={13} />
                      </button>
                    </div>
                  </li>
                )
              })}
            </ul>
          )}
          <p className="mt-3 flex items-start gap-1.5 rounded-lg border border-primary/20 bg-primary/5 px-2.5 py-2 text-[11px] leading-relaxed text-muted-foreground">
            <CheckIcon width={13} height={13} className="mt-0.5 shrink-0 text-primary" />
            <span>AI 大模型配置已独立到「管理 → 配置中心」，这里只负责通用配置文件。</span>
          </p>
        </div>
      </aside>
    </div>
  )
}
