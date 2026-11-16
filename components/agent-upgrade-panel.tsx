"use client"

import { useEffect, useRef, useState } from "react"
import { History, RefreshCw, Rocket, RotateCcw, Square } from "lucide-react"
import { runAgentUpgrade, upgradeStorageKey, type AgentUpgradeOperation, type Session, type Target } from "@/lib/client"
import { AuditPanel } from "./audit-panel"
import { Button, Input } from "./ui"

const agentImagePattern = /^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]{1,5})?(?:\/[a-z0-9]+(?:[._-][a-z0-9]+)*)+(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})$/
const phases = { dry_run: "参数预览", preflight: "预检", apply: "升级", rollback: "回滚" }
const statuses: Record<AgentUpgradeOperation["status"], string> = { running: "正在执行", paused: "观察已暂停", failed: "失败", verified: "验收通过", audit_failed: "审计失败", previewed: "预览完成", preflight_passed: "预检通过，未提交升级", outcome_unknown: "结果未确认", rolled_back: "已恢复前一版本" }

type Props = { session: Session; target: Target; sessionId: string }

function restoreOperation(raw: string): AgentUpgradeOperation {
  const value = JSON.parse(raw) as AgentUpgradeOperation
  if (!value || typeof value !== "object" ||
      !/^[a-f0-9]{8}-(?:[a-f0-9]{4}-){3}[a-f0-9]{12}$/.test(value.id) ||
      !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value.sessionId) || value.sessionId.includes("..") ||
      !Object.hasOwn(phases, value.phase) || !Object.hasOwn(statuses, value.status) ||
      !Number.isFinite(value.deadline) ||
      (value.verificationComplete !== undefined && typeof value.verificationComplete !== "boolean") ||
      (value.rollbackReason !== undefined && !["upgrade_verification_protocol_missing", "upgrade_verification_commit_mismatch"].includes(value.rollbackReason)) ||
      (value.auditFailure !== undefined && (!value.auditFailure || !Number.isSafeInteger(value.auditFailure.audit_id) || value.auditFailure.audit_id <= 0 ||
        value.auditFailure.upgrade_operation_id !== value.id || value.auditFailure.result == null || !value.auditError)) ||
      !["image", "sourceCommit", "expectedCommit", "ciURL", "output", "error", "auditError"].every((key) => typeof value[key as keyof AgentUpgradeOperation] === "string") ||
      !["job", "namespace", "release"].every((key) => value[key as keyof AgentUpgradeOperation] === undefined || typeof value[key as keyof AgentUpgradeOperation] === "string")) {
    throw new Error("已保存的升级记录格式无效，无法恢复；请按操作 ID 核对审计")
  }
  return value.status === "running" ? { ...value, status: "paused", error: "上次页面观察已中断；可恢复观察，未重放变更请求" } : value
}

export function AgentUpgradePanel(props: Props) {
  return <UpgradePanel key={JSON.stringify([props.session.gateway, props.session.username, props.session.tokenId, props.target.cluster, props.target.instance])} {...props} />
}

function UpgradePanel({ session, target, sessionId }: Props) {
  const [image, setImage] = useState("")
  const [commit, setCommit] = useState("")
  const [ci, setCI] = useState("")
  const [running, setRunning] = useState(false)
  const [ready, setReady] = useState(false)
  const [operation, setOperation] = useState<AgentUpgradeOperation | null>(null)
  const [error, setError] = useState("")
  const [storageError, setStorageError] = useState("")
  const [showAudit, setShowAudit] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const mounted = useRef(false)

  useEffect(() => {
    mounted.current = true
    try {
      const raw = sessionStorage.getItem(upgradeStorageKey(session, target))
      if (raw) {
        const restored = restoreOperation(raw)
        setOperation(restored)
        setImage(restored.image)
        setCommit(restored.sourceCommit)
        setCI(restored.ciURL)
      }
    } catch (err) {
      setStorageError(err instanceof Error ? err.message : "无法读取升级记录")
    }
    setReady(true)
    return () => { mounted.current = false; abortRef.current?.abort() }
    // The parent key owns user/target changes. Session selection does not change a saved operation.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  function save(next: AgentUpgradeOperation) {
    if (!mounted.current) return
    setOperation(next)
    try {
      sessionStorage.setItem(upgradeStorageKey(session, target), JSON.stringify(next))
    } catch {
      if (mounted.current) setStorageError("升级记录保存失败，刷新后可能无法恢复；请保留当前页面并核对本次审计")
    }
  }

  async function execute(next: AgentUpgradeOperation, resume = false) {
    if (abortRef.current) return
    const ac = new AbortController()
    abortRef.current = ac
    setRunning(true)
    setError("")
    let latest = next
    try {
      // Persist the operation identity before any request can create a Job.
      sessionStorage.setItem(upgradeStorageKey(session, target), JSON.stringify(next))
      setStorageError("")
      await runAgentUpgrade(session, target, next, ac.signal, value => { latest = value; save(value) }, resume)
    } catch (err) {
      if (mounted.current) setError(latest.error || (err instanceof Error && err.name === "AbortError" ? "已停止观察，集群 Job 可能继续执行" : err instanceof Error ? err.message : "升级请求失败"))
    } finally {
      if (mounted.current) setRunning(false)
      abortRef.current = null
    }
  }

  function start(phase: "dry_run" | "preflight" | "rollback") {
    if (disabled) return
    setError("")
    const sourceCommit = commit.trim()
    const ciURL = ci.trim()
    if (phase !== "rollback") {
      if (!agentImagePattern.test(image.trim())) { setError("Agent 镜像必须为 repository:tag 或 repository@sha256:<64 位 digest>"); return }
      try {
        const url = new URL(ciURL)
        if (!/^[a-f0-9]{40}$/.test(sourceCommit) || url.protocol !== "https:" || url.username || url.password || url.search || url.hash) throw new Error()
      } catch {
        setError("请填写 40 位来源提交和不含用户名、密码、查询参数或片段的 HTTPS CI 记录 URL")
        return
      }
    }
    const next: AgentUpgradeOperation = {
      id: crypto.randomUUID(), sessionId, phase, status: "running",
      image: phase === "rollback" ? "" : image.trim(),
      sourceCommit: phase === "rollback" ? "" : sourceCommit,
      expectedCommit: phase === "rollback" ? "" : sourceCommit,
      ciURL: phase === "rollback" ? "" : ciURL,
      deadline: Date.now() + 21 * 60_000, output: "", error: "", auditError: "",
    }
    setShowAudit(false)
    void execute(next)
  }

  const canResume = Boolean(operation?.job && operation?.namespace && ["running", "paused", "audit_failed", "outcome_unknown"].includes(operation.status))
  const unresolved = Boolean(operation && (["running", "paused", "outcome_unknown"].includes(operation.status) ||
    (operation.status === "audit_failed" && operation.job && !operation.verificationComplete)))
  const offline = target.status === "offline"
  const disabled = running || !ready || offline || unresolved || Boolean(storageError) || session.demo === true

  return (
    <section className="flex w-full min-w-0 flex-col gap-3 px-4 py-5">
      <h2 className="text-base font-medium">升级现有 Agent</h2>
      <p className="break-all text-sm text-muted-foreground">{target.cluster}/{target.instance} · {sessionId}</p>
      {!ready && <p role="status" className="text-sm text-muted-foreground">正在恢复升级记录…</p>}
      {offline && <p role="status" className="text-sm text-warning">目标当前离线，恢复连接后可继续观察</p>}
      {session.demo && <p role="status" className="text-sm text-muted-foreground">演示会话不执行 Agent 升级</p>}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        {([["Agent 镜像", image, setImage], ["Agent 来源提交", commit, setCommit], ["Agent CI 记录 URL", ci, setCI]] as const).map(([label, value, setter]) => (
          <label key={label} className="flex min-w-0 flex-col gap-1 text-xs text-muted-foreground">{label}
            <Input aria-label={label} value={value} disabled={running || !ready} spellCheck={false} onChange={(e) => setter(e.target.value)} />
          </label>
        ))}
      </div>
      <div className="flex flex-wrap gap-2">
        <Button disabled={disabled || !image || !commit || !ci} onClick={() => start("dry_run")}><RefreshCw aria-hidden="true" className="size-4 shrink-0" />参数预览</Button>
        <Button disabled={disabled || !image || !commit || !ci} onClick={() => start("preflight")} variant="primary"><Rocket aria-hidden="true" className="size-4 shrink-0" />预检并升级</Button>
        <Button disabled={disabled} onClick={() => start("rollback")}><RotateCcw aria-hidden="true" className="size-4 shrink-0" />回滚前一成功版本</Button>
        {canResume && <Button disabled={running || !ready || offline} onClick={() => operation && execute(operation, true)}><RefreshCw aria-hidden="true" className="size-4 shrink-0" />恢复观察</Button>}
        {running && <Button onClick={() => abortRef.current?.abort()}><Square aria-hidden="true" className="size-4 shrink-0" />停止观察</Button>}
      </div>
      {storageError && <p role="alert" className="break-words text-sm text-destructive">{storageError}</p>}
      {operation && (
        <div className="flex min-w-0 flex-col gap-2 border-t pt-4">
          <p role="status" className={`text-sm font-medium ${operation.status === "verified" ? "text-success" : ["failed", "audit_failed"].includes(operation.status) ? "text-destructive" : "text-foreground"}`}>{phases[operation.phase]} · {statuses[operation.status]}</p>
          <p className="break-all font-mono text-xs text-muted-foreground">操作 {operation.id} · session {operation.sessionId}</p>
          {operation.job && <p className="break-all font-mono text-xs text-muted-foreground">Job {operation.namespace}/{operation.job} · release {operation.release}</p>}
          {operation.expectedCommit && <p className="break-all font-mono text-xs text-muted-foreground">expected_release_commit={operation.expectedCommit}</p>}
          {operation.status === "verified" && <p className="text-sm text-success">Job、实际 release_commit 与 structuredSSH 已核验</p>}
          {operation.verificationComplete && operation.status === "audit_failed" && <p role="status" className="text-sm text-warning">{operation.phase === "preflight" ? "预检已完成，未提交升级" : operation.phase === "rollback" ? "回滚版本验收通过，审计尚未完成" : "升级版本验收通过，审计尚未完成"}</p>}
          {operation.status === "outcome_unknown" && <p role="alert" className="text-sm text-warning">Job 可能已经创建；请恢复观察或查看本次审计核对结果，不要重复提交升级。</p>}
          {operation.auditError && <p role="alert" className="break-words text-sm text-destructive">{operation.auditError}</p>}
          {(error || operation.error) && <p role="alert" className="break-words text-sm text-destructive">{error || operation.error}</p>}
          {operation.output && <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-md border p-3 text-xs">{operation.output}</pre>}
          <Button aria-expanded={showAudit} onClick={() => setShowAudit(!showAudit)} className="self-start"><History aria-hidden="true" className="size-4" />{showAudit ? "收起本次审计" : "查看本次审计"}</Button>
        </div>
      )}
      {!operation && error && <p role="alert" className="break-words text-sm text-destructive">{error}</p>}
      {showAudit && operation && <div className="min-h-80 border-t"><AuditPanel key={operation.id} session={session} target={target} sessionId={operation.sessionId} operationId={operation.id} /></div>}
    </section>
  )
}
