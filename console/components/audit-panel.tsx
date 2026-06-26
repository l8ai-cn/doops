"use client"

import { useCallback, useEffect, useState } from "react"
import { fetchAudit, type AuditEvent, type Session, type Target } from "@/lib/client"
import { HistoryIcon, RefreshIcon } from "./icons"

function fmtTime(s?: string) {
  if (!s) return "—"
  try {
    return new Date(s).toLocaleString("zh-CN", { hour12: false })
  } catch {
    return s
  }
}

export function AuditPanel({
  session,
  target,
  sessionId,
}: {
  session: Session
  target: Target
  sessionId: string
}) {
  const [events, setEvents] = useState<AuditEvent[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const [scope, setScope] = useState<"session" | "target">("target")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    const params: Record<string, string> = {
      cluster: target.cluster,
      instance: target.instance,
      limit: "50",
    }
    if (scope === "session") params.session = sessionId
    const r = await fetchAudit(session, params)
    if (r.error) {
      setError(
        r.status === 403
          ? "当前 token 无 admin 权限，无法查询审计记录"
          : r.error,
      )
      setEvents([])
    } else {
      setEvents(r.events || [])
    }
    setLoading(false)
  }, [session, target, sessionId, scope])

  useEffect(() => {
    load()
  }, [load])

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center justify-between border-b bg-card px-4 py-2.5">
        <div className="inline-flex rounded-lg border bg-background p-0.5 text-xs">
          <button
            onClick={() => setScope("target")}
            className={`rounded-md px-2.5 py-1 transition-colors ${
              scope === "target" ? "bg-primary text-primary-foreground" : "text-muted-foreground"
            }`}
          >
            该目标
          </button>
          <button
            onClick={() => setScope("session")}
            className={`rounded-md px-2.5 py-1 transition-colors ${
              scope === "session" ? "bg-primary text-primary-foreground" : "text-muted-foreground"
            }`}
          >
            本会话
          </button>
        </div>
        <button
          onClick={load}
          disabled={loading}
          className="flex items-center gap-1.5 rounded-md px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:opacity-50"
        >
          <RefreshIcon width={14} height={14} className={loading ? "animate-spin" : ""} /> 刷新
        </button>
      </div>

      <div className="flex-1 overflow-y-auto p-4">
        {error && (
          <p className="rounded-lg bg-destructive/15 px-3 py-2 text-sm text-destructive">{error}</p>
        )}
        {!error && events.length === 0 && !loading && (
          <div className="flex h-full flex-col items-center justify-center gap-2 text-muted-foreground">
            <HistoryIcon width={28} height={28} />
            <p className="text-sm">暂无审计记录</p>
          </div>
        )}
        <ul className="flex flex-col gap-2">
          {events.map((e) => (
            <li key={e.id} className="rounded-xl border bg-card p-3">
              <div className="flex flex-wrap items-center gap-2">
                <span
                  className={`rounded-md px-1.5 py-0.5 text-xs font-medium ${
                    e.status === "error"
                      ? "bg-destructive/15 text-destructive"
                      : "bg-success/15 text-success"
                  }`}
                >
                  {e.action || "?"}
                </span>
                {e.session && (
                  <span className="font-mono text-xs text-muted-foreground">{e.session}</span>
                )}
                <span className="ml-auto text-xs text-muted-foreground">
                  {fmtTime(e.started_at)}
                </span>
              </div>
              {e.command_summary && (
                <p className="mt-1.5 break-words font-mono text-xs text-foreground/90">
                  {e.command_summary}
                </p>
              )}
              {e.tail && (
                <pre className="mt-1.5 max-h-24 overflow-y-auto whitespace-pre-wrap break-words rounded-md bg-background/60 p-2 font-mono text-xs text-muted-foreground">
                  {e.tail}
                </pre>
              )}
            </li>
          ))}
        </ul>
      </div>
    </div>
  )
}
