import type { Metadata } from "next"
import { DeployGuide } from "@/components/deploy-guide"

export const metadata: Metadata = {
  title: "部署 Doops Agent · 部署指南",
  description:
    "如何在你的服务器上部署 Doops Agent：准备工作、获取接入令牌、一键安装、启动自启与故障排查。没有可用实例时，可自行部署或联系管理员。",
}

export default function DeployDocsPage() {
  return <DeployGuide />
}
