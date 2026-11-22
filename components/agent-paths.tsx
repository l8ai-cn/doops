"use client"

import { useEffect, useState, type ReactNode } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { parseAgentPaths, type AgentPaths } from "@/lib/agent-paths"

export function useAgentPaths(session: Session | null, target: Target | null) {
  const [attempt, setAttempt] = useState(0)
  const [state, setState] = useState<{ key: string; paths?: AgentPaths; error?: string } | null>(null)
  const key = JSON.stringify([session?.gateway, target?.cluster, target?.instance, attempt])
  useEffect(() => {
    if (!session || !target) return
    let active = true
    async function load() {
      if (!session || !target) return
      try {
        let text = ""
        if (session.demo) text = "{}"
        else await callTool(session, {
          cluster: target.cluster, instance: target.instance, tool: TOOLS.nodeInfo,
          arguments: { session_id: "console-agent-paths", section: "paths" },
        }, (event) => {
          if (event.type === "result") text += extractText(event.result)
        })
        const paths = parseAgentPaths(text)
        if (active) setState({ key, paths })
      } catch (error) {
        const failure = error as Error & { code?: number }
        if (active) {
          if (failure.code === -32602 && failure.message === "unsupported node info section") {
            setState({ key, paths: parseAgentPaths("{}") })
          } else setState({ key, error: failure.message })
        }
      }
    }
    void load()
    return () => { active = false }
  }, [session?.gateway, session?.demo, target?.cluster, target?.instance, key])
  return { ...(state?.key === key ? state : null), retry: () => setAttempt((value) => value + 1) }
}

export function AgentPathsLoader({ session, target, children }: {
  session: Session
  target: Target
  children: (paths: AgentPaths) => ReactNode
}) {
  const state = useAgentPaths(session, target)
  if (!state.paths && !state.error) return <p>正在读取 Agent 目录配置…</p>
  if (!state.paths) return <div><p role="alert">{state.error}</p><button onClick={state.retry}>重试</button></div>
  return children(state.paths)
}
