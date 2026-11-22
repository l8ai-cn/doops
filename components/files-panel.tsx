"use client"

import { Button } from "./ui"
import { useCallback, useEffect, useRef, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import {
  joinWorkspacePath,
  workspaceRelativePath,
  workspaceRootPath,
} from "@/lib/workspace-path"
import { listDirectoryCommand, parseDirectoryListing } from "@/lib/files-listing"
import {
  checkDeploymentRequest,
  cleanWorkspaceRequest,
  EMPTY_CHECK_INPUT,
  EMPTY_MAINTENANCE_LOG,
  maintenanceFailure,
  maintenanceOutput,
  maintenanceResultText,
  type CheckDeploymentInput,
  type MaintenanceKind,
  type MaintenanceLog,
} from "@/lib/files-maintenance"
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
  RocketIcon,
  TrashIcon,
} from "./icons"
import { useAppDialog } from "./app-dialog"
import { AgentPathsLoader } from "./agent-paths"

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

export function FilesPanel(props: {
  session: Session
  target: Target
  sessionId: string
  sessionDraft?: string
  onSessionChange?: (id: string) => void
  onSessionCommit?: (id: string) => void
}) {
  return (
    <div className="flex h-full min-h-0 flex-col">
      {props.onSessionChange && (
        <label className="flex shrink-0 items-center gap-1.5 border-b px-4 py-2 text-xs text-muted-foreground">
          session
          <input
            value={props.sessionDraft ?? props.sessionId}
            onChange={(e) => props.onSessionChange?.(e.target.value)}
            onBlur={(e) => props.onSessionCommit?.(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") props.onSessionCommit?.(e.currentTarget.value) }}
            className="w-40 rounded-md border bg-background px-2 py-1 font-mono text-xs text-foreground outline-none focus:border-ring"
          />
        </label>
      )}
      <AgentPathsLoader session={props.session} target={props.target}>
        {(paths) => <FilesPanelContent key={JSON.stringify([props.session.gateway, props.target.cluster, props.target.instance, props.sessionId, paths])} {...props} base={paths.workspace_root} />}
      </AgentPathsLoader>
    </div>
  )
}

function FilesPanelContent({
  session,
  target,
  sessionId,
  base,
}: {
  session: Session
  target: Target
  sessionId: string
  base: string
}) {
  const [cwd, setCwd] = useState(() => workspaceRootPath(sessionId, base) || "")
  const workspaceRoot = workspaceRootPath(sessionId, base)
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
  const [maint, setMaint] = useState<({ tool: MaintenanceKind; running: boolean } & MaintenanceLog) | null>(null)
  const [checkOpen, setCheckOpen] = useState(false)
  const [checkInput, setCheckInput] = useState<CheckDeploymentInput>(EMPTY_CHECK_INPUT)
  const [checkErr, setCheckErr] = useState("")
  const fileInputRef = useRef<HTMLInputElement>(null)
  const dirInputRef = useRef<HTMLInputElement>(null)
  const { prompt, dialog } = useAppDialog()

  const list = useCallback(
    async (dir: string) => {
      setListBusy(true)
      setListErr("")
      let buf = ""
      try {
        const safeDir = workspaceRelativePath(sessionId, dir, base)
        const safeRoot = workspaceRootPath(sessionId, base)
        if (safeDir === null || !safeRoot) {
          setEntries([])
          setListErr("工作区路径无效")
          return
        }
        const remoteDir = safeDir ? `${safeRoot}/${safeDir}` : safeRoot
        await callTool(
          session,
          {
            cluster: target.cluster,
            instance: target.instance,
            tool: TOOLS.shell,
            arguments: { session_id: sessionId, command: listDirectoryCommand(safeRoot, remoteDir) },
          },
          (ev) => {
            if (ev.type === "output") buf += ev.data
            // doops_shell streams stdout as output; the final result is only a
            // generic completion receipt and must not become a directory entry.
            else if (ev.type === "error") setListErr(ev.error)
          },
        )
        const parsed = parseDirectoryListing(buf)
        if (parsed.error) setListErr(parsed.error)
        setEntries(parsed.entries)
        setCwd(remoteDir)
      } catch (err) {
        setListErr((err as Error).message)
      } finally {
        setListBusy(false)
      }
    },
    [session, target, sessionId, base],
  )

  useEffect(() => {
    list(workspaceRoot || "")
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId])

  async function openFile(path: string) {
    setBusy(true)
    setStatus(null)
    let buf = ""
    try {
      const relativePath = workspaceRelativePath(sessionId, path, base)
      if (relativePath === null || !relativePath) {
        setStatus({ kind: "err", text: "文件路径必须位于当前工作区内" })
        return
      }
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.fileRead,
          arguments: { session_id: sessionId, path: relativePath },
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
      const relativePath = workspaceRelativePath(sessionId, openPath, base)
      if (relativePath === null || !relativePath) {
        setStatus({ kind: "err", text: "文件路径必须位于当前工作区内" })
        return
      }
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.fileWrite,
          arguments: { session_id: sessionId, path: relativePath, content },
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

  async function newFile() {
    const name = await prompt({
      title: "新建文件",
      message: "文件名相对于当前目录，保存后才会真正创建。",
      label: "文件名",
      placeholder: "notes.txt",
      confirmLabel: "创建",
    })
    if (!name) return
    const path = joinWorkspacePath(sessionId, cwd, name, base)
    if (!path) {
      setStatus({ kind: "err", text: "文件名必须位于当前工作区内" })
      return
    }
    setOpenPath(path)
    setContent("")
    setDirty(true)
    setStatus({ kind: "ok", text: "输入内容后点击保存即可创建" })
  }

  // 返回是否成功；callTool 的 error 事件和随后抛出的同一错误只记一次。
  async function runMaintenance(kind: MaintenanceKind, args: Record<string, string>) {
    setMaint({ tool: kind, ...EMPTY_MAINTENANCE_LOG, running: true })
    let failed = false
    const fail = (raw: string) => {
      failed = true
      setMaint((p) => (p ? { ...p, ...maintenanceFailure(p, kind, raw) } : p))
    }
    const tool = kind === "check" ? TOOLS.checkDeployment : TOOLS.cleanWorkspace
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool,
          arguments: args,
        },
        (ev) => {
          if (ev.type === "output") {
            setMaint((p) => (p ? { ...p, ...maintenanceOutput(p, ev.data) } : p))
          } else if (ev.type === "result") {
            const text = maintenanceResultText(kind, extractText(ev.result))
            setMaint((p) => (p ? { ...p, ...maintenanceOutput(p, text) } : p))
          } else if (ev.type === "error") {
            fail(ev.error)
          }
        },
      )
    } catch (err) {
      fail((err as Error).message)
    } finally {
      setMaint((p) => (p ? { ...p, running: false } : p))
    }
    return !failed
  }

  async function submitCheck(e: React.FormEvent) {
    e.preventDefault()
    let args: Record<string, string>
    try {
      args = checkDeploymentRequest(sessionId, checkInput)
    } catch (err) {
      setCheckErr((err as Error).message)
      return
    }
    setCheckErr("")
    setCheckOpen(false)
    await runMaintenance("check", args)
  }

  async function cleanWorkspace() {
    let args: Record<string, string>
    try {
      args = cleanWorkspaceRequest(sessionId)
    } catch (err) {
      setMaint({ tool: "clean", lines: [], errors: [(err as Error).message], running: false })
      return
    }
    setCheckOpen(false)
    if (await runMaintenance("clean", args)) {
      // 工作区已整体删除：不再展示旧目录和已打开的文件，也不重新列举（列根目录会重建空工作区）。
      setEntries([])
      setListErr("")
      setCwd(workspaceRoot || "")
      setOpenPath(null)
      setContent("")
      setDirty(false)
      setStatus(null)
    } else {
      list(cwd)
    }
  }

  // 通过 doops_file_write 逐个写入文件，子目录先 mkdir -p
  async function writeRemoteFile(path: string, text: string) {
    const relativePath = workspaceRelativePath(sessionId, path, base)
    if (relativePath === null || !relativePath) throw new Error("上传路径必须位于当前工作区内")
    let err: string | null = null
    await callTool(
      session,
      {
        cluster: target.cluster,
        instance: target.instance,
        tool: TOOLS.fileWrite,
        arguments: { session_id: sessionId, path: relativePath, content: text },
      },
      (ev) => {
        if (ev.type === "error") err = ev.error
      },
    )
    if (err) throw new Error(err)
  }

  async function mkdirRemote(dir: string) {
    const safeDir = workspaceRelativePath(sessionId, dir, base)
    const safeRoot = workspaceRootPath(sessionId, base)
    if (safeDir === null || !safeRoot) throw new Error("上传目录必须位于当前工作区内")
    const remoteDir = safeDir ? `${safeRoot}/${safeDir}` : safeRoot
    await callTool(
      session,
      {
        cluster: target.cluster,
        instance: target.instance,
        tool: TOOLS.shell,
        arguments: { session_id: sessionId, command: `mkdir -p ${shq(remoteDir)}` },
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
    const destinations = files.map((file) => {
      const rel = (file as File & { webkitRelativePath?: string }).webkitRelativePath || file.name
      return { file, rel, path: joinWorkspacePath(sessionId, cwd, rel, base) }
    })
    if (destinations.some((item) => !item.path)) {
      setStatus({ kind: "err", text: "上传路径必须位于当前工作区内" })
      if (fileInputRef.current) fileInputRef.current.value = ""
      if (dirInputRef.current) dirInputRef.current.value = ""
      return
    }
    const safeDestinations = destinations as Array<{ file: File; rel: string; path: string }>

    // 预先计算需要创建的子目录（文件夹上传时 webkitRelativePath 带路径）
    for (const { rel } of safeDestinations) {
      const slash = rel.lastIndexOf("/")
      if (slash > 0) {
        const dir = joinWorkspacePath(sessionId, cwd, rel.slice(0, slash), base)
        if (dir) dirsToMake.add(dir)
      }
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
    for (const { file: f, rel, path: dest } of safeDestinations) {
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

  const pushCommand = `doops -session ${sessionId} push \\
  --target <configured-target> \\
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

  const crumbs = (workspaceRelativePath(sessionId, cwd, base) || "").split("/").filter(Boolean)

  return (
    <div className="flex h-full min-h-0 flex-col">
      {/* 上传工具条 */}
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-b bg-card/60 px-4 py-2">
        <span className="text-xs font-medium text-muted-foreground">上传到</span>
        <code className="hidden rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground sm:inline">
          {cwd}
        </code>
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
          <Button
            onClick={() => fileInputRef.current?.click()}
            disabled={upload?.active}
            size="sm"
          >
            <UploadIcon width={14} height={14} /> 上传文件
          </Button>
          <Button
            onClick={() => dirInputRef.current?.click()}
            disabled={upload?.active}
            size="sm"
          >
            <FolderUpIcon width={14} height={14} /> 上传文件夹
          </Button>
          <button
            onClick={() => setShowCmd((v) => !v)}
            className={`flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs transition-colors hover:bg-muted ${
              showCmd ? "border-primary/50 text-foreground" : "text-foreground"
            }`}
          >
            <CopyIcon width={14} height={14} /> 上传命令
          </button>
          <span className="mx-1 h-4 w-px bg-border" aria-hidden />
          <Button
            onClick={() => {
              setCheckErr("")
              setCheckOpen((v) => !v)
            }}
            disabled={maint?.running}
            title="核对 Deployment 主容器镜像是否与期望一致"
            size="sm"
            className={checkOpen ? "border-primary/50" : ""}
          >
            <RocketIcon width={14} height={14} /> 检查部署
          </Button>
          <Button
            onClick={cleanWorkspace}
            disabled={maint?.running}
            title="删除本会话的整个工作区和 Git 裸仓库"
            size="sm"
          >
            <TrashIcon width={14} height={14} /> 清理工作区
          </Button>
        </div>
      </div>

      {/* 检查部署参数 */}
      {checkOpen && (
        <form onSubmit={submitCheck} noValidate className="shrink-0 border-b bg-muted/30 px-3 py-3">
          <p className="mb-2 text-xs text-muted-foreground">
            在目标节点用 kubectl 读取 Deployment 主容器镜像，与期望镜像比对。三项均为必填。
          </p>
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              命名空间
              <input
                name="namespace"
                value={checkInput.namespace}
                onChange={(e) => setCheckInput((p) => ({ ...p, namespace: e.target.value }))}
                className="input w-40"
                placeholder="apps"
                autoComplete="off"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              Deployment
              <input
                name="deployment"
                value={checkInput.deployment}
                onChange={(e) => setCheckInput((p) => ({ ...p, deployment: e.target.value }))}
                className="input w-44"
                placeholder="api"
                autoComplete="off"
              />
            </label>
            <label className="flex min-w-64 flex-1 flex-col gap-1 text-xs text-muted-foreground">
              期望镜像
              <input
                name="image"
                value={checkInput.image}
                onChange={(e) => setCheckInput((p) => ({ ...p, image: e.target.value }))}
                className="input w-full font-mono"
                placeholder="registry.example.com/team/api:1.2.3"
                autoComplete="off"
              />
            </label>
            <div className="flex gap-2">
              <button
                type="submit"
                disabled={maint?.running}
                className="rounded-lg bg-primary px-3 py-2 text-xs font-medium text-primary-foreground disabled:opacity-40"
              >
                开始检查
              </button>
              <button
                type="button"
                onClick={() => setCheckOpen(false)}
                className="rounded-lg border px-3 py-2 text-xs text-foreground transition-colors hover:bg-muted"
              >
                取消
              </button>
            </div>
          </div>
          {checkErr && (
            <p role="alert" className="mt-2 text-xs text-destructive">
              {checkErr}
            </p>
          )}
        </form>
      )}

      {/* 维护动作结果 */}
      {maint && (
        <div className="shrink-0 border-b bg-muted/30 px-3 py-3">
          <div className="mb-1.5 flex items-center justify-between">
            <p className="flex items-center gap-1.5 text-xs font-medium text-foreground">
              {maint.tool === "check" ? (
                <RocketIcon width={13} height={13} className="text-primary" />
              ) : (
                <TrashIcon width={13} height={13} className="text-primary" />
              )}
              {maint.tool === "check" ? "部署检查" : "工作区清理"}
              {maint.running && <span className="text-muted-foreground">· 执行中…</span>}
            </p>
            {!maint.running && (
              <button
                onClick={() => setMaint(null)}
                className="rounded-md border px-2 py-0.5 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              >
                关闭
              </button>
            )}
          </div>
          <pre className="max-h-44 overflow-y-auto whitespace-pre-wrap rounded-lg border bg-background p-3 font-mono text-xs leading-relaxed text-foreground">
            {maint.lines.join("") || (maint.running ? "正在执行…" : maint.errors.length ? "" : "已完成，无输出")}
            {maint.errors.map((e, i) => (
              <span key={i} className="block text-destructive">
                错误：{e}
              </span>
            ))}
          </pre>
        </div>
      )}

      {/* push 命令面板 */}
      {showCmd && (
        <div className="shrink-0 border-b bg-muted/30 px-3 py-3">
          <div className="mb-1.5 flex items-center justify-between">
            <p className="text-xs text-muted-foreground">
              大文件 / 整个目录用 CLI 极速同步（Git HTTP），上传到 <code className="font-mono">{workspaceRoot}</code>
            </p>
            <Button
              onClick={copyCommand}
              variant="primary" size="sm"
            >
              {copied ? <CheckIcon width={13} height={13} /> : <CopyIcon width={13} height={13} />}
              {copied ? "已复制" : "复制命令"}
            </Button>
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
          <div className="progress-track mt-1.5 h-1.5">
            <div
              className="progress-fill"
              style={{ width: `${upload.total ? (upload.done / upload.total) * 100 : 0}%` }}
            />
          </div>
        </div>
      )}

      <div className="flex min-h-0 flex-1 flex-col md:flex-row">
        {/* 文件浏览器 */}
        <aside className="flex max-h-52 w-full shrink-0 flex-col border-b bg-card/40 md:max-h-none md:w-64 md:border-r md:border-b-0">
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
            <button onClick={() => list(workspaceRoot || "")} className="shrink-0 hover:text-foreground">
              {sessionId}
            </button>
            {crumbs.map((c, i) => {
              const path = `${workspaceRoot}/${crumbs.slice(0, i + 1).join("/")}`
              return (
                <span key={path} className="flex shrink-0 items-center">
                  <span className="px-0.5">/</span>
                  <button onClick={() => list(path)} className="hover:text-foreground">
                    {c}
                  </button>
                </span>
              )
            })}
          </div>

          <div className="min-h-0 flex-1 overflow-y-auto p-1">
            {workspaceRoot && cwd !== workspaceRoot && (
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
              const full = joinWorkspacePath(sessionId, cwd, e.name, base)
              if (!full) return null
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
        <section className="flex min-h-0 min-w-0 flex-1 flex-col">
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
      {dialog}
    </div>
  )
}
