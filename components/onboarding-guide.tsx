"use client"

import { useState } from "react"
import { ServerIcon, SparkIcon, TerminalIcon, CheckIcon, CopyIcon } from "./icons"

const INSTALL_CMD = "curl -fsSL https://get.doops.sh | sh -s -- --gateway <你的网关地址>"

const STEPS = [
  {
    icon: ServerIcon,
    title: "欢迎使用 Doops 控制台",
    body: "这是一个让运维像聊天一样简单的平台。下面用三步带你快速上手，全程不到一分钟。",
    tip: "如果你只是想先看看效果，可以直接关闭本引导，使用演示数据探索。",
  },
  {
    icon: TerminalIcon,
    title: "第一步：接入你的机器",
    body: "在你的服务器上执行下面这行安装命令，Agent 会自动注册到网关，几秒后就能在左侧看到这台机器。",
    showCmd: true,
  },
  {
    icon: SparkIcon,
    title: "第二步：开始运维",
    body: "在左侧选中机器后，打开「AI 对话」用中文描述需求，或在「终端」直接执行命令。「概览」页随时查看机器健康状态。",
    tip: "试试在 AI 对话里输入：帮我看看磁盘空间还剩多少。",
  },
]

export function OnboardingGuide({ onClose }: { onClose: () => void }) {
  const [step, setStep] = useState(0)
  const [copied, setCopied] = useState(false)
  const cur = STEPS[step]
  const Icon = cur.icon
  const last = step === STEPS.length - 1

  async function copyCmd() {
    try {
      await navigator.clipboard.writeText(INSTALL_CMD)
      setCopied(true)
      setTimeout(() => setCopied(false), 1800)
    } catch {
      /* ignore */
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-background/70 p-4 backdrop-blur-sm">
      <div className="w-full max-w-lg rounded-2xl border bg-card p-6 shadow-xl">
        <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-xl bg-primary/15 text-primary">
          <Icon width={24} height={24} />
        </div>
        <h2 className="text-xl font-semibold tracking-tight text-card-foreground">{cur.title}</h2>
        <p className="mt-2 text-sm leading-relaxed text-muted-foreground">{cur.body}</p>

        {cur.showCmd && (
          <div className="mt-4">
            <div className="flex items-center justify-between rounded-lg border bg-background p-3">
              <code className="overflow-x-auto pr-2 font-mono text-xs text-foreground">{INSTALL_CMD}</code>
              <button
                onClick={copyCmd}
                className="flex shrink-0 items-center gap-1 rounded-md bg-primary px-2 py-1 text-xs font-medium text-primary-foreground transition-opacity hover:opacity-90"
              >
                {copied ? <CheckIcon width={13} height={13} /> : <CopyIcon width={13} height={13} />}
                {copied ? "已复制" : "复制"}
              </button>
            </div>
            <p className="mt-2 text-xs text-muted-foreground">
              请把 <code className="font-mono">{"<你的网关地址>"}</code> 替换为实际网关地址（可在登录页看到）。
            </p>
          </div>
        )}

        {cur.tip && (
          <p className="mt-4 rounded-lg bg-primary/10 px-3 py-2 text-xs text-primary">{cur.tip}</p>
        )}

        <div className="mt-6 flex items-center justify-between">
          <div className="flex items-center gap-1.5">
            {STEPS.map((_, i) => (
              <span
                key={i}
                className={`h-1.5 rounded-full transition-all ${
                  i === step ? "w-5 bg-primary" : "w-1.5 bg-muted"
                }`}
              />
            ))}
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={onClose}
              className="rounded-lg px-3 py-1.5 text-sm text-muted-foreground transition-colors hover:text-foreground"
            >
              跳过
            </button>
            {step > 0 && (
              <button
                onClick={() => setStep((s) => s - 1)}
                className="rounded-lg border px-3 py-1.5 text-sm text-foreground transition-colors hover:bg-muted"
              >
                上一步
              </button>
            )}
            <button
              onClick={() => (last ? onClose() : setStep((s) => s + 1))}
              className="rounded-lg bg-primary px-4 py-1.5 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              {last ? "开始使用" : "下一步"}
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
