"use client"

import { useCallback, useEffect, useState } from "react"
import type { Session } from "@/lib/client"
import {
  listJobs,
  createJob,
  deleteJob,
  setJobEnabled,
  runJobNow,
  listJobIssues,
  type SchedulerJob,
  type SchedulerIssue,
} from "@/lib/admin"
import {
  RefreshIcon,
  PlusIcon,
  TrashIcon,
  CheckIcon,
  RocketIcon,
  HistoryIcon,
  ChevronRightIcon,
} from "./icons"

const SCAN_MODES: { id: string; label: string; hint: string }[] = [
  { id: "ask", label: "AI 巡检", hint: "用自然语言描述要检查什么，由智能体判断" },
  { id: "exec", label: "命令巡检", hint: "执行固定命令，根据输出判断异常" },
  { id: "audit", label: "审计巡检", hint: "扫描审计日志中的异常事件" },
]

function intervalLabel(sec: number): string {
  if (sec % 3600 === 0) return `每 ${sec / 3600} 小时`
  if (sec % 60 === 0) return `每 ${sec / 60} 分钟`
  return `每 ${sec} 秒`
}

export function AdminJobs({ session }: { session: Session }) {
  const [jobs, setJobs] = useState<SchedulerJob[]>([])
  const [issues, setIssues] = useState<SchedulerIssue[]>([])
  const [loading, setLoading] = useState(false)
  const [creating, setCreating] = useState(false)
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null)
  const [running, setRunning] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [j, i] = await Promise.all([listJobs(session), listJobIssues(session)])
      setJobs(j)
      setIssues(i)
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    load()
  }, [load])

  async function toggle(job: SchedulerJob) {
    try {
      await setJobEnabled(session, job.id, !job.enabled)
      setJobs((prev) => prev.map((j) => (j.id === job.id ? { ...j, enabled: !j.enabled } : j)))
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    }
  }

  async function runNow(job: SchedulerJob) {
    setRunning(job.id)
    setStatus(null)
    try {
      const summary = await runJobNow(session, job.id)
      setStatus({ kind: "ok", text: `「${job.name}」执行完成：${summary}` })
      await load()
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setRunning(null)
    }
  }

  async function remove(job: SchedulerJob) {
    if (!window.confirm(`确定删除巡检任务「${job.name}」？`)) return
    try {
      await deleteJob(session, job.id)
      setJobs((prev) => prev.filter((j) => j.id !== job.id))
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    }
  }

  return (
    <div className="h-full overflow-auto">
      <div className="mx-auto max-w-5xl px-5 py-6">
        <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-lg font-semibold text-foreground">定时巡检 / 自动提单</h1>
            <p className="mt-1 text-sm text-muted-foreground text-pretty">
              按周期自动扫描机器，发现异常时去重后自动到 GitHub / CNB 提交 issue，无需值守。
            </p>
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={load}
              disabled={loading}
              className="flex items-center gap-1.5 rounded-lg border px-3 py-2 text-sm text-foreground transition-colors hover:bg-muted disabled:opacity-50"
            >
              <RefreshIcon width={15} height={15} className={loading ? "animate-spin" : ""} />
              刷新
            </button>
            <button
              onClick={() => setCreating(true)}
              className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              <PlusIcon width={15} height={15} />
              新建任务
            </button>
          </div>
        </div>

        {status && (
          <div
            className={`mb-4 rounded-lg border px-3 py-2 text-sm ${
              status.kind === "ok"
                ? "border-primary/40 bg-primary/10 text-foreground"
                : "border-destructive/40 bg-destructive/10 text-destructive"
            }`}
          >
            {status.text}
          </div>
        )}

        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          {/* 任务列表 */}
          <section className="flex flex-col gap-3">
            <h2 className="text-sm font-medium text-foreground">巡检任务 ({jobs.length})</h2>
            {jobs.length === 0 ? (
              <div className="rounded-xl border border-dashed bg-card p-6 text-center text-sm text-muted-foreground">
                还没有巡检任务，点击「新建任务」创建第一个。
              </div>
            ) : (
              jobs.map((job) => (
                <div key={job.id} className="rounded-xl border bg-card p-4">
                  <div className="flex items-start gap-2">
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-medium text-foreground">{job.name}</span>
                        <span
                          className={`rounded-full px-2 py-0.5 text-[11px] ${
                            job.enabled
                              ? "bg-primary/15 text-primary"
                              : "bg-muted text-muted-foreground"
                          }`}
                        >
                          {job.enabled ? "运行中" : "已暂停"}
                        </span>
                      </div>
                      <div className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
                        <span className="font-mono">
                          {job.cluster_glob}/{job.instance_glob}
                        </span>
                        <span>{intervalLabel(job.interval_sec)}</span>
                        <span>{SCAN_MODES.find((m) => m.id === job.scan_mode)?.label || job.scan_mode}</span>
                      </div>
                      <div className="mt-1 text-xs text-muted-foreground">
                        提单到 <span className="font-mono">{job.platform}</span>
                        {job.repo_slug ? ` · ${job.repo_slug}` : ""}
                        {job.last_run_at ? ` · 上次 ${fmt(job.last_run_at)}` : " · 从未执行"}
                      </div>
                    </div>
                  </div>
                  <div className="mt-3 flex flex-wrap items-center gap-2">
                    <button
                      onClick={() => runNow(job)}
                      disabled={running === job.id}
                      className="flex items-center gap-1 rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted disabled:opacity-50"
                    >
                      <RocketIcon width={13} height={13} />
                      {running === job.id ? "执行中…" : "立即执行"}
                    </button>
                    <button
                      onClick={() => toggle(job)}
                      className="rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
                    >
                      {job.enabled ? "暂停" : "启用"}
                    </button>
                    <button
                      onClick={() => remove(job)}
                      className="ml-auto flex items-center gap-1 rounded-lg border px-2.5 py-1.5 text-xs text-destructive transition-colors hover:bg-destructive/10"
                    >
                      <TrashIcon width={13} height={13} />
                      删除
                    </button>
                  </div>
                </div>
              ))
            )}
          </section>

          {/* 已提 issue 记录 */}
          <section className="flex flex-col gap-3">
            <h2 className="flex items-center gap-2 text-sm font-medium text-foreground">
              <HistoryIcon width={15} height={15} className="text-primary" />
              已提 issue 记录 ({issues.length})
            </h2>
            {issues.length === 0 ? (
              <div className="rounded-xl border border-dashed bg-card p-6 text-center text-sm text-muted-foreground">
                暂无记录。任务命中异常并提单后会显示在这里。
              </div>
            ) : (
              <div className="flex flex-col gap-2">
                {issues.map((iss) => (
                  <a
                    key={iss.id}
                    href={iss.issue_url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="group rounded-xl border bg-card p-3 transition-colors hover:bg-muted"
                  >
                    <div className="flex items-center gap-2">
                      <span
                        className={`rounded-full px-2 py-0.5 text-[11px] ${
                          iss.status === "open"
                            ? "bg-warning/20 text-warning"
                            : "bg-muted text-muted-foreground"
                        }`}
                      >
                        {iss.status === "open" ? "待处理" : "已关闭"}
                      </span>
                      <span className="min-w-0 flex-1 truncate text-sm text-foreground">
                        {iss.title}
                      </span>
                      <ChevronRightIcon
                        width={14}
                        height={14}
                        className="shrink-0 text-muted-foreground transition-transform group-hover:translate-x-0.5"
                      />
                    </div>
                    <div className="mt-1 flex flex-wrap gap-x-3 text-xs text-muted-foreground">
                      <span className="font-mono">
                        {iss.cluster}/{iss.instance}
                      </span>
                      <span>{iss.repo_slug}</span>
                      {iss.created_at && <span>{fmt(iss.created_at)}</span>}
                    </div>
                  </a>
                ))}
              </div>
            )}
          </section>
        </div>
      </div>

      {creating && (
        <CreateJobDialog
          session={session}
          onClose={() => setCreating(false)}
          onCreated={(job) => {
            setJobs((prev) => [job, ...prev])
            setCreating(false)
            setStatus({ kind: "ok", text: `已创建巡检任务「${job.name}」` })
          }}
        />
      )}
    </div>
  )
}

function CreateJobDialog({
  session,
  onClose,
  onCreated,
}: {
  session: Session
  onClose: () => void
  onCreated: (job: SchedulerJob) => void
}) {
  const [name, setName] = useState("")
  const [clusterGlob, setClusterGlob] = useState("*")
  const [instanceGlob, setInstanceGlob] = useState("*")
  const [intervalMin, setIntervalMin] = useState(10)
  const [scanMode, setScanMode] = useState("ask")
  const [scanText, setScanText] = useState("")
  const [platform, setPlatform] = useState("github")
  const [repoSlug, setRepoSlug] = useState("")
  const [tokenEnv, setTokenEnv] = useState("GITHUB_TOKEN")
  const [apiBase, setApiBase] = useState("")
  const [dedupHours, setDedupHours] = useState(24)
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState("")

  const modeHint = SCAN_MODES.find((m) => m.id === scanMode)?.hint

  async function submit() {
    if (!name.trim()) {
      setErr("请填写任务名称")
      return
    }
    if (!repoSlug.trim()) {
      setErr("请填写提单目标仓库（如 org/repo）")
      return
    }
    setSaving(true)
    setErr("")
    const scanConfig =
      scanMode === "exec"
        ? { command: scanText }
        : scanMode === "ask"
          ? { instruction: scanText }
          : {}
    try {
      const job = await createJob(session, {
        name: name.trim(),
        cluster_glob: clusterGlob.trim() || "*",
        instance_glob: instanceGlob.trim() || "*",
        interval_sec: Math.max(1, intervalMin) * 60,
        scan_mode: scanMode,
        scan_config: scanConfig,
        platform,
        repo_slug: repoSlug.trim(),
        labels: "auto",
        token_env: tokenEnv.trim() || (platform === "github" ? "GITHUB_TOKEN" : "CNB_TOKEN"),
        api_base: apiBase.trim(),
        dedup_window_sec: Math.max(1, dedupHours) * 3600,
        enabled: true,
      })
      onCreated(job)
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      onClick={onClose}
    >
      <div
        className="max-h-[88vh] w-full max-w-lg overflow-auto rounded-xl border bg-card p-5"
        onClick={(e) => e.stopPropagation()}
      >
        <h3 className="text-base font-semibold text-foreground">新建巡检任务</h3>
        <p className="mt-1 text-xs text-muted-foreground">
          设置扫描范围与周期，命中异常时会自动提单。
        </p>

        <div className="mt-4 flex flex-col gap-3">
          <LabeledInput label="任务名称" value={name} onChange={setName} placeholder="如：磁盘水位巡检" />

          <div className="grid grid-cols-2 gap-3">
            <LabeledInput label="集群 (glob)" value={clusterGlob} onChange={setClusterGlob} placeholder="* 或 prod-cn" mono />
            <LabeledInput label="实例 (glob)" value={instanceGlob} onChange={setInstanceGlob} placeholder="* 或 web-*" mono />
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="flex flex-col gap-1">
              <span className="text-xs font-medium text-foreground">执行周期（分钟）</span>
              <input
                type="number"
                min={1}
                value={intervalMin}
                onChange={(e) => setIntervalMin(Number(e.target.value))}
                className="input"
              />
            </div>
            <div className="flex flex-col gap-1">
              <span className="text-xs font-medium text-foreground">去重窗口（小时）</span>
              <input
                type="number"
                min={1}
                value={dedupHours}
                onChange={(e) => setDedupHours(Number(e.target.value))}
                className="input"
              />
            </div>
          </div>

          <div className="flex flex-col gap-1">
            <span className="text-xs font-medium text-foreground">扫描方式</span>
            <div className="flex gap-1 rounded-lg border bg-muted/40 p-0.5">
              {SCAN_MODES.map((m) => (
                <button
                  key={m.id}
                  onClick={() => setScanMode(m.id)}
                  className={`flex-1 rounded-md px-2 py-1.5 text-xs font-medium transition-colors ${
                    scanMode === m.id
                      ? "bg-primary text-primary-foreground"
                      : "text-muted-foreground hover:text-foreground"
                  }`}
                >
                  {m.label}
                </button>
              ))}
            </div>
            {modeHint && <span className="text-xs text-muted-foreground">{modeHint}</span>}
          </div>

          {scanMode !== "audit" && (
            <div className="flex flex-col gap-1">
              <span className="text-xs font-medium text-foreground">
                {scanMode === "exec" ? "扫描命令" : "扫描指令"}
              </span>
              <textarea
                value={scanText}
                onChange={(e) => setScanText(e.target.value)}
                rows={2}
                placeholder={
                  scanMode === "exec"
                    ? "df -h / | awk 'NR==2{print $5}'"
                    : "检查磁盘使用率，超过 85% 时报告"
                }
                className="input font-mono text-xs"
              />
            </div>
          )}

          <div className="grid grid-cols-2 gap-3">
            <div className="flex flex-col gap-1">
              <span className="text-xs font-medium text-foreground">提单平台</span>
              <select
                value={platform}
                onChange={(e) => {
                  setPlatform(e.target.value)
                  setTokenEnv(e.target.value === "github" ? "GITHUB_TOKEN" : "CNB_TOKEN")
                }}
                className="input"
              >
                <option value="github">GitHub</option>
                <option value="cnb">CNB (Gitee)</option>
              </select>
            </div>
            <LabeledInput label="令牌环境变量" value={tokenEnv} onChange={setTokenEnv} mono />
          </div>

          <LabeledInput label="目标仓库" value={repoSlug} onChange={setRepoSlug} placeholder="org/repo" mono />

          {platform === "cnb" && (
            <LabeledInput label="API Base（可选）" value={apiBase} onChange={setApiBase} placeholder="https://api.cnb.cool" mono />
          )}

          {err && <p className="text-xs text-destructive">{err}</p>}
        </div>

        <div className="mt-5 flex justify-end gap-2">
          <button
            onClick={onClose}
            className="rounded-lg border px-3 py-2 text-sm text-foreground transition-colors hover:bg-muted"
          >
            取消
          </button>
          <button
            onClick={submit}
            disabled={saving}
            className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
          >
            <CheckIcon width={15} height={15} />
            {saving ? "创建中…" : "创建任务"}
          </button>
        </div>
      </div>
    </div>
  )
}

function LabeledInput({
  label,
  value,
  onChange,
  placeholder,
  mono,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  placeholder?: string
  mono?: boolean
}) {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-xs font-medium text-foreground">{label}</span>
      <input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        className={`input ${mono ? "font-mono text-xs" : ""}`}
      />
    </label>
  )
}

function fmt(iso: string): string {
  const d = new Date(iso)
  const diff = Date.now() - d.getTime()
  const min = Math.floor(diff / 60000)
  if (min < 1) return "刚刚"
  if (min < 60) return `${min} 分钟前`
  const h = Math.floor(min / 60)
  if (h < 24) return `${h} 小时前`
  return `${Math.floor(h / 24)} 天前`
}
