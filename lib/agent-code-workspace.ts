import { workspaceFileArtifacts, type AgentArtifactItem, type CodeWorkspaceChange, type CodeWorkspaceChanges, type CodeWorkspaceDiff, type CodeWorkspaceEntry, type CodeWorkspaceFile, type CodeWorkspaceGitLog, type CodeWorkspaceGitSummary, type CodeWorkspaceSearchResult, type CodeWorkspaceSource } from "@agent-cloud/agent-ui"
import { callTool, extractText, type Session, type Target } from "./client"
import { TOOLS } from "./gateway"
import { workspaceRootPath } from "./workspace-path"

const WORKSPACE_ARTIFACT_REPRESENTATION = "workspace"

interface CodeWorkspaceOptions {
  session: Session
  target: Target
  sessionId: string
  workspaceRoot?: string
}

function normalizeCodePath(path: string): string | null {
  if (!path || path === ".") return ""
  if (path.startsWith("/") || path.includes("\0") || path !== path.trim()) return null
  const parts = path.split("/")
  if (parts.some((part) => !part || part === "." || part === "..")) return null
  return parts.join("/")
}

function contentType(path: string): string {
  const extension = path.split(".").pop()?.toLowerCase()
  const types: Record<string, string> = {
    css: "text/css", csv: "text/csv", go: "text/x-go", html: "text/html",
    java: "text/x-java-source", js: "text/javascript", json: "application/json",
    md: "text/markdown", py: "text/x-python", rs: "text/x-rust", sh: "text/x-shellscript",
    sql: "application/sql", ts: "text/typescript", tsx: "text/tsx", yaml: "application/yaml",
    yml: "application/yaml",
  }
  return (extension && types[extension]) || "text/plain"
}

function artifactItems(sourceId: string, changes: CodeWorkspaceChanges): AgentArtifactItem[] {
  return workspaceFileArtifacts(sourceId, changes.changes).map((item) => ({
    ...item,
    grants: [{
      actions: ["artifact.download"],
      grantId: `${sourceId}:workspace-read`,
      representationIds: [WORKSPACE_ARTIFACT_REPRESENTATION],
    }],
    representations: [{
      filename: item.filename,
      mediaType: item.mimeType || contentType(item.filename),
      representationId: WORKSPACE_ARTIFACT_REPRESENTATION,
      revision: 0n,
      role: "primary",
      status: "ready" as const,
    }],
    selectedRepresentationId: WORKSPACE_ARTIFACT_REPRESENTATION,
  }))
}

async function invoke(options: CodeWorkspaceOptions, tool: string, args: Record<string, unknown>, signal: AbortSignal): Promise<string> {
  let streamed = ""
  let result = ""
  await callTool(options.session, {
    cluster: options.target.cluster,
    instance: options.target.instance,
    tool,
    arguments: { session_id: options.sessionId, ...args },
    signal,
  }, (event) => {
    if (event.type === "output") streamed += event.data
    if (event.type === "result") {
      const text = extractText(event.result)
      if (text && text !== "Operation complete.") result += text
    }
  })
  return streamed || result
}

async function workspaceRead(options: CodeWorkspaceOptions, args: Record<string, unknown>, signal: AbortSignal): Promise<Record<string, unknown>> {
  const text = await invoke(options, TOOLS.workspaceRead, args, signal)
  try {
    const value: unknown = JSON.parse(text)
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("工作区读取返回了无效协议")
    return value as Record<string, unknown>
  } catch (error) {
    throw new Error(`工作区读取返回了无效协议：${error instanceof Error ? error.message : String(error)}`)
  }
}

async function readFile(options: CodeWorkspaceOptions, path: string, signal: AbortSignal): Promise<CodeWorkspaceFile> {
  const relative = normalizeCodePath(path)
  if (relative === null || !relative) throw new Error("代码工作区路径无效")
  const value = await workspaceRead(options, { operation: "read", path: relative }, signal)
  if (typeof value.content !== "string" || !["utf-8", "base64"].includes(String(value.encoding)) ||
      typeof value.contentType !== "string" || typeof value.bytes !== "number" || typeof value.binary !== "boolean") {
    throw new Error("工作区文件返回了无效协议")
  }
  return {
    binary: value.binary,
    bytes: value.bytes,
    content: value.content,
    contentType: value.contentType,
    encoding: value.encoding as "utf-8" | "base64",
    path: relative,
    truncated: false,
  }
}

export function createDoopsCodeWorkspace(options: CodeWorkspaceOptions): CodeWorkspaceSource {
  const root = workspaceRootPath(options.sessionId, options.workspaceRoot)
  if (!root) throw new Error("代码工作区 session 无效")
  const source: CodeWorkspaceSource = {
    workspaceKey: `${options.target.cluster}/${options.target.instance}`,
    workspaceRoot: root,
    workspaceRootForSession: () => root,
    capabilities: { list: true, read: true, diff: true, search: true },
    async list(sessionId, dir, signal): Promise<CodeWorkspaceEntry[]> {
      if (sessionId !== options.sessionId) throw new Error("代码工作区 session 不匹配")
      const relative = normalizeCodePath(dir)
      if (relative === null) throw new Error("代码工作区目录无效")
      const value = await workspaceRead(options, { operation: "list", path: relative }, signal)
      return Array.isArray(value.entries) ? value.entries as CodeWorkspaceEntry[] : []
    },
    async read(sessionId, path, signal) {
      if (sessionId !== options.sessionId) throw new Error("代码工作区 session 不匹配")
      return readFile(options, path, signal)
    },
    async changes(sessionId, signal): Promise<CodeWorkspaceChanges> {
      if (sessionId !== options.sessionId) throw new Error("代码工作区 session 不匹配")
      const value = await workspaceRead(options, { operation: "changes" }, signal)
      return {
        changes: Array.isArray(value.changes) ? value.changes as CodeWorkspaceChange[] : [],
        gitAvailable: value.gitAvailable === true,
        gitStatus: value.gitStatus === "ready" ? "ready" : value.gitStatus === "no_repository" ? "no_repository" : "unavailable",
      }
    },
    async diff(sessionId, path, signal): Promise<CodeWorkspaceDiff> {
      if (sessionId !== options.sessionId) throw new Error("代码工作区 session 不匹配")
      const relative = normalizeCodePath(path)
      if (relative === null || !relative) throw new Error("代码 Diff 路径无效")
      const value = await workspaceRead(options, { operation: "diff", path: relative }, signal)
      return value as unknown as CodeWorkspaceDiff
    },
    async search(sessionId, query, signal): Promise<CodeWorkspaceSearchResult> {
      if (sessionId !== options.sessionId) throw new Error("代码工作区 session 不匹配")
      const needle = query.trim()
      if (!needle) return { entries: [], truncated: false }
      const value = await workspaceRead(options, { operation: "search", query: needle }, signal)
      return { entries: Array.isArray(value.entries) ? value.entries as CodeWorkspaceEntry[] : [], truncated: value.truncated === true }
    },
    git: {
      async summary(sessionId, signal): Promise<CodeWorkspaceGitSummary> {
        if (sessionId !== options.sessionId) throw new Error("代码工作区 session 不匹配")
        const value = await workspaceRead(options, { operation: "git-summary" }, signal)
        return value as unknown as CodeWorkspaceGitSummary
      },
      async log(sessionId, ref, signal): Promise<CodeWorkspaceGitLog> {
        if (sessionId !== options.sessionId) throw new Error("代码工作区 session 不匹配")
        const value = await workspaceRead(options, { operation: "git-log", ref: ref.trim() || "HEAD" }, signal)
        return value as unknown as CodeWorkspaceGitLog
      },
    },
  }
  return source
}

export async function loadDoopsWorkspaceArtifact(source: CodeWorkspaceSource, sessionId: string, artifactId: string): Promise<Blob> {
  const prefix = "workspace:"
  if (!artifactId.startsWith(prefix)) throw new Error("不支持的成果引用")
  const path = artifactId.slice(prefix.length)
  const file = await source.read(sessionId, path, new AbortController().signal)
  if (file.encoding === "base64") {
    const bytes = Uint8Array.from(atob(file.content), (character) => character.charCodeAt(0))
    return new Blob([bytes], { type: file.contentType })
  }
  return new Blob([file.content], { type: file.contentType })
}

export function workspaceArtifactsFromChanges(sourceId: string, changes: CodeWorkspaceChanges): AgentArtifactItem[] {
  return artifactItems(sourceId, changes)
}
