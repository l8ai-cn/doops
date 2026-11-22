const WORKSPACE_ROOT = "/root/ws"
const SESSION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/

function normalizeRelativePath(path: string) {
  if (!path || path === ".") return ""
  if (path !== path.trim() || path.startsWith("/") || path.includes("\0")) return null
  const parts = path.split("/")
  if (parts.some((part) => !part || part === "." || part === "..")) return null
  return parts.join("/")
}

export function workspaceRootPath(sessionId: string, base = WORKSPACE_ROOT) {
  const id = sessionId.trim()
  if (!SESSION_ID_PATTERN.test(id) || id.includes("..")) return null
  return `${base.replace(/\/+$/, "")}/${id}`
}

export function workspaceRelativePath(sessionId: string, absolutePath: string, base = WORKSPACE_ROOT) {
  const root = workspaceRootPath(sessionId, base)
  if (!root) return null
  if (absolutePath === root) return ""
  if (!absolutePath.startsWith(`${root}/`)) return null
  return normalizeRelativePath(absolutePath.slice(root.length + 1))
}

export function workspaceAbsolutePath(sessionId: string, relativePath: string, base = WORKSPACE_ROOT) {
  const root = workspaceRootPath(sessionId, base)
  const relative = normalizeRelativePath(relativePath)
  if (!root || relative === null) return null
  return relative ? `${root}/${relative}` : root
}

export function joinWorkspacePath(sessionId: string, directory: string, name: string, root = WORKSPACE_ROOT) {
  const base = workspaceRelativePath(sessionId, directory, root)
  const child = normalizeRelativePath(name)
  if (base === null || child === null) return null
  return workspaceAbsolutePath(sessionId, base ? `${base}/${child}` : child, root)
}
