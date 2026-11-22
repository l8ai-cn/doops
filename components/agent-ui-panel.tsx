"use client"

import { AgentWorkspace, type AgentArtifactItem, type AgentSessionRuntime, type CodeWorkspaceSource } from "@agent-cloud/agent-ui"
import { KeyRound, Plus, RefreshCw } from "lucide-react"
import { useCallback, useEffect, useMemo, useState, useSyncExternalStore } from "react"
import { randomSession } from "@/lib/gateway"
import { isDoopsAgentSessionBusy } from "@/lib/agent-ui-runtime"
import { workspaceArtifactsFromChanges } from "@/lib/agent-code-workspace"
import { createVisibilityScopedRuntime } from "@/lib/agent-error"
import { Button } from "./ui"
import type { Session, Target } from "@/lib/client"
import { AgentExecBar } from "./agent-exec-bar"

export function AgentUiPanel({
  session,
  target,
  onRunInTerminal,
  runtime: sourceRuntime,
  sessionId,
  codeWorkspace,
  active,
  onConfigureModel,
  onSessionChange,
  onExecRunningChange,
}: {
  session: Session
  target: Target
  onRunInTerminal?: (command: string) => void
  runtime: AgentSessionRuntime
  sessionId: string
  codeWorkspace: CodeWorkspaceSource
  active: boolean
  onConfigureModel?: () => void
  onSessionChange?: (id: string) => void
  onExecRunningChange?: (running: boolean) => void
}) {
  // 面板在其他标签下保持挂载（hidden），此时不渲染智能体连接错误，避免出现在凭据等页面里。
  // 初始可见性只在创建时读取，之后由 effect 同步。
  const scoped = useMemo(() => createVisibilityScopedRuntime(sourceRuntime, active), [sourceRuntime])
  useEffect(() => scoped.setVisible(active), [scoped, active])
  const runtime = scoped.runtime
  const subscribe = useCallback(
    (listener: () => void) => runtime.subscribe(sessionId, listener),
    [runtime, sessionId],
  )
  const getSnapshot = useCallback(
    () => runtime.getSnapshot(sessionId),
    [runtime, sessionId],
  )
  const snapshot = useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
  // 执行栏里的命令还在跑时也不能换会话：面板会重新挂载，正在执行的命令输出会丢失。
  const [execRunning, setExecRunning] = useState(false)
  const busy = isDoopsAgentSessionBusy(snapshot) || execRunning
  // 同时告诉外层：执行栏有命令在跑时不能切换目标。卸载时归零，避免外层一直被锁住。
  useEffect(() => {
    onExecRunningChange?.(execRunning)
    return () => onExecRunningChange?.(false)
  }, [execRunning, onExecRunningChange])
  const [workspaceArtifacts, setWorkspaceArtifacts] = useState<AgentArtifactItem[]>([])
  const [workspaceError, setWorkspaceError] = useState("")
  const workspaceSourceId = useMemo(() => `workspace:${codeWorkspace.workspaceKey}/${sessionId}`, [codeWorkspace.workspaceKey, sessionId])

  useEffect(() => {
    if (!active) return
    const controller = new AbortController()
    setWorkspaceError("")
    void codeWorkspace.changes(sessionId, controller.signal)
      .then((changes) => {
        if (!controller.signal.aborted) setWorkspaceArtifacts(workspaceArtifactsFromChanges(workspaceSourceId, changes))
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setWorkspaceArtifacts([])
          setWorkspaceError(error instanceof Error ? error.message : String(error))
        }
      })
    return () => controller.abort()
  }, [active, codeWorkspace, sessionId, snapshot.status, snapshot.latestUserCommandId, workspaceSourceId])

  const startNewSession = useCallback(() => {
    if (busy) return
    onSessionChange?.(randomSession("console"))
  }, [busy, onSessionChange])

  return (
    <div className="flex h-full min-h-0 flex-col">
    <AgentWorkspace
      className="agent-ui min-h-0 flex-1"
      clientLabel="doops-console"
      headerActions={
        <div className="flex shrink-0 items-center gap-1">
          {workspaceError && <span role="alert" className="max-w-40 truncate text-xs text-destructive" title={workspaceError}>成果读取失败</span>}
          <Button
            aria-label="重新检查连接"
            variant="ghost"
            size="sm"
            icon
            disabled={snapshot.connection === "connecting" || snapshot.connection === "reconnecting" || (busy && snapshot.connection !== "disconnected")}
            onClick={() => {
              if (snapshot.connection !== "disconnected") runtime.close(sessionId)
              void runtime.open(sessionId).catch(() => {
                // The shared runtime publishes connection failures in its snapshot.
              })
            }}
            title="重新检查连接"
            type="button"
          ><RefreshCw aria-hidden="true" className="size-3.5" /></Button>
          {onConfigureModel && (
            <Button
              aria-label="打开凭据"
              variant="ghost"
              size="sm"
              disabled={busy}
              onClick={onConfigureModel}
              title="DoAgent 模型来自该目标绑定的 modelProvider 凭据"
              type="button"
            >
              <KeyRound aria-hidden="true" className="size-3.5" />
              <span className="hidden sm:inline">打开凭据</span>
            </Button>
          )}
          <Button
            aria-label="新会话"
            variant="ghost"
            size="sm"
            disabled={busy || !onSessionChange}
            onClick={startNewSession}
            title="新会话"
            type="button"
          >
            <Plus aria-hidden="true" className="size-3.5" />
            <span className="hidden sm:inline">新会话</span>
          </Button>
        </div>
      }
      defaultMode="work"
      locale="zh-CN"
      presentation="developer"
      runtime={runtime}
      sessionId={sessionId}
      codeWorkspace={codeWorkspace}
      workspaceArtifacts={workspaceArtifacts}
      showFullscreen={false}
    />
    <AgentExecBar
      session={session}
      target={target}
      sessionId={sessionId}
      items={snapshot.items}
      onRunInTerminal={onRunInTerminal}
      onRunningChange={setExecRunning}
    />
    </div>
  )
}
