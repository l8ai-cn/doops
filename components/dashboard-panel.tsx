"use client"

import { sessionIsAdmin, type Session, type Target } from "@/lib/client"
import { emptyTargetsCopy } from "@/lib/empty-targets"
import {
  ServerIcon, TerminalIcon, SparkIcon, FileIcon, FileTextIcon, RefreshIcon,
  ChevronRightIcon, RocketIcon, PlugIcon, HelpIcon,
} from "./icons"
import { Button } from "./ui"

type QuickTab = "terminal" | "ask" | "files" | "kb"

export function DashboardPanel({
  session,
  targets,
  selected,
  loading,
  onRefresh,
  onOpenTab,
  onDeployAgent,
}: {
  session: Session
  targets: Target[]
  selected: Target | null
  loading: boolean
  refreshVersion?: number
  onRefresh: () => void
  onOpenTab: (tab: QuickTab, target?: Target) => void
  onDeployAgent: () => void
}) {
  const current = selected && targets.find(target => target.key === selected.key)

  return (
    <div className="h-full overflow-auto px-4 py-6">
      <div className="mb-4 flex items-center justify-between gap-3">
        <h1 className="text-xl font-semibold text-foreground">当前目标</h1>
        <Button onClick={onRefresh} disabled={loading} size="sm">
          <RefreshIcon width={15} height={15} className={loading ? "animate-spin" : ""} />
          刷新
        </Button>
      </div>
      {current ? (
        <>
          <section className="card-surface mb-4 p-4">
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <ServerIcon width={18} height={18} className="shrink-0 text-primary" />
              <h2 className="break-all text-base font-semibold text-foreground">{current.instance}</h2>
              <StatusPill busy={current.busy} activeOps={current.active_ops} status={current.status} />
            </div>
            <div className="mb-4 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
              <span className="break-all">路由分组 <span className="font-mono">{current.cluster}</span></span>
              {current.connected_at && <span>已连接 {fmtConnectedDuration(current.connected_at)}</span>}
              {current.last_seen && <span>最近心跳 {fmtRelative(current.last_seen)}</span>}
              {current.remote && <span className="break-all font-mono">{current.remote}</span>}
            </div>
            <dl className="grid grid-cols-3 gap-2">
              <Metric label="正在执行" value={current.active_ops ?? 0} />
              <Metric label="排队中" value={current.queued_ops ?? 0} />
              <Metric label="活跃会话" value={current.sessions?.length ?? 0} />
            </dl>
            {current.busy && current.busy_reason && (
              <p className="mt-2 text-xs text-warning">{busyReasonLabel(current.busy_reason)}</p>
            )}
          </section>
          <TargetActions selected={current} onOpenTab={onOpenTab} />
        </>
      ) : targets.length > 0 ? (
        <p className="card-surface p-6 text-center text-sm text-muted-foreground">请在顶部选择一个 Agent 目标，查看其运行状态。</p>
      ) : loading ? (
        <p role="status" className="card-surface p-6 text-center text-sm text-muted-foreground">正在加载 Agent 目标…</p>
      ) : (
        <EmptyState isAdmin={sessionIsAdmin(session)} onDeployAgent={onDeployAgent} />
      )}
    </div>
  )
}

function Metric({ label, value }: { label: string; value: number }) {
  return (
    <div className="rounded-md bg-muted/40 px-3 py-2">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-1 text-lg font-semibold tabular-nums text-foreground">{value}</dd>
    </div>
  )
}

function TargetActions({ selected, onOpenTab }: {
  selected: Target
  onOpenTab: (tab: QuickTab, target?: Target) => void
}) {
  return (
    <section className="card-surface p-4">
      <h2 className="mb-3 text-sm font-medium text-foreground">快捷操作</h2>
      <button
        onClick={() => onOpenTab("ask", selected)}
        className="mb-2 flex w-full items-center gap-3 rounded-lg border border-primary/40 bg-primary/10 px-4 py-3 text-left transition-colors hover:bg-primary/15"
      >
        <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-primary text-primary-foreground">
          <SparkIcon width={18} height={18} />
        </span>
        <span className="min-w-0">
          <span className="block text-sm font-medium text-foreground">用 AI 助手运维</span>
          <span className="block text-xs text-muted-foreground">打开当前目标的 AI 助手</span>
        </span>
        <ChevronRightIcon width={16} height={16} className="ml-auto shrink-0 text-primary" />
      </button>

      <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
        <QuickAction
          icon={FileIcon}
          title="文件管理"
          desc="浏览、上传、编辑"
          onClick={() => onOpenTab("files", selected)}
        />
        <QuickAction
          icon={FileTextIcon}
          title="知识库"
          desc="在线运维文档"
          onClick={() => onOpenTab("kb", selected)}
        />
        <QuickAction
          icon={TerminalIcon}
          title="终端"
          desc="直接执行命令"
          onClick={() => onOpenTab("terminal", selected)}
        />
      </div>
    </section>
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
      className="group flex items-center gap-3 rounded-xl border border-border/60 bg-background/60 p-3 text-left transition-colors hover:border-primary/40 hover:bg-muted/60"
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

function StatusPill({ busy, activeOps = 0, status }: { busy?: boolean; activeOps?: number; status?: string }) {
  if (busy) {
    return (
      <span className="rounded-full bg-warning/20 px-2 py-0.5 text-[11px] text-warning">忙碌</span>
    )
  }
  if (status === "active" || activeOps > 0) {
    return (
      <span className="rounded-full bg-primary/15 px-2 py-0.5 text-[11px] text-primary">活动</span>
    )
  }
  return (
    <span className="rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">空闲</span>
  )
}

function busyReasonLabel(reason: string): string {
  const map: Record<string, string> = {
    exclusive_operation: "独占操作中，其他会话需等待",
    target_queue: "任务排队中，等待当前操作完成",
  }
  return map[reason] || reason
}

function fmtConnectedDuration(iso: string): string {
  const hours = Math.floor((Date.now() - new Date(iso).getTime()) / 3_600_000)
  if (hours < 1) return "不足 1 小时"
  if (hours < 24) return `${hours} 小时`
  return `${Math.floor(hours / 24)} 天`
}

function fmtRelative(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime()
  const min = Math.floor(diff / 60000)
  if (min < 1) return "刚刚"
  if (min < 60) return `${min} 分钟前`
  const h = Math.floor(min / 60)
  if (h < 24) return `${h} 小时前`
  return `${Math.floor(h / 24)} 天前`
}

function EmptyState({ isAdmin, onDeployAgent }: { isAdmin: boolean; onDeployAgent: () => void }) {
  const copy = emptyTargetsCopy(isAdmin)
  return (
    <div className="card-surface p-8 text-center">
      <span className="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-full bg-muted text-muted-foreground">
        <PlugIcon width={26} height={26} />
      </span>
      <h2 className="text-base font-semibold text-foreground">{copy.title}</h2>
      <p className="mx-auto mt-2 max-w-md text-sm text-muted-foreground text-pretty">
        {copy.description}
      </p>

      <div className={`mx-auto mt-5 grid gap-3 text-left ${copy.showDeployAgent ? "max-w-lg sm:grid-cols-2" : "max-w-sm"}`}>
        {copy.showDeployAgent && (
          <button
            onClick={onDeployAgent}
            className="group flex flex-col rounded-lg border bg-background p-4 text-left transition-colors hover:border-primary/50 hover:bg-muted"
          >
            <span className="mb-1.5 flex items-center gap-2 text-sm font-medium text-foreground">
              <RocketIcon width={16} height={16} className="text-primary" />
              部署 DoOps Agent
            </span>
            <span className="text-xs leading-relaxed text-muted-foreground">
              查看首次接入步骤并连接 Agent。
            </span>
            <span className="mt-2 flex items-center gap-1 text-xs font-medium text-primary">
              查看部署指南
              <ChevronRightIcon
                width={13}
                height={13}
                className="transition-transform group-hover:translate-x-0.5"
              />
            </span>
          </button>
        )}
        <div className="flex flex-col rounded-lg border bg-background p-4">
          <span className="mb-1.5 flex items-center gap-2 text-sm font-medium text-foreground">
            <HelpIcon width={16} height={16} className="text-primary" />
            {copy.contactTitle}
          </span>
          <span className="text-xs leading-relaxed text-muted-foreground">
            {copy.contactHint}
          </span>
        </div>
      </div>
    </div>
  )
}
