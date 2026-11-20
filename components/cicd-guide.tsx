"use client"

import { useMemo, useState } from "react"
import type { Session } from "@/lib/client"
import { cicdGatewaySnippetValue } from "@/lib/cicd-gateway"
import {
  KeyIcon,
  CopyIcon,
  CheckIcon,
  GitIcon,
  ShieldIcon,
  LayersIcon,
  TerminalIcon,
  RocketIcon,
} from "./icons"
import { SegmentedControl, Input } from "./ui"

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

export function CicdGuide(_props: { session: Session }) {
  // 示例只使用占位符；当前 Web 会话 token 不能进入可复制的 CI/CD 代码。
  const [gateway, setGateway] = useState("")
  const [provider, setProvider] = useState<Provider>("curl")

  // 地址不合法（带账号密码、http、查询参数或片段）时片段继续用占位符，不把输入写进可复制的代码。
  const { value: gw, error: gatewayError } = cicdGatewaySnippetValue(gateway)

  const snippet = useMemo(() => {
    if (provider === "curl") {
      return {
        lang: "bash",
        code: `# 安装固定 Release 的 CLI 与同一提交的 Skill；不向业务 target 同步源码
export DOOPS_VERSION="<PINNED_DOOPS_VERSION>"
git clone --depth 1 --branch "$DOOPS_VERSION" https://github.com/symansun/doops.git /tmp/doops-release
bash /tmp/doops-release/backend/scripts/install.sh --version "$DOOPS_VERSION" --project "$PWD"
export PATH="$HOME/.local/bin:$PATH"
export DOOPS_GATEWAY="${gw}"
export DOOPS_SESSION="release-<RELEASE_ID>"
export DOOPS_CREDENTIAL="<CREDENTIAL_RESOURCE_NAME>"

doops add \\
  --name <ENVIRONMENT_TARGET> \\
  --gateway "$DOOPS_GATEWAY" \\
  --cluster <CLUSTER> \\
  --instance <INSTANCE>

printf '%s\\n' "$DOOPS_API_KEY" | doops -session "$DOOPS_SESSION" login \\
  --target <ENVIRONMENT_TARGET> \\
  --token-stdin

doops -session "$DOOPS_SESSION" ask \\
  --target <ENVIRONMENT_TARGET> \\
  --apply \\
  --credential "$DOOPS_CREDENTIAL" \\
        --msg "target: <TARGET_SCOPE>; release: <RELEASE>; namespace: <NAMESPACE>; chart: <CHART_REFERENCE>; optional image/value overrides: <IMAGE_OR_VALUES>; reason: <REASON>; acceptance: <OPTIONAL_CHECKS>。"`,
      }
    }
    if (provider === "github") {
      return {
        lang: ".github/workflows/deploy.yml",
        code: `name: Deploy via DoOps
on:
  push:
    branches: [main]

jobs:
  deploy:
    runs-on: ubuntu-latest
    env:
      DOOPS_VERSION: <PINNED_DOOPS_VERSION>
    steps:
      - uses: actions/checkout@v4
      - name: Install released DoOps CLI
        run: |
          git clone --depth 1 --branch "$DOOPS_VERSION" https://github.com/symansun/doops.git /tmp/doops-release
          bash /tmp/doops-release/backend/scripts/install.sh --version "$DOOPS_VERSION" --project "$GITHUB_WORKSPACE"
      - name: Trigger DoOps deploy
        env:
          DOOPS_GATEWAY: "${gw}"
          DOOPS_API_KEY: \${{ secrets.DOOPS_API_KEY }}
          DOOPS_CREDENTIAL: <CREDENTIAL_RESOURCE_NAME>
          IMAGE_REF: <IMAGE_REPOSITORY>@sha256:<IMAGE_DIGEST>
        run: |
          export PATH="$HOME/.local/bin:$PATH"
          export DOOPS_SESSION="release-\${GITHUB_SHA::12}"
          doops add --name <ENVIRONMENT_TARGET> \\
            --gateway "$DOOPS_GATEWAY" \\
            --cluster <CLUSTER> \\
            --instance <INSTANCE>
          printf '%s\\n' "$DOOPS_API_KEY" | doops -session "$DOOPS_SESSION" login \\
            --target <ENVIRONMENT_TARGET> \\
            --token-stdin
          doops -session "release-\${GITHUB_SHA::12}" ask \\
            --target <ENVIRONMENT_TARGET> \\
            --apply \\
            --credential "$DOOPS_CREDENTIAL" \\
            --msg "target: <TARGET_SCOPE>; release: <RELEASE>; namespace: <NAMESPACE>; chart: <CHART_REFERENCE>; optional image: $IMAGE_REF; reason: <REASON>; optional acceptance: <OPTIONAL_CHECKS>。"`,
      }
    }
    return {
      lang: ".gitlab-ci.yml",
      code: `deploy:
  stage: deploy
  image: alpine:3.22@sha256:<PINNED_ALPINE_DIGEST>
  variables:
    DOOPS_GATEWAY: "${gw}"
    DOOPS_VERSION: "<PINNED_DOOPS_VERSION>"
    DOOPS_CREDENTIAL: "<CREDENTIAL_RESOURCE_NAME>"
    IMAGE_REF: "<IMAGE_REPOSITORY>@sha256:<IMAGE_DIGEST>"
  script:
    - |
      apk add --no-cache bash curl git
      git clone --depth 1 --branch "$DOOPS_VERSION" https://github.com/symansun/doops.git /tmp/doops-release
      bash /tmp/doops-release/backend/scripts/install.sh --version "$DOOPS_VERSION" --project "$CI_PROJECT_DIR"
      export PATH="$HOME/.local/bin:$PATH"
      export DOOPS_SESSION="release-$CI_COMMIT_SHORT_SHA"
      doops add --name <ENVIRONMENT_TARGET> \\
        --gateway "$DOOPS_GATEWAY" \\
        --cluster <CLUSTER> \\
        --instance <INSTANCE>
      printf '%s\\n' "$DOOPS_API_KEY" | doops -session "$DOOPS_SESSION" login \\
        --target <ENVIRONMENT_TARGET> \\
        --token-stdin
      doops -session "release-$CI_COMMIT_SHORT_SHA" ask \\
        --target <ENVIRONMENT_TARGET> \\
        --apply \\
        --credential "$DOOPS_CREDENTIAL" \\
        --msg "target: <TARGET_SCOPE>; release: <RELEASE>; namespace: <NAMESPACE>; chart: <CHART_REFERENCE>; optional image: $IMAGE_REF; reason: <REASON>; optional acceptance: <OPTIONAL_CHECKS>。"
  only:
    - main`,
    }
  }, [provider, gw])

  return (
    <div className="px-4 py-10">
      {/* 标题 */}
      <div className="mb-8">
        <div className="mb-3 inline-flex items-center gap-1.5 rounded-full border bg-card px-2.5 py-1 text-xs font-medium text-muted-foreground">
          <GitIcon width={13} height={13} />
          CI/CD 接入
        </div>
        <h1 className="text-2xl font-semibold text-balance">将 DoOps 接入 CI/CD</h1>
        <p className="mt-2 text-pretty text-sm leading-relaxed text-muted-foreground">
          流水线通过明确的自然语言任务调用 DoOps。任务至少说明 target、release、namespace 和 Chart；镜像、原因和验收按任务需要补充。
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
          代表你的身份与权限。请由管理员在「平台管理 → 令牌」中单独签发 CI/CD 用户令牌，并只保存到流水线的加密 Secrets。
        </p>

        <div className="flex flex-col gap-3 rounded-xl border bg-card p-4">
          <label className="flex flex-col gap-1.5 text-xs font-medium text-muted-foreground">
            网关地址 (Gateway)
            <Input
              value={gateway}
              onChange={(e) => setGateway(e.target.value)}
              placeholder="https://gateway.example.com"
              aria-invalid={gatewayError ? true : undefined}
              aria-describedby={gatewayError ? "cicd-gateway-error" : undefined}
              className={`w-full font-mono ${
                gatewayError ? "border-destructive" : ""
              }`}
            />
          </label>
          {gatewayError && (
            <p id="cicd-gateway-error" role="alert" className="text-xs text-destructive">
              {gatewayError}。下方片段仍使用占位符 <code className="font-mono">&lt;DOOPS_GATEWAY_URL&gt;</code>。
            </p>
          )}

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
          在你的代码托管平台保存 Gateway 地址和用户令牌，供流水线通过 stdin 登录：
        </p>
        <ul className="space-y-2 text-sm text-muted-foreground">
          <li className="flex gap-2">
            <CheckIcon width={16} height={16} className="mt-0.5 shrink-0 text-primary" />
            <span>
              <code className="rounded bg-muted px-1 font-mono text-foreground">DOOPS_API_KEY</code> —
              专用于 CI/CD 的用户令牌（设为加密 / 隐藏，不在此页面输入）
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
          需要受管凭据时，再配置{" "}
          <code className="rounded bg-muted px-1 font-mono text-foreground">DOOPS_CREDENTIAL</code>{" "}
          资源名。它只是 metadata 引用，不是 secret；同一发布需要多个凭据时重复
          <code className="mx-1 rounded bg-muted px-1">--credential</code>。
        </p>
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
          选择你的平台，复制下面的示例。需要镜像或业务验收时再补充相应信息，然后通过
          <code className="mx-1 rounded bg-muted px-1">--apply</code>
          声明本次是变更任务。
        </p>
        <p className="mb-3 text-xs text-muted-foreground">
          在线 target 发布现有 Helm release 不要求本机 kubeconfig、工作区干净或先同步业务源码。
          Chart、values 和镜像参数由目标 Helm 与任务决定；Secrets 不进入命令参数或发布描述。
        </p>

        {/* 平台切换 */}
        <SegmentedControl
          value={provider}
          onChange={setProvider}
          className="mb-1 flex w-full flex-wrap"
          ariaLabel="CI/CD 平台"
          options={PROVIDERS.map((p) => ({ value: p.id, label: p.label }))}
        />

        <CodeBlock code={snippet.code} lang={snippet.lang} />

        <p className="mt-2 text-xs text-muted-foreground text-pretty leading-relaxed">
          target、release、namespace 和 chart 必须由发布任务明确给出；镜像、变更原因和验收条件按任务需要写入。
          CI 产物可作为任务事实，不会成为所有 Helm Chart 的全局发布门禁。
          没有凭据时删除示例中的 <code className="rounded bg-muted px-1">--credential</code> 行。
        </p>
      </section>

      {/* 工作原理 */}
      <section className="rounded-xl border bg-card p-5">
        <h2 className="mb-3 flex items-center gap-2 text-base font-semibold">
          <RocketIcon width={16} height={16} className="text-primary" />
          工作原理
        </h2>
        <ol className="ml-4 list-decimal space-y-1.5 text-sm text-muted-foreground">
          <li>CLI 将显式 target 与完整发布要求发送给 Gateway。</li>
          <li>Gateway 校验权限和 session 资源边界；凭据仅以 metadata-only 引用授权和物化。</li>
          <li>doagent 执行获得授权的任务并返回真实模块观察结果。</li>
        </ol>
      </section>
    </div>
  )
}
