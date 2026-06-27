import type { Metadata } from "next"
import { CasesPage } from "@/components/cases-page"

export const metadata: Metadata = {
  title: "使用案例 — Doops 智能运维控制台",
  description:
    "从单机初创到多区域集群，看不同规模的团队如何用 Doops 把日常运维交给 AI：自然语言运维、定时巡检、配置下发与审计。",
}

export default function Page() {
  return <CasesPage />
}
