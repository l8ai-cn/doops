"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import { callTool, extractText, type Session, type Target } from "@/lib/client"
import { TOOLS } from "@/lib/gateway"
import { SparkIcon, SendIcon, StopIcon } from "./icons"

interface Block {
  kind: "user" | "stream" | "result" | "err" | "info"
  text: string
}

const MODELS = ["openai/gpt-5.4", "anthropic/claude-opus-4.6", "google/gemini-3-flash"]

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
  const [blocks, setBlocks] = useState<Block[]>([])
  const [running, setRunning] = useState(false)
  const abortRef = useRef<AbortController | null>(null)
  const logRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    logRef.current?.scrollTo({ top: logRef.current.scrollHeight })
  }, [blocks])

  const append = useCallback(
    (text: string) =>
      setBlocks((p) => {
        const last = p[p.length - 1]
        if (last && last.kind === "stream") {
          const copy = p.slice(0, -1)
          return [...copy, { kind: "stream", text: last.text + text }]
        }
        return [...p, { kind: "stream", text }]
      }),
    [],
  )

  async function run() {
    const text = instruction.trim()
    if (!text || running) return
    setInstruction("")
    setBlocks((p) => [...p, { kind: "user", text }])
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
          arguments: { session_id: sessionId, instruction: text, model },
          signal: ac.signal,
        },
        (ev) => {
          if (ev.type === "output") append(ev.data)
          else if (ev.type === "error") setBlocks((p) => [...p, { kind: "err", text: ev.error }])
          else if (ev.type === "result") {
            const t = extractText(ev.result)
            if (t) setBlocks((p) => [...p, { kind: "result", text: t }])
          }
        },
      )
    } catch (err) {
      if ((err as Error).name !== "AbortError")
        setBlocks((p) => [...p, { kind: "err", text: (err as Error).message }])
    } finally {
      setRunning(false)
      abortRef.current = null
    }
  }

  return (
    <div className="flex h-full flex-col">
      <div ref={logRef} className="flex-1 overflow-y-auto p-4">
        {blocks.length === 0 && (
          <div className="flex h-full flex-col items-center justify-center gap-2 text-muted-foreground">
            <SparkIcon width={28} height={28} />
            <p className="text-sm">用自然语言向 {target.instance} 下发运维 / 部署任务（doops_agent_prompt）</p>
            <p className="text-xs">需要 ask 权限 · 适用于首次适配、排障、生成部署脚本</p>
            <button
              onClick={() => setInstruction(deployTemplate(sessionId))}
              className="mt-2 rounded-lg border px-3 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
            >
              填入标准部署指令模板
            </button>
          </div>
        )}
        <div className="flex flex-col gap-3">
          {blocks.map((b, i) => {
            if (b.kind === "user")
              return (
                <div key={i} className="ml-auto max-w-[85%] rounded-xl rounded-br-sm bg-primary/15 px-3 py-2 text-sm text-foreground">
                  <span className="whitespace-pre-wrap break-words">{b.text}</span>
                </div>
              )
            if (b.kind === "err")
              return (
                <pre key={i} className="whitespace-pre-wrap break-words rounded-lg bg-destructive/15 px-3 py-2 font-mono text-xs text-destructive">
                  {b.text}
                </pre>
              )
            const label = b.kind === "result" ? "最终结果" : "执行过程"
            return (
              <div key={i} className="rounded-xl border bg-card px-3 py-2">
                <div className="mb-1 text-xs font-medium text-muted-foreground">{label}</div>
                <pre className="whitespace-pre-wrap break-words font-mono text-xs leading-relaxed text-foreground/90">
                  {b.text}
                </pre>
              </div>
            )
          })}
        </div>
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
            placeholder="例如：检查仓库，构建镜像并更新 deployment/app，保留日志和脚本"
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
        <p className="mt-1.5 text-xs text-muted-foreground">Cmd/Ctrl + Enter 发送</p>
      </div>
    </div>
  )
}
