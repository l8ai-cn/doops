"use client"

import { useEffect, useMemo, useState } from "react"
import {
  fetchAudit,
  type Session,
  type Target,
  type AuditEvent,
  type AuditSummary,
} from "@/lib/client"
import { listJobs, listInstances, type SchedulerJob } from "@/lib/admin"
import {
  ServerIcon,
  ActivityIcon,
  TerminalIcon,
  SparkIcon,
  FileIcon,
  FileTextIcon,
  RefreshIcon,
  ChevronRightIcon,
  RocketIcon,
  HistoryIcon,
  PlugIcon,
  HelpIcon,
  LayersIcon,
  UsersIcon,
} from "./icons"

type QuickTab = "terminal" | "ask" | "files" | "kb"

type ClusterSummary = {
  name: string
  machines: Target[]
  connections: number
  sessions: number
  busy: number
  activeOps: number
  queuedOps: number
}

type ManagedScope = {
  total: number
  online: number
  offline: number
  isAdmin: boolean
}

const SCAN_MODE_LABELS: Record<string, string> = {
  ask: "AI 巡检",
  exec: "命令巡检",
  audit: "审计巡检",
}

export function DashboardPanel({
  session,
  targets,
  selected,
  loading,
  onRefresh,
  onOpenTab,
  onOpenJobs,
  onDeployAgent,
}: {
  session: Session
  targets: Target[]
  selected: Target | null
  loading: boolean
  onRefresh: () => void
  onOpenTab: (tab: QuickTab, target?: Target) => void
  onOpenJobs: () => void
  onDeployAgent: () => void
}) {
  const [audit, setAudit] = useState<AuditEvent[]>([])
  const [auditSummary, setAuditSummary] = useState<AuditSummary | null>(null)
  const [jobs, setJobs] = useState<SchedulerJob[]>([])
  const [managed, setManaged] = useState<ManagedScope | null>(null)
  const [auditDenied, setAuditDenied] = useState(false)

  useEffect(() => {
    let alive = true
    Promise.all([
      fetchAudit(session, { limit: "12" }),
      listJobs(session).catch(() => [] as SchedulerJob[]),
      listInstances(session).catch(() => null),
    ])
      .then(([auditRes, jobList, instances]) => {
        if (!alive) return
        if (auditRes.error && auditRes.status === 403) {
          setAuditDenied(true)
          setAudit([])
          setAuditSummary(null)
        } else {
          setAuditDenied(false)
          setAudit((auditRes.events || []).slice(0, 12))
          setAuditSummary(auditRes.summary || null)
        }
        setJobs(jobList)
        if (instances) {
          const online = instances.filter((row) => row.status === "online").length
          setManaged({
            total: instances.length,
            online,
            offline: instances.length - online,
            isAdmin: true,
          })
        } else {
          setManaged(null)
        }
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [session, targets.length])

  const metrics = useMemo(() => computeTargetMetrics(targets), [targets])
  const clusters = metrics.clusters

  const enabledJobs = jobs.filter((j) => j.enabled).length
  const recentRuns = auditSummary?.total ?? audit.length
  const recentFailed = auditSummary?.failed ?? audit.filter(isFailedEvent).length
  const managedHint = managed
    ? `${managed.online} 在线 · ${managed.offline} 离线`
    : `${metrics.agentConnections} 台当前可见`

  const selectedCluster = selected
    ? clusters.find((c) => c.name === selected.cluster) || null
    : null

  return (
    <div className="h-full overflow-auto">
      <div className="mx-auto max-w-6xl px-5 py-6">
        <div className="mb-6 flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-xl font-semibold text-foreground text-balance">
              {session.username ? `你好，${session.username}` : "欢迎使用 Doops"}
            </h1>
            <p className="mt-1 text-sm text-muted-foreground text-pretty">
              Agent 隧道连接、控制台会话、任务执行与定时巡检的一屏总览。
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
          <EmptyState onDeployAgent={onDeployAgent} />
        ) : (
          <>
            <div className="mb-6 grid grid-cols-2 gap-3 lg:grid-cols-3 xl:grid-cols-6">
              <StatCard
                label="在线连接"
                value={metrics.agentConnections}
                icon={PlugIcon}
                tone="primary"
                hint="Agent 隧道已接入"
              />
              <StatCard
                label="活跃会话"
                value={metrics.activeSessions}
                icon={UsersIcon}
                tone={metrics.activeSessions > 0 ? "primary" : "muted"}
                hint="控制台 RPC 会话"
              />
              <StatCard
                label={managed ? "纳管机器" : "可见机器"}
                value={managed?.total ?? metrics.agentConnections}
                icon={ServerIcon}
                tone="primary"
                hint={managedHint}
              />
              <StatCard label="纳管集群" value={metrics.clusterCount} icon={LayersIcon} tone="muted" />
              <StatCard
                label="正在执行"
                value={metrics.activeOps}
                icon={RocketIcon}
                tone={metrics.activeOps > 0 ? "warning" : "muted"}
                hint={
                  metrics.queuedOps > 0
                    ? `排队 ${metrics.queuedOps}`
                    : metrics.busyCount > 0
                      ? `${metrics.busyCount} 台忙碌`
                      : undefined
                }
              />
              <StatCard
                label="定时任务"
                value={enabledJobs}
                icon={ActivityIcon}
                tone={enabledJobs > 0 ? "primary" : "muted"}
                hint={
                  jobs.length > 0
                    ? `共 ${jobs.length} 个`
                    : auditDenied
                      ? "需管理员权限"
                      : recentRuns > 0
                        ? `近期执行 ${recentRuns}`
                        : undefined
                }
              />
            </div>

            <div className="mb-4 grid grid-cols-1 gap-4 xl:grid-cols-2">
              <CurrentMachineSection
                selected={selected}
                cluster={selectedCluster}
                onOpenTab={onOpenTab}
              />
              <ClusterOverviewSection clusters={clusters} />
            </div>

            <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
              <ScheduledJobsSection jobs={jobs} onOpenJobs={onOpenJobs} />
              <RecentActivitySection
                audit={audit}
                denied={auditDenied}
                summary={auditSummary}
                failedCount={recentFailed}
              />
            </div>
          </>
        )}
      </div>
    </div>
  )
}

function computeTargetMetrics(targets: Target[]) {
  const byCluster = new Map<string, Target[]>()
  for (const target of targets) {
    const list = byCluster.get(target.cluster) || []
    list.push(target)
    byCluster.set(target.cluster, list)
  }

  const clusters: ClusterSummary[] = [...byCluster.entries()]
    .map(([name, machines]) => ({
      name,
      machines,
      connections: machines.length,
      sessions: machines.reduce((sum, machine) => sum + (machine.sessions?.length || 0), 0),
      busy: machines.filter((machine) => machine.busy).length,
      activeOps: machines.reduce((sum, machine) => sum + (machine.active_ops || 0), 0),
      queuedOps: machines.reduce((sum, machine) => sum + (machine.queued_ops || 0), 0),
    }))
    .sort((a, b) => a.name.localeCompare(b.name))

  return {
    agentConnections: targets.length,
    activeSessions: targets.reduce((sum, target) => sum + (target.sessions?.length || 0), 0),
    clusterCount: clusters.length,
    busyCount: targets.filter((target) => target.busy).length,
    activeOps: targets.reduce((sum, target) => sum + (target.active_ops || 0), 0),
    queuedOps: targets.reduce((sum, target) => sum + (target.queued_ops || 0), 0),
    resourceLocks: targets.reduce((sum, target) => sum + (target.resources?.length || 0), 0),
    clusters,
  }
}

function CurrentMachineSection({
  selected,
  cluster,
  onOpenTab,
}: {
  selected: Target | null
  cluster: ClusterSummary | null
  onOpenTab: (tab: QuickTab, target?: Target) => void
}) {
  if (!selected) {
    return (
      <section className="rounded-xl border bg-card p-5">
        <SectionTitle icon={ServerIcon} title="当前机器" />
        <p className="py-8 text-center text-sm text-muted-foreground">
          请在顶部选择一台机器，查看其运行状态与快捷操作。
        </p>
      </section>
    )
  }

  return (
    <section className="rounded-xl border bg-card p-5">
      <SectionTitle icon={ServerIcon} title="当前机器" />
      <div className="mb-4 flex items-start gap-3">
        <span className="flex h-11 w-11 shrink-0 items-center justify-center rounded-xl bg-primary/15 text-primary">
          <ServerIcon width={20} height={20} />
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <h3 className="text-base font-semibold text-foreground">{selected.instance}</h3>
            <StatusPill busy={selected.busy} status={selected.status} />
          </div>
          <p className="mt-0.5 font-mono text-xs text-muted-foreground">{selected.cluster}</p>
          <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
            <span>在线连接 1</span>
            <span>活跃会话 {selected.sessions?.length || 0}</span>
            <span>执行中 {selected.active_ops || 0}</span>
            <span>排队 {selected.queued_ops || 0}</span>
            {selected.connected_at ? (
              <span>已连接 {fmtConnectedDuration(selected.connected_at)}</span>
            ) : null}
            {selected.last_seen ? <span>最近心跳 {fmtRelative(selected.last_seen)}</span> : null}
          </div>
          {selected.remote ? (
            <p className="mt-1 font-mono text-[11px] text-muted-foreground/80">{selected.remote}</p>
          ) : null}
          {selected.busy && selected.busy_reason ? (
            <p className="mt-1 text-xs text-warning">{busyReasonLabel(selected.busy_reason)}</p>
          ) : null}
        </div>
      </div>

      {cluster ? (
        <div className="mb-4 rounded-lg border bg-background/60 px-3 py-2.5 text-xs text-muted-foreground">
          <span className="font-medium text-foreground">所属集群</span>
          <span className="mx-2 text-border">·</span>
          <span className="font-mono text-foreground">{cluster.name}</span>
          <span className="mx-2 text-border">·</span>
          <span>{cluster.connections} 条连接</span>
          {cluster.sessions > 0 ? <span> · {cluster.sessions} 个会话</span> : null}
          {cluster.busy > 0 ? <span> · {cluster.busy} 台忙碌</span> : null}
          {cluster.activeOps > 0 ? <span> · 执行 {cluster.activeOps}</span> : null}
        </div>
      ) : null}

      <button
        onClick={() => onOpenTab("ask", selected)}
        className="mb-2 flex w-full items-center gap-3 rounded-lg border border-primary/40 bg-primary/10 px-4 py-3 text-left transition-colors hover:bg-primary/15"
      >
        <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-primary text-primary-foreground">
          <SparkIcon width={18} height={18} />
        </span>
        <span className="min-w-0">
          <span className="block text-sm font-medium text-foreground">用 AI 助手运维</span>
          <span className="block text-xs text-muted-foreground">自然语言描述需求，自动执行命令与排查</span>
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

function ClusterOverviewSection({ clusters }: { clusters: ClusterSummary[] }) {
  return (
    <section className="rounded-xl border bg-card p-5">
      <SectionTitle icon={LayersIcon} title="集群概览" />
      <p className="mb-4 text-xs text-muted-foreground">
        按集群汇总 Agent 隧道连接、活跃会话与任务负载。
      </p>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        {clusters.map((cluster) => (
          <div
            key={cluster.name}
            className="rounded-lg border bg-background/60 p-3.5 transition-colors hover:border-primary/30"
          >
            <div className="mb-2 flex items-center justify-between gap-2">
              <span className="truncate font-mono text-sm font-medium text-foreground">
                {cluster.name}
              </span>
              <ClusterHealthBadge connections={cluster.connections} busy={cluster.busy} />
            </div>
            <div className="grid grid-cols-2 gap-2 text-xs">
              <Metric label="在线连接" value={`${cluster.connections}`} tone="primary" />
              <Metric
                label="活跃会话"
                value={String(cluster.sessions)}
                tone={cluster.sessions > 0 ? "primary" : "default"}
              />
              <Metric
                label="执行中"
                value={String(cluster.activeOps)}
                tone={cluster.activeOps > 0 ? "primary" : "default"}
              />
              <Metric label="排队" value={String(cluster.queuedOps)} />
            </div>
          </div>
        ))}
      </div>
    </section>
  )
}

function ScheduledJobsSection({
  jobs,
  onOpenJobs,
}: {
  jobs: SchedulerJob[]
  onOpenJobs: () => void
}) {
  const preview = jobs.slice(0, 5)

  return (
    <section className="rounded-xl border bg-card p-5">
      <div className="mb-3 flex items-center justify-between gap-2">
        <SectionTitle icon={ActivityIcon} title="定时任务" />
        <button
          onClick={onOpenJobs}
          className="flex items-center gap-1 text-xs text-muted-foreground transition-colors hover:text-foreground"
        >
          全部任务
          <ChevronRightIcon width={13} height={13} />
        </button>
      </div>

      {jobs.length === 0 ? (
        <div className="rounded-lg border border-dashed px-4 py-8 text-center text-sm text-muted-foreground">
          还没有定时巡检任务。
          <button onClick={onOpenJobs} className="ml-1 text-primary transition-opacity hover:opacity-80">
            去创建
          </button>
        </div>
      ) : (
        <div className="flex flex-col divide-y">
          {preview.map((job) => (
            <div key={job.id} className="flex items-start gap-3 py-2.5 text-sm">
              <span
                className={`mt-0.5 shrink-0 rounded-full px-2 py-0.5 text-[11px] ${
                  job.enabled
                    ? "bg-primary/15 text-primary"
                    : "bg-muted text-muted-foreground"
                }`}
              >
                {job.enabled ? "运行中" : "已暂停"}
              </span>
              <div className="min-w-0 flex-1">
                <div className="truncate font-medium text-foreground">{job.name}</div>
                <div className="mt-0.5 flex flex-wrap gap-x-2 gap-y-0.5 text-xs text-muted-foreground">
                  <span>{intervalLabel(job.interval_sec)}</span>
                  <span>{SCAN_MODE_LABELS[job.scan_mode] || job.scan_mode}</span>
                  <span className="font-mono">
                    {job.cluster_glob}/{job.instance_glob}
                  </span>
                </div>
              </div>
              <span className="shrink-0 text-xs text-muted-foreground">
                {job.last_run_at ? fmtRelative(job.last_run_at) : "未执行"}
              </span>
            </div>
          ))}
        </div>
      )}
    </section>
  )
}

function RecentActivitySection({
  audit,
  denied,
  summary,
  failedCount,
}: {
  audit: AuditEvent[]
  denied: boolean
  summary: AuditSummary | null
  failedCount: number
}) {
  return (
    <section className="rounded-xl border bg-card p-5">
      <SectionTitle icon={HistoryIcon} title="近期活动" />
      {denied ? (
        <p className="py-8 text-center text-sm text-muted-foreground">
          审计日志需要管理员权限。普通用户可在各机器的 AI 助手 / 终端中查看自己的操作记录。
        </p>
      ) : audit.length === 0 ? (
        <p className="py-8 text-center text-sm text-muted-foreground">暂无记录，操作后会显示在这里</p>
      ) : (
        <>
          {summary ? (
            <div className="mb-3 flex flex-wrap gap-3 text-xs text-muted-foreground">
              <span>共 {summary.total} 次</span>
              <span className="text-primary">成功 {summary.success}</span>
              {failedCount > 0 ? <span className="text-destructive">失败 {failedCount}</span> : null}
              {summary.running > 0 ? <span>进行中 {summary.running}</span> : null}
            </div>
          ) : null}
          <div className="flex flex-col divide-y">
            {audit.map((event) => (
              <div key={event.id} className="flex items-center gap-2 py-2.5 text-sm">
                <ActionBadge action={event.action} />
                <span className="min-w-0 flex-1 truncate text-foreground">
                  {event.command_summary || event.action || "操作"}
                </span>
                <span className="hidden shrink-0 font-mono text-xs text-muted-foreground sm:inline">
                  {event.instance || ""}
                </span>
                <StatusTag status={event.status} />
              </div>
            ))}
          </div>
        </>
      )}
    </section>
  )
}

function SectionTitle({
  icon: Icon,
  title,
}: {
  icon: typeof ServerIcon
  title: string
}) {
  return (
    <h2 className="mb-3 flex items-center gap-2 text-sm font-medium text-foreground">
      <Icon width={16} height={16} className="text-primary" />
      {title}
    </h2>
  )
}

function Metric({
  label,
  value,
  tone = "default",
}: {
  label: string
  value: string
  tone?: "default" | "primary" | "warning"
}) {
  const valueCls =
    tone === "primary"
      ? "text-primary"
      : tone === "warning"
        ? "text-warning"
        : "text-foreground"
  return (
    <div className="rounded-md bg-muted/40 px-2.5 py-2">
      <div className="text-[11px] text-muted-foreground">{label}</div>
      <div className={`mt-0.5 text-sm font-medium ${valueCls}`}>{value}</div>
    </div>
  )
}

function ClusterHealthBadge({ connections, busy }: { connections: number; busy: number }) {
  const healthy = connections - busy
  const tone =
    busy === 0
      ? "bg-primary/15 text-primary"
      : busy < connections
        ? "bg-warning/20 text-warning"
        : "bg-destructive/15 text-destructive"
  const label = busy === 0 ? "全部空闲" : `${healthy}/${connections} 空闲`
  return <span className={`shrink-0 rounded-full px-2 py-0.5 text-[11px] ${tone}`}>{label}</span>
}

function StatCard({
  label,
  value,
  icon: Icon,
  tone,
  hint,
}: {
  label: string
  value: number
  icon: typeof ServerIcon
  tone: "primary" | "warning" | "muted"
  hint?: string
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
      <div className="min-w-0">
        <div className="text-2xl font-semibold leading-none text-foreground">{value}</div>
        <div className="mt-1 text-xs text-muted-foreground">{label}</div>
        {hint ? <div className="mt-0.5 truncate text-[11px] text-muted-foreground/80">{hint}</div> : null}
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

function StatusPill({ busy, status }: { busy?: boolean; status?: string }) {
  if (busy) {
    return (
      <span className="rounded-full bg-warning/20 px-2 py-0.5 text-[11px] text-warning">忙碌</span>
    )
  }
  if (status === "active") {
    return (
      <span className="rounded-full bg-primary/15 px-2 py-0.5 text-[11px] text-primary">活动</span>
    )
  }
  return (
    <span className="rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">空闲</span>
  )
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
  const fail = isFailedStatus(status)
  const cls = ok ? "text-primary" : fail ? "text-destructive" : "text-muted-foreground"
  const label = ok ? "成功" : fail ? "失败" : status
  return <span className={`shrink-0 text-xs ${cls}`}>{label}</span>
}

function isFailedStatus(status: string): boolean {
  return status === "error" || status === "failed"
}

function isFailedEvent(event: AuditEvent): boolean {
  return !!event.status && isFailedStatus(event.status)
}

function intervalLabel(sec: number): string {
  if (sec % 3600 === 0) return `每 ${sec / 3600} 小时`
  if (sec % 60 === 0) return `每 ${sec / 60} 分钟`
  return `每 ${sec} 秒`
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

function EmptyState({ onDeployAgent }: { onDeployAgent: () => void }) {
  return (
    <div className="rounded-xl border bg-card p-8 text-center">
      <span className="mx-auto mb-4 flex h-14 w-14 items-center justify-center rounded-full bg-muted text-muted-foreground">
        <PlugIcon width={26} height={26} />
      </span>
      <h2 className="text-base font-semibold text-foreground">还没有纳管的机器</h2>
      <p className="mx-auto mt-2 max-w-md text-sm text-muted-foreground text-pretty">
        Doops 通过在你的服务器上安装一个轻量 agent 来工作。你可以自己部署一台，或联系管理员为你接入。
      </p>

      <div className="mx-auto mt-5 grid max-w-lg gap-3 text-left sm:grid-cols-2">
        <button
          onClick={onDeployAgent}
          className="group flex flex-col rounded-lg border bg-background p-4 text-left transition-colors hover:border-primary/50 hover:bg-muted"
        >
          <span className="mb-1.5 flex items-center gap-2 text-sm font-medium text-foreground">
            <RocketIcon width={16} height={16} className="text-primary" />
            部署 Doops Agent
          </span>
          <span className="text-xs leading-relaxed text-muted-foreground">
            有服务器权限？按部署指南几分钟即可接入一台机器。
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
        <div className="flex flex-col rounded-lg border bg-background p-4">
          <span className="mb-1.5 flex items-center gap-2 text-sm font-medium text-foreground">
            <HelpIcon width={16} height={16} className="text-primary" />
            联系管理员
          </span>
          <span className="text-xs leading-relaxed text-muted-foreground">
            没有服务器权限或不确定 gateway 地址？请管理员为你接入机器并签发令牌。
          </span>
        </div>
      </div>

      <div className="mx-auto mt-4 max-w-lg rounded-lg border bg-background p-4 text-left">
        <div className="mb-2 text-xs font-medium text-foreground">快速安装（在你的服务器上执行）：</div>
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
