"use client"

import { useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { FileIcon, DownloadIcon, SaveIcon } from "./icons"

export function FilesPanel({
  session,
  target,
  sessionId,
}: {
  session: Session
  target: Target
  sessionId: string
}) {
  const [path, setPath] = useState(`/root/ws/${sessionId}/`)
  const [content, setContent] = useState("")
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null)
  const [busy, setBusy] = useState(false)

  async function read() {
    if (!path.trim() || busy) return
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
          arguments: { path: path.trim() },
        },
        (ev) => {
          if (ev.type === "output") buf += ev.data
          else if (ev.type === "result") buf += extractText(ev.result)
          else if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
        },
      )
      setContent(buf)
      if (buf) setStatus({ kind: "ok", text: "读取成功" })
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }

  async function write() {
    if (!path.trim() || busy) return
    setBusy(true)
    setStatus(null)
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.fileWrite,
          arguments: { path: path.trim(), content },
        },
        (ev) => {
          if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
          else if (ev.type === "result") setStatus({ kind: "ok", text: "写入成功" })
        },
      )
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex h-full flex-col p-4">
      <div className="mb-3 flex items-center gap-2">
        <span className="font-mono text-sm text-muted-foreground">路径</span>
        <input
          value={path}
          onChange={(e) => setPath(e.target.value)}
          placeholder="/root/ws/<session>/deploy.sh"
          className="flex-1 rounded-lg border bg-background px-3 py-2 font-mono text-sm text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
        />
        <button
          onClick={read}
          disabled={busy}
          className="flex items-center gap-1.5 rounded-lg border px-3 py-2 text-sm text-foreground transition-colors hover:bg-muted disabled:opacity-50"
        >
          <DownloadIcon width={16} height={16} /> 读取
        </button>
        <button
          onClick={write}
          disabled={busy}
          className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
        >
          <SaveIcon width={16} height={16} /> 写入
        </button>
      </div>

      {status && (
        <p
          className={`mb-3 rounded-lg px-3 py-2 text-sm ${
            status.kind === "ok"
              ? "bg-success/15 text-success"
              : "bg-destructive/15 text-destructive"
          }`}
        >
          {status.text}
        </p>
      )}

      <div className="relative flex-1">
        <textarea
          value={content}
          onChange={(e) => setContent(e.target.value)}
          placeholder="文件内容（仅适用于小文本文件；大文件请用 push/pull）"
          className="h-full w-full resize-none rounded-lg border bg-background p-3 font-mono text-sm leading-relaxed text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
        />
        {!content && (
          <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center gap-2 text-muted-foreground">
            <FileIcon width={28} height={28} />
            <p className="text-sm">doops_file_read / doops_file_write · 需要 read / write 权限</p>
          </div>
        )}
      </div>
    </div>
  )
}
