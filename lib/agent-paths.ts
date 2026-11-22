export interface AgentPaths {
  workspace_root: string
  kb_root: string
}

export function parseAgentPaths(text: string): AgentPaths {
  const value = JSON.parse(text)
  const root = (key: string, fallback: string) => {
    const path = value[key] ?? fallback
    if (typeof path !== "string" || !path.startsWith("/") || /[\0\r\n]/.test(path) || path.split("/").includes("..")) {
      throw new Error("Agent 返回了无效的目录配置")
    }
    return path.replace(/\/+$/, "") || "/"
  }
  return { workspace_root: root("workspace_root", "/root/ws"), kb_root: root("kb_root", "/root/kb") }
}
