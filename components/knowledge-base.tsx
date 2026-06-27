"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { Markdown } from "./markdown"
import {
  FileTextIcon,
  SaveIcon,
  RefreshIcon,
  PlusIcon,
  TrashIcon,
  CheckIcon,
} from "./icons"

// 知识库根目录（持久化，不随会话工作区清理）
const KB_ROOT = "/root/kb"
type Mode = "edit" | "preview" | "split"

function shq(s: string) {
  return "'" + s.replace(/'/g, "'\\''") + "'"
}

export function KnowledgeBase({
  session,
  target,
  sessionId,
}: {
  session: Session
  target: Target
  sessionId: string
}) {
  const [docs, setDocs] = useState<string[]>([])
  const [listBusy, setListBusy] = useState(false)
  const [listErr, setListErr] = useState("")

  const [openDoc, setOpenDoc] = useState<string | null>(null)
  const [content, setContent] = useState("")
  const [dirty, setDirty] = useState(false)
  const [busy, setBusy] = useState(false)
  const [mode, setMode] = useState<Mode>("split")
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null)
  const taRef = useRef<HTMLTextAreaElement>(null)

  const docPath = useCallback((name: string) => `${KB_ROOT}/${name}`, [])

  const refresh = useCallback(async () => {
    setListBusy(true)
    setListErr("")
    let buf = ""
    try {
      // 确保目录存在后再列举
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.shell,
          arguments: { session_id: sessionId, command: `mkdir -p ${shq(KB_ROOT)}; ls -1Ap ${shq(KB_ROOT)}` },
        },
        (ev) => {
          if (ev.type === "output") buf += ev.data
          else if (ev.type === "result") buf += extractText(ev.result)
          else if (ev.type === "error") setListErr(ev.error)
        },
      )
      const names = buf
        .split("\n")
        .map((l) => l.trim())
        .filter((l) => l && !l.startsWith("exit code") && !l.startsWith("$"))
        .filter((l) => !l.endsWith("/") && /\.md$/i.test(l))
      setDocs(names.sort((a, b) => a.localeCompare(b)))
    } catch (err) {
      setListErr((err as Error).message)
    } finally {
      setListBusy(false)
    }
  }, [session, target.cluster, target.instance, sessionId])

  useEffect(() => {
    refresh()
  }, [refresh])

  async function openFile(name: string) {
    if (dirty && !confirm("当前文档有未保存的修改，确定切换吗？")) return
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
          arguments: { session_id: sessionId, path: docPath(name) },
        },
        (ev) => {
          if (ev.type === "output") buf += ev.data
          else if (ev.type === "result") buf += extractText(ev.result)
          else if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
        },
      )
      setOpenDoc(name)
      setContent(buf)
      setDirty(false)
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }

  async function save() {
    if (!openDoc || busy) return
    setBusy(true)
    setStatus(null)
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.fileWrite,
          arguments: { session_id: sessionId, path: docPath(openDoc), content },
        },
        (ev) => {
          if (ev.type === "error") setStatus({ kind: "err", text: ev.error })
          else if (ev.type === "result") {
            setStatus({ kind: "ok", text: "已保存" })
            setDirty(false)
          }
        },
      )
      // 新建文档保存后刷新列表
      if (!docs.includes(openDoc)) refresh()
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }

  function newDoc() {
    let name = prompt("新建文档名称（自动补全 .md 后缀）")
    if (!name) return
    name = name.trim()
    if (!/\.md$/i.test(name)) name += ".md"
    if (docs.includes(name)) {
      openFile(name)
      return
    }
    setOpenDoc(name)
    setContent(`# ${name.replace(/\.md$/i, "")}\n\n`)
    setDirty(true)
    setMode("split")
    setStatus({ kind: "ok", text: "编辑后点击保存即可创建" })
    setTimeout(() => taRef.current?.focus(), 0)
  }

  async function deleteDoc(name: string) {
    if (!confirm(`确定删除文档「${name}」？此操作不可恢复。`)) return
    setBusy(true)
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.shell,
          arguments: { session_id: sessionId, command: `rm -f ${shq(docPath(name))}` },
        },
        () => {},
      )
      if (openDoc === name) {
        setOpenDoc(null)
        setContent("")
        setDirty(false)
      }
      refresh()
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setBusy(false)
    }
  }

  const modeBtn = (m: Mode, label: string) => (
    <button
      onClick={() => setMode(m)}
      className={`rounded-md px-2.5 py-1 text-xs font-medium transition-colors ${
        mode === m ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:bg-muted hover:text-foreground"
      }`}
    >
      {label}
    </button>
  )

  return (
    <div className="flex h-full min-h-0">
      {/* 文档列表 */}
      <aside className="flex w-56 shrink-0 flex-col border-r bg-card/40">
        <div className="flex items-center justify-between gap-1 border-b px-3 py-2">
          <span className="flex items-center gap-1.5 text-xs font-semibold text-foreground">
            <FileTextIcon width={14} height={14} className="text-primary" />
            知识库
          </span>
          <div className="flex items-center gap-0.5">
            <button
              onClick={newDoc}
              className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              title="新建文档"
            >
              <PlusIcon width={15} height={15} />
            </button>
            <button
              onClick={refresh}
              className="rounded-md p-1 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              title="刷新"
            >
              <RefreshIcon width={14} height={14} className={listBusy ? "animate-spin" : ""} />
            </button>
          </div>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto p-1.5">
          {listErr ? (
            <p className="px-2 py-3 text-xs text-destructive">{listErr}</p>
          ) : docs.length === 0 ? (
            <p className="px-2 py-3 text-xs text-muted-foreground">
              {listBusy ? "加载中…" : "暂无文档，点击 + 新建"}
            </p>
          ) : (
            docs.map((name) => (
              <div
                key={name}
                className={`group flex items-center gap-1.5 rounded-md px-2 py-1.5 text-sm transition-colors ${
                  openDoc === name ? "bg-primary/10 text-foreground" : "text-muted-foreground hover:bg-muted"
                }`}
              >
                <button
                  onClick={() => openFile(name)}
                  className="flex min-w-0 flex-1 items-center gap-1.5 text-left"
                >
                  <FileTextIcon width={13} height={13} className="shrink-0" />
                  <span className="truncate">{name.replace(/\.md$/i, "")}</span>
                </button>
                <button
                  onClick={() => deleteDoc(name)}
                  className="shrink-0 rounded p-0.5 text-muted-foreground opacity-0 transition-opacity hover:text-destructive group-hover:opacity-100"
                  title="删除"
                >
                  <TrashIcon width={13} height={13} />
                </button>
              </div>
            ))
          )}
        </div>
      </aside>

      {/* 编辑区 */}
      <div className="flex min-w-0 flex-1 flex-col">
        {openDoc ? (
          <>
            <div className="flex shrink-0 flex-wrap items-center gap-2 border-b bg-card px-3 py-2">
              <span className="flex items-center gap-1.5 text-sm font-medium text-foreground">
                {openDoc.replace(/\.md$/i, "")}
                {dirty && <span className="h-1.5 w-1.5 rounded-full bg-primary" title="未保存" />}
              </span>
              <div className="ml-auto flex items-center gap-2">
                <div className="flex items-center gap-0.5 rounded-lg border bg-muted/40 p-0.5">
                  {modeBtn("edit", "编辑")}
                  {modeBtn("split", "分屏")}
                  {modeBtn("preview", "预览")}
                </div>
                {status && (
                  <span
                    className={`flex items-center gap-1 text-xs ${
                      status.kind === "ok" ? "text-success" : "text-destructive"
                    }`}
                  >
                    {status.kind === "ok" && <CheckIcon width={12} height={12} />}
                    {status.text}
                  </span>
                )}
                <button
                  onClick={save}
                  disabled={busy || !dirty}
                  className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
                >
                  <SaveIcon width={14} height={14} /> 保存
                </button>
              </div>
            </div>

            <div className="flex min-h-0 flex-1">
              {(mode === "edit" || mode === "split") && (
                <textarea
                  ref={taRef}
                  value={content}
                  onChange={(e) => {
                    setContent(e.target.value)
                    setDirty(true)
                  }}
                  spellCheck={false}
                  placeholder="在此输入 Markdown 内容…"
                  className={`min-h-0 flex-1 resize-none bg-background p-4 font-mono text-sm leading-relaxed text-foreground outline-none ${
                    mode === "split" ? "border-r" : ""
                  }`}
                />
              )}
              {(mode === "preview" || mode === "split") && (
                <div className="min-h-0 flex-1 overflow-y-auto bg-background p-4">
                  {content.trim() ? (
                    <Markdown content={content} />
                  ) : (
                    <p className="text-sm text-muted-foreground">预览区：开始输入即可看到渲染效果</p>
                  )}
                </div>
              )}
            </div>
          </>
        ) : (
          <div className="flex flex-1 flex-col items-center justify-center gap-3 px-4 text-center text-muted-foreground">
            <div className="flex h-12 w-12 items-center justify-center rounded-xl bg-primary/10 text-primary">
              <FileTextIcon width={26} height={26} />
            </div>
            <p className="max-w-sm text-pretty text-sm">
              在 {target.instance} 上在线维护 Markdown 知识库文档，支持边写边预览
            </p>
            <button
              onClick={newDoc}
              className="flex items-center gap-1.5 rounded-lg bg-primary px-3.5 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              <PlusIcon width={15} height={15} /> 新建文档
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
