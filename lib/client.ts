"use client"

import type { Target, AuditEvent, AuditSummary } from "./gateway"
import { consoleFetch } from "./network-error.ts"
import { GatewaySessionError } from "./gateway-auth.ts"

export type { Target, AuditEvent, AuditSummary }

export interface Session {
  gateway: string
  token?: string
  username?: string
  tokenId?: string
  issuedAt?: string
  expiresAt?: string
  demo?: boolean
  // 只决定是否显示管理入口；来自 Gateway 当前身份接口，Gateway 仍是权限边界。
  isAdmin?: boolean
}

// 演示会话默认保持原来的管理员界面，显式 isAdmin=false 时模拟普通用户；
// 真实会话只有服务端探测为 200 时才是管理员。
export function sessionIsAdmin(s: Session): boolean {
  if (s.demo === true) return s.isAdmin !== false
  return s.isAdmin === true
}

export type RpcEvent =
  | { type: "open" }
  | { type: "output"; data: string; session?: string; kind?: string }
  | { type: "result"; result: unknown }
  | { type: "error"; error: string; code?: number; data?: unknown }
  | { type: "done" }

export type ToolCancel = () => Promise<void>

export class RpcError extends Error {
  code?: number
  data?: unknown

  constructor(message: string, code?: number, data?: unknown) {
    super(message)
    this.name = "RpcError"
    this.code = code
    this.data = data
  }
}

function authHeaders(s: Session): HeadersInit {
  void s
  return { "Content-Type": "application/json" }
}

export async function login(
  gateway: string,
  username: string,
  password: string,
): Promise<Session> {
  const res = await consoleFetch("/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ gateway, username, password }),
  })
  const data = await res.json()
  if (!res.ok) throw new Error(data.error || "登录失败")
  return {
    gateway: data.gateway,
    username: data.username,
    isAdmin: data.isAdmin === true,
    tokenId: data.tokenId,
    issuedAt: data.issuedAt,
    expiresAt: data.expiresAt,
  }
}

export async function restoreServerSession(): Promise<Session | null> {
  const res = await consoleFetch("/api/session", { cache: "no-store" })
  const data = await res.json()
  if (!res.ok) throw new Error(data.error || `恢复会话失败 (${res.status})`)
  if (data.session === null) return null
  if (!data.session?.gateway) throw new Error("Gateway 会话响应无效")
  return {
    gateway: data.session.gateway,
    username: data.session.username || undefined,
    isAdmin: data.session.isAdmin === true,
    tokenId: data.session.tokenId,
    issuedAt: data.session.issuedAt,
    expiresAt: data.session.expiresAt,
  }
}

export async function persistTokenSession(
  gateway: string,
  token: string,
): Promise<Session> {
  const res = await consoleFetch("/api/session", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ gateway, token }),
  })
  const data = await res.json()
  if (!res.ok) throw new Error(data.error || "保存会话失败")
  return {
    gateway: data.gateway,
    username: data.username,
    isAdmin: data.isAdmin === true,
    tokenId: data.tokenId,
    issuedAt: data.issuedAt,
    expiresAt: data.expiresAt,
  }
}

export async function clearServerSession(): Promise<void> {
  const res = await consoleFetch("/api/session", { method: "DELETE" })
  if (!res.ok) {
    const data = await res.json().catch(() => ({}))
    throw new Error(data.error || `断开连接失败 (${res.status})`)
  }
}

export async function fetchTargets(s: Session): Promise<Target[]> {
  if (s.demo) {
    // ?demo=1&targets=none 模拟没有可用目标，用来查看空状态。
    if (typeof window !== "undefined" && new URLSearchParams(window.location.search).get("targets") === "none") return []
    const { DEMO_TARGETS } = await import("./demo")
    return DEMO_TARGETS
  }
  const res = await consoleFetch("/api/targets", { headers: authHeaders(s) })
  const data = await res.json()
  // message 是代理给页面的中文说明；error 可能是 Gateway 类别原文。
  if (!res.ok) {
    throw new GatewaySessionError(data.message || data.error || "查询目标失败", {
      status: res.status,
      code: typeof data.code === "string" ? data.code : undefined,
    })
  }
  return (data.targets || []) as Target[]
}

export async function fetchAudit(
  s: Session,
  params: Record<string, string>,
): Promise<{ events?: AuditEvent[]; summary?: AuditSummary; error?: string; status?: number }> {
  if (s.demo) {
    const { DEMO_AUDIT } = await import("./demo.ts")
    const events = DEMO_AUDIT.filter((e) =>
      (!params.instance || e.instance === params.instance) &&
      (!params.cluster || e.cluster === params.cluster) &&
      (!params.session || e.session === params.session) &&
      (!params.action || e.action === params.action) &&
      (!params.status || e.status === params.status) &&
      (!params.correlation_id || e.correlation_id === params.correlation_id) &&
      (!params.before_id || e.id < Number(params.before_id)) &&
      (!params.since || Boolean(e.started_at && Date.parse(e.started_at) >= Date.parse(params.since))) &&
      (!params.until || Boolean(e.started_at && Date.parse(e.started_at) < Date.parse(params.until))) &&
      !(params.hide_upgrade_observe === "true" && e.action === "agent:upgrade" && e.status === "observing"),
    ).sort((a, b) => b.id - a.id).slice(0, Number(params.limit) || 50)
    return { events, summary: summarizeAuditEvents(events) }
  }
  const qs = new URLSearchParams(params)
  const res = await consoleFetch(`/api/audit?${qs.toString()}`, { headers: authHeaders(s) })
  const data = await res.json()
  if (!res.ok) return { error: data.message || data.error || "查询审计失败", status: res.status }
  return {
    events: (data.events || []) as AuditEvent[],
    summary: data.summary as AuditSummary | undefined,
  }
}

function summarizeAuditEvents(events: AuditEvent[]): AuditSummary {
  const summary: AuditSummary = {
    total: 0,
    running: 0,
    success: 0,
    failed: 0,
    bytes_in: 0,
    bytes_out: 0,
    status: {},
  }
  for (const event of events) {
    const status = event.status || "unknown"
    summary.total += 1
    summary.bytes_in += event.bytes_in || 0
    summary.bytes_out += event.bytes_out || 0
    summary.status![status] = (summary.status![status] || 0) + 1
    if (["running", "executing", "accepted", "observing"].includes(status)) summary.running += 1
    else if (status === "success") summary.success += 1
    else summary.failed += 1
  }
  return summary
}

// 调用一个 gateway 工具并流式返回事件
export async function callTool(
  s: Session,
  opts: {
    cluster: string
    instance: string
    tool: string
    arguments: Record<string, unknown>
    signal?: AbortSignal
    requiredProtocol?: "structuredSSH"
    expectedReleaseCommit?: string
    onCancelReady?: (cancel: ToolCancel) => void
  },
  onEvent: (ev: RpcEvent) => void,
): Promise<void> {
  if (s.demo) {
    const { demoCallTool } = await import("./demo")
    const demoController = new AbortController()
    opts.onCancelReady?.(async () => {
      demoController.abort()
    })
    await demoCallTool({ ...opts, signal: opts.signal || demoController.signal }, onEvent)
    return
  }
  const requestId = globalThis.crypto.randomUUID()
  let cancelPromise: Promise<void> | undefined
  const cancel: ToolCancel = async () => {
    if (!cancelPromise) {
      cancelPromise = (async () => {
        // The server registers the request before returning its response.
        // A stop click must not overtake the initial operation request.
        const started = await response
        if (!started.ok) throw new Error("原请求未被接受，无法请求停止")
        const cancelResponse = await consoleFetch("/api/rpc", {
          method: "POST",
          headers: authHeaders(s),
          body: JSON.stringify({
            cluster: opts.cluster,
            instance: opts.instance,
            arguments: { session_id: opts.arguments.session_id },
            cancelRequestId: requestId,
          }),
        })
        let detail: { error?: string; request_id?: string; cancel_requested?: boolean } = {}
        try { detail = await cancelResponse.json() } catch { /* noop */ }
        if (!cancelResponse.ok) throw new Error(detail.error || `停止请求失败 (${cancelResponse.status})`)
        if (detail.request_id !== requestId || detail.cancel_requested !== true) {
          throw new Error("停止请求返回了无效确认")
        }
      })().catch((error) => {
        cancelPromise = undefined
        throw error
      })
    }
    return cancelPromise
  }
  const response = consoleFetch("/api/rpc", {
    method: "POST",
    headers: authHeaders(s),
    body: JSON.stringify({
      cluster: opts.cluster,
      instance: opts.instance,
      tool: opts.tool,
      arguments: opts.arguments,
      requiredProtocol: opts.requiredProtocol,
      expectedReleaseCommit: opts.expectedReleaseCommit,
      requestId,
    }),
    signal: opts.signal,
  })
  opts.onCancelReady?.(cancel)
  const res = await response

  if (!res.ok || !res.body) {
    let msg = `请求失败 (${res.status})`
    try {
      const data = await res.json()
      msg = data.error || msg
    } catch {
      /* noop */
    }
    onEvent({ type: "error", error: msg })
    onEvent({ type: "done" })
    throw new Error(msg)
  }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""
  let terminal = false
  let failure: RpcError | undefined
  function receive(line: string) {
    let event: RpcEvent
    try { event = JSON.parse(line) as RpcEvent } catch { throw new Error("运维服务返回了无效事件") }
    if (!event || !["open", "output", "result", "error", "done"].includes(event.type)) throw new Error("运维服务返回了未知事件")
    if (event.type === "result") {
      terminal = true
      if ((event.result as { isError?: boolean })?.isError) {
        failure = new RpcError(extractText(event.result) || "工具执行失败", undefined, { result: event.result })
        onEvent({ type: "error", error: failure.message, data: failure.data })
        return
      }
    }
    if (event.type === "error") {
      terminal = true
      failure = new RpcError(event.error || "工具执行失败", event.code, event.data)
    }
    onEvent(event)
  }

  try { while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    let idx: number
    while ((idx = buffer.indexOf("\n")) >= 0) {
      const line = buffer.slice(0, idx).trim()
      buffer = buffer.slice(idx + 1)
      if (!line) continue
      receive(line)
    }
  }
  buffer += decoder.decode()
  if (buffer.trim()) {
    receive(buffer.trim())
  }
  if (!terminal) throw new RpcError("连接已结束，未收到工具执行结果", -32008)
  if (failure) throw failure
  } finally { reader.releaseLock() }
}

// 从工具结果里提取文本内容（MCP content 数组或字符串）
export function extractText(result: unknown): string {
  if (result == null) return ""
  if (typeof result === "string") return result
  const r = result as any
  if (Array.isArray(r.content)) {
    return r.content
      .map((c: any) => (typeof c === "string" ? c : c?.text ?? ""))
      .join("")
  }
  if (typeof r.text === "string") return r.text
  try {
    return JSON.stringify(r, null, 2)
  } catch {
    return String(result)
  }
}

export interface AgentUpgradeOperation {
  id: string
  sessionId: string
  phase: "dry_run" | "preflight" | "apply" | "rollback"
  status: "running" | "paused" | "failed" | "verified" | "audit_failed" | "previewed" | "preflight_passed" | "outcome_unknown" | "rolled_back"
  image: string
  sourceCommit: string
  expectedCommit: string
  ciURL: string
  job?: string
  namespace?: string
  release?: string
  deadline: number
  output: string
  error: string
  auditError: string
  auditFailure?: { audit_id: number; upgrade_operation_id: string; result: unknown }
  verificationComplete?: boolean
  rollbackReason?: string
}

export function upgradeStorageKey(s: Session, target: Pick<Target, "cluster" | "instance">): string {
  const identity = s.username ? `user:${s.username}` : s.tokenId ? `token-id:${s.tokenId}` : ""
  if (!identity) throw new Error("当前登录缺少用户标识，无法隔离保存升级记录")
  return `doops.agent-upgrade.v1:${JSON.stringify([s.gateway, identity, target.cluster, target.instance])}`
}

function upgradeResponse(result: unknown) {
  const fields = (result as { structuredContent?: Record<string, unknown> } | null)?.structuredContent
  if (!fields || typeof fields !== "object" || typeof fields.outcome !== "string") {
    throw new Error("Agent 未返回有效的 structuredContent 升级结果")
  }
  for (const field of ["job", "namespace", "release", "expected_release_commit", "upgrade_operation_id", "phase", "job_phase"]) {
    if (fields[field] !== undefined && typeof fields[field] !== "string") throw new Error("Agent 升级结果字段无效")
  }
  return { status: `self-upgrade-${fields.outcome}`, fields: fields as Record<string, string> }
}

function waitForUpgradePoll(signal: AbortSignal) {
  return new Promise<void>((resolve, reject) => {
    const cancel = () => { clearTimeout(timer); reject(new DOMException("观察已停止", "AbortError")) }
    const timer = setTimeout(() => { signal.removeEventListener("abort", cancel); resolve() }, 5000)
    if (signal.aborted) cancel()
    else signal.addEventListener("abort", cancel, { once: true })
  })
}

// A resumed operation only observes its saved Job; it can never dispatch apply.
export async function runAgentUpgrade(
  session: Session,
  target: Pick<Target, "cluster" | "instance">,
  initial: AgentUpgradeOperation,
  signal: AbortSignal,
  onChange: (operation: AgentUpgradeOperation) => void,
  resume = false,
): Promise<void> {
  let operation = { ...initial }
  let verificationRejected = false
  let auditFailure = operation.auditError ? new RpcError("audit_completion_failed", -32603, operation.auditFailure) : undefined
  const update = (change: Partial<AgentUpgradeOperation>) => {
    operation = { ...operation, ...change }
    onChange(operation)
  }
  function retainAcceptance(result: unknown) {
    const fields = (result as { structuredContent?: Record<string, unknown> } | null)?.structuredContent
    if ((result as { isError?: boolean } | null)?.isError !== false || fields?.outcome !== "accepted" ||
        fields.upgrade_operation_id !== operation.id || fields.phase !== operation.phase ||
        !["job", "namespace", "release", "expected_release_commit"].every(key => typeof fields[key] === "string" && fields[key] !== "") ||
        !/^[a-f0-9]{40}$/.test(fields.expected_release_commit as string) ||
        (operation.phase !== "rollback" && fields.expected_release_commit !== operation.expectedCommit)) return
    update({
      status: "outcome_unknown", job: fields.job as string, namespace: fields.namespace as string, release: fields.release as string,
      ...(operation.phase === "rollback" && { expectedCommit: fields.expected_release_commit as string }),
    })
  }
  function rememberAuditFailure(error: RpcError, submitting: boolean) {
    if (error.message !== "audit_completion_failed" || error.code !== -32603) return false
    const data = error.data as { audit_id?: number; upgrade_operation_id?: string; result?: unknown } | undefined
    const valid = typeof data?.audit_id === "number" && Number.isSafeInteger(data.audit_id) && data.audit_id > 0 &&
      data.upgrade_operation_id === operation.id && data.result != null
    auditFailure ??= error
    update({
      ...(submitting && { status: "outcome_unknown" as const }),
      auditError: operation.auditError || `审计终态写入失败（操作 ${operation.id}${valid ? `，审计 ID ${data!.audit_id}` : ""}）。已提交的 Job 继续核验，审计问题仍需处理。`,
      ...(valid && { auditFailure: operation.auditFailure ?? { audit_id: data!.audit_id!, upgrade_operation_id: operation.id, result: data!.result } }),
    })
    if (valid && submitting) retainAcceptance(data!.result)
    return valid
  }
  async function invoke(args: Record<string, unknown>, verify = false) {
    signal.throwIfAborted()
    let result: unknown
    try {
      await callTool(session, {
        ...target, tool: "doops_agent_upgrade", signal,
        requiredProtocol: verify ? "structuredSSH" : undefined,
        expectedReleaseCommit: verify ? operation.expectedCommit : undefined,
        arguments: {
          session_id: operation.sessionId,
          upgrade_operation_id: operation.id,
          ...(operation.sourceCommit && { source_commit: operation.sourceCommit, ci_url: operation.ciURL }),
          ...(operation.expectedCommit && { expected_release_commit: operation.expectedCommit }),
          ...args,
        },
      }, (event) => {
        if (event.type === "result") {
          result = event.result
          if (!args.observe_job && !args.dry_run) retainAcceptance(result)
        }
        if (event.type === "error") rememberAuditFailure(new RpcError(event.error, event.code, event.data), !args.observe_job && !args.dry_run)
      })
    } catch (error) {
      if (!(error instanceof RpcError)) {
        if (!args.observe_job && !args.dry_run && (error instanceof TypeError || signal.aborted)) update({ status: "outcome_unknown" })
        throw error
      }
      const data = error.data as { audit_id?: number; upgrade_operation_id?: string; result?: unknown } | undefined
      const isAuditFailure = error.message === "audit_completion_failed" && error.code === -32603
      const hasAuditEvidence = rememberAuditFailure(error, !args.observe_job && !args.dry_run)
      if (verify && hasAuditEvidence) {
        const fields = (data!.result as { structuredContent?: Record<string, unknown> })?.structuredContent
        if (fields?.phase === "verify") {
          if (fields.upgrade_operation_id !== operation.id || fields.expected_release_commit !== operation.expectedCommit ||
              fields.required_protocol !== "structuredSSH" || typeof fields.structured_ssh !== "boolean") throw error
          if (fields.outcome === "success" && fields.category === "" && fields.release_commit === operation.expectedCommit &&
              fields.structured_ssh === true && (data!.result as { isError?: boolean }).isError === false) return data!.result
          if (fields.outcome === "error" && (data!.result as { isError?: boolean }).isError === true &&
              ((fields.category === "upgrade_verification_protocol_missing" && !fields.structured_ssh) ||
               (fields.category === "upgrade_verification_commit_mismatch" && typeof fields.release_commit === "string" && fields.release_commit !== operation.expectedCommit))) {
            throw new RpcError(fields.category as string, -32003, data)
          }
          throw error
        }
      }
      if (error.code === -32008) update({ status: "outcome_unknown" })
      if (data?.result !== undefined) {
        update({ output: extractText(data.result) })
        let fields: Record<string, string>
        try { fields = upgradeResponse(data.result).fields } catch { throw error }
        if (fields.upgrade_operation_id === operation.id) {
          if (fields.outcome === "failed" && (args.observe_job ? matchesObservation(fields) : fields.phase === operation.phase)) update({ status: "failed" })
          if (fields.outcome === "unknown") {
            update({ status: "outcome_unknown" })
            if (!args.observe_job && fields.phase === operation.phase && fields.job && fields.namespace) {
              update({ job: fields.job, namespace: fields.namespace, release: fields.release })
            }
          }
        }
      }
      if (!hasAuditEvidence) {
        if (isAuditFailure && !args.observe_job && !args.dry_run) update({ status: "outcome_unknown" })
        throw error
      }
      result = data!.result
      if ((result as { isError?: boolean } | null)?.isError) throw error
    }
    update({ output: extractText(result) })
    return result
  }
  function matchesObservation(fields: Record<string, string>) {
    return fields.job === operation.job && fields.namespace === operation.namespace &&
      fields.upgrade_operation_id === operation.id && fields.phase === "observe" &&
      fields.job_phase === operation.phase && Boolean(fields.release) &&
      (!operation.release || fields.release === operation.release) &&
      (!operation.expectedCommit || fields.expected_release_commit === operation.expectedCommit)
  }
  function checkObservation(result: unknown) {
    const parsed = upgradeResponse(result)
    if (!matchesObservation(parsed.fields)) {
      throw new Error("观察结果与本次升级 Job 或操作 ID 不一致")
    }
    // An unknown create result can carry only the deterministic Job identity.
    // Recover the missing receipt fields from that Job's correlated observation.
    if (!operation.release) update({ release: parsed.fields.release })
    if (operation.phase === "rollback" && !operation.expectedCommit && /^[a-f0-9]{40}$/.test(parsed.fields.expected_release_commit || "")) {
      update({ expectedCommit: parsed.fields.expected_release_commit })
    }
    if (parsed.status === "self-upgrade-failed") {
      update({ status: "failed" })
      throw new Error(`升级 Job 失败：${extractText(result)}`)
    }
    if (!["self-upgrade-running", "self-upgrade-succeeded"].includes(parsed.status)) throw new Error("Agent 返回了无效的升级状态")
    return parsed.status
  }
  async function accept(phase: AgentUpgradeOperation["phase"]) {
    update({ phase, verificationComplete: false, job: undefined, namespace: undefined, release: undefined, deadline: Date.now() + 21 * 60_000 })
    const result = await invoke(phase === "rollback" ? { rollback: true } : { image: operation.image, mode: "k8s", preflight: phase === "preflight" })
    update({ status: "outcome_unknown" })
    const { status, fields } = upgradeResponse(result)
    if (status !== "self-upgrade-accepted" || !fields.job || !fields.namespace || !fields.release ||
        fields.upgrade_operation_id !== operation.id || fields.phase !== phase) {
      throw new Error("Agent 未返回本次升级 Job 的有效受理回执；请按操作 ID 核对审计")
    }
    update({ status: "running", job: fields.job, namespace: fields.namespace, release: fields.release })
    if (phase === "rollback") {
      if (!/^[a-f0-9]{40}$/.test(fields.expected_release_commit || "")) throw new Error("回滚已受理，但缺少有效 expected_release_commit，无法完成验收")
      update({ expectedCommit: fields.expected_release_commit })
    } else if (fields.expected_release_commit !== operation.expectedCommit) {
      throw new Error("Job 回执的 expected_release_commit 与本次来源提交不一致")
    }
  }
  async function observe() {
    if (!operation.job || !operation.namespace) throw new Error("没有可恢复观察的 Job，请按操作 ID 核对审计；不要重复提交")
    while (true) {
      const args = { observe_job: operation.job, namespace: operation.namespace }
      const result = await invoke(args)
      if (checkObservation(result) === "self-upgrade-succeeded") {
        if (operation.phase !== "preflight") {
          if (!/^[a-f0-9]{40}$/.test(operation.expectedCommit)) throw new Error("缺少预期 release_commit，不能标记验收成功")
          try {
            const verified = await invoke(args, true)
            const fields = (verified as { structuredContent?: Record<string, unknown> })?.structuredContent
            if (fields?.phase === "verify") {
              if (fields.upgrade_operation_id !== operation.id || fields.outcome !== "success" || fields.category !== "" ||
                  fields.expected_release_commit !== operation.expectedCommit || fields.release_commit !== operation.expectedCommit ||
                  fields.required_protocol !== "structuredSSH" || fields.structured_ssh !== true) throw new Error("最终验收证据与本次操作不一致")
            } else if (checkObservation(verified) !== "self-upgrade-succeeded") throw new Error("最终验收时 Job 不再处于成功状态")
          } catch (error) {
            verificationRejected = error instanceof RpcError && error.code === -32003 &&
              ["upgrade_verification_protocol_missing", "upgrade_verification_commit_mismatch"].includes(error.message)
            throw error
          }
        }
        update({ verificationComplete: true })
        return
      }
      if (Date.now() >= operation.deadline) throw new Error("Job 观察超时，请核对目标状态；已提交的 Job 继续由集群控制器执行")
      await waitForUpgradePoll(signal)
    }
  }
  try {
    update({ status: "running", error: "" })
    if (resume) {
      await observe()
    } else if (operation.phase === "dry_run") {
      const result = await invoke({ image: operation.image, mode: "k8s", dry_run: true })
      if (upgradeResponse(result).status !== "self-upgrade-dry_run") throw new Error("Agent 未返回有效预览")
    } else {
      if (operation.phase === "preflight") {
        await accept("preflight")
        await observe()
        signal.throwIfAborted()
        if (operation.auditError) {
          update({ status: "audit_failed", error: "预检审计未完成，已停止后续 apply" })
          throw auditFailure
        }
        await accept("apply")
      } else if (operation.phase === "rollback") {
        await accept("rollback")
      } else {
        throw new Error("升级必须先完成预检")
      }
      await observe()
    }
    if (auditFailure) throw auditFailure
    update({
      status: operation.rollbackReason ? "rolled_back" : operation.phase === "dry_run" ? "previewed" : operation.phase === "preflight" ? "preflight_passed" : "verified",
      ...(operation.rollbackReason && { error: `升级验收未通过，已回滚并核验前一成功版本：${operation.rollbackReason}` }),
    })
  } catch (error) {
    if (operation.phase === "apply" && verificationRejected && !signal.aborted) {
      const reason = (error as RpcError).message
      update({ rollbackReason: reason, expectedCommit: "", error: "", status: "running" })
      try {
        await accept("rollback")
        await observe()
        update({ status: auditFailure ? "audit_failed" : "rolled_back", error: `升级验收未通过，已回滚并核验前一成功版本：${operation.rollbackReason}` })
      } catch (rollbackError) {
        update({
          status: operation.status === "outcome_unknown" ? "outcome_unknown" : operation.status === "failed" || !operation.job ? "failed" : "paused",
          error: `升级验收未通过；回滚尚未确认：${rollbackError instanceof Error ? rollbackError.message : "回滚请求失败"}`,
        })
        throw rollbackError
      }
      throw error
    }
    update({
      status: operation.status === "outcome_unknown" ? "outcome_unknown" : operation.status === "failed" ? "failed" : operation.auditError ? "audit_failed" : !operation.job ? "failed" : "paused",
      error: operation.error || (error instanceof Error && error.name === "AbortError"
        ? "已停止页面观察；已提交的 Job 继续由集群内控制器执行"
        : error instanceof Error ? error.message : "升级请求失败"),
    })
    throw error
  }
}
