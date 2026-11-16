"use client"

import { AgentWorkspace, type AgentSessionSnapshot } from "@agent-cloud/agent-ui"
import { ArrowLeft, ArrowUpCircle, CircleAlert, History, KeyRound, Loader2, Plus, RefreshCw, Square } from "lucide-react"
import { useCallback, useEffect, useMemo, useState, useSyncExternalStore } from "react"
import type { Session, Target } from "@/lib/client"
import { randomSession } from "@/lib/gateway"
import {
  DEPLOYMENT_EXTENSION, deploymentAvailability, deploymentExtensionCommand, deploymentNewSessionWarning, deploymentOutcome,
  deploymentStopFailure, type DeploymentAvailability, type DeploymentInput, type DeploymentNotice, type DeploymentOutcome,
} from "@/lib/deployment"
import { createDoopsAgentUiRuntime, isDoopsAgentSessionBusy } from "@/lib/agent-ui-runtime"
import { createDoopsCodeWorkspace } from "@/lib/agent-code-workspace"
import { browserSessionStorage, loadDeployState, saveDeployDraft, saveDeploySession } from "@/lib/console-persistence"
import { DeploymentForm } from "./deployment-form"

// Helm release channel: the same `doops.operations/apply` command as `doops ask --apply`,
// sent through the hosted agent session so the native timeline shows the trace and the Agent's live evidence.
export function WorkloadDeployPanel({ session, target, onOpenCredentials, onOpenUpgrade }: {
  session: Session
  target: Target
  // Passed only for platform admins; it decides what is shown, the Gateway still checks every operation.
  onOpenCredentials?: () => void
  onOpenUpgrade?: () => void
}) {
  // Keep the release session across view switches so a running apply stays visible after returning. It is kept per
  // Gateway, user and target; a token login keeps it in memory only. Logout clears both (console-persistence.ts).
  const [releaseState, setReleaseState] = useState(() => {
    const saved = loadDeployState(browserSessionStorage(), session, target.key)
    return { ...saved, sessionId: saved.sessionId && /^deploy-[a-z0-9]{1,16}$/.test(saved.sessionId) ? saved.sessionId : randomSession("deploy") }
  })
  const [viewingReceipt, setViewingReceipt] = useState(false)
  const readOnly = viewingReceipt && !!releaseState.previousRelease
  const sessionId = readOnly ? releaseState.previousRelease!.sessionId : releaseState.sessionId
  useEffect(() => {
    saveDeploySession(browserSessionStorage(), session, target.key, releaseState.sessionId, releaseState.previousRelease)
  }, [session, target.key, releaseState.sessionId, releaseState.previousRelease])
  const loadDraft = useCallback(() => loadDeployState(browserSessionStorage(), session, target.key).draft, [session, target.key])
  const saveDraft = useCallback((text: string | null) => saveDeployDraft(browserSessionStorage(), session, target.key, text), [session, target.key])
  const codeWorkspace = useMemo(() => createDoopsCodeWorkspace({ session, target, sessionId }), [session, target.cluster, target.instance, sessionId])
  const runtime = useMemo(
    () => createDoopsAgentUiRuntime({ session, target, sessionId, codeWorkspace, readOnly }),
    [session, target.cluster, target.instance, sessionId, codeWorkspace, readOnly],
  )
  const subscribe = useCallback((listener: () => void) => runtime.subscribe(sessionId, listener), [runtime, sessionId])
  const getSnapshot = useCallback(() => runtime.getSnapshot(sessionId), [runtime, sessionId])
  const snapshot = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
  const busy = isDoopsAgentSessionBusy(snapshot)
  const supportsApply = !!runtime.executeExtension && snapshot.capabilities.extensions.some(
    (capability) => capability.namespace === DEPLOYMENT_EXTENSION.namespace &&
      capability.semanticType === DEPLOYMENT_EXTENSION.semanticType && capability.schemaVersion === DEPLOYMENT_EXTENSION.schemaVersion,
  )
  const availability = deploymentAvailability({ connection: snapshot.connection, error: snapshot.error, supportsApply, running: busy })
  const outcome = deploymentOutcome({ status: snapshot.status, error: snapshot.error })
  const newSessionWarning = deploymentNewSessionWarning(outcome.kind) ||
    (releaseState.previousRelease && deploymentNewSessionWarning(releaseState.previousRelease.status))
  const [stopping, setStopping] = useState(false)
  const [stopError, setStopError] = useState("")
  const startNewRelease = useCallback(() => {
    const current = runtime.getSnapshot(sessionId)
    if (isDoopsAgentSessionBusy(current)) return
    const previousRelease = !readOnly && deploymentOutcome(current).kind === "unknown"
      ? { sessionId, status: "unknown" as const }
      : releaseState.previousRelease
    setStopError("")
    setReleaseState((state) => ({ ...state, sessionId: randomSession("deploy"), previousRelease }))
    setViewingReceipt(false)
  }, [runtime, sessionId, readOnly, releaseState.previousRelease])
  // A stop only asks the Agent to stop; the server answers once the original call reaches a terminal state.
  const stop = useCallback(async () => {
    if (readOnly) return
    setStopping(true)
    setStopError("")
    try { await runtime.interrupt(sessionId, crypto.randomUUID()) } catch (error) {
      setStopError(deploymentStopFailure(error instanceof Error ? error.message : String(error)))
    } finally { setStopping(false) }
  }, [runtime, sessionId, readOnly])
  const reconnect = useCallback(() => {
    if (snapshot.connection !== "disconnected") runtime.close(sessionId)
    void runtime.open(sessionId).catch(() => {
      // The runtime publishes connection failures in its snapshot.
    })
  }, [runtime, sessionId, snapshot.connection])

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <header className="flex shrink-0 flex-wrap items-center justify-between gap-3 border-b bg-card px-4 py-3">
        <div className="min-w-0">
          <h2 className="text-base font-semibold">发布 Helm release</h2>
          <p className="mt-0.5 text-xs text-muted-foreground">
            等同 <code className="rounded bg-muted px-1 py-0.5 font-mono text-[11px]">doops ask --target {target.cluster}/{target.instance} --apply</code>，由目标 Agent 执行用户给出的 Helm 请求并返回实时证据。
          </p>
        </div>
        <div className="flex items-center gap-2">
          <ConnectionBadge snapshot={snapshot} availability={availability} />
          <button
            type="button"
            disabled={busy}
            onClick={startNewRelease}
            title={`当前会话 ${sessionId}`}
            className="flex h-8 items-center gap-1.5 rounded-md border px-3 text-xs transition-colors hover:bg-muted disabled:cursor-not-allowed disabled:opacity-50"
          ><Plus aria-hidden="true" className="size-3.5" />新发布</button>
        </div>
      </header>
      {newSessionWarning && (
        <div role="status" className="flex flex-wrap items-center gap-2 border-b px-4 py-2 text-xs text-amber-600 dark:text-amber-400">
          <p className="min-w-0 flex-1">{newSessionWarning}</p>
          {releaseState.previousRelease && (
            <button type="button" disabled={!readOnly && busy}
              onClick={() => {
                if (!readOnly && isDoopsAgentSessionBusy(runtime.getSnapshot(sessionId))) return
                setStopError("")
                setViewingReceipt((current) => !current)
              }}
              title={readOnly ? releaseState.sessionId : releaseState.previousRelease.sessionId}
              className="flex h-7 shrink-0 items-center gap-1.5 rounded-md border px-2.5 text-xs hover:bg-muted disabled:opacity-50">
              {readOnly ? <ArrowLeft aria-hidden="true" className="size-3.5" /> : <History aria-hidden="true" className="size-3.5" />}
              {readOnly ? "返回当前发布" : "查看原回执"}
            </button>
          )}
        </div>
      )}

      {/* Narrow screens scroll the whole page with the form at full height; wide screens scroll each column. */}
      <div className="grid min-h-0 flex-1 grid-cols-1 overflow-y-auto lg:grid-cols-[minmax(0,30rem)_minmax(0,1fr)] lg:grid-rows-[minmax(0,1fr)] lg:overflow-hidden">
        <div className="border-b bg-muted/20 p-4 lg:min-h-0 lg:overflow-y-auto lg:border-b-0 lg:border-r">
          {!availability.canSubmit && availability.notice && (
            <AvailabilityNotice notice={availability.notice} onReconnect={reconnect} onOpenCredentials={onOpenCredentials} onOpenUpgrade={onOpenUpgrade} />
          )}
          {readOnly ? (
            <div className="space-y-2 text-xs">
              <h3 className="text-sm font-medium">原发布回执 · 只读</h3>
              <p className="break-all font-mono text-muted-foreground">{sessionId}</p>
              <p className="text-amber-600 dark:text-amber-400">上次记录：发布结果未确认。以原会话实时回执和目标状态为准。</p>
            </div>
          ) : <DeploymentForm
            key={sessionId}
            target={target}
            loadDraft={loadDraft}
            saveDraft={saveDraft}
            canSubmit={availability.canSubmit}
            blockedReason={availability.canSubmit ? undefined : availability.blockedReason}
            onDeploy={async (input: DeploymentInput) => {
              // The form gates on channel availability; Gateway authorization remains authoritative.
              if (readOnly) throw new Error("原发布回执只读")
              if (!availability.canSubmit) throw new Error(availability.blockedReason)
              if (!runtime.executeExtension) throw new Error("当前智能体不支持部署变更")
              await runtime.executeExtension(sessionId, crypto.randomUUID(), deploymentExtensionCommand(target, input))
            }}
          />}
        </div>
        <div className="flex h-[36rem] min-w-0 flex-col lg:h-auto lg:min-h-0">
          <div className="flex shrink-0 items-center justify-between border-b px-4 py-2">
            <div className="min-w-0">
              <h3 className="text-sm font-medium">执行过程</h3>
              <p className="truncate text-[11px] text-muted-foreground">以 Helm release 的实际终态为准；工具失败、连接中断或缺少终态都不算成功</p>
            </div>
            {!readOnly && busy && snapshot.capabilities.interrupt && (
              <button type="button" disabled={stopping || snapshot.connection !== "connected"} onClick={() => void stop()}
                className="flex h-7 shrink-0 items-center gap-1.5 rounded-md border border-destructive/50 px-2.5 text-xs text-destructive transition-colors hover:bg-destructive/10 disabled:opacity-40">
                <Square aria-hidden="true" className="size-3" />{stopping ? "正在停止…" : "停止"}
              </button>
            )}
          </div>
          <div className="min-h-0 flex-1">
            <AgentWorkspace
              key={`deploy-${target.key}-${sessionId}`}
              className="agent-ui h-full min-h-0"
              clientLabel="doops-console"
              defaultView="conversation"
              locale="zh-CN"
              presentation="developer"
              runtime={runtime}
              sessionId={sessionId}
              readOnly={readOnly}
              codeWorkspace={readOnly ? undefined : codeWorkspace}
              defaultWorkbenchMode="work"
              showFullscreen={false}
              composer={<RunStatus outcome={outcome} stopError={stopError} onOpenCredentials={onOpenCredentials} />}
            />
          </div>
        </div>
      </div>
    </div>
  )
}

function AvailabilityNotice({ notice, onReconnect, onOpenCredentials, onOpenUpgrade }: {
  notice: DeploymentNotice
  onReconnect: () => void
  onOpenCredentials?: () => void
  onOpenUpgrade?: () => void
}) {
  const connecting = notice.kind === "connecting"
  const buttons = notice.actions.flatMap((action): { action: string; label: string; Icon: typeof RefreshCw; onClick: () => void }[] => {
    if (action === "reconnect") return [{ action, label: "重新连接", Icon: RefreshCw, onClick: onReconnect }]
    if (action === "open_credentials" && onOpenCredentials) return [{ action, label: "去凭据页配置模型", Icon: KeyRound, onClick: onOpenCredentials }]
    if (action === "upgrade_agent" && onOpenUpgrade) return [{ action, label: "去升级 Agent", Icon: ArrowUpCircle, onClick: onOpenUpgrade }]
    return []
  })
  return (
    <div role={connecting ? "status" : "alert"} data-notice={notice.kind}
      className={`mb-4 rounded-lg border p-3 ${connecting ? "bg-card" : "border-amber-500/40 bg-amber-500/10"}`}>
      <div className="flex items-start gap-2">
        {connecting
          ? <Loader2 aria-hidden="true" className="mt-0.5 size-4 shrink-0 animate-spin text-muted-foreground" />
          : <CircleAlert aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" />}
        <div className="min-w-0">
          <p className="text-sm font-medium">{notice.title}</p>
          <p className="mt-1 break-words text-xs text-muted-foreground">{notice.detail}</p>
          {buttons.length > 0 && (
            <div className="mt-2.5 flex flex-wrap gap-2">
              {buttons.map(({ action, label, Icon, onClick }) => (
                <button key={action} type="button" onClick={onClick}
                  className="flex h-7 items-center gap-1.5 rounded-md border bg-background px-2.5 text-xs transition-colors hover:bg-muted">
                  <Icon aria-hidden="true" className="size-3.5" />{label}
                </button>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function ConnectionBadge({ snapshot, availability }: { snapshot: AgentSessionSnapshot; availability: DeploymentAvailability }) {
  const connecting = snapshot.connection === "connecting" || snapshot.connection === "reconnecting"
  const [tone, label] = availability.canSubmit
    ? ["bg-emerald-500", "Agent 已就绪"]
    : snapshot.connection === "connected" ? ["bg-amber-500", isDoopsAgentSessionBusy(snapshot) ? "发布执行中" : "通道不可用"]
      : connecting ? ["bg-amber-500 animate-pulse", "正在连接 Agent"] : ["bg-destructive", "Agent 不可用"]
  return (
    <span className="flex h-8 max-w-64 items-center gap-1.5 rounded-md border px-2.5 text-xs text-muted-foreground" title={availability.canSubmit ? undefined : availability.blockedReason}>
      <span aria-hidden="true" className={`size-1.5 shrink-0 rounded-full ${tone}`} />
      <span className="truncate">{label}</span>
    </span>
  )
}

// The workspace composer slot reports what the last release did to the target; releases are submitted from the form.
function RunStatus({ outcome, stopError, onOpenCredentials }: {
  outcome: DeploymentOutcome
  stopError: string
  onOpenCredentials?: () => void
}) {
  const tone = outcome.kind === "failed" ? "text-destructive"
    : outcome.kind === "unknown" || outcome.kind === "cancelled" ? "text-amber-600 dark:text-amber-400"
      : outcome.kind === "succeeded" ? "text-emerald-600 dark:text-emerald-400" : "text-muted-foreground"
  return (
    <div role="status" aria-live="polite" data-outcome={outcome.kind} className="max-h-40 shrink-0 overflow-y-auto border-t px-4 py-2.5 text-xs">
      <p className={`font-medium ${tone}`}>{outcome.title}</p>
      {outcome.detail && <p className="mt-0.5 break-words text-muted-foreground">{outcome.detail}</p>}
      {stopError && <p role="alert" className="mt-1 break-words text-destructive">{stopError}</p>}
      {outcome.actions.includes("open_credentials") && onOpenCredentials && (
        <button type="button" onClick={onOpenCredentials}
          className="mt-2 flex h-7 items-center gap-1.5 rounded-md border bg-background px-2.5 text-xs transition-colors hover:bg-muted">
          <KeyRound aria-hidden="true" className="size-3.5" />去凭据页
        </button>
      )}
    </div>
  )
}
