"use client"

import { useCallback, useEffect, useState } from "react"
import { fetchTargets, type Session, type Target } from "@/lib/client"
import { randomSession } from "@/lib/gateway"
import { ConnectScreen } from "./connect-screen"
import { TargetSidebar } from "./target-sidebar"
import { TerminalPanel } from "./terminal-panel"
import { AskPanel } from "./ask-panel"
import { FilesPanel } from "./files-panel"
import { AuditPanel } from "./audit-panel"
import {
  TerminalIcon,
  SparkIcon,
  FileIcon,
  HistoryIcon,
  LogoutIcon,
  ServerIcon,
} from "./icons"

type Tab = "terminal" | "ask" | "files" | "audit"

const TABS: { id: Tab; label: string; icon: typeof TerminalIcon }[] = [
  { id: "terminal", label: "终端", icon: TerminalIcon },
  { id: "ask", label: "AI 运维 / 部署", icon: SparkIcon },
  { id: "files", label: "文件", icon: FileIcon },
  { id: "audit", label: "审计", icon: HistoryIcon },
]

const GW_KEY = "doops.gateway"

export function ConsoleShell() {
  const [session, setSession] = useState<Session | null>(null)
  const [defaultGateway, setDefaultGateway] = useState("http://localhost:42222")
  const [targets, setTargets] = useState<Target[]>([])
  const [selected, setSelected] = useState<Target | null>(null)
  const [sessionId, setSessionId] = useState(randomSession())
  const [tab, setTab] = useState<Tab>("terminal")
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")

  useEffect(() => {
    const saved = typeof window !== "undefined" ? localStorage.getItem(GW_KEY) : null
    if (saved) setDefaultGateway(saved)
  }, [])

  const refresh = useCallback(async () => {
    if (!session) return
    setLoading(true)
    setError("")
    try {
      const t = await fetchTargets(session)
      setTargets(t)
      setSelected((cur) => {
        if (cur) {
          const still = t.find((x) => x.key === cur.key)
          return still || cur
        }
        return t[0] || null
      })
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    if (!session) return
    refresh()
    const id = setInterval(refresh, 10000)
    return () => clearInterval(id)
  }, [session, refresh])

  function handleConnected(s: Session) {
    localStorage.setItem(GW_KEY, s.gateway)
    setSession(s)
  }

  function logout() {
    setSession(null)
    setTargets([])
    setSelected(null)
  }

  function selectTarget(t: Target) {
    setSelected(t)
    setSessionId(randomSession())
  }

  if (!session) {
    return <ConnectScreen defaultGateway={defaultGateway} onConnected={handleConnected} />
  }

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex shrink-0 items-center justify-between border-b bg-card px-4 py-2.5">
        <div className="flex items-center gap-2">
          <div className="flex h-7 w-7 items-center justify-center rounded-lg bg-primary/15 text-primary">
            <ServerIcon width={16} height={16} />
          </div>
          <span className="text-sm font-semibold text-foreground">Doops Console</span>
          <span className="hidden font-mono text-xs text-muted-foreground sm:inline">
            {session.gateway}
          </span>
        </div>
        <div className="flex items-center gap-3">
          {session.username && (
            <span className="text-xs text-muted-foreground">{session.username}</span>
          )}
          <button
            onClick={logout}
            className="flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
          >
            <LogoutIcon width={14} height={14} /> 断开
          </button>
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        <TargetSidebar
          targets={targets}
          selectedKey={selected?.key || null}
          onSelect={selectTarget}
          onRefresh={refresh}
          loading={loading}
          error={error}
        />

        <main className="flex min-w-0 flex-1 flex-col">
          {!selected ? (
            <div className="flex flex-1 flex-col items-center justify-center gap-2 text-muted-foreground">
              <ServerIcon width={32} height={32} />
              <p className="text-sm">从左侧选择一个在线目标开始操作</p>
            </div>
          ) : (
            <>
              <div className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b bg-card px-4 py-2">
                <div className="flex items-center gap-2">
                  <span className="text-sm font-medium text-foreground">{selected.instance}</span>
                  <span className="font-mono text-xs text-muted-foreground">
                    {selected.cluster}
                  </span>
                </div>
                <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                  session
                  <input
                    value={sessionId}
                    onChange={(e) => setSessionId(e.target.value)}
                    className="w-44 rounded-md border bg-background px-2 py-1 font-mono text-xs text-foreground outline-none focus:border-ring"
                  />
                </label>
              </div>

              <nav className="flex shrink-0 gap-1 border-b bg-card px-2">
                {TABS.map((t) => {
                  const Icon = t.icon
                  const active = tab === t.id
                  return (
                    <button
                      key={t.id}
                      onClick={() => setTab(t.id)}
                      className={`-mb-px flex items-center gap-1.5 border-b-2 px-3 py-2.5 text-sm transition-colors ${
                        active
                          ? "border-primary text-foreground"
                          : "border-transparent text-muted-foreground hover:text-foreground"
                      }`}
                    >
                      <Icon width={16} height={16} />
                      {t.label}
                    </button>
                  )
                })}
              </nav>

              <div className="min-h-0 flex-1">
                {tab === "terminal" && (
                  <TerminalPanel
                    key={`term-${selected.key}-${sessionId}`}
                    session={session}
                    target={selected}
                    sessionId={sessionId}
                  />
                )}
                {tab === "ask" && (
                  <AskPanel
                    key={`ask-${selected.key}-${sessionId}`}
                    session={session}
                    target={selected}
                    sessionId={sessionId}
                  />
                )}
                {tab === "files" && (
                  <FilesPanel
                    key={`files-${selected.key}-${sessionId}`}
                    session={session}
                    target={selected}
                    sessionId={sessionId}
                  />
                )}
                {tab === "audit" && (
                  <AuditPanel
                    key={`audit-${selected.key}-${sessionId}`}
                    session={session}
                    target={selected}
                    sessionId={sessionId}
                  />
                )}
              </div>
            </>
          )}
        </main>
      </div>
    </div>
  )
}
