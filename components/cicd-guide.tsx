"use client"

import { useMemo, useState } from "react"
import type { Session } from "@/lib/client"
import {
  KeyIcon,
  EyeIcon,
  EyeOffIcon,
  CopyIcon,
  CheckIcon,
  GitIcon,
  ShieldIcon,
  LayersIcon,
  TerminalIcon,
  RocketIcon,
} from "./icons"

// 可复制的代码块
function CodeBlock({ code, lang }: { code: string; lang?: string }) {
  const [copied, setCopied] = useState(false)
  function copy() {
    navigator.clipboard?.writeText(code).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }
  return (
    <div className="group relative my-2 overflow-hidden rounded-lg border bg-muted">
      {lang && (
        <div className="border-b bg-background/60 px-3 py-1 font-mono text-[11px] text-muted-foreground">
          {lang}
        </div>
      )}
      <pre className="overflow-x-auto px-3 py-2.5 pr-10 font-mono text-xs leading-relaxed text-foreground">
        {code}
      </pre>
      <button
        onClick={copy}
        aria-label="复制代码"
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

type Provider = "curl" | "github" | "gitlab"

const PROVIDERS: { id: Provider; label: string }[] = [
  { id: "curl", label: "DoOps CLI" },
  { id: "github", label: "GitHub Actions" },
  { id: "gitlab", label: "GitLab CI" },
]

export function CicdGuide({ session }: { session: Session }) {
  // 默认用当前会话令牌与网关地址；演示或空值时回退到占位示例
  const sessionKey = session.token && !session.demo ? session.token : ""
  const sessionGateway =
    session.gateway && !session.gateway.startsWith("demo://") ? session.gateway : ""

  const [apiKey, setApiKey] = useState(sessionKey)
  const [gateway, setGateway] = useState(sessionGateway)
  const [reveal, setReveal] = useState(false)
  const [provider, setProvider] = useState<Provider>("curl")

  // 用于示例中的展示值（未填写时给占位符）
  const gw = gateway.trim() || "https://gateway.example.com"
  const keyDisplay = apiKey.trim() || "<DOOPS_API_KEY>"

  const snippet = useMemo(() => {
    if (provider === "curl") {
      return {
        lang: "bash",
        code: `# target / cluster / instance 必须与 deploy/environments.yaml 完全一致
export DOOPS_GATEWAY="${gw}"
export DOOPS_API_KEY="${keyDisplay}"

doops add \\
  --name <ENVIRONMENT_TARGET> \\
  --gateway "$DOOPS_GATEWAY" \\
  --cluster <CLUSTER> \\
  --instance <INSTANCE> \\
  --token "$DOOPS_API_KEY"

doops -session "release-$(git rev-parse --short HEAD)" cicd run \\
  -f <DEPLOYMENT_TEMPLATE> \\
  --target <ENVIRONMENT_TARGET> \\
  --dry-run \\
  --set releaseId="$(git rev-parse HEAD)"

doops -session "release-$(git rev-parse --short HEAD)" cicd run \\
  -f <DEPLOYMENT_TEMPLATE> \\
  --target <ENVIRONMENT_TARGET> \\
  --set releaseId="$(git rev-parse HEAD)"`,
      }
    }
    if (provider === "github") {
      return {
        lang: ".github/workflows/deploy.yml",
        code: `name: Deploy via Doops
on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - name: Trigger Doops deploy
        env:
          DOOPS_GATEWAY: ${gw}
          DOOPS_API_KEY: \${{ secrets.DOOPS_API_KEY }}
        run: |
          doops add --name <ENVIRONMENT_TARGET> \\
            --gateway "$DOOPS_GATEWAY" \\
            --cluster <CLUSTER> \\
            --instance <INSTANCE> \\
            --token "$DOOPS_API_KEY"
          doops -session "release-\${GITHUB_SHA::12}" cicd run \\
            -f <DEPLOYMENT_TEMPLATE> \\
            --target <ENVIRONMENT_TARGET> \\
            --set releaseId="$GITHUB_SHA"`,
      }
    }
    return {
      lang: ".gitlab-ci.yml",
      code: `deploy:
  stage: deploy
  image: curlimages/curl:latest
  variables:
    DOOPS_GATEWAY: "${gw}"
  script:
    - |
      doops add --name <ENVIRONMENT_TARGET> \\
        --gateway "$DOOPS_GATEWAY" \\
        --cluster <CLUSTER> \\
        --instance <INSTANCE> \\
        --token "$DOOPS_API_KEY"
      doops -session "release-$CI_COMMIT_SHORT_SHA" cicd run \\
        -f <DEPLOYMENT_TEMPLATE> \\
        --target <ENVIRONMENT_TARGET> \\
        --set releaseId="$CI_COMMIT_SHA"
  only:
    - main`,
    }
  }, [provider, gw, keyDisplay])

  return (
    <div className="mx-auto max-w-3xl px-5 py-10">
      {/* 标题 */}
      <div className="mb-8">
        <div className="mb-3 inline-flex items-center gap-1.5 rounded-full border bg-card px-2.5 py-1 text-xs font-medium text-muted-foreground">
          <GitIcon width={13} height={13} />
          CI/CD 接入
        </div>
        <h1 className="text-2xl font-semibold text-balance">将 Doops 接入 CI/CD</h1>
        <p className="mt-2 text-pretty text-sm leading-relaxed text-muted-foreground">
          流水线只调用 DoOps v2 声明式入口。目标环境由显式 target 和环境注册表确定，
          由 doops-cicd Skill 通过 Ask 执行并产出结构化运行证据。
        </p>
      </div>

      {/* 1. 配置 API Key */}
      <section className="mb-8">
        <h2 className="mb-1 flex items-center gap-2 text-lg font-semibold">
          <KeyIcon width={18} height={18} className="text-primary" />
          1. 配置 API Key
        </h2>
        <p className="mb-4 text-sm text-muted-foreground">
          API Key 即你的<strong className="text-foreground">用户访问令牌</strong>，
          代表你的身份与权限。下面默认填入了当前会话使用的令牌，你也可以在「管理 → 令牌」中单独签发一枚专用于 CI/CD 的令牌。
        </p>

        <div className="flex flex-col gap-3 rounded-xl border bg-card p-4">
          <label className="flex flex-col gap-1.5 text-xs font-medium text-muted-foreground">
            网关地址 (Gateway)
            <input
              value={gateway}
              onChange={(e) => setGateway(e.target.value)}
              placeholder="https://gateway.example.com"
              className="w-full rounded-md border bg-background px-3 py-2 font-mono text-sm text-foreground outline-none focus:border-ring"
            />
          </label>

          <label className="flex flex-col gap-1.5 text-xs font-medium text-muted-foreground">
            API Key
            <div className="flex items-center gap-2">
              <div className="relative flex-1">
                <input
                  type={reveal ? "text" : "password"}
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                  placeholder="粘贴或签发一枚用户令牌"
                  className="w-full rounded-md border bg-background px-3 py-2 pr-10 font-mono text-sm text-foreground outline-none focus:border-ring"
                />
                <button
                  type="button"
                  onClick={() => setReveal((v) => !v)}
                  aria-label={reveal ? "隐藏 API Key" : "显示 API Key"}
                  className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground transition-colors hover:text-foreground"
                >
                  {reveal ? <EyeOffIcon width={16} height={16} /> : <EyeIcon width={16} height={16} />}
                </button>
              </div>
              <CopyButton value={apiKey} />
            </div>
          </label>

          <div className="flex items-start gap-2 rounded-lg border border-primary/30 bg-primary/5 p-3 text-xs text-muted-foreground">
            <ShieldIcon width={15} height={15} className="mt-0.5 shrink-0 text-primary" />
            <span className="text-pretty leading-relaxed">
              请勿把 API Key 写入代码仓库。务必保存到 CI/CD 平台的{" "}
              <strong className="text-foreground">加密密钥 / Secrets</strong> 中，
              通过环境变量 <code className="rounded bg-muted px-1">DOOPS_API_KEY</code> 注入。
            </span>
          </div>
        </div>
      </section>

      {/* 2. 在 CI 平台设置 Secret */}
      <section className="mb-8">
        <h2 className="mb-1 flex items-center gap-2 text-lg font-semibold">
          <LayersIcon width={18} height={18} className="text-primary" />
          2. 在 CI 平台保存为密钥
        </h2>
        <p className="mb-3 text-sm text-muted-foreground">
          在你的代码托管平台添加两个密钥变量，供流水线读取：
        </p>
        <ul className="space-y-2 text-sm text-muted-foreground">
          <li className="flex gap-2">
            <CheckIcon width={16} height={16} className="mt-0.5 shrink-0 text-primary" />
            <span>
              <code className="rounded bg-muted px-1 font-mono text-foreground">DOOPS_API_KEY</code> —
              上方的 API Key（设为加密 / 隐藏）
            </span>
          </li>
          <li className="flex gap-2">
            <CheckIcon width={16} height={16} className="mt-0.5 shrink-0 text-primary" />
            <span>
              <code className="rounded bg-muted px-1 font-mono text-foreground">DOOPS_GATEWAY</code> —
              网关地址（可明文）
            </span>
          </li>
        </ul>
        <p className="mt-3 text-xs text-muted-foreground">
          GitHub：仓库 Settings → Secrets and variables → Actions；
          GitLab：Settings → CI/CD → Variables。
        </p>
      </section>

      {/* 3. 调用示例 */}
      <section className="mb-8">
        <h2 className="mb-1 flex items-center gap-2 text-lg font-semibold">
          <TerminalIcon width={18} height={18} className="text-primary" />
          3. 在流水线中调用
        </h2>
        <p className="mb-3 text-sm text-muted-foreground">
          选择你的平台，复制下面的示例。先执行 dry-run，确认计划后再授权正式变更。
        </p>

        {/* 平台切换 */}
        <div className="mb-1 flex flex-wrap gap-1 rounded-lg border bg-muted/40 p-0.5">
          {PROVIDERS.map((p) => (
            <button
              key={p.id}
              onClick={() => setProvider(p.id)}
              className={`flex-1 whitespace-nowrap rounded-md px-3 py-1.5 text-xs font-medium transition-colors ${
                provider === p.id
                  ? "bg-primary text-primary-foreground"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              {p.label}
            </button>
          ))}
        </div>

        <CodeBlock code={snippet.code} lang={snippet.lang} />

        <p className="mt-2 text-xs text-muted-foreground text-pretty leading-relaxed">
          <code className="rounded bg-muted px-1">ENVIRONMENT_TARGET</code>、
          <code className="rounded bg-muted px-1">CLUSTER</code> 和
          <code className="rounded bg-muted px-1">INSTANCE</code> 必须从仓库环境注册表原样读取；
          <code className="rounded bg-muted px-1">DEPLOYMENT_TEMPLATE</code> 必须指向已提交的
          DoOps workflow。不要改成 Shell、SSH 或直接 Kubernetes 命令。
        </p>
      </section>

      {/* 工作原理 */}
      <section className="rounded-xl border bg-card p-5">
        <h2 className="mb-3 flex items-center gap-2 text-base font-semibold">
          <RocketIcon width={16} height={16} className="text-primary" />
          工作原理
        </h2>
        <ol className="ml-4 list-decimal space-y-1.5 text-sm text-muted-foreground">
          <li>CLI 校验 workflow 和显式 target，同步精确代码快照后通过 Ask 调用 doagent。</li>
          <li>Gateway 校验权限、工作区提交和 session 资源边界。</li>
          <li>doagent 执行 doops-cicd Skill，输出 DeploymentRun 和真实模块观察结果。</li>
        </ol>
      </section>
    </div>
  )
}

function CopyButton({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  function copy() {
    if (!value) return
    navigator.clipboard?.writeText(value).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }
  return (
    <button
      type="button"
      onClick={copy}
      disabled={!value}
      className="flex shrink-0 items-center gap-1 rounded-md border bg-background px-2.5 py-2 text-xs text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
    >
      {copied ? (
        <>
          <CheckIcon width={13} height={13} className="text-primary" /> 已复制
        </>
      ) : (
        <>
          <CopyIcon width={13} height={13} /> 复制
        </>
      )}
    </button>
  )
}
