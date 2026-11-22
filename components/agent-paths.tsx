"use client"

import { useEffect, useState, type ReactNode } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { parseAgentPaths, type AgentPaths } from "@/lib/agent-paths"

export function AgentPathsLoader({ session, target, sessionId, children }: {
  session: Session
  target: Target
  sessionId: string
  children: (paths: AgentPaths) => ReactNode
}) {
  const [state, setState] = useState<{ key: string; paths?: AgentPaths; error?: string } | null>(null)
  const key = JSON.stringify([session.gateway, target.cluster, target.instance, sessionId])
  useEffect(() => {
    let active = true
    async function load() {
      try {
        let text = ""
        if (session.demo) text = "{}"
        else await callTool(session, {
          cluster: target.cluster, instance: target.instance, tool: TOOLS.nodeInfo,
          arguments: { session_id: sessionId, section: "paths" },
        }, (event) => {
          if (event.type === "result") text += extractText(event.result)
        })
        const paths = parseAgentPaths(text)
        if (active) setState({ key, paths })
      } catch (error) {
        if (active) setState({ key, error: (error as Error).message })
      }
    }
    void load()
    return () => { active = false }
  }, [session, target.cluster, target.instance, sessionId, key])
  if (state?.key !== key) return <p>正在读取 Agent 目录配置…</p>
  if (!state.paths) return <p role="alert">{state.error}</p>
  return children(state.paths)
}
