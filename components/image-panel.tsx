"use client"

import { Boxes, Send, Square } from "lucide-react"
import { useEffect, useRef, useState } from "react"
import { Button, Input, Select, SegmentedControl } from "./ui"
import { callTool, extractText, type Session, type Target, type ToolCancel } from "@/lib/client"
import {
  appendImageOutput,
  createImageOutput,
  durationWarning,
  explainImageFailure,
  finishImageOutput,
  imageBuildRequest,
  imageCompletion,
  imageCopyRequest,
  imageOutputText,
  imageResults,
  registrySyncRequest,
  type ImageBuildInput,
  type ImageCopyInput,
  type ImageDigest,
  type ImageFailure,
  type ImageMode,
  type ImageOutput,
  type RegistrySyncInput,
  type ShellToolRequest,
} from "@/lib/image"

const MODES: { id: ImageMode; label: string; hint: string }[] = [
  { id: "build", label: "构建", hint: "doops image build：在目标上用 BuildKit 构建远端目录里的 Dockerfile" },
  { id: "copy", label: "复制", hint: "doops image copy：用 Skopeo 在任意传输方式之间复制镜像并校验 manifest digest" },
  { id: "sync", label: "Registry 同步", hint: "doops registry sync：把同一 tag 的多个镜像从源 Registry 同步到目标 Registry" },
]

const LABEL_CLASS = "flex min-w-0 flex-col gap-1 text-xs text-muted-foreground"

interface RunSummary {
  mode: ImageMode
  digests: ImageDigest[]
  failure: ImageFailure | null
  verify: boolean
}

export function ImagePanel({
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
  const [mode, setMode] = useState<ImageMode>("build")
  const [build, setBuild] = useState<ImageBuildInput>({ context: "", dockerfile: "", platform: "linux/amd64", output: "oci", outputPath: "", image: "", push: false })
  const [copy, setCopy] = useState<ImageCopyInput>({ source: "", destination: "", sourceAuthfile: "", destinationAuthfile: "", all: true, verify: true, timeout: 600 })
  const [sync, setSync] = useState<RegistrySyncInput>({ sourceRegistry: "", targetRegistry: "", tag: "", images: "", sourceAuthfile: "", targetAuthfile: "", timeout: 600 })
  const [header, setHeader] = useState("")
  const [outputText, setOutputText] = useState("")
  const [progress, setProgress] = useState("")
  const [summary, setSummary] = useState<RunSummary | null>(null)
  const [stopNote, setStopNote] = useState("")
  const [running, setRunning] = useState(false)
  const [stopPending, setStopPending] = useState(false)
  const [error, setError] = useState("")
  const cancelRef = useRef<ToolCancel | null>(null)
  const stopRequestedRef = useRef(false)
  // 输出放在 ref 里按帧刷新：长输出时不会每行都重渲染整个日志。
  const outputRef = useRef<ImageOutput>(createImageOutput())
  const frameRef = useRef<number | null>(null)
  const mountedRef = useRef(true)
  const logRef = useRef<HTMLPreElement>(null)
  const stickToBottomRef = useRef(true)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      if (frameRef.current !== null) cancelAnimationFrame(frameRef.current)
      frameRef.current = null
    }
  }, [])

  useEffect(() => {
    const log = logRef.current
    if (log && stickToBottomRef.current) log.scrollTop = log.scrollHeight
  }, [outputText])

  function flushOutput() {
    if (frameRef.current !== null) cancelAnimationFrame(frameRef.current)
    frameRef.current = null
    if (!mountedRef.current) return
    setOutputText(imageOutputText(outputRef.current))
    setProgress(outputRef.current.progress)
  }

  function scheduleFlush() {
    if (!mountedRef.current || frameRef.current !== null) return
    frameRef.current = requestAnimationFrame(() => {
      frameRef.current = null
      flushOutput()
    })
  }

  function changed() {
    setError("")
  }

  function updateBuild<K extends keyof ImageBuildInput>(key: K, value: ImageBuildInput[K]) {
    setBuild((current) => ({ ...current, [key]: value }))
    changed()
  }
  function updateCopy<K extends keyof ImageCopyInput>(key: K, value: ImageCopyInput[K]) {
    setCopy((current) => ({ ...current, [key]: value }))
    changed()
  }
  function updateSync<K extends keyof RegistrySyncInput>(key: K, value: RegistrySyncInput[K]) {
    setSync((current) => ({ ...current, [key]: value }))
    changed()
  }

  function buildRequest(): ShellToolRequest {
    if (mode === "build") return imageBuildRequest(sessionId, build)
    if (mode === "copy") return imageCopyRequest(sessionId, copy)
    return registrySyncRequest(sessionId, sync)
  }

  async function run() {
    if (running || stopPending) return
    let request: ShellToolRequest
    try {
      request = buildRequest()
    } catch (err) {
      setError((err as Error).message)
      return
    }
    // 目标、模式和超时在发出时取定：执行期间目标列表刷新或界面切换都不改变结果的解读。
    const runMode = mode
    const where = { cluster: target.cluster, instance: target.instance }
    const timeout = runMode === "copy" ? copy.timeout : runMode === "sync" ? sync.timeout : undefined
    const verify = runMode !== "copy" || copy.verify
    const output = createImageOutput()
    outputRef.current = output
    stopRequestedRef.current = false
    stickToBottomRef.current = true
    setError("")
    setSummary(null)
    setStopNote("")
    setHeader(`目标 ${where.cluster}/${where.instance} · ${MODES.find((m) => m.id === runMode)?.label}`)
    flushOutput()
    setRunning(true)
    setStopPending(false)
    onRunningChange(true)
    cancelRef.current = null
    let failureMessage = ""
    try {
      await callTool(
        session,
        {
          cluster: where.cluster,
          instance: where.instance,
          tool: request.tool,
          arguments: request.arguments,
          onCancelReady: (cancel) => { cancelRef.current = cancel },
        },
        (event) => {
          if (event.type === "output") {
            appendImageOutput(output, event.data)
            scheduleFlush()
          }
          if (event.type === "result") {
            const completion = imageCompletion(extractText(event.result))
            if (!completion.ok) failureMessage = completion.text
            else if (completion.text) {
              appendImageOutput(output, completion.text + "\n")
              scheduleFlush()
            }
          }
        },
      )
    } catch (err) {
      failureMessage = (err as Error).message || "工具执行失败"
    } finally {
      finishImageOutput(output)
      const { digests } = imageResults(runMode, output)
      const failure = failureMessage
        ? explainImageFailure(failureMessage, output, { stopRequested: stopRequestedRef.current, target: where, timeout })
        : null
      if (outputRef.current === output) {
        flushOutput()
        if (mountedRef.current) setSummary({ mode: runMode, digests, failure, verify })
      }
      setRunning(false)
      setStopPending(false)
      setStopNote("")
      onRunningChange(false)
      cancelRef.current = null
    }
  }

  function stop() {
    if (!cancelRef.current || stopPending) return
    stopRequestedRef.current = true
    setStopPending(true)
    setStopNote("已请求停止，等待 Agent 返回终态；请勿立即重试")
    void cancelRef.current().catch((err) => {
      stopRequestedRef.current = false
      setStopPending(false)
      setStopNote(`停止请求失败：${(err as Error).message}`)
    })
  }

  const current = MODES.find((m) => m.id === mode)!
  const warning = durationWarning(mode, mode === "build" ? build : mode === "copy" ? copy : sync)

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b bg-card px-4 py-3">
        <div className="flex min-w-0 items-center gap-2">
          <Boxes aria-hidden="true" className="size-4 text-primary" />
          <div className="min-w-0">
            <h2 className="truncate text-sm font-semibold text-foreground">镜像工作台</h2>
            <p className="truncate text-xs text-muted-foreground">BuildKit · Skopeo · {target.cluster}/{target.instance}</p>
          </div>
        </div>
        <SegmentedControl
          value={mode}
          onChange={(value) => { setMode(value); changed() }}
          disabled={running}
          ariaLabel="镜像操作"
          options={MODES.map(({ id, label }) => ({ value: id, label }))}
        />
      </header>

      <div className="min-h-0 flex-1 overflow-y-auto p-4">
        <form
          className="flex w-full flex-col gap-3"
          onSubmit={(event) => { event.preventDefault(); void run() }}
        >
          <p className="text-xs text-muted-foreground">{current.hint}。私有 Registry 使用 Gateway 为本目标下发的 authfile，界面不接收凭据内容；只有目标把 authfile 放在别处时才需要填写远端路径。</p>
          <fieldset disabled={running} className="grid min-w-0 grid-cols-1 gap-3 sm:grid-cols-2">
            {mode === "build" && (
              <>
                <label className={LABEL_CLASS}>构建上下文目录
                  <Input aria-label="构建上下文目录" required value={build.context} onChange={(e) => updateBuild("context", e.target.value)} placeholder="/root/ws/app" />
                </label>
                <label className={LABEL_CLASS}>Dockerfile（可选）
                  <Input aria-label="Dockerfile" value={build.dockerfile} onChange={(e) => updateBuild("dockerfile", e.target.value)} placeholder="默认 <上下文>/Dockerfile" />
                </label>
                <label className={LABEL_CLASS}>平台（逗号分隔，可选）
                  <Input aria-label="平台" value={build.platform} onChange={(e) => updateBuild("platform", e.target.value)} placeholder="linux/amd64,linux/arm64" />
                </label>
                <label className={LABEL_CLASS}>输出方式
                  <Select aria-label="输出方式" value={build.output} onChange={(e) => { const output = e.target.value as ImageBuildInput["output"]; setBuild((c) => ({ ...c, output, push: output === "oci" ? false : c.push })); changed() }}>
                    <option value="oci">OCI 归档</option>
                    <option value="image">镜像</option>
                  </Select>
                </label>
                {build.output === "oci" ? (
                  <label className={`${LABEL_CLASS} sm:col-span-2`}>OCI 归档路径
                    <Input aria-label="OCI 归档路径" required value={build.outputPath} onChange={(e) => updateBuild("outputPath", e.target.value)} placeholder="/root/ws/app.oci" />
                  </label>
                ) : (
                  <>
                    <label className={LABEL_CLASS}>镜像引用
                      <Input aria-label="镜像引用" required value={build.image} onChange={(e) => updateBuild("image", e.target.value)} placeholder="registry.example.com/team/app:v1.2.3" />
                    </label>
                    <label className="flex items-center gap-2 self-end pb-2 text-xs text-muted-foreground">
                      <input type="checkbox" checked={build.push} onChange={(e) => updateBuild("push", e.target.checked)} />
                      构建后经 BuildKit 推送到 Registry
                    </label>
                  </>
                )}
              </>
            )}

            {mode === "copy" && (
              <>
                <label className={`${LABEL_CLASS} sm:col-span-2`}>源镜像（含传输方式）
                  <Input aria-label="源镜像" required value={copy.source} onChange={(e) => updateCopy("source", e.target.value)} placeholder="docker://registry.example.com/team/app:v1" />
                </label>
                <label className={`${LABEL_CLASS} sm:col-span-2`}>目标镜像（含传输方式）
                  <Input aria-label="目标镜像" required value={copy.destination} onChange={(e) => updateCopy("destination", e.target.value)} placeholder="docker://mirror.example.com/team/app:v1 或 oci-archive:/root/ws/app.oci" />
                </label>
                <label className={LABEL_CLASS}>源 authfile 远端路径（可选）
                  <Input aria-label="源 authfile" value={copy.sourceAuthfile} onChange={(e) => updateCopy("sourceAuthfile", e.target.value)} placeholder="默认使用 Gateway 下发的 authfile" />
                </label>
                <label className={LABEL_CLASS}>目标 authfile 远端路径（可选）
                  <Input aria-label="目标 authfile" value={copy.destinationAuthfile} onChange={(e) => updateCopy("destinationAuthfile", e.target.value)} placeholder="默认使用 Gateway 下发的 authfile" />
                </label>
                <label className={LABEL_CLASS}>单步超时（秒）
                  <Input aria-label="超时" type="number" min={1} max={86400} step={1} value={copy.timeout} onChange={(e) => updateCopy("timeout", Number(e.target.value))} />
                </label>
                <div className="flex flex-col justify-end gap-1 pb-1 text-xs text-muted-foreground">
                  <label className="flex items-center gap-2"><input type="checkbox" checked={copy.all} onChange={(e) => updateCopy("all", e.target.checked)} />复制全部平台 manifest（--all）</label>
                  <label className="flex items-center gap-2"><input type="checkbox" checked={copy.verify} onChange={(e) => updateCopy("verify", e.target.checked)} />复制后比对源和目标 manifest digest</label>
                </div>
              </>
            )}

            {mode === "sync" && (
              <>
                <label className={LABEL_CLASS}>源 Registry 路径
                  <Input aria-label="源 Registry" required value={sync.sourceRegistry} onChange={(e) => updateSync("sourceRegistry", e.target.value)} placeholder="registry.example.com/source/team" />
                </label>
                <label className={LABEL_CLASS}>目标 Registry 路径
                  <Input aria-label="目标 Registry" required value={sync.targetRegistry} onChange={(e) => updateSync("targetRegistry", e.target.value)} placeholder="harbor.example.com/mirror/team" />
                </label>
                <label className={LABEL_CLASS}>不可变 tag
                  <Input aria-label="tag" required value={sync.tag} onChange={(e) => updateSync("tag", e.target.value)} placeholder="release-20261101" />
                </label>
                <label className={LABEL_CLASS}>单步超时（秒）
                  <Input aria-label="超时" type="number" min={1} max={86400} step={1} value={sync.timeout} onChange={(e) => updateSync("timeout", Number(e.target.value))} />
                </label>
                <label className={`${LABEL_CLASS} sm:col-span-2`}>镜像名（不含 Registry 和 tag，逗号或空格分隔）
                  <Input aria-label="镜像名" required value={sync.images} onChange={(e) => updateSync("images", e.target.value)} placeholder="api, worker" />
                </label>
                <label className={LABEL_CLASS}>源 authfile 远端路径（可选）
                  <Input aria-label="源 authfile" value={sync.sourceAuthfile} onChange={(e) => updateSync("sourceAuthfile", e.target.value)} placeholder="默认使用 Gateway 下发的 authfile" />
                </label>
                <label className={LABEL_CLASS}>目标 authfile 远端路径（可选）
                  <Input aria-label="目标 authfile" value={sync.targetAuthfile} onChange={(e) => updateSync("targetAuthfile", e.target.value)} placeholder="默认使用 Gateway 下发的 authfile" />
                </label>
              </>
            )}
          </fieldset>

          {warning && <p role="status" className="text-xs text-amber-600">{warning}</p>}
          {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" disabled={running || stopPending} variant="primary">
              <Send aria-hidden="true" className="size-4" />执行{current.label}
            </Button>
            {running && (
              <Button type="button" onClick={stop} disabled={stopPending}>
                <Square aria-hidden="true" className="size-4" />{stopPending ? "停止中…" : "停止"}
              </Button>
            )}
            {stopNote && <span role="status" className="text-xs text-muted-foreground">{stopNote}</span>}
          </div>
        </form>

        {summary && (
          <section aria-label="执行结果" className="mt-4 w-full rounded-md border bg-card p-3 text-xs">
            {summary.failure ? (
              <p role="alert" className="text-destructive">{summary.failure.text}</p>
            ) : (
              <p className="text-foreground">
                执行完成
                {summary.digests.length === 0 && (summary.verify ? "，Agent 输出中没有 manifest digest" : "，未开启 digest 校验，没有 manifest digest")}
              </p>
            )}
            {summary.digests.length > 0 && (
              <>
                <p className="mt-2 text-muted-foreground">{summary.failure ? "失败前已完成并校验：" : "manifest digest："}</p>
                <ul className="mt-1 flex flex-col gap-1">
                  {summary.digests.map((item) => (
                    <li key={`${item.ref} ${item.digest}`} className="break-all font-mono text-foreground">
                      {item.ref && <span className="text-muted-foreground">{item.ref} </span>}
                      {item.digest}
                      {item.kind === "manifest list" && <span className="text-muted-foreground">（manifest list）</span>}
                    </li>
                  ))}
                </ul>
              </>
            )}
          </section>
        )}

        {header && (
          <div className="mt-4 w-full">
            <p className="text-xs text-muted-foreground">{header}{running && progress ? ` · ${progress}` : ""}</p>
            {outputText && (
              <pre
                ref={logRef}
                aria-label="执行输出"
                onScroll={(event) => {
                  const log = event.currentTarget
                  stickToBottomRef.current = log.scrollHeight - log.scrollTop - log.clientHeight < 24
                }}
                className="mt-1 max-h-[60vh] overflow-auto whitespace-pre-wrap break-all rounded-md border bg-background p-3 font-mono text-xs text-foreground"
              >
                {outputText}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  )
}
