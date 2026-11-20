"use client"

import { useEffect, useRef, useState } from "react"
import type { Target } from "@/lib/client"
import { targetSwitcherEmptyCopy } from "@/lib/empty-targets"
import { ServerIcon, RefreshIcon, ChevronDownIcon, CheckIcon } from "./icons"
import { Button } from "./ui"

function statusDot(t: Target): string {
  if (t.busy) return "bg-warning"
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
  isAdmin,
}: {
  targets: Target[]
  selected: Target | null
  onSelect: (t: Target) => void
  onRefresh: () => void
  loading: boolean
  // 未传入时空状态用对两种角色都成立的说法。
  isAdmin?: boolean
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

  if (targets.length === 0) {
    const empty = targetSwitcherEmptyCopy(isAdmin)
    return (
      <div title={empty.hint} className="flex items-center gap-2 rounded-lg border bg-muted/40 px-2.5 py-1.5">
        <ServerIcon width={14} height={14} className="text-muted-foreground" />
        <span className="whitespace-nowrap text-xs text-muted-foreground">{empty.label}</span>
        <Button
          onClick={onRefresh}
          disabled={loading}
          variant="ghost"
          size="sm"
          icon
          aria-label="刷新 Agent 目标列表"
        >
          <RefreshIcon width={13} height={13} className={loading ? "animate-spin" : ""} />
        </Button>
      </div>
    )
  }

  const current = selected && targets.find(target => target.key === selected.key)

  return (
    <div ref={ref} className="relative min-w-0">
      <Button
        onClick={() => setOpen((v) => !v)}
        size="sm"
        className="max-w-full"
        aria-haspopup="listbox"
        aria-expanded={open}
      >
        <span
          className={`h-2 w-2 shrink-0 rounded-full ${current ? statusDot(current) : "bg-muted-foreground"}`}
          aria-hidden
        />
        <span className="max-w-[40vw] truncate text-xs font-medium text-foreground sm:max-w-[200px]">
          {current?.instance || "选择 Agent 目标"}
        </span>
        <span className="hidden truncate font-mono text-[11px] text-muted-foreground sm:inline">
          {current?.cluster}
        </span>
        {targets.length > 1 && (
          <span className="rounded-full bg-background px-1.5 text-[10px] text-muted-foreground">
            {targets.length}
          </span>
        )}
        <ChevronDownIcon width={13} height={13} className={`text-muted-foreground transition-transform ${open ? "rotate-180" : ""}`} />
      </Button>

      {open && (
        <div
          role="listbox"
          aria-label="在线 Agent 目标"
          className="absolute left-0 top-full z-50 mt-1 max-h-80 w-72 max-w-[calc(100vw_-_4.25rem)] overflow-y-auto rounded-lg border bg-card p-1 shadow-lg"
        >
          <div className="flex items-center justify-between px-2 py-1.5">
            <span className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
              在线 Agent 目标 {targets.length}
            </span>
            <Button
              onClick={onRefresh}
              disabled={loading}
              variant="ghost"
              size="sm"
              icon
              aria-label="刷新 Agent 目标列表"
            >
              <RefreshIcon width={13} height={13} className={loading ? "animate-spin" : ""} />
            </Button>
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
