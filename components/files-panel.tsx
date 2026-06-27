"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import {
  FileIcon,
  FolderIcon,
  SaveIcon,
  RefreshIcon,
  PlusIcon,
  ChevronRightIcon,
  UploadIcon,
  FolderUpIcon,
  CopyIcon,
  CheckIcon,
} from "./icons"

interface Entry {
  name: string
  isDir: boolean
}

interface UploadProgress {
  total: number
  done: number
  current: string
  failed: number
  active: boolean
}

function joinPath(dir: string, name: string) {
  return dir.replace(/\/+$/, "") + "/" + name
}

function parentDir(dir: string) {
  const trimmed = dir.replace(/\/+$/, "")
  const idx = trimmed.lastIndexOf("/")
  return idx <= 0 ? "/" : trimmed.slice(0, idx)
}

// shell 安全转义（单引号包裹）
function shq(s: string) {
  return "'" + s.replace(/'/g, "'\\''") + "'"
}

// 大文件 / 二进制不适合走 file_write，建议用 doops push
const MAX_INLINE_BYTES = 256 * 1024
const TEXT_EXT = /\.(txt|md|json|ya?ml|toml|ini|conf|cfg|env|sh|bash|zsh|py|js|ts|tsx|jsx|go|rs|java|c|h|cpp|cs|rb|php|sql|html|css|scss|xml|csv|log|gitignore|dockerfile|properties|lock)$/i

function looksTextual(file: File) {
  if (file.size > MAX_INLINE_BYTES) return false
  if (TEXT_EXT.test(file.name)) return true
  // 无扩展名的小文件（如 Dockerfile、Makefile）也按文本尝试
  return !/\.[a-z0-9]+$/i.test(file.name) || file.type.startsWith("text/") || file.type === "application/json"
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

  const [upload, setUpload] = useState<UploadProgress | null>(null)
  const [showCmd, setShowCmd] = useState(false)
  const [copied, setCopied] = useState(false)
  const fileInputRef = useRef<HTMLInputElement>(null)
  const dirInputRef = useRef<HTMLInputElement>(null)

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

  // 通过 doops_file_write 逐个写入文件，子目录先 mkdir -p
  async function writeRemoteFile(path: string, text: string) {
    let err: string | null = null
    await callTool(
      session,
      {
        cluster: target.cluster,
        instance: target.instance,
        tool: TOOLS.fileWrite,
        arguments: { session_id: sessionId, path, content: text },
      },
      (ev) => {
        if (ev.type === "error") err = ev.error
      },
    )
    if (err) throw new Error(err)
  }

  async function mkdirRemote(dir: string) {
    await callTool(
      session,
      {
        cluster: target.cluster,
        instance: target.instance,
        tool: TOOLS.shell,
        arguments: { session_id: sessionId, command: `mkdir -p ${shq(dir)}` },
      },
      () => {},
    )
  }

  // 一键上传：浏览器选择的文件/文件夹，逐个写入当前目录（仅小文本文件）
  async function handleUpload(fileList: FileList | null) {
    if (!fileList || fileList.length === 0) return
    const files = Array.from(fileList)
    const skipped: string[] = []
    const dirsToMake = new Set<string>()

    // 预先计算需要创建的子目录（文件夹上传时 webkitRelativePath 带路径）
    for (const f of files) {
      const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name
      const slash = rel.lastIndexOf("/")
      if (slash > 0) dirsToMake.add(joinPath(cwd, rel.slice(0, slash)))
    }

    setStatus(null)
    setUpload({ total: files.length, done: 0, current: "", failed: 0, active: true })

    // 先创建目录树（按路径深度排序，浅的先建）
    for (const d of Array.from(dirsToMake).sort((a, b) => a.length - b.length)) {
      try {
        await mkdirRemote(d)
      } catch {
        /* 忽略，写文件时若失败再计入 */
      }
    }

    let done = 0
    let failed = 0
    for (const f of files) {
      const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name
      const dest = joinPath(cwd, rel)
      setUpload((p) => (p ? { ...p, current: rel } : p))
      if (!looksTextual(f)) {
        skipped.push(rel)
        failed++
        done++
        setUpload((p) => (p ? { ...p, done, failed } : p))
        continue
      }
      try {
        const text = await f.text()
        await writeRemoteFile(dest, text)
      } catch {
        failed++
      }
      done++
      setUpload((p) => (p ? { ...p, done, failed } : p))
    }

    setUpload((p) => (p ? { ...p, active: false, current: "" } : p))
    const ok = done - failed
    if (skipped.length > 0) {
      setStatus({
        kind: ok > 0 ? "ok" : "err",
        text: `已上传 ${ok}/${files.length} 个文本文件。${skipped.length} 个大文件/二进制被跳过，请用下方「复制上传命令」走 doops push。`,
      })
    } else {
      setStatus({
        kind: failed === 0 ? "ok" : "err",
        text: `上传完成：成功 ${ok}，失败 ${failed}。`,
      })
    }
    await list(cwd)
    // 清空 input，便于重复选择同一文件
    if (fileInputRef.current) fileInputRef.current.value = ""
    if (dirInputRef.current) dirInputRef.current.value = ""
  }

  const pushCommand = `doops push \\
  --cluster ${target.cluster} \\
  --instance ${target.instance} \\
  -session ${sessionId} \\
  --src ./your-folder`

  async function copyCommand() {
    try {
      await navigator.clipboard.writeText(pushCommand.replace(/\\\n\s*/g, " "))
      setCopied(true)
      setTimeout(() => setCopied(false), 1800)
    } catch {
      setStatus({ kind: "err", text: "复制失败，请手动选择命令复制" })
    }
  }

  const crumbs = cwd.split("/").filter(Boolean)

  return (
    <div className="flex h-full min-h-0 flex-col">
      {/* 上传工具条 */}
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-b bg-card/60 px-3 py-2">
        <span className="text-xs font-medium text-muted-foreground">上传到</span>
        <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-foreground">{cwd}</code>
        <div className="ml-auto flex flex-wrap items-center gap-1.5">
          <input
            ref={fileInputRef}
            type="file"
            multiple
            className="hidden"
            onChange={(e) => handleUpload(e.target.files)}
          />
          <input
            ref={dirInputRef}
            type="file"
            className="hidden"
            // @ts-expect-error 非标准但被主流浏览器支持
            webkitdirectory=""
            directory=""
            multiple
            onChange={(e) => handleUpload(e.target.files)}
          />
          <button
            onClick={() => fileInputRef.current?.click()}
            disabled={upload?.active}
            className="flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted disabled:opacity-50"
          >
            <UploadIcon width={14} height={14} /> 上传文件
          </button>
          <button
            onClick={() => dirInputRef.current?.click()}
            disabled={upload?.active}
            className="flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted disabled:opacity-50"
          >
            <FolderUpIcon width={14} height={14} /> 上传文件夹
          </button>
          <button
            onClick={() => setShowCmd((v) => !v)}
            className={`flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs transition-colors hover:bg-muted ${
              showCmd ? "border-primary/50 text-foreground" : "text-foreground"
            }`}
          >
            <CopyIcon width={14} height={14} /> 上传命令
          </button>
        </div>
      </div>

      {/* push 命令面板 */}
      {showCmd && (
        <div className="shrink-0 border-b bg-muted/30 px-3 py-3">
          <div className="mb-1.5 flex items-center justify-between">
            <p className="text-xs text-muted-foreground">
              大文件 / 整个目录用 CLI 极速同步（Git HTTP），上传到 <code className="font-mono">/root/ws/{sessionId}</code>
            </p>
            <button
              onClick={copyCommand}
              className="flex items-center gap-1 rounded-md bg-primary px-2 py-1 text-xs font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              {copied ? <CheckIcon width={13} height={13} /> : <CopyIcon width={13} height={13} />}
              {copied ? "已复制" : "复制命令"}
            </button>
          </div>
          <pre className="overflow-x-auto rounded-lg border bg-background p-3 font-mono text-xs leading-relaxed text-foreground">
            {pushCommand}
          </pre>
        </div>
      )}

      {/* 上传进度 */}
      {upload && (
        <div className="shrink-0 border-b bg-card px-3 py-2">
          <div className="flex items-center justify-between text-xs">
            <span className="text-foreground">
              {upload.active ? "上传中…" : "上传结束"} {upload.done}/{upload.total}
              {upload.failed > 0 && <span className="ml-1 text-destructive">（{upload.failed} 失败/跳过）</span>}
            </span>
            <span className="truncate pl-2 font-mono text-muted-foreground">{upload.current}</span>
          </div>
          <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted">
            <div
              className="h-full rounded-full bg-primary transition-all"
              style={{ width: `${upload.total ? (upload.done / upload.total) * 100 : 0}%` }}
            />
          </div>
        </div>
      )}

      <div className="flex min-h-0 flex-1">
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
                  {e.isDir && (
                    <ChevronRightIcon width={13} height={13} className="ml-auto shrink-0 text-muted-foreground" />
                  )}
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
                <p className="text-pretty text-center text-sm">从左侧选择文件打开编辑，或用上方按钮上传文件/文件夹</p>
                <p className="text-xs">小文本走 file_write 一键上传 · 大文件/目录请复制 doops push 命令</p>
              </div>
            )}
          </div>
        </section>
      </div>
    </div>
  )
}
