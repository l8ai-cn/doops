"use client"

import Link from "next/link"
import {
  ServerIcon,
  SparkIcon,
  TerminalIcon,
  ShieldIcon,
  HistoryIcon,
  LayersIcon,
  FileIcon,
  RocketIcon,
  CheckIcon,
  ActivityIcon,
} from "./icons"

const FEATURES = [
  {
    icon: SparkIcon,
    title: "自然语言运维",
    desc: "用中文描述你的需求，AI 智能体自动规划并在你的机器上执行，无需记忆复杂命令。",
  },
  {
    icon: TerminalIcon,
    title: "远程终端",
    desc: "浏览器里直接连到你的服务器执行命令，实时输出，无需 SSH 客户端。",
  },
  {
    icon: ActivityIcon,
    title: "定时巡检 · 自动提单",
    desc: "周期扫描机器异常，自动归类并提交 issue 到 GitHub / Gitee，无人值守也安心。",
  },
  {
    icon: LayersIcon,
    title: "配置模板 · 一键下发",
    desc: "在浏览器保存常用大模型与配置文件模板，通过 doops 发布到指定节点。",
  },
  {
    icon: FileIcon,
    title: "文件管理 · 整目录上传",
    desc: "在线浏览、编辑、上传文件与文件夹，也支持极速 CLI 同步大目录。",
  },
  {
    icon: ShieldIcon,
    title: "权限与审计",
    desc: "按用户、集群、动作精细授权，所有操作留痕可追溯，团队协作更放心。",
  },
]

const STEPS = [
  {
    n: "1",
    title: "在机器上安装 Agent",
    desc: "复制一行安装命令到你的服务器执行，节点会自动注册到网关。",
  },
  {
    n: "2",
    title: "连接控制台",
    desc: "用账号密码或 Token 登录控制台，即可看到你的所有在线机器。",
  },
  {
    n: "3",
    title: "开始运维",
    desc: "用自然语言下达任务，或打开终端执行命令，运维从此变简单。",
  },
]

export function LandingPage() {
  return (
    <div className="min-h-dvh bg-background text-foreground">
      {/* 顶栏 */}
      <header className="sticky top-0 z-20 border-b border-border/60 bg-background/80 backdrop-blur">
        <div className="mx-auto flex max-w-6xl items-center justify-between px-4 py-3">
          <div className="flex items-center gap-2">
            <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-primary/15 text-primary">
              <ServerIcon width={18} height={18} />
            </div>
            <span className="text-lg font-semibold tracking-tight">Doops</span>
          </div>
          <nav className="hidden items-center gap-6 text-sm text-muted-foreground md:flex">
            <a href="#features" className="transition-colors hover:text-foreground">
              功能
            </a>
            <a href="#how" className="transition-colors hover:text-foreground">
              快速上手
            </a>
            <Link href="/docs/deploy" className="transition-colors hover:text-foreground">
              部署文档
            </Link>
            <Link href="/cases" className="transition-colors hover:text-foreground">
              使用案例
            </Link>
            <a href="#faq" className="transition-colors hover:text-foreground">
              常见问题
            </a>
          </nav>
          <div className="flex items-center gap-2">
            <Link
              href="/console?demo=1"
              className="hidden rounded-lg border px-3 py-1.5 text-sm text-foreground transition-colors hover:bg-muted sm:inline-block"
            >
              在线演示
            </Link>
            <Link
              href="/console"
              className="rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              进入控制台
            </Link>
          </div>
        </div>
      </header>

      {/* 主视觉 */}
      <section className="relative overflow-hidden">
        <div className="mx-auto max-w-6xl px-4 py-20 text-center md:py-28">
          <div className="mx-auto mb-5 inline-flex items-center gap-2 rounded-full border border-primary/30 bg-primary/10 px-3 py-1 text-xs text-primary">
            <SparkIcon width={13} height={13} />
            AI 驱动的智能运维平台
          </div>
          <h1 className="mx-auto max-w-3xl text-balance text-4xl font-semibold leading-tight tracking-tight md:text-6xl">
            让运维像聊天一样简单
          </h1>
          <p className="mx-auto mt-5 max-w-2xl text-pretty text-base leading-relaxed text-muted-foreground md:text-lg">
            Doops 把你的服务器接入一个智能控制台。用自然语言下达任务，AI
            自动执行；定时巡检、配置下发、团队权限一应俱全。哪怕只有一台机器，也能轻松管好。
          </p>
          <div className="mt-8 flex flex-col items-center justify-center gap-3 sm:flex-row">
            <Link
              href="/console"
              className="flex w-full items-center justify-center gap-2 rounded-lg bg-primary px-6 py-3 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 sm:w-auto"
            >
              <RocketIcon width={16} height={16} />
              免费开始使用
            </Link>
            <Link
              href="/console?demo=1"
              className="flex w-full items-center justify-center gap-2 rounded-lg border px-6 py-3 text-sm font-medium text-foreground transition-colors hover:bg-muted sm:w-auto"
            >
              <SparkIcon width={16} height={16} />
              先看在线演示
            </Link>
          </div>
          <p className="mt-4 text-xs text-muted-foreground">无需信用卡 · 演示模式无需安装任何东西</p>
        </div>
      </section>

      {/* 功能 */}
      <section id="features" className="border-t border-border/60 py-20">
        <div className="mx-auto max-w-6xl px-4">
          <div className="mx-auto mb-12 max-w-2xl text-center">
            <h2 className="text-balance text-3xl font-semibold tracking-tight">一个平台，搞定日常运维</h2>
            <p className="mt-3 text-pretty text-muted-foreground">
              从执行命令到无人值守巡检，Doops 覆盖你每天都要做的运维工作。
            </p>
          </div>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {FEATURES.map((f) => {
              const Icon = f.icon
              return (
                <div
                  key={f.title}
                  className="rounded-xl border bg-card p-6 transition-colors hover:border-primary/40"
                >
                  <div className="mb-4 flex h-10 w-10 items-center justify-center rounded-lg bg-primary/15 text-primary">
                    <Icon width={20} height={20} />
                  </div>
                  <h3 className="mb-1.5 text-base font-medium text-card-foreground">{f.title}</h3>
                  <p className="text-sm leading-relaxed text-muted-foreground">{f.desc}</p>
                </div>
              )
            })}
          </div>
        </div>
      </section>

      {/* 快速上手 */}
      <section id="how" className="border-t border-border/60 bg-card/30 py-20">
        <div className="mx-auto max-w-6xl px-4">
          <div className="mx-auto mb-12 max-w-2xl text-center">
            <h2 className="text-balance text-3xl font-semibold tracking-tight">���步即可上手</h2>
            <p className="mt-3 text-pretty text-muted-foreground">
              不懂复杂运维也没关系，跟���三步就能开始。
            </p>
          </div>
          <div className="grid gap-6 md:grid-cols-3">
            {STEPS.map((s, i) => (
              <div key={s.n} className="relative">
                <div className="rounded-xl border bg-card p-6">
                  <div className="mb-4 flex h-9 w-9 items-center justify-center rounded-full bg-primary text-sm font-semibold text-primary-foreground">
                    {s.n}
                  </div>
                  <h3 className="mb-1.5 text-base font-medium text-card-foreground">{s.title}</h3>
                  <p className="text-sm leading-relaxed text-muted-foreground">{s.desc}</p>
                </div>
                {i < STEPS.length - 1 && (
                  <div className="absolute -right-3 top-12 hidden h-px w-6 bg-border md:block" />
                )}
              </div>
            ))}
          </div>
          <div className="mt-10 text-center">
            <Link
              href="/console"
              className="inline-flex items-center justify-center gap-2 rounded-lg bg-primary px-6 py-3 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              <RocketIcon width={16} height={16} />
              立即开始
            </Link>
          </div>
        </div>
      </section>

      {/* FAQ */}
      <section id="faq" className="border-t border-border/60 py-20">
        <div className="mx-auto max-w-3xl px-4">
          <h2 className="mb-10 text-center text-balance text-3xl font-semibold tracking-tight">
            常见问题
          </h2>
          <div className="flex flex-col gap-5">
            {[
              {
                q: "我只有一台服务器，适合用吗？",
                a: "完全适合。Doops 从单机到集群都能用，单台机器也能享受自然语言运维、定时巡检和文件管理。",
              },
              {
                q: "我不太懂运维命令，能用起来吗？",
                a: "可以。你只需用中文描述想做什么，AI 智能体会帮你规划并执行；控制台也有引导带你一步步操作。",
              },
              {
                q: "我的密钥和数据安全吗？",
                a: "Token 仅保存在你的浏览器与本机，后端只做请求转发、不落盘。所有操作都有审计记录可追溯。",
              },
              {
                q: "不想安装可以先体验吗？",
                a: "可以，点击「在线演示」即可进入内置演示数据的完整控制台，无需连接任何真实服务器。",
              },
            ].map((item) => (
              <div key={item.q} className="rounded-xl border bg-card p-5">
                <h3 className="mb-1.5 flex items-center gap-2 text-sm font-medium text-card-foreground">
                  <CheckIcon width={15} height={15} className="text-primary" />
                  {item.q}
                </h3>
                <p className="pl-6 text-sm leading-relaxed text-muted-foreground">{item.a}</p>
              </div>
            ))}
          </div>
        </div>
      </section>

      {/* 底部 CTA */}
      <section className="border-t border-border/60 bg-card/30 py-16">
        <div className="mx-auto max-w-3xl px-4 text-center">
          <h2 className="text-balance text-2xl font-semibold tracking-tight md:text-3xl">
            准备好让运维变简单了吗？
          </h2>
          <p className="mt-3 text-pretty text-muted-foreground">现在就进入控制台，几分钟内接入你的第一台机器。</p>
          <Link
            href="/console"
            className="mt-6 inline-flex items-center justify-center gap-2 rounded-lg bg-primary px-6 py-3 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
          >
            <RocketIcon width={16} height={16} />
            进入控制台
          </Link>
        </div>
      </section>

      <footer className="border-t border-border/60 py-8">
        <div className="mx-auto flex max-w-6xl flex-col items-center justify-between gap-3 px-4 text-sm text-muted-foreground sm:flex-row">
          <div className="flex items-center gap-2">
            <div className="flex h-6 w-6 items-center justify-center rounded-md bg-primary/15 text-primary">
              <ServerIcon width={14} height={14} />
            </div>
            <span>Doops · 智能运维控制台</span>
          </div>
          <div className="flex items-center gap-4">
            <Link href="/console" className="transition-colors hover:text-foreground">
              控制台
            </Link>
            <a href="#features" className="transition-colors hover:text-foreground">
              功能
            </a>
            <Link href="/docs/deploy" className="transition-colors hover:text-foreground">
              部署文档
            </Link>
            <Link href="/cases" className="transition-colors hover:text-foreground">
              使用案例
            </Link>
            <a href="#faq" className="transition-colors hover:text-foreground">
              常见问题
            </a>
          </div>
        </div>
      </footer>
    </div>
  )
}
