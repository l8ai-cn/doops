"use client"

import { useCallback, useEffect, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import {
  FileIcon,
  FolderIcon,
  SaveIcon,
  RefreshIcon,
  PlusIcon,
  ChevronRightIcon,
} from "./icons"

interface Entry {
  name: string
  isDir: boolean
}

function joinPath(dir: string, name: string) {
  return dir.replace(/\/+$/, "") + "/" + name
}

function parentDir(dir: string) {
  const trimmed = dir.replace(/\/+$/, "")
  const idx = trimmed.lastIndexOf("/")
  return idx <= 0 ? "/" : trimmed.slice(0, idx)
}

export function FilesPanel({
  session,
  target,
  sessionId,
}: {
  session: Session
  target: Target
  sessionId: string
}) {
  const [cwd, setCwd] = useState(`/root/ws/${sessionId}`)
  const [entries, setEntries] = useState<Entry[]>([])
  const [listBusy, setListBusy] = useState(false)
  const [listErr, setListErr] = useState("")

  const [openPath, setOpenPath] = useState<string | null>(null)
  const [content, setContent] = useState("")
  const [dirty, setDirty] = useState(false)
  const [busy, setBusy] = useState(false)
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null)

  const list = useCallback(
    async (dir: string) => {
      setListBusy(true)
      setListErr("")
      let buf = ""
      try {
        await callTool(
          session,
          {
            cluster: target.cluster,
            instance: target.instance,
            tool: TOOLS.shell,
            arguments: { session_id: sessionId, command: `ls -1Ap "${dir}"` },
          },
          (ev) => {
            if (ev.type === "output") buf += ev.data
            else if (ev.type === "result") buf += extractText(ev.result)
            else if (ev.type === "error") setListErr(ev.error)
          },
        )
        const lines = buf
          .split("\n")
          .map((l) => l.trim())
          .filter((l) => l && !l.startsWith("exit code") && !l.startsWith("$"))
        if (lines.some((l) => l.includes("No such file") || l.includes("没有那个文件"))) {
          setListErr(lines.join(" "))
          setEntries([])
        } else {
          const parsed: Entry[] = lines.map((l) => ({
            name: l.replace(/\/$/, ""),
            isDir: l.endsWith("/"),
          }))
          parsed.sort((a, b) => (a.isDir === b.isDir ? a.name.localeCompare(b.name) : a.isDir ? -1 : 1))
          setEntries(parsed)
        }
        setCwd(dir)
      } catch (err) {
        setListErr((err as Error).message)
      } finally {
        setListBusy(false)
      }
    },
    [session, target, sessionId],
  )

  useEffect(() => {
    list(`/root/ws/${sessionId}`)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId])

  async function openFile(path: string) {
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
          arguments: { session_id: sessionId, path },
        },
        (ev) => {
          if (ev.type === "output") buf += ev.data
          else if (ev.type === "result") buf += extractText(ev.result)
          else if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
        },
      )
      setOpenPath(path)
      setContent(buf)
      setDirty(false)
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }

  async function save() {
    if (!openPath || busy) return
    setBusy(true)
    setStatus(null)
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.fileWrite,
          arguments: { session_id: sessionId, path: openPath, content },
        },
        (ev) => {
          if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
          else if (ev.type === "result") {
            setStatus({ kind: "ok", text: "写入成功" })
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

  function newFile() {
    const name = prompt("新建文件名（相对当前目录）")
    if (!name) return
    setOpenPath(joinPath(cwd, name))
    setContent("")
    setDirty(true)
    setStatus({ kind: "ok", text: "输入内容后点击保存即可创建" })
  }

  const crumbs = cwd.split("/").filter(Boolean)

  return (
    <div className="flex h-full min-h-0">
      {/* 文件浏览器 */}
      <aside className="flex w-64 shrink-0 flex-col border-r bg-card/40">
        <div className="flex items-center justify-between gap-1 border-b px-3 py-2">
          <span className="text-xs font-medium text-muted-foreground">文件浏览器</span>
          <div className="flex items-center gap-1">
            <button
              onClick={newFile}
              title="新建文件"
              className="rounded p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            >
              <PlusIcon width={15} height={15} />
            </button>
            <button
              onClick={() => list(cwd)}
              title="刷新"
              className="rounded p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            >
              <RefreshIcon width={15} height={15} className={listBusy ? "animate-spin" : ""} />
            </button>
          </div>
        </div>

        <div className="flex items-center gap-0.5 overflow-x-auto border-b px-2 py-1.5 text-xs text-muted-foreground">
          <button onClick={() => list("/")} className="shrink-0 hover:text-foreground">
            /
          </button>
          {crumbs.map((c, i) => {
            const path = "/" + crumbs.slice(0, i + 1).join("/")
            return (
              <span key={path} className="flex shrink-0 items-center">
                <button onClick={() => list(path)} className="hover:text-foreground">
                  {c}
                </button>
                {i < crumbs.length - 1 && <span className="px-0.5">/</span>}
              </span>
            )
          })}
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto p-1">
          {cwd !== "/" && (
            <button
              onClick={() => list(parentDir(cwd))}
              className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-muted"
            >
              <FolderIcon width={15} height={15} /> ..
            </button>
          )}
          {listErr && <p className="px-2 py-2 text-xs text-destructive">{listErr}</p>}
          {!listErr && entries.length === 0 && !listBusy && (
            <p className="px-2 py-2 text-xs text-muted-foreground">空目录</p>
          )}
          {entries.map((e) => {
            const full = joinPath(cwd, e.name)
            const active = openPath === full
            return (
              <button
                key={e.name}
                onClick={() => (e.isDir ? list(full) : openFile(full))}
                className={`flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm transition-colors ${
                  active ? "bg-primary/15 text-foreground" : "text-foreground/90 hover:bg-muted"
                }`}
              >
                {e.isDir ? (
                  <FolderIcon width={15} height={15} className="shrink-0 text-primary" />
                ) : (
                  <FileIcon width={15} height={15} className="shrink-0 text-muted-foreground" />
                )}
                <span className="truncate">{e.name}</span>
                {e.isDir && <ChevronRightIcon width={13} height={13} className="ml-auto shrink-0 text-muted-foreground" />}
              </button>
            )
          })}
        </div>
      </aside>

      {/* 编辑器 */}
      <section className="flex min-w-0 flex-1 flex-col">
        <div className="flex shrink-0 items-center justify-between gap-2 border-b bg-card px-3 py-2">
          <span className="truncate font-mono text-xs text-muted-foreground">
            {openPath || "未打开文件"}
            {dirty && <span className="ml-1 text-warning">●</span>}
          </span>
          <button
            onClick={save}
            disabled={!openPath || busy || !dirty}
            className="flex shrink-0 items-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
          >
            <SaveIcon width={15} height={15} /> 保存
          </button>
        </div>

        {status && (
          <p
            className={`mx-3 mt-2 rounded-lg px-3 py-2 text-sm ${
              status.kind === "ok" ? "bg-success/15 text-success" : "bg-destructive/15 text-destructive"
            }`}
          >
            {status.text}
          </p>
        )}

        <div className="relative min-h-0 flex-1 p-3">
          {openPath ? (
            <textarea
              value={content}
              onChange={(e) => {
                setContent(e.target.value)
                setDirty(true)
              }}
              spellCheck={false}
              className="h-full w-full resize-none rounded-lg border bg-background p-3 font-mono text-sm leading-relaxed text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
            />
          ) : (
            <div className="flex h-full flex-col items-center justify-center gap-2 text-muted-foreground">
              <FileIcon width={28} height={28} />
              <p className="text-sm text-pretty text-center">从左侧选择文件打开编辑</p>
              <p className="text-xs">doops_file_read / doops_file_write · 仅适用于小文本，大文件请用 push/pull</p>
            </div>
          )}
        </div>
      </section>
    </div>
  )
}
