"use client"

import type { Target } from "@/lib/client"
import { RefreshIcon, ServerIcon } from "./icons"

function statusColor(t: Target): { dot: string; label: string } {
  if (t.busy) return { dot: "bg-destructive", label: "忙碌" }
  if (t.status === "active") return { dot: "bg-accent", label: "活动" }
  return { dot: "bg-success", label: "空闲" }
}

export function TargetSidebar({
  targets,
  selectedKey,
  onSelect,
  onRefresh,
  loading,
  error,
}: {
  targets: Target[]
  selectedKey: string | null
  onSelect: (t: Target) => void
  onRefresh: () => void
  loading: boolean
  error: string
}) {
  return (
    <aside className="flex h-full w-72 shrink-0 flex-col border-r bg-card">
      <div className="flex items-center justify-between border-b px-4 py-3">
        <div className="flex items-center gap-2">
          <ServerIcon width={16} height={16} className="text-muted-foreground" />
          <span className="text-sm font-medium text-foreground">在线目标</span>
          <span className="rounded-full bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
            {targets.length}
          </span>
        </div>
        <button
          onClick={onRefresh}
          disabled={loading}
          className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground disabled:opacity-50"
          aria-label="刷新目标列表"
        >
          <RefreshIcon width={16} height={16} className={loading ? "animate-spin" : ""} />
        </button>
      </div>

      <div className="flex-1 overflow-y-auto p-2">
        {error && (
          <p className="m-2 rounded-lg bg-destructive/15 px-3 py-2 text-xs text-destructive">
            {error}
          </p>
        )}
        {!error && targets.length === 0 && !loading && (
          <p className="px-3 py-8 text-center text-sm text-muted-foreground">暂无在线目标</p>
        )}
        <ul className="flex flex-col gap-1">
          {targets.map((t) => {
            const sc = statusColor(t)
            const active = t.key === selectedKey
            return (
              <li key={t.key}>
                <button
                  onClick={() => onSelect(t)}
                  className={`w-full rounded-lg border px-3 py-2.5 text-left transition-colors ${
                    active
                      ? "border-primary/50 bg-primary/10"
                      : "border-transparent hover:bg-muted"
                  }`}
                >
                  <div className="flex items-center gap-2">
                    <span className={`h-2 w-2 shrink-0 rounded-full ${sc.dot}`} />
                    <span className="truncate text-sm font-medium text-foreground">
                      {t.instance}
                    </span>
                  </div>
                  <div className="mt-0.5 truncate pl-4 font-mono text-xs text-muted-foreground">
                    {t.cluster}
                  </div>
                  <div className="mt-1 flex items-center gap-2 pl-4 text-xs text-muted-foreground">
                    <span>{sc.label}</span>
                    {typeof t.active_ops === "number" && t.active_ops > 0 && (
                      <span>· {t.active_ops} 执行中</span>
                    )}
                    {typeof t.queued_ops === "number" && t.queued_ops > 0 && (
                      <span>· {t.queued_ops} 排队</span>
                    )}
                  </div>
                </button>
              </li>
            )
          })}
        </ul>
      </div>
    </aside>
  )
}
