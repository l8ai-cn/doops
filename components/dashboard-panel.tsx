"use client"

import { useEffect, useState } from "react"
import { fetchAudit, type Session, type Target, type AuditEvent } from "@/lib/client"
import {
  ServerIcon,
  ActivityIcon,
  TerminalIcon,
  SparkIcon,
  FileIcon,
  RefreshIcon,
  ChevronRightIcon,
  RocketIcon,
  HistoryIcon,
  PlugIcon,
} from "./icons"

type QuickTab = "terminal" | "ask" | "files" | "config"

export function DashboardPanel({
  session,
  targets,
  loading,
  onRefresh,
  onOpenTab,
}: {
  session: Session
  targets: Target[]
  loading: boolean
  onRefresh: () => void
  onOpenTab: (tab: QuickTab, target?: Target) => void
}) {
  const [audit, setAudit] = useState<AuditEvent[]>([])

  useEffect(() => {
    let alive = true
    fetchAudit(session, { limit: "8" })
      .then((r) => {
        if (alive && r.events) setAudit(r.events.slice(0, 8))
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [session, targets.length])

  const online = targets.length
  const busy = targets.filter((t) => t.busy).length
  const activeOps = targets.reduce((s, t) => s + (t.active_ops || 0), 0)
  const queuedOps = targets.reduce((s, t) => s + (t.queued_ops || 0), 0)

  // 按集群分组
  const byCluster = new Map<string, Target[]>()
  for (const t of targets) {
    const list = byCluster.get(t.cluster) || []
    list.push(t)
    byCluster.set(t.cluster, list)
  }

  const single = targets.length === 1 ? targets[0] : null

  return (
    <div className="h-full overflow-auto">
      <div className="mx-auto max-w-5xl px-5 py-6">
        {/* 欢迎区 */}
        <div className="mb-6 flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-xl font-semibold text-foreground text-balance">
              {session.username ? `你好，${session.username}` : "欢迎使用 Doops"}
            </h1>
            <p className="mt-1 text-sm text-muted-foreground text-pretty">
              这里是运维总览。无需记命令，选好机器后用终端或 AI 对话即可完成操作。
            </p>
          </div>
          <button
            onClick={onRefresh}
            disabled={loading}
            className="flex items-center gap-1.5 rounded-lg border px-3 py-2 text-sm text-foreground transition-colors hover:bg-muted disabled:opacity-50"
          >
            <RefreshIcon width={15} height={15} className={loading ? "animate-spin" : ""} />
            刷新
          </button>
        </div>

        {targets.length === 0 ? (
          <EmptyState />
        ) : (
          <>
            {/* 统计卡 */}
            <div className="mb-6 grid grid-cols-2 gap-3 lg:grid-cols-4">
              <StatCard label="在线机器" value={online} icon={ServerIcon} tone="primary" />
              <StatCard label="忙碌中" value={busy} icon={ActivityIcon} tone="warning" />
              <StatCard label="正在执行" value={activeOps} icon={RocketIcon} tone="primary" />
              <StatCard label="排队等待" value={queuedOps} icon={HistoryIcon} tone="muted" />
            </div>

            {/* 单实例：突出快捷操作 */}
            {single && (
              <div className="mb-6 rounded-xl border bg-card p-4">
                <div className="mb-3 flex items-center gap-2">
                  <span className="flex h-8 w-8 items-center justify-center rounded-lg bg-primary/15 text-primary">
                    <ServerIcon width={17} height={17} />
                  </span>
                  <div>
                    <div className="text-sm font-medium text-foreground">{single.instance}</div>
                    <div className="font-mono text-xs text-muted-foreground">{single.cluster}</div>
                  </div>
                  <StatusDot status={single.status} busy={single.busy} className="ml-auto" />
                </div>
                <p className="mb-3 text-sm text-muted-foreground">
                  你的机器已连接，选择下面任意一项开始：
                </p>
                <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                  <QuickAction
                    icon={TerminalIcon}
                    title="打开终端"
                    desc="执行命令、查看状态"
                    onClick={() => onOpenTab("terminal", single)}
                  />
                  <QuickAction
                    icon={SparkIcon}
                    title="AI 对话 / 部署"
                    desc="用自然语言完成运维"
                    onClick={() => onOpenTab("ask", single)}
                  />
                  <QuickAction
                    icon={FileIcon}
                    title="文件管理"
                    desc="浏览、上传、编辑文件"
                    onClick={() => onOpenTab("files", single)}
                  />
                  <QuickAction
                    icon={RocketIcon}
                    title="模型与配置"
                    desc="配置 AI 模型与密钥"
                    onClick={() => onOpenTab("config", single)}
                  />
                </div>
              </div>
            )}

            <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
              {/* 实例健康（多实例时） */}
              {!single && (
                <section className="rounded-xl border bg-card p-4">
                  <h2 className="mb-3 flex items-center gap-2 text-sm font-medium text-foreground">
                    <ServerIcon width={16} height={16} className="text-primary" />
                    机器健康
                  </h2>
                  <div className="flex flex-col gap-4">
                    {[...byCluster.entries()].map(([cluster, list]) => (
                      <div key={cluster}>
                        <div className="mb-1.5 font-mono text-xs text-muted-foreground">
                          {cluster}
                        </div>
                        <div className="flex flex-col gap-1">
                          {list.map((t) => (
                            <button
                              key={t.key}
                              onClick={() => onOpenTab("terminal", t)}
                              className="flex items-center gap-2 rounded-lg border px-3 py-2 text-left transition-colors hover:bg-muted"
                            >
                              <StatusDot status={t.status} busy={t.busy} />
                              <span className="text-sm text-foreground">{t.instance}</span>
                              <span className="ml-auto flex items-center gap-2 text-xs text-muted-foreground">
                                {(t.active_ops || 0) > 0 && <span>执行 {t.active_ops}</span>}
                                {(t.queued_ops || 0) > 0 && <span>排队 {t.queued_ops}</span>}
                                <ChevronRightIcon width={14} height={14} />
                              </span>
                            </button>
                          ))}
                        </div>
                      </div>
                    ))}
                  </div>
                </section>
              )}

              {/* 近期活动 */}
              <section className="rounded-xl border bg-card p-4">
                <h2 className="mb-3 flex items-center gap-2 text-sm font-medium text-foreground">
                  <HistoryIcon width={16} height={16} className="text-primary" />
                  近期活动
                </h2>
                {audit.length === 0 ? (
                  <p className="py-6 text-center text-sm text-muted-foreground">
                    暂无记录，操作后会显示在这里
                  </p>
                ) : (
                  <div className="flex flex-col divide-y">
                    {audit.map((e) => (
                      <div key={e.id} className="flex items-center gap-2 py-2 text-sm">
                        <ActionBadge action={e.action} />
                        <span className="min-w-0 flex-1 truncate text-foreground">
                          {e.command_summary || e.action || "操作"}
                        </span>
                        <span className="shrink-0 font-mono text-xs text-muted-foreground">
                          {e.instance || ""}
                        </span>
                        <StatusTag status={e.status} />
                      </div>
                    ))}
                  </div>
                )}
              </section>
            </div>
          </>
        )}
      </div>
    </div>
  )
}

function StatCard({
  label,
  value,
  icon: Icon,
  tone,
}: {
  label: string
  value: number
  icon: typeof ServerIcon
  tone: "primary" | "warning" | "muted"
}) {
  const toneCls =
    tone === "primary"
      ? "bg-primary/15 text-primary"
      : tone === "warning"
        ? "bg-warning/20 text-warning"
        : "bg-muted text-muted-foreground"
  return (
    <div className="flex items-center gap-3 rounded-xl border bg-card p-4">
      <span className={`flex h-10 w-10 items-center justify-center rounded-lg ${toneCls}`}>
        <Icon width={20} height={20} />
      </span>
      <div>
        <div className="text-2xl font-semibold leading-none text-foreground">{value}</div>
        <div className="mt-1 text-xs text-muted-foreground">{label}</div>
      </div>
    </div>
  )
}

function QuickAction({
  icon: Icon,
  title,
  desc,
  onClick,
}: {
  icon: typeof ServerIcon
  title: string
  desc: string
  onClick: () => void
}) {
  return (
    <button
      onClick={onClick}
      className="group flex items-center gap-3 rounded-lg border bg-background p-3 text-left transition-colors hover:border-primary/50 hover:bg-muted"
    >
      <span className="flex h-9 w-9 items-center justify-center rounded-lg bg-primary/15 text-primary">
        <Icon width={18} height={18} />
      </span>
      <span className="min-w-0 flex-1">
        <span className="block text-sm font-medium text-foreground">{title}</span>
        <span className="block text-xs text-muted-foreground">{desc}</span>
      </span>
      <ChevronRightIcon
        width={16}
        height={16}
        className="text-muted-foreground transition-transform group-hover:translate-x-0.5"
      />
    </button>
  )
}

function StatusDot({
  status,
  busy,
  className = "",
}: {
  status?: string
  busy?: boolean
  className?: string
}) {
  const color = busy
    ? "bg-warning"
    : status === "active"
      ? "bg-primary"
      : "bg-muted-foreground"
  return <span className={`h-2.5 w-2.5 shrink-0 rounded-full ${color} ${className}`} />
}

function ActionBadge({ action }: { action?: string }) {
  const map: Record<string, string> = {
    exec: "命令",
    shell: "命令",
    ask: "AI",
    agent_prompt: "AI",
    read: "读取",
    file_read: "读取",
    write: "写入",
    file_write: "写入",
    push: "上传",
    pull: "下载",
    info: "信息",
    node_info: "信息",
    check: "检查",
    check_deployment: "检查",
    clean: "清理",
    clean_workspace: "清理",
  }
  const key = (action || "").replace(/^doops_/, "")
  return (
    <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[11px] text-muted-foreground">
      {map[key] || key || "操作"}
    </span>
  )
}

function StatusTag({ status }: { status?: string }) {
  if (!status) return null
  const ok = status === "success" || status === "ok" || status === "completed"
  const fail = status === "error" || status === "failed"
  const cls = ok
    ? "text-primary"
    : fail
      ? "text-destructive"
      : "text-muted-foreground"
  const label = ok ? "成功" : fail ? "失败" : status
  return <span className={`shrink-0 text-xs ${cls}`}>{label}</span>
}

function EmptyState() {
  return (
    <div className="rounded-xl border bg-card p-8 text-center">
      <span className="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-full bg-muted text-muted-foreground">
        <PlugIcon width={26} height={26} />
      </span>
      <h2 className="text-base font-semibold text-foreground">还没有连接的机器</h2>
      <p className="mx-auto mt-2 max-w-md text-sm text-muted-foreground text-pretty">
        Doops 通过在你的服务器上安装一个轻量 agent 来工作。安装并启动后，机器会自动出现在这里，随后即可远程执行命令、上传文件、用 AI 完成部署。
      </p>
      <div className="mx-auto mt-5 max-w-md rounded-lg border bg-background p-4 text-left">
        <div className="mb-2 text-xs font-medium text-foreground">在你的服务器上执行：</div>
        <pre className="overflow-x-auto rounded-md bg-muted px-3 py-2 font-mono text-xs text-foreground">
          curl -fsSL https://doops.sh/install.sh | sh
        </pre>
        <p className="mt-2 text-xs text-muted-foreground">
          安装脚本会引导你填写 gateway 地址与接入令牌。完成后回到此页面点击「刷新」。
        </p>
      </div>
    </div>
  )
}
