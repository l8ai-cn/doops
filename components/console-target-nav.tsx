"use client"

import { Boxes, Database, Globe2 } from "lucide-react"
import { useId, useState } from "react"
import { ActivityIcon, ChevronDownIcon, FileIcon, FileTextIcon, HistoryIcon, KeyIcon, SparkIcon, TerminalIcon } from "./icons"

const directTabs = [
  { id: "overview", label: "当前目标", icon: ActivityIcon },
  { id: "ask", label: "AI 助手", icon: SparkIcon },
] as const

const groups = [
  { id: "resources", label: "操作资源", items: [
    { id: "database", label: "数据库", icon: Database },
    { id: "website", label: "网站", icon: Globe2 },
    { id: "k8s", label: "Kubernetes", icon: Boxes },
    { id: "image", label: "镜像", icon: Boxes },
    { id: "files", label: "文件", icon: FileIcon },
  ] },
  { id: "tools", label: "运维工具", items: [
    { id: "credentials", label: "凭据", icon: KeyIcon },
    { id: "audit", label: "审计", icon: HistoryIcon },
    { id: "kb", label: "知识库", icon: FileTextIcon },
    { id: "terminal", label: "终端", icon: TerminalIcon },
  ] },
] as const

type NavItem = (typeof directTabs)[number] | (typeof groups)[number]["items"][number]
export type TargetTab = NavItem["id"]

export function TargetToolsNav({ tab, disabled, onChange }: {
  tab: TargetTab
  disabled?: boolean
  onChange: (tab: TargetTab) => void
}) {
  const navId = useId()
  const activeGroup = groups.find(group => group.items.some(item => item.id === tab))
  const [menu, setMenu] = useState({ tab, expanded: activeGroup?.id })
  if (menu.tab !== tab) setMenu({ tab, expanded: activeGroup?.id })

  function selectTab(id: TargetTab) {
    if (id === tab) setMenu({ tab, expanded: activeGroup?.id })
    onChange(id)
  }

  function renderTab({ id, label, icon: Icon }: NavItem) {
    return (
      <button
        key={id}
        type="button"
        className="page-tab justify-center"
        disabled={disabled}
        aria-current={tab === id ? "page" : undefined}
        onClick={() => selectTab(id)}
        title={id === "terminal" ? "高级：直接执行 shell 命令" : undefined}
      >
        <Icon width={16} height={16} aria-hidden="true" />
        {label}
        {id === "terminal" && <span className="rounded bg-muted px-1 py-0.5 text-[10px] text-muted-foreground">高级</span>}
      </button>
    )
  }

  return (
    <nav aria-label="控制台功能" className="min-w-0 shrink-0 border-b bg-card">
      <div className="grid grid-cols-2 px-2 sm:flex sm:flex-wrap sm:gap-1 sm:px-4">
        {directTabs.map(renderTab)}
        {groups.map(group => {
          const open = menu.expanded === group.id
          const current = group.items.find(item => item.id === tab)
          return (
            <button
              key={group.id}
              type="button"
              className="page-tab justify-center aria-expanded:bg-muted"
              disabled={disabled}
              aria-expanded={open}
              aria-controls={`${navId}-${group.id}`}
              aria-current={current && !open ? "page" : undefined}
              onClick={() => setMenu({ tab, expanded: open ? undefined : group.id })}
            >
              <span className="sm:flex sm:items-center sm:gap-1.5">
                {group.label}
                {current && !open && <span className="block text-xs text-primary">{current.label}</span>}
              </span>
              <ChevronDownIcon width={14} height={14} aria-hidden="true" className={open ? "rotate-180" : undefined} />
            </button>
          )
        })}
      </div>
      {groups.map(group => (
        <div
          key={group.id}
          id={`${navId}-${group.id}`}
          hidden={menu.expanded !== group.id}
          role="group"
          aria-label={group.label}
          className="border-t px-2 sm:px-4"
        >
          {menu.expanded === group.id && <div className="flex flex-wrap gap-x-1">{group.items.map(renderTab)}</div>}
        </div>
      ))}
    </nav>
  )
}
