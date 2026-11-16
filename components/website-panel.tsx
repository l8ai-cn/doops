"use client"

import { Globe2, Send, Square } from "lucide-react"
import { useRef, useState } from "react"
import { callTool, extractText, type Session, type Target, type ToolCancel } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { addAgentRunNote, addAgentRunOutput, addAgentRunResult, startAgentRun, type AgentRun, type AgentRunNote } from "@/lib/agent-output"
import { AgentRunOutput } from "@/components/agent-run-output"
import { Button, Input, Select, SegmentedControl, Textarea } from "./ui"

type WebsiteMode = "check" | "plan" | "apply"

const QUICK_REQUESTS = [
  { label: "站点巡检", mode: "check" as const, request: "检查当前目标上的 DoWeb 站点、Nginx 配置、TLS、DNS 和 HTTP 健康状态，只读执行并返回证据。" },
  { label: "生成发布计划", mode: "plan" as const, request: "根据我的需求生成 SiteSpec、Nginx 配置计划、验证步骤和回滚方案，不执行变更。" },
  { label: "回滚站点", mode: "apply" as const, request: "检查当前站点最近一次发布，若存在明确的上一版本则生成并执行受控回滚，完成 nginx -t、reload 和健康验证。" },
]

export function WebsitePanel({
  session,
  target,
  sessionId,
  onRunningChange,
}: {
  session: Session
  target: Target
  sessionId: string
  onRunningChange: (running: boolean) => void
}) {
  const [mode, setMode] = useState<WebsiteMode>("check")
  const [runtimeMode, setRuntimeMode] = useState<"host" | "docker">("host")
  const [request, setRequest] = useState("")
  const [siteID, setSiteID] = useState("")
  const [domain, setDomain] = useState("")
  const [dist, setDist] = useState("")
  const [credentials, setCredentials] = useState("")
  const [agentRun, setAgentRun] = useState<AgentRun | null>(null)
  const [running, setRunning] = useState(false)
  const [stopPending, setStopPending] = useState(false)
  const [error, setError] = useState("")
  const cancelRef = useRef<ToolCancel | null>(null)

  function push(note: AgentRunNote) {
    setAgentRun((current) => current && addAgentRunNote(current, note))
  }

  async function run() {
    const text = request.trim()
    if (!text || running || stopPending) return
    const credentialRefs = mode === "apply" ? credentials.split(/[\s,]+/).filter(Boolean) : []
    if (credentialRefs.some((value) => !/^[a-z0-9][a-z0-9.-]*$/.test(value))) {
      setError("受管凭据只能填写资源名")
      return
    }
    setError("")
    setRunning(true)
    setStopPending(false)
    onRunningChange(true)
    cancelRef.current = null
    const instruction = [
      `使用 DoWeb skill 处理目标 ${target.cluster}/${target.instance}。`,
      `Nginx runtime_mode=${runtimeMode}`,
      mode === "check" ? "保持只读，不修改 Nginx 或站点文件。" : "先输出结构化 SiteSpec、证据和结果。",
      mode === "plan" ? "只生成计划，不执行 Nginx 变更。" : "",
      mode === "apply" ? "只有在 Gateway 授权、站点范围和变更条件齐全时才执行；必须完成 nginx -t、reload、健康检查并保留回滚证据。" : "",
      siteID.trim() ? `site_id=${siteID.trim()}` : "",
      domain.trim() ? `domain=${domain.trim()}` : "",
      dist.trim() ? `dist=${dist.trim()}` : "",
      `用户需求：${text}`,
    ].filter(Boolean).join("\n")
    setAgentRun(startAgentRun(`目标 ${target.cluster}/${target.instance} · ${modeLabel(mode)}`))
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.prompt,
          arguments: {
            session_id: sessionId,
            operation: mode === "apply" ? "apply" : "ask",
            skill: "doweb",
            instruction,
            ...(credentialRefs.length ? { credential_refs: credentialRefs } : {}),
          },
          onCancelReady: (cancel) => { cancelRef.current = cancel },
        },
        (event) => {
          if (event.type === "output") setAgentRun((current) => current && addAgentRunOutput(current, event.kind, event.data))
          if (event.type === "result") {
            const result = extractText(event.result)
            setAgentRun((current) => current && addAgentRunResult(current, result))
          }
        },
      )
    } catch (err) {
      if ((err as Error).name !== "AbortError") {
        const message = (err as Error).message
        setError(message)
        push({ kind: "err", text: message })
      }
    } finally {
      setRunning(false)
      setStopPending(false)
      onRunningChange(false)
      cancelRef.current = null
    }
  }

  function stop() {
    if (!cancelRef.current || stopPending) return
    setStopPending(true)
    push({ kind: "info", text: "已请求停止，等待远端终态；请勿立即重试" })
    void cancelRef.current().catch((err) => {
      setStopPending(false)
      push({ kind: "err", text: `停止请求失败：${(err as Error).message}` })
    })
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b bg-card px-4 py-3">
        <div className="flex min-w-0 items-center gap-2">
          <Globe2 aria-hidden="true" className="size-4 text-primary" />
          <div className="min-w-0">
            <h2 className="truncate text-sm font-semibold text-foreground">网站工作台</h2>
            <p className="truncate text-xs text-muted-foreground">DoWeb · {target.cluster}/{target.instance}</p>
          </div>
        </div>
        <SegmentedControl
          value={mode}
          disabled={running}
          ariaLabel="网站操作模式"
          onChange={setMode}
          options={(["check", "plan", "apply"] as const).map((value) => ({ value, label: modeLabel(value) }))}
        />
      </header>
      <div className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-4">
        <div className="flex flex-wrap gap-2">
          {QUICK_REQUESTS.map((item) => (
            <Button key={item.label} type="button" size="sm" disabled={running} onClick={() => { setMode(item.mode); setRequest(item.request); setError("") }} className="text-xs text-muted-foreground hover:border-primary/50 hover:text-foreground">
              {item.label}
            </Button>
          ))}
        </div>
        <div className="grid gap-2 sm:grid-cols-3">
          <Input aria-label="站点 ID" value={siteID} onChange={(event) => setSiteID(event.target.value)} disabled={running} placeholder="站点 ID" />
          <Input aria-label="域名" value={domain} onChange={(event) => setDomain(event.target.value)} disabled={running} placeholder="域名，例如 app.example.com" />
          <Input aria-label="dist 路径" value={dist} onChange={(event) => setDist(event.target.value)} disabled={running} placeholder="当前 session 的 dist 路径" />
        </div>
        <label className="flex items-center gap-2 text-xs text-muted-foreground">Nginx<Select scale="sm" aria-label="Nginx 运行方式" value={runtimeMode} disabled={running} onChange={(event) => setRuntimeMode(event.target.value as "host" | "docker")}><option value="host">Host</option><option value="docker">Docker</option></Select></label>
        <label className="flex min-h-28 grow shrink-0 flex-col gap-1.5">
          <span className="text-xs font-medium text-foreground">操作需求</span>
          <Textarea aria-label="操作需求" value={request} onChange={(event) => setRequest(event.target.value)} disabled={running} placeholder="描述站点、域名、dist、代理、TLS 或巡检目标。" className="min-h-28 flex-1 resize-y text-sm leading-6 focus:ring-2 focus:ring-ring/20" />
        </label>
        {mode === "apply" && (
          <Input aria-label="受管凭据" value={credentials} onChange={(event) => setCredentials(event.target.value)} disabled={running} placeholder="受管凭据资源名，可填写多个，用空格分隔" />
        )}
        {error && <p role="alert" className="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</p>}
        <div className="flex items-center justify-end gap-2">
          {running || stopPending ? <Button type="button" onClick={stop} disabled={stopPending} variant="danger-outline"><Square aria-hidden="true" className="size-3.5" /> {stopPending ? "停止中…" : "停止"}</Button> : <Button type="button" onClick={() => void run()} disabled={!request.trim()} variant="primary"><Send aria-hidden="true" className="size-3.5" /> {mode === "apply" ? "提交受控变更" : "运行 DoWeb"}</Button>}
        </div>
        <section className="min-h-40 rounded-md border bg-background/60 p-3" aria-live="polite">
          <AgentRunOutput run={agentRun} running={running} empty="SiteSpec、nginx -t、reload、健康检查和回滚证据会由目标 Agent 返回，并进入统一 Gateway 审计。" />
        </section>
      </div>
    </div>
  )
}

function modeLabel(mode: WebsiteMode): string {
  if (mode === "check") return "只读巡检"
  if (mode === "plan") return "发布计划"
  return "受控发布"
}
