"use client"

import { Send, Square } from "lucide-react"
import { useRef, useState } from "react"
import { callTool, extractText, type Session, type Target, type ToolCancel } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { addAgentRunNote, addAgentRunOutput, addAgentRunResult, startAgentRun, type AgentRun, type AgentRunNote } from "@/lib/agent-output"
import { AgentRunOutput } from "@/components/agent-run-output"
import { DatabaseInventory, type DatabaseCandidate } from "./database-inventory"
import { DatabaseExplorer } from "./database-explorer"

type DatabaseMode = "query" | "discover" | "plan"

const QUICK_REQUESTS: Array<{ label: string; mode: DatabaseMode; request: string }> = [
  { label: "扫描数据库实例", mode: "discover", request: "扫描当前目标集群中的数据库实例和可识别数据库，返回 database_asset_id、引擎、命名空间、服务、端口、数据库名和需要补充的凭据，不读取 Secret 值。" },
  { label: "数据库巡检", mode: "query", request: "检查当前目标上的数据库健康、连接可用性、慢查询和明显结构风险，只读执行并返回证据。" },
  { label: "生成变更计划", mode: "plan", request: "根据我的需求解析数据库资产，生成可读的变更计划、影响范围、风险和验证步骤，不执行变更。" },
  { label: "回放执行证据", mode: "query", request: "检查最近一次 DoSql 操作的 journal 和 evidence，说明执行结果、验证状态和是否存在未知结果。" },
]

export function DatabasePanel({
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
  const [mode, setMode] = useState<DatabaseMode>("query")
  const [request, setRequest] = useState("")
  const [databaseHint, setDatabaseHint] = useState("")
  const [agentRun, setAgentRun] = useState<AgentRun | null>(null)
  const [running, setRunning] = useState(false)
  const [selectedAsset, setSelectedAsset] = useState<DatabaseCandidate | null>(null)
  const [sqlRunning, setSqlRunning] = useState(false)
  const [stopPending, setStopPending] = useState(false)
  const [error, setError] = useState("")
  const cancelRef = useRef<ToolCancel | null>(null)
  const operationsRef = useRef<HTMLDetailsElement>(null)
  const requestRef = useRef<HTMLTextAreaElement>(null)

  function openQuery(database: DatabaseCandidate) {
    setMode("query")
    setDatabaseHint([database.database_asset_id, database.engine, `${database.namespace}/${database.service}`, database.database_name].filter(Boolean).join(" · "))
    setRequest("")
    setError("")
    setAgentRun(null)
    if (operationsRef.current) operationsRef.current.open = true
    requestRef.current?.focus()
  }

  function push(note: AgentRunNote) {
    setAgentRun((current) => current && addAgentRunNote(current, note))
  }

  async function run() {
    const text = request.trim()
    if (!text || running || stopPending) return
    setError("")
    setRunning(true)
    setStopPending(false)
    onRunningChange(true)
    cancelRef.current = null
    const instruction = [
      `使用 DoSql skill 处理目标 ${target.cluster}/${target.instance}。`,
      mode === "discover" ? "只扫描当前目标集群的非 Secret Kubernetes 元数据，返回数据库实例候选，不读取凭据或 Secret 值。" : mode === "query" ? "保持只读，不执行数据库变更。" : "先输出结构化、可读的结果。",
      mode === "plan" ? "只生成变更计划，不执行 SQL 或 MongoDB 变更。" : "",
      databaseHint.trim() ? `数据库提示：${databaseHint.trim()}` : "",
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
            operation: "ask",
            skill: "dosql",
            ...(mode === "query" ? { credential_mode: "auto" } : {}),
            instruction,
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
    <div className="h-full overflow-y-auto p-4">
      <div hidden={!!selectedAsset}>
        <DatabaseInventory session={session} target={target} sessionId={sessionId} running={running || sqlRunning} onOpen={setSelectedAsset} />
      </div>
      {selectedAsset && <DatabaseExplorer key={selectedAsset.database_asset_id} session={session} target={target} sessionId={sessionId} asset={selectedAsset}
        busy={running || sqlRunning} onRunningChange={(value) => { setSqlRunning(value); onRunningChange(value) }}
        onBack={() => setSelectedAsset(null)} onQuery={openQuery} />}
      <details className="card-surface mt-4 p-4" ref={operationsRef}>
        <summary className="cursor-pointer text-sm font-semibold">数据库操作</summary>
        <fieldset disabled={sqlRunning} className="mt-4 flex min-w-0 flex-col gap-3">
          <div className="flex w-fit flex-wrap items-center gap-1 rounded-md border bg-background p-0.5" role="group" aria-label="数据库操作模式">
            {(["query", "discover", "plan"] as const).map((value) => (
              <button
                key={value}
                type="button"
                aria-pressed={mode === value}
                disabled={running}
                onClick={() => setMode(value)}
                className={`rounded px-2 py-1 text-xs transition-colors ${mode === value ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:text-foreground"}`}
              >
                {modeLabel(value)}
              </button>
            ))}
          </div>
          <div className="flex flex-wrap gap-2">
            {QUICK_REQUESTS.map((item) => (
              <button
                key={item.label}
                type="button"
                disabled={running}
                onClick={() => {
                  setMode(item.mode)
                  setRequest(item.request)
                  setError("")
                }}
                className="rounded-md border px-2.5 py-1.5 text-xs text-muted-foreground transition-colors hover:border-primary/50 hover:text-foreground disabled:opacity-50"
              >
                {item.label}
              </button>
            ))}
          </div>

          <div className="grid gap-2">
            <label className="flex min-w-0 flex-col gap-1.5">
              <span className="text-xs font-medium text-foreground">数据库提示</span>
              <input
                value={databaseHint}
                onChange={(event) => setDatabaseHint(event.target.value)}
                disabled={running}
                placeholder="数据库名称或资产提示（可选）"
                className="min-w-0 rounded-md border bg-background px-3 py-2 text-sm text-foreground outline-none focus:border-ring disabled:opacity-60"
              />
            </label>
          </div>

          <label className="flex min-h-32 grow shrink-0 flex-col gap-1.5">
            <span className="text-xs font-medium text-foreground">操作需求</span>
            <textarea
              ref={requestRef}
              value={request}
              onChange={(event) => setRequest(event.target.value)}
              disabled={running}
              placeholder="描述数据库、环境和要查询或计划的业务目标，不要粘贴密码或连接串。"
              className="min-h-32 flex-1 resize-y rounded-md border bg-background px-3 py-2 text-sm leading-6 text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/20 disabled:opacity-60"
            />
          </label>

          {error && <p role="alert" className="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{error}</p>}

          <div className="flex items-center justify-end gap-2">
            {running || stopPending ? (
              <button type="button" onClick={stop} disabled={stopPending} className="flex items-center gap-1.5 rounded-md border border-destructive/50 px-3 py-2 text-sm text-destructive hover:bg-destructive/10 disabled:opacity-60">
                <Square aria-hidden="true" className="size-3.5" /> {stopPending ? "停止中…" : "停止"}
              </button>
            ) : (
              <button type="button" onClick={() => void run()} disabled={!request.trim()} className="flex items-center gap-1.5 rounded-md bg-primary px-3 py-2 text-sm font-medium text-primary-foreground hover:opacity-90 disabled:opacity-50">
                <Send aria-hidden="true" className="size-3.5" /> 运行 DoSql
              </button>
            )}
          </div>

          <section className="min-h-40 rounded-md border bg-background/60 p-3" aria-live="polite">
            <AgentRunOutput run={agentRun} running={running} empty="结果、journal 和 evidence 会由目标 Agent 返回，并进入统一 Gateway 审计。" />
          </section>
        </fieldset>
      </details>
    </div>
  )
}

function modeLabel(mode: DatabaseMode): string {
  if (mode === "query") return "只读查询"
  if (mode === "discover") return "扫描发现"
  return "变更计划"
}
