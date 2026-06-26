"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { TerminalIcon, SendIcon, StopIcon } from "./icons"

interface Line {
  kind: "cmd" | "out" | "err" | "info"
  text: string
}

export function TerminalPanel({
  session,
  target,
  sessionId,
}: {
  session: Session
  target: Target
  sessionId: string
}) {
  const [command, setCommand] = useState("")
  const [lines, setLines] = useState<Line[]>([])
  const [running, setRunning] = useState(false)
  const [history, setHistory] = useState<string[]>([])
  const [histIdx, setHistIdx] = useState(-1)
  const abortRef = useRef<AbortController | null>(null)
  const logRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight })
  }, [lines])

  const push = useCallback((line: Line) => setLines((p) => [...p, line]), [])

  async function run() {
    const cmd = command.trim()
    if (!cmd || running) return
    setHistory((h) => [...h, cmd])
    setHistIdx(-1)
    setCommand("")
    push({ kind: "cmd", text: cmd })
    setRunning(true)
    const ac = new AbortController()
    abortRef.current = ac
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.shell,
          arguments: { session_id: sessionId, command: cmd },
          signal: ac.signal,
        },
        (ev) => {
          if (ev.type === "output") push({ kind: "out", text: ev.data })
          else if (ev.type === "error") push({ kind: "err", text: ev.error })
          else if (ev.type === "result") {
            const txt = extractText(ev.result)
            if (txt) push({ kind: "out", text: txt })
          }
        },
      )
    } catch (err) {
      if ((err as Error).name !== "AbortError") push({ kind: "err", text: (err as Error).message })
    } finally {
      setRunning(false)
      abortRef.current = null
    }
  }

  function stop() {
    abortRef.current?.abort()
    push({ kind: "info", text: "— 已中断 —" })
  }

  function onKeyDown(e: React.KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault()
      run()
    } else if (e.key === "ArrowUp" && !command.includes("\n")) {
      e.preventDefault()
      const idx = histIdx < 0 ? history.length - 1 : histIdx - 1
      if (idx >= 0) {
        setHistIdx(idx)
        setCommand(history[idx])
      }
    } else if (e.key === "ArrowDown" && histIdx >= 0) {
      e.preventDefault()
      const idx = histIdx + 1
      if (idx < history.length) {
        setHistIdx(idx)
        setCommand(history[idx])
      } else {
        setHistIdx(-1)
        setCommand("")
      }
    }
  }

  return (
    <div className="flex h-full flex-col">
      <div
        ref={logRef}
        className="flex-1 overflow-y-auto bg-background/60 p-4 font-mono text-sm leading-relaxed"
      >
        {lines.length === 0 && (
          <div className="flex h-full flex-col items-center justify-center gap-2 text-muted-foreground">
            <TerminalIcon width={28} height={28} />
            <p className="text-sm">在 {target.instance} 上执行确定性 Shell 命令（doops_shell）</p>
            <p className="text-xs">需要 exec 权限 · Enter 执行 · Shift+Enter 换行 · ↑/↓ 历史</p>
          </div>
        )}
        {lines.map((l, i) => {
          if (l.kind === "cmd")
            return (
              <div key={i} className="mt-2 flex gap-2 text-foreground">
                <span className="select-none text-primary">$</span>
                <span className="whitespace-pre-wrap break-all">{l.text}</span>
              </div>
            )
          if (l.kind === "err")
            return (
              <pre key={i} className="whitespace-pre-wrap break-all text-destructive">
                {l.text}
              </pre>
            )
          if (l.kind === "info")
            return (
              <div key={i} className="my-1 text-xs text-muted-foreground">
                {l.text}
              </div>
            )
          return (
            <pre key={i} className="whitespace-pre-wrap break-all text-foreground/90">
              {l.text}
            </pre>
          )
        })}
      </div>

      <div className="border-t bg-card p-3">
        <div className="flex items-end gap-2">
          <span className="select-none pb-2 font-mono text-primary">$</span>
          <textarea
            value={command}
            onChange={(e) => setCommand(e.target.value)}
            onKeyDown={onKeyDown}
            placeholder="hostname && kubectl get nodes"
            rows={1}
            className="max-h-32 min-h-9 flex-1 resize-none rounded-lg border bg-background px-3 py-2 font-mono text-sm text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
          />
          {running ? (
            <button
              onClick={stop}
              className="flex items-center gap-1.5 rounded-lg bg-destructive px-3 py-2 text-sm font-medium text-destructive-foreground transition-opacity hover:opacity-90"
            >
              <StopIcon width={16} height={16} /> 停止
            </button>
          ) : (
            <button
              onClick={run}
              disabled={!command.trim()}
              className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
            >
              <SendIcon width={16} height={16} /> 执行
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
