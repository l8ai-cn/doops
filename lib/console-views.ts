// The console's top-level views. The header lists the same entries at every
// width: inline beside the target switcher from 640px up, and as a row of its
// own below the header on narrower screens, so none of them needs scrolling.

export type ConsoleView = "console" | "assets" | "deploy" | "admin"

export interface ConsoleViewEntry {
  id: ConsoleView
  label: string
}

const ENTRIES: ConsoleViewEntry[] = [
  { id: "console", label: "目标运维" },
  { id: "assets", label: "资产" },
  { id: "deploy", label: "发布部署" },
  { id: "admin", label: "平台管理" },
]

// The admin entry only decides what is shown; the Gateway still checks every
// admin request.
export function consoleViewEntries(isAdmin: boolean): ConsoleViewEntry[] {
  return ENTRIES.filter((entry) => entry.id !== "admin" || isAdmin)
}

// Where "打开凭据" leads from the AI assistant and the release page. DoAgent
// models come only from modelProvider credentials, so this is the console's
// credentials tab, never the admin config center. Only admins get the link.
export function credentialsDestination(isAdmin: boolean): { view: "console"; tab: "credentials" } | null {
  return isAdmin ? { view: "console", tab: "credentials" } : null
}
