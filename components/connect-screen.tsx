"use client"

import { useState } from "react"
import { login, fetchTargets, type Session } from "@/lib/client"
import { PlugIcon, ServerIcon } from "./icons"

export function ConnectScreen({
  defaultGateway,
  onConnected,
}: {
  defaultGateway: string
  onConnected: (s: Session) => void
}) {
  const [gateway, setGateway] = useState(defaultGateway)
  const [mode, setMode] = useState<"password" | "token">("password")
  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [token, setToken] = useState("")
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")

  async function connect() {
    setError("")
    if (!gateway.trim()) {
      setError("请填写 Gateway 地址")
      return
    }
    setBusy(true)
    try {
      let session: Session
      if (mode === "password") {
        if (!username || !password) throw new Error("请填写用户名和密码")
        const r = await login(gateway, username, password)
        session = { gateway, token: r.token, username: r.username }
      } else {
        if (!token.trim()) throw new Error("请粘贴 user token")
        session = { gateway, token: token.trim() }
      }
      // 验证 token 有效性
      await fetchTargets(session)
      onConnected(session)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="flex min-h-dvh items-center justify-center p-4">
      <div className="w-full max-w-md">
        <div className="mb-8 flex flex-col items-center text-center">
          <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-xl bg-primary/15 text-primary">
            <ServerIcon width={24} height={24} />
          </div>
          <h1 className="text-2xl font-semibold tracking-tight text-foreground">Doops Console</h1>
          <p className="mt-1 text-sm text-muted-foreground text-balance">
            智能运维控制台 · 连接 doops-gateway 执行命令、自然语言运维与部署
          </p>
        </div>

        <div className="rounded-xl border bg-card p-6 shadow-sm">
          <label className="mb-1.5 block text-xs font-medium text-muted-foreground">
            Gateway 地址
          </label>
          <input
            value={gateway}
            onChange={(e) => setGateway(e.target.value)}
            placeholder="http://203.0.113.10:42222"
            className="mb-4 w-full rounded-lg border bg-background px-3 py-2 font-mono text-sm text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
          />

          <div className="mb-4 inline-flex rounded-lg border bg-background p-1 text-sm">
            <button
              onClick={() => setMode("password")}
              className={`rounded-md px-3 py-1.5 transition-colors ${
                mode === "password"
                  ? "bg-primary text-primary-foreground"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              账号密码
            </button>
            <button
              onClick={() => setMode("token")}
              className={`rounded-md px-3 py-1.5 transition-colors ${
                mode === "token"
                  ? "bg-primary text-primary-foreground"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              粘贴 Token
            </button>
          </div>

          {mode === "password" ? (
            <div className="flex flex-col gap-3">
              <input
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="用户名"
                autoComplete="username"
                className="w-full rounded-lg border bg-background px-3 py-2 text-sm text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
              />
              <input
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && connect()}
                type="password"
                placeholder="密码"
                autoComplete="current-password"
                className="w-full rounded-lg border bg-background px-3 py-2 text-sm text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
              />
            </div>
          ) : (
            <textarea
              value={token}
              onChange={(e) => setToken(e.target.value)}
              placeholder="dgw_user_xxx"
              rows={3}
              className="w-full resize-none rounded-lg border bg-background px-3 py-2 font-mono text-sm text-foreground outline-none focus:border-ring focus:ring-2 focus:ring-ring/30"
            />
          )}

          {error && (
            <p className="mt-3 rounded-lg bg-destructive/15 px-3 py-2 text-sm text-destructive">
              {error}
            </p>
          )}

          <button
            onClick={connect}
            disabled={busy}
            className="mt-4 flex w-full items-center justify-center gap-2 rounded-lg bg-primary px-4 py-2.5 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
          >
            <PlugIcon width={16} height={16} />
            {busy ? "连接中…" : "连接"}
          </button>
        </div>

        <p className="mt-4 text-center text-xs text-muted-foreground">
          Token 仅保存在浏览器内存与本机；后端只做请求转发，不落盘。
        </p>
      </div>
    </main>
  )
}
