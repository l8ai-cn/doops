"use client"

import { memo, useState, type ReactNode } from "react"
import ReactMarkdown from "react-markdown"
import remarkGfm from "remark-gfm"
import { CopyIcon, CheckIcon } from "./icons"

function CodeBlock({ children, className }: { children: ReactNode; className?: string }) {
  const [copied, setCopied] = useState(false)
  const lang = (className || "").replace(/language-/, "")
  const raw = extractRawText(children)

  function copy() {
    navigator.clipboard?.writeText(raw).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <div className="group/code my-2 overflow-hidden rounded-lg border bg-muted/40">
      <div className="flex items-center justify-between border-b bg-muted/60 px-3 py-1">
        <span className="font-mono text-[11px] text-muted-foreground">{lang || "代码"}</span>
        <button
          onClick={copy}
          className="flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] text-muted-foreground transition-colors hover:bg-background hover:text-foreground"
          title="复制代码"
        >
          {copied ? <CheckIcon width={12} height={12} /> : <CopyIcon width={12} height={12} />}
          {copied ? "已复制" : "复制"}
        </button>
      </div>
      <pre className="overflow-x-auto px-3 py-2.5 font-mono text-xs leading-relaxed text-foreground">
        <code>{children}</code>
      </pre>
    </div>
  )
}

function extractRawText(node: ReactNode): string {
  if (typeof node === "string") return node
  if (Array.isArray(node)) return node.map(extractRawText).join("")
  if (node && typeof node === "object" && "props" in node) {
    // @ts-expect-error props children traversal
    return extractRawText(node.props?.children)
  }
  return ""
}

export const Markdown = memo(function Markdown({ content }: { content: string }) {
  return (
    <div className="text-sm leading-relaxed text-foreground">
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          p: ({ children }) => <p className="my-1.5 first:mt-0 last:mb-0 break-words">{children}</p>,
          h1: ({ children }) => (
            <h1 className="mb-2 mt-3 text-base font-semibold text-foreground first:mt-0">{children}</h1>
          ),
          h2: ({ children }) => (
            <h2 className="mb-1.5 mt-3 text-sm font-semibold text-foreground first:mt-0">{children}</h2>
          ),
          h3: ({ children }) => (
            <h3 className="mb-1 mt-2.5 text-sm font-semibold text-foreground first:mt-0">{children}</h3>
          ),
          ul: ({ children }) => <ul className="my-1.5 ml-4 list-disc space-y-1">{children}</ul>,
          ol: ({ children }) => <ol className="my-1.5 ml-4 list-decimal space-y-1">{children}</ol>,
          li: ({ children }) => <li className="break-words pl-0.5 marker:text-muted-foreground">{children}</li>,
          a: ({ children, href }) => (
            <a
              href={href}
              target="_blank"
              rel="noreferrer"
              className="font-medium text-primary underline underline-offset-2 hover:opacity-80"
            >
              {children}
            </a>
          ),
          strong: ({ children }) => <strong className="font-semibold text-foreground">{children}</strong>,
          em: ({ children }) => <em className="italic">{children}</em>,
          blockquote: ({ children }) => (
            <blockquote className="my-2 border-l-2 border-primary/40 pl-3 text-muted-foreground">
              {children}
            </blockquote>
          ),
          hr: () => <hr className="my-3 border-border" />,
          code: ({ className, children, ...props }) => {
            const isBlock = "node" in props && (props.node as { tagName?: string })?.tagName === "code"
            // 行内 code：无 language- 前缀且不含换行
            const isInline = !className && !String(children).includes("\n")
            if (isInline) {
              return (
                <code className="rounded bg-muted px-1 py-0.5 font-mono text-[0.85em] text-foreground">
                  {children}
                </code>
              )
            }
            return <CodeBlock className={className}>{children}</CodeBlock>
          },
          pre: ({ children }) => <>{children}</>,
          table: ({ children }) => (
            <div className="my-2 overflow-x-auto">
              <table className="w-full border-collapse text-xs">{children}</table>
            </div>
          ),
          thead: ({ children }) => <thead className="bg-muted/50">{children}</thead>,
          th: ({ children }) => (
            <th className="border px-2 py-1 text-left font-semibold text-foreground">{children}</th>
          ),
          td: ({ children }) => <td className="border px-2 py-1 text-foreground/90">{children}</td>,
        }}
      >
        {content}
      </ReactMarkdown>
    </div>
  )
})
