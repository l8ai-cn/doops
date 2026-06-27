"use client"

import { useState } from "react"
import Link from "next/link"
import {
  ServerIcon,
  TerminalIcon,
  ShieldIcon,
  RocketIcon,
  KeyIcon,
  CheckIcon,
  CopyIcon,
  HelpIcon,
  ChevronRightIcon,
  PlugIcon,
  RefreshIcon,
} from "./icons"

// 可复制命令块
function Cmd({ children }: { children: string }) {
  const [copied, setCopied] = useState(false)
  function copy() {
    navigator.clipboard?.writeText(children).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }
  return (
    <div className="group relative my-2 overflow-hidden rounded-lg border bg-muted">
      <pre className="overflow-x-auto px-3 py-2.5 pr-10 font-mono text-xs leading-relaxed text-foreground">
        {children}
      </pre>
      <button
        onClick={copy}
        aria-label="复制命令"
        className="absolute right-1.5 top-1.5 flex items-center gap-1 rounded-md border bg-background px-1.5 py-1 text-[11px] text-muted-foreground transition-colors hover:text-foreground"
      >
        {copied ? (
          <>
            <CheckIcon width={12} height={12} className="text-primary" /> 已复制
          </>
        ) : (
          <>
            <CopyIcon width={12} height={12} /> 复制
          </>
        )}
      </button>
    </div>
  )
}

type Step = {
  n: number
  icon: typeof ServerIcon
  title: string
  body: React.ReactNode
}

const STEPS: Step[] = [
  {
    n: 1,
    icon: ShieldIcon,
    title: "准备工作",
    body: (
      <ul className="ml-4 list-disc space-y-1 text-sm text-muted-foreground">
        <li>一台 Linux 服务器（支持 amd64 / arm64），可联网访问 gateway 地址</li>
        <li>具备 root 或 sudo 权限</li>
        <li>
          向管理员索取 <span className="font-medium text-foreground">gateway 地址</span> 与一枚{" "}
          <span className="font-medium text-foreground">Agent 接入令牌</span>
        </li>
      </ul>
    ),
  },
  {
    n: 2,
    icon: KeyIcon,
    title: "获取 Agent 接入令牌",
    body: (
      <div className="space-y-2 text-sm text-muted-foreground">
        <p>
          令牌由管理员在控制台「管理 → 令牌」中签发，类型选择{" "}
          <span className="font-medium text-foreground">Agent 令牌</span>。
        </p>
        <p>
          如果你不是管理员，请把目标服务器信息发给管理员，索取一枚 Agent 令牌与 gateway 地址。
        </p>
      </div>
    ),
  },
  {
    n: 3,
    icon: TerminalIcon,
    title: "一键安装",
    body: (
      <div className="space-y-2 text-sm text-muted-foreground">
        <p>在服务器上执行安装脚本，它会自动下载 agent 并引导你完成配置：</p>
        <Cmd>{"curl -fsSL https://doops.sh/install.sh | sh"}</Cmd>
        <p>
          脚本会提示填写 <span className="font-mono text-foreground">gateway 地址</span> 与{" "}
          <span className="font-mono text-foreground">接入令牌</span>。也可用环境变量免交互安装：
        </p>
        <Cmd>
          {
            "DOOPS_GATEWAY=https://gateway.example.com \\\nDOOPS_TOKEN=<你的Agent令牌> \\\nsh -c \"$(curl -fsSL https://doops.sh/install.sh)\""
          }
        </Cmd>
      </div>
    ),
  },
  {
    n: 4,
    icon: RocketIcon,
    title: "启动并设为开机自启",
    body: (
      <div className="space-y-2 text-sm text-muted-foreground">
        <p>安装完成后，启动 agent 服务并设为开机自启：</p>
        <Cmd>{"sudo systemctl enable --now doops-agent"}</Cmd>
        <p>查看运行状态：</p>
        <Cmd>{"systemctl status doops-agent --no-pager"}</Cmd>
      </div>
    ),
  },
  {
    n: 5,
    icon: ServerIcon,
    title: "回到控制台确认",
    body: (
      <div className="space-y-2 text-sm text-muted-foreground">
        <p>
          agent 连接成功后，机器会自动出现在控制台。回到{" "}
          <Link href="/console" className="text-primary underline-offset-2 hover:underline">
            控制台
          </Link>{" "}
          点击「刷新」，即可看到这台新机器，随后就能用 AI 助手运维。
        </p>
      </div>
    ),
  },
]

const MANUAL = [
  { label: "下载二进制", cmd: "curl -fsSL https://doops.sh/agent-linux-amd64 -o /usr/local/bin/doops-agent && chmod +x /usr/local/bin/doops-agent" },
  { label: "写入配置", cmd: "sudo mkdir -p /etc/doops && printf 'gateway: %s\\ntoken: %s\\n' \"$DOOPS_GATEWAY\" \"$DOOPS_TOKEN\" | sudo tee /etc/doops/agent.yaml" },
  { label: "创建并启动服务", cmd: "sudo doops-agent install-service && sudo systemctl enable --now doops-agent" },
]

const TROUBLE = [
  {
    q: "机器没有出现在控制台",
    a: "确认 agent 服务在运行（systemctl status doops-agent），并检查服务器能否访问 gateway 地址（curl -I <gateway>）。确认无误后回到控制台点击刷新。",
  },
  {
    q: "提示令牌无效或已过期",
    a: "Agent 令牌可能已被撤销或过期，请联系管理员在「管理 → 令牌」重新签发一枚 Agent 令牌。",
  },
  {
    q: "连接被拒绝 / 网络超时",
    a: "通常是防火墙或安全组拦截。请放通服务器到 gateway 的出站连接（HTTPS 443），云服务器需在安全组中配置。",
  },
  {
    q: "查看 agent 日志",
    a: "执行 journalctl -u doops-agent -n 100 --no-pager 查看最近日志，定位连接或认证问题。",
  },
]

// 正文：公开文档页与控制台内「部署 Agent」视图共用
// onEnterConsole 提供时（控制台内嵌），底部 CTA 变为「刷新机器列表」回调；否则链接到 /console
export function DeployGuideBody({ onEnterConsole }: { onEnterConsole?: () => void }) {
  return (
    <div className="mx-auto max-w-3xl px-5 py-10">
        {/* 标题 */}
        <div className="mb-8">
          <span className="mb-3 inline-flex items-center gap-1.5 rounded-full border bg-card px-3 py-1 text-xs text-muted-foreground">
            <PlugIcon width={13} height={13} className="text-primary" />
            部署指南
          </span>
          <h1 className="text-2xl font-semibold text-balance">部署 Doops Agent</h1>
          <p className="mt-2 text-pretty text-sm leading-relaxed text-muted-foreground">
            Doops 通过在你的服务器上安装一个轻量 agent 来工作。agent 启动后会主动连接 gateway，机器随即出现在控制台，之后即可远程执行命令、管理文件、用 AI 完成部署与巡检。
          </p>
        </div>

        {/* 没有实例时的两条路径 */}
        <div className="mb-10 grid gap-3 sm:grid-cols-2">
          <div className="rounded-xl border bg-card p-4">
            <div className="mb-1.5 flex items-center gap-2 text-sm font-medium">
              <RocketIcon width={16} height={16} className="text-primary" />
              自己部署
            </div>
            <p className="text-xs leading-relaxed text-muted-foreground">
              有服务器的 root/sudo 权限？按下面步骤几分钟即可接入一台机器。
            </p>
          </div>
          <div className="rounded-xl border bg-card p-4">
            <div className="mb-1.5 flex items-center gap-2 text-sm font-medium">
              <HelpIcon width={16} height={16} className="text-primary" />
              联系管理员
            </div>
            <p className="text-xs leading-relaxed text-muted-foreground">
              没有服务器权限或不确定 gateway 地址？联系团队管理员为你接入机器并签发令牌。
            </p>
          </div>
        </div>

        {/* 步骤 */}
        <section className="space-y-6">
          {STEPS.map((s) => (
            <div key={s.n} className="flex gap-4">
              <div className="flex flex-col items-center">
                <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-primary/15 text-primary">
                  <s.icon width={18} height={18} />
                </span>
                {s.n < STEPS.length && <span className="mt-1 w-px flex-1 bg-border" />}
              </div>
              <div className="flex-1 pb-2">
                <h2 className="mb-2 flex items-center gap-2 text-base font-medium">
                  <span className="text-xs text-muted-foreground">步骤 {s.n}</span>
                  {s.title}
                </h2>
                {s.body}
              </div>
            </div>
          ))}
        </section>

        {/* 手动安装 */}
        <section className="mt-12">
          <h2 className="mb-1 text-lg font-semibold">手动安装（可选）</h2>
          <p className="mb-3 text-sm text-muted-foreground">
            若无法使用一键脚本（如离线环境），可手动安装：
          </p>
          <ol className="space-y-3">
            {MANUAL.map((m, i) => (
              <li key={i}>
                <div className="mb-1 text-sm font-medium text-foreground">
                  {i + 1}. {m.label}
                </div>
                <Cmd>{m.cmd}</Cmd>
              </li>
            ))}
          </ol>
        </section>

        {/* 故障排查 */}
        <section className="mt-12">
          <h2 className="mb-3 text-lg font-semibold">故障排查</h2>
          <div className="divide-y rounded-xl border bg-card">
            {TROUBLE.map((t, i) => (
              <details key={i} className="group px-4 py-3">
                <summary className="flex cursor-pointer list-none items-center gap-2 text-sm font-medium text-foreground">
                  <ChevronRightIcon
                    width={15}
                    height={15}
                    className="text-muted-foreground transition-transform group-open:rotate-90"
                  />
                  {t.q}
                </summary>
                <p className="ml-6 mt-2 text-sm leading-relaxed text-muted-foreground">{t.a}</p>
              </details>
            ))}
          </div>
        </section>

        {/* 底部 CTA */}
        <div className="mt-12 flex flex-col items-center gap-3 rounded-xl border bg-card p-6 text-center">
          <p className="text-sm text-muted-foreground">机器已接入？开始运维</p>
          {onEnterConsole ? (
            <button
              onClick={onEnterConsole}
              className="flex items-center gap-1.5 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              <RefreshIcon width={16} height={16} />
              刷新机器列表
            </button>
          ) : (
            <Link
              href="/console"
              className="flex items-center gap-1.5 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              <ServerIcon width={16} height={16} />
              进入控制台
            </Link>
          )}
        </div>
    </div>
  )
}

// 公开文档页：带营销顶栏，复用上方正文
export function DeployGuide() {
  return (
    <main className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-10 border-b bg-background/80 backdrop-blur">
        <div className="mx-auto flex max-w-3xl items-center justify-between px-5 py-3">
          <Link href="/" className="flex items-center gap-2 text-sm font-semibold">
            <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-primary text-primary-foreground">
              <ServerIcon width={16} height={16} />
            </span>
            Doops
          </Link>
          <Link
            href="/console"
            className="flex items-center gap-1 rounded-lg border px-3 py-1.5 text-sm text-foreground transition-colors hover:bg-muted"
          >
            进入控制台
            <ChevronRightIcon width={15} height={15} />
          </Link>
        </div>
      </header>
      <DeployGuideBody />
    </main>
  )
}
