"use client"

import { useCallback, useEffect, useState } from "react"
import Link from "next/link"
import { fetchTargets, type Session, type Target } from "@/lib/client"
import { randomSession } from "@/lib/gateway"
import { DEMO_TOKEN } from "@/lib/demo"
import { ConnectScreen } from "./connect-screen"
import { TargetSwitcher } from "./target-switcher"
import { TerminalPanel } from "./terminal-panel"
import { AskPanel } from "./ask-panel"
import { FilesPanel } from "./files-panel"
import { KnowledgeBase } from "./knowledge-base"
import { ConfigPanel } from "./config-panel"
import { AuditPanel } from "./audit-panel"
import { AdminConsole } from "./admin-console"
import { DashboardPanel } from "./dashboard-panel"
import { OnboardingGuide } from "./onboarding-guide"
import {
  TerminalIcon,
  SparkIcon,
  FileIcon,
  FileTextIcon,
  HistoryIcon,
  KeyIcon,
  LogoutIcon,
  ServerIcon,
  ShieldIcon,
  ActivityIcon,
  HelpIcon,
} from "./icons"

type Tab = "overview" | "ask" | "files" | "kb" | "config" | "audit" | "terminal"

const TABS: { id: Tab; label: string; icon: typeof TerminalIcon; secondary?: boolean }[] = [
  { id: "overview", label: "概览", icon: ActivityIcon },
  { id: "ask", label: "AI 助手", icon: SparkIcon },
  { id: "files", label: "文件", icon: FileIcon },
  { id: "kb", label: "知识库", icon: FileTextIcon },
  { id: "config", label: "配置文件", icon: KeyIcon },
  { id: "audit", label: "审计", icon: HistoryIcon },
  { id: "terminal", label: "终端", icon: TerminalIcon, secondary: true },
]

const GW_KEY = "doops.gateway"
const DEMO_KEY = "doops.demo"
const ONBOARD_KEY = "doops.onboarded"

export function ConsoleShell() {
  const [session, setSession] = useState<Session | null>(null)
  const [defaultGateway, setDefaultGateway] = useState("http://localhost:42222")
  const [targets, setTargets] = useState<Target[]>([])
  const [selected, setSelected] = useState<Target | null>(null)
  const [sessionId, setSessionId] = useState(randomSession())
  const [tab, setTab] = useState<Tab>("overview")
  const [view, setView] = useState<"console" | "admin">("console")
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const [showGuide, setShowGuide] = useState(false)

  useEffect(() => {
    if (typeof window === "undefined") return
    const saved = localStorage.getItem(GW_KEY)
    if (saved) setDefaultGateway(saved)
    // 自动进入演示模式：?demo=1 或上次以演示模式连接过
    const wantsDemo =
      new URLSearchParams(window.location.search).get("demo") === "1" ||
      localStorage.getItem(DEMO_KEY) === "1"
    if (wantsDemo) {
      localStorage.setItem(DEMO_KEY, "1")
      setSession({ gateway: "demo://local", token: DEMO_TOKEN, username: "演示用户", demo: true })
      if (localStorage.getItem(ONBOARD_KEY) !== "1") setShowGuide(true)
    }
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
    if (s.demo) {
      localStorage.setItem(DEMO_KEY, "1")
    } else {
      localStorage.setItem(GW_KEY, s.gateway)
      localStorage.removeItem(DEMO_KEY)
    }
    setSession(s)
    if (localStorage.getItem(ONBOARD_KEY) !== "1") setShowGuide(true)
  }

  function closeGuide() {
    setShowGuide(false)
    localStorage.setItem(ONBOARD_KEY, "1")
  }

  function logout() {
    localStorage.removeItem(DEMO_KEY)
    setSession(null)
    setTargets([])
    setSelected(null)
  }

  function selectTarget(t: Target) {
    setSelected(t)
    setSessionId(randomSession())
    // AI 优先：切换机器后，默认进入 AI 助手而非终端
    setTab((cur) => (cur === "overview" || cur === "terminal" ? "ask" : cur))
  }

  if (!session) {
    return <ConnectScreen defaultGateway={defaultGateway} onConnected={handleConnected} />
  }

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex shrink-0 items-center justify-between border-b bg-card px-4 py-2.5">
        <div className="flex min-w-0 items-center gap-2">
          <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-lg bg-primary/15 text-primary">
            <ServerIcon width={16} height={16} />
          </div>
          <span className="hidden truncate text-sm font-semibold text-foreground sm:inline">
            Doops Console
          </span>
          {view === "console" && (
            <TargetSwitcher
              targets={targets}
              selected={selected}
              onSelect={selectTarget}
              onRefresh={refresh}
              loading={loading}
            />
          )}
          <div className="ml-1 flex shrink-0 items-center gap-0.5 rounded-lg border bg-muted/40 p-0.5 sm:ml-2">
            <button
              onClick={() => setView("console")}
              className={`whitespace-nowrap rounded-md px-2.5 py-1 text-xs font-medium transition-colors ${
                view === "console"
                  ? "bg-primary text-primary-foreground"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              控制台
            </button>
            <button
              onClick={() => setView("admin")}
              className={`flex items-center gap-1 whitespace-nowrap rounded-md px-2.5 py-1 text-xs font-medium transition-colors ${
                view === "admin"
                  ? "bg-primary text-primary-foreground"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              <ShieldIcon width={13} height={13} /> 管理
            </button>
          </div>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          {session.username && (
            <span className="hidden text-xs text-muted-foreground sm:inline">{session.username}</span>
          )}
          <button
            onClick={() => setShowGuide(true)}
            title="新手引导"
            className="flex items-center gap-1.5 rounded-lg border px-2 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
          >
            <HelpIcon width={14} height={14} /> <span className="hidden sm:inline">帮助</span>
          </button>
          <button
            onClick={logout}
            title="断开连接"
            className="flex items-center gap-1.5 rounded-lg border px-2 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
          >
            <LogoutIcon width={14} height={14} /> <span className="hidden sm:inline">断开</span>
          </button>
        </div>
      </header>

      {showGuide && <OnboardingGuide onClose={closeGuide} />}

      {view === "admin" ? (
        <AdminConsole session={session} />
      ) : (
      <div className="flex min-h-0 flex-1">
        <main className="flex min-w-0 flex-1 flex-col">
          <nav className="flex shrink-0 gap-1 overflow-x-auto border-b bg-card px-2">
            {TABS.map((t) => {
              const Icon = t.icon
              const active = tab === t.id
              return (
                <button
                  key={t.id}
                  onClick={() => setTab(t.id)}
                  title={t.secondary ? "高级：直接执行 shell 命令" : undefined}
                  className={`-mb-px flex shrink-0 items-center gap-1.5 border-b-2 px-3 py-2.5 text-sm transition-colors ${
                    t.secondary ? "ml-auto" : ""
                  } ${
                    active
                      ? "border-primary text-foreground"
                      : t.secondary
                        ? "border-transparent text-muted-foreground/70 hover:text-foreground"
                        : "border-transparent text-muted-foreground hover:text-foreground"
                  }`}
                >
                  <Icon width={16} height={16} />
                  {t.label}
                  {t.secondary && (
                    <span className="rounded bg-muted px-1 py-0.5 text-[10px] font-normal text-muted-foreground">
                      高级
                    </span>
                  )}
                </button>
              )
            })}
          </nav>

          {tab === "overview" ? (
            <DashboardPanel
              session={session}
              targets={targets}
              loading={loading}
              onRefresh={refresh}
              onOpenTab={(nextTab, target) => {
                if (target) selectTarget(target)
                setTab(nextTab)
              }}
            />
          ) : !selected ? (
            <div className="flex flex-1 flex-col items-center justify-center gap-3 px-4 text-center text-muted-foreground">
              <ServerIcon width={32} height={32} />
              <p className="text-sm">还没有可用的机器，部署 agent 或联系管理员接入后即可使用此功能</p>
              <div className="flex flex-wrap items-center justify-center gap-2">
                <Link
                  href="/docs/deploy"
                  className="rounded-lg bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground transition-opacity hover:opacity-90"
                >
                  部署 Doops Agent
                </Link>
                <button
                  onClick={() => setTab("overview")}
                  className="rounded-lg border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
                >
                  返回概览
                </button>
              </div>
            </div>
          ) : (
            <>
              {/* files / ask / kb 面板自带头部，避免重复占一行 */}
              {tab !== "files" && tab !== "ask" && tab !== "kb" && (
                <div className="flex shrink-0 flex-wrap items-center justify-end gap-2 border-b bg-card px-4 py-1.5">
                  <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                    session
                    <input
                      value={sessionId}
                      onChange={(e) => setSessionId(e.target.value)}
                      className="w-44 rounded-md border bg-background px-2 py-1 font-mono text-xs text-foreground outline-none focus:border-ring"
                    />
                  </label>
                </div>
              )}

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
                    onConfigureModel={() => setView("admin")}
                    onSessionChange={setSessionId}
                  />
                )}
                {tab === "files" && (
                  <FilesPanel
                    key={`files-${selected.key}-${sessionId}`}
                    session={session}
                    target={selected}
                    sessionId={sessionId}
                    onSessionChange={setSessionId}
                  />
                )}
                {tab === "kb" && (
                  <KnowledgeBase
                    key={`kb-${selected.key}`}
                    session={session}
                    target={selected}
                    sessionId={sessionId}
                  />
                )}
                {tab === "config" && (
                  <ConfigPanel
                    key={`config-${selected.key}-${sessionId}`}
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
      )}
    </div>
  )
}
