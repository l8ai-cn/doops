"use client"

import { Button } from "./ui"
import { useState } from "react"
import { ServerIcon, SparkIcon, TerminalIcon } from "./icons"

const STEPS = [
  {
    icon: ServerIcon,
    title: "从资产看板了解平台状态",
    body: "打开「资产」查看可见集群、异常和本周巡检。在「机器台账」或「集群台账」可以找到指定资产。",
  },
  {
    icon: TerminalIcon,
    title: "选择目标 Agent",
    body: "在「目标运维」顶部选择已授权的在线 Agent。新 Agent 的安装步骤在「发布部署 → 首次接入」。",
  },
  {
    icon: SparkIcon,
    title: "查看资源或执行操作",
    body: "在「操作资源」中查数据库、网站或 Kubernetes，在「运维工具」中打开凭据、审计或终端。也可以进入「AI 助手」描述需求。",
  },
]

export function OnboardingGuide({ onClose }: { onClose: () => void }) {
  const [step, setStep] = useState(0)
  const cur = STEPS[step]
  const Icon = cur.icon
  const last = step === STEPS.length - 1

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-background/70 p-4 backdrop-blur-sm">
      <div className="w-full max-w-lg rounded-xl border bg-card p-6 shadow-xl">
        <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-xl bg-primary/15 text-primary">
          <Icon width={24} height={24} />
        </div>
        <h2 className="text-xl font-semibold tracking-tight text-card-foreground">{cur.title}</h2>
        <p className="mt-2 text-sm leading-relaxed text-muted-foreground">{cur.body}</p>

        <div className="mt-6 flex items-center justify-between">
          <div className="flex items-center gap-1.5">
            {STEPS.map((_, i) => (
              <span
                key={i}
                className={`h-1.5 rounded-full transition-all ${
                  i === step ? "brand-gradient w-5" : "w-1.5 bg-muted"
                }`}
              />
            ))}
          </div>
          <div className="flex items-center gap-2">
            <Button
              onClick={onClose}
              variant="ghost"
            >
              跳过
            </Button>
            {step > 0 && (
              <Button
                onClick={() => setStep((s) => s - 1)}

              >
                上一步
              </Button>
            )}
            <Button
              onClick={() => (last ? onClose() : setStep((s) => s + 1))}
              variant="primary"
            >
              {last ? "开始使用" : "下一步"}
            </Button>
          </div>
        </div>
      </div>
    </div>
  )
}
