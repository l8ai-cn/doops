import type { Metadata, Viewport } from "next"
import "./globals.css"

export const metadata: Metadata = {
  title: "Doops Console — 智能运维控制台",
  description:
    "连接 doops-gateway，通过 doops agent 执行命令、自然语言运维与自动化部署的智能运维控制台。",
}

export const viewport: Viewport = {
  themeColor: "#1a1d23",
  width: "device-width",
  initialScale: 1,
}

export default function RootLayout({
  children,
}: {
  children: React.ReactNode
}) {
  return (
    <html lang="zh-CN" className="bg-background">
      <body className="font-sans antialiased">{children}</body>
    </html>
  )
}
