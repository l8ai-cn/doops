"use client"

import { useEffect, useRef, useState } from "react"
import type { Target } from "@/lib/client"
import { ServerIcon, RefreshIcon, ChevronDownIcon, CheckIcon } from "./icons"

function statusDot(t: Target): string {
  if (t.busy) return "bg-destructive"
  if (t.status === "active") return "bg-accent"
  return "bg-success"
}

function statusLabel(t: Target): string {
  if (t.busy) return "忙碌"
  if (t.status === "active") return "活动"
  return "空闲"
}

export function TargetSwitcher({
  targets,
  selected,
  onSelect,
  onRefresh,
  loading,
}: {
  targets: Target[]
  selected: Target | null
  onSelect: (t: Target) => void
  onRefresh: () => void
  loading: boolean
}) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    function onDoc(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener("mousedown", onDoc)
    return () => document.removeEventListener("mousedown", onDoc)
  }, [open])

  // 无机器：提示等待接入
  if (targets.length === 0) {
    return (
      <div className="flex items-center gap-2 rounded-lg border bg-muted/40 px-2.5 py-1.5">
        <ServerIcon width={14} height={14} className="text-muted-foreground" />
        <span className="text-xs text-muted-foreground">暂无在线机器</span>
        <button
          onClick={onRefresh}
          disabled={loading}
          className="rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
          aria-label="刷新机器列表"
        >
          <RefreshIcon width={13} height={13} className={loading ? "animate-spin" : ""} />
        </button>
      </div>
    )
  }

  const single = targets.length === 1

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => !single && setOpen((v) => !v)}
        className={`flex items-center gap-2 rounded-lg border bg-muted/40 px-2.5 py-1.5 transition-colors ${
          single ? "cursor-default" : "hover:bg-muted"
        }`}
        aria-haspopup={single ? undefined : "listbox"}
        aria-expanded={single ? undefined : open}
      >
        <span
          className={`h-2 w-2 shrink-0 rounded-full ${selected ? statusDot(selected) : "bg-muted-foreground"}`}
          aria-hidden
        />
        <span className="max-w-[40vw] truncate text-xs font-medium text-foreground sm:max-w-[200px]">
          {selected?.instance || "选择机器"}
        </span>
        <span className="hidden font-mono text-[11px] text-muted-foreground sm:inline">
          {selected?.cluster}
        </span>
        {!single && (
          <>
            <span className="rounded-full bg-background px-1.5 text-[10px] text-muted-foreground">
              {targets.length}
            </span>
            <ChevronDownIcon
              width={13}
              height={13}
              className={`text-muted-foreground transition-transform ${open ? "rotate-180" : ""}`}
            />
          </>
        )}
      </button>

      {open && !single && (
        <div
          role="listbox"
          className="absolute left-0 top-full z-50 mt-1 max-h-80 w-72 max-w-[85vw] overflow-y-auto rounded-lg border bg-card p-1 shadow-lg"
        >
          <div className="flex items-center justify-between px-2 py-1.5">
            <span className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
              在线机器 {targets.length}
            </span>
            <button
              onClick={onRefresh}
              disabled={loading}
              className="rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
              aria-label="刷新机器列表"
            >
              <RefreshIcon width={13} height={13} className={loading ? "animate-spin" : ""} />
            </button>
          </div>
          {targets.map((t) => {
            const active = selected?.key === t.key
            return (
              <button
                key={t.key}
                role="option"
                aria-selected={active}
                onClick={() => {
                  onSelect(t)
                  setOpen(false)
                }}
                className={`flex w-full items-center gap-2 rounded-md px-2 py-2 text-left transition-colors ${
                  active ? "bg-primary/10" : "hover:bg-muted"
                }`}
              >
                <span className={`h-2 w-2 shrink-0 rounded-full ${statusDot(t)}`} aria-hidden />
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-sm text-foreground">{t.instance}</span>
                  <span className="block truncate font-mono text-[11px] text-muted-foreground">
                    {t.cluster} · {statusLabel(t)}
                  </span>
                </span>
                {active && <CheckIcon width={14} height={14} className="shrink-0 text-primary" />}
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}
