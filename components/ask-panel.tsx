"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { SparkIcon, SendIcon, StopIcon, PlusIcon, ChevronRightIcon } from "./icons"

interface Turn {
  id: string
  user: string
  process: string
  answer: string
  error?: string
  running: boolean
}

const MODELS = ["openai/gpt-5.4", "anthropic/claude-opus-4.6", "google/gemini-3-flash"]

const QUICK_PROMPTS = [
  "检查节点状态并给出健康巡检报告",
  "把仓库构建成镜像并滚动更新 deployment/app",
  "回滚到上一个稳定版本",
]

function deployTemplate(sessionId: string) {
  return `你正在部署仓库。只能在 /root/ws/${sessionId} 内工作。
先检查项目结构；如果已有 deploy.sh，优先使用它。
使用 BuildKit 构建镜像；Kubernetes 变更必须先 server-side dry-run，再真实 apply，
等待 rollout 完成，并把最终报告写入 /root/ws/${sessionId}/doops-report.md。
所有命令必须可追溯。`
}

export function AskPanel({
  session,
  target,
  sessionId,
}: {
  session: Session
  target: Target
  sessionId: string
}) {
  const [instruction, setInstruction] = useState("")
  const [model, setModel] = useState(MODELS[0])
  const [turns, setTurns] = useState<Turn[]>([])
  const [running, setRunning] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const logRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight })
  }, [turns])

  const patchLast = useCallback((fn: (t: Turn) => Turn) => {
    setTurns((p) => {
      if (p.length === 0) return p
      const copy = p.slice()
      copy[copy.length - 1] = fn(copy[copy.length - 1])
      return copy
    })
  }, [])

  async function run() {
    const text = instruction.trim()
    if (!text || running) return
    setInstruction("")
    const turn: Turn = { id: crypto.randomUUID(), user: text, process: "", answer: "", running: true }
    setTurns((p) => [...p, turn])
    setRunning(true)
    const ac = new AbortController()
    abortRef.current = ac
    try {
      await callTool(
        session,
        {
          cluster: target.cluster,
          instance: target.instance,
          tool: TOOLS.prompt,
          // 同一 session_id 让 ACP agent 维持多轮上下文
          arguments: { session_id: sessionId, instruction: text, model },
          signal: ac.signal,
        },
        (ev) => {
          if (ev.type === "output") patchLast((t) => ({ ...t, process: t.process + ev.data }))
          else if (ev.type === "error") patchLast((t) => ({ ...t, error: ev.error }))
          else if (ev.type === "result") {
            const txt = extractText(ev.result)
            if (txt) patchLast((t) => ({ ...t, answer: txt }))
          }
        },
      )
    } catch (err) {
      if ((err as Error).name !== "AbortError")
        patchLast((t) => ({ ...t, error: (err as Error).message }))
    } finally {
      patchLast((t) => ({ ...t, running: false }))
      setRunning(false)
      abortRef.current = null
    }
  }

  function newConversation() {
    abortRef.current?.abort()
    setTurns([])
  }

  return (
    <div className="flex h-full flex-col">
      <div className="flex shrink-0 items-center justify-between border-b bg-card/60 px-4 py-2">
        <div className="flex items-center gap-2 text-xs text-muted-foreground">
          <SparkIcon width={14} height={14} />
          <span>ACP 对话</span>
          <span className="font-mono">session: {sessionId}</span>
        </div>
        <button
          onClick={newConversation}
          disabled={turns.length === 0}
          className="flex items-center gap-1 rounded-md border px-2 py-1 text-xs text-foreground transition-colors hover:bg-muted disabled:opacity-40"
        >
          <PlusIcon width={13} height={13} /> 新会话
        </button>
      </div>

      <div ref={logRef} className="flex-1 overflow-y-auto p-4">
        {turns.length === 0 ? (
          <div className="flex h-full flex-col items-center justify-center gap-3 text-muted-foreground">
            <SparkIcon width={28} height={28} />
            <p className="text-sm text-pretty text-center">
              与 {target.instance} 上的 ACP 智能体多轮对话，下发运维 / 部署任务
            </p>
            <p className="text-xs">需要 ask 权限 · 上下文在同一 session 内保持</p>
            <div className="mt-2 flex flex-col items-center gap-2">
              {QUICK_PROMPTS.map((q) => (
                <button
                  key={q}
                  onClick={() => setInstruction(q)}
                  className="rounded-lg border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
                >
                  {q}
                </button>
              ))}
              <button
                onClick={() => setInstruction(deployTemplate(sessionId))}
                className="rounded-lg border border-primary/40 bg-primary/10 px-3 py-1.5 text-xs text-primary transition-colors hover:bg-primary/20"
              >
                填入标准部署指令模板
              </button>
            </div>
          </div>
        ) : (
          <div className="mx-auto flex max-w-3xl flex-col gap-5">
            {turns.map((t) => (
              <TurnView key={t.id} turn={t} />
            ))}
          </div>
        )}
      </div>

      <div className="border-t bg-card p-3">
        <div className="mb-2 flex items-center gap-2">
          <label className="text-xs text-muted-foreground">模型</label>
          <select
            value={model}
            onChange={(e) => setModel(e.target.value)}
            className="rounded-md border bg-background px-2 py-1 font-mono text-xs text-foreground outline-none focus:border-ring"
          >
            {MODELS.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        </div>
        <div className="flex items-end gap-2">
          <textarea
            value={instruction}
            onChange={(e) => setInstruction(e.target.value)}
            onKeyDown={(e) => {
              if ((e.metaKey || e.ctrlKey) && e.key === "Enter") {
                e.preventDefault()
                run()
              }
            }}
            placeholder="继续对话或下发新任务，例如：检查仓库，构建镜像并更新 deployment/app"
            rows={3}
            className="max-h-48 flex-1 resize-none rounded-lg border bg-background px-3 py-2 text-sm text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
          />
          {running ? (
            <button
              onClick={() => abortRef.current?.abort()}
              className="flex items-center gap-1.5 rounded-lg bg-destructive px-3 py-2 text-sm font-medium text-destructive-foreground transition-opacity hover:opacity-90"
            >
              <StopIcon width={16} height={16} /> 停止
            </button>
          ) : (
            <button
              onClick={run}
              disabled={!instruction.trim()}
              className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
            >
              <SendIcon width={16} height={16} /> 发送
            </button>
          )}
        </div>
        <p className="mt-1.5 text-xs text-muted-foreground">Cmd/Ctrl + Enter 发送 · 同一 session 保持上下文</p>
      </div>
    </div>
  )
}

function TurnView({ turn }: { turn: Turn }) {
  const [showProcess, setShowProcess] = useState(true)
  const hasProcess = turn.process.trim().length > 0
  return (
    <div className="flex flex-col gap-2">
      <div className="ml-auto max-w-[85%] rounded-xl rounded-br-sm bg-primary/15 px-3 py-2 text-sm text-foreground">
        <span className="whitespace-pre-wrap break-words">{turn.user}</span>
      </div>

      <div className="mr-auto flex w-full max-w-[92%] flex-col gap-2">
        {hasProcess && (
          <div className="rounded-xl border bg-card">
            <button
              onClick={() => setShowProcess((s) => !s)}
              className="flex w-full items-center gap-1.5 px-3 py-2 text-xs font-medium text-muted-foreground"
            >
              <ChevronRightIcon
                width={13}
                height={13}
                className={`transition-transform ${showProcess ? "rotate-90" : ""}`}
              />
              执行过程
              {turn.running && <span className="ml-1 animate-pulse text-primary">●</span>}
            </button>
            {showProcess && (
              <pre className="max-h-72 overflow-y-auto whitespace-pre-wrap break-words border-t px-3 py-2 font-mono text-xs leading-relaxed text-foreground/80">
                {turn.process}
              </pre>
            )}
          </div>
        )}

        {turn.answer && (
          <div className="rounded-xl rounded-bl-sm border border-primary/30 bg-primary/5 px-3 py-2.5">
            <div className="mb-1 flex items-center gap-1.5 text-xs font-medium text-primary">
              <SparkIcon width={13} height={13} /> 智能体回答
            </div>
            <p className="whitespace-pre-wrap break-words text-sm leading-relaxed text-foreground">
              {turn.answer}
            </p>
          </div>
        )}

        {turn.error && (
          <pre className="whitespace-pre-wrap break-words rounded-lg bg-destructive/15 px-3 py-2 font-mono text-xs text-destructive">
            {turn.error}
          </pre>
        )}

        {turn.running && !hasProcess && !turn.answer && (
          <div className="flex items-center gap-2 text-xs text-muted-foreground">
            <span className="animate-pulse text-primary">●</span> 智能体思考中…
          </div>
        )}
      </div>
    </div>
  )
}
