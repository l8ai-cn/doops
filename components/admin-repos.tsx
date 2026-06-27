"use client"

import { useCallback, useEffect, useState } from "react"
import type { Session } from "@/lib/client"
import {
  listRepos,
  createRepo,
  updateRepo,
  deleteRepo,
  testRepo,
  type GitRepo,
  type GitRepoInput,
} from "@/lib/admin"
import {
  RefreshIcon,
  PlusIcon,
  TrashIcon,
  CheckIcon,
  GitIcon,
  ExternalLinkIcon,
  PlugIcon,
  EyeIcon,
  EyeOffIcon,
  SettingsIcon,
} from "./icons"

function platformLabel(url: string): string {
  if (/github\.com/i.test(url)) return "GitHub"
  if (/gitee\.com/i.test(url)) return "Gitee"
  if (/gitlab/i.test(url)) return "GitLab"
  if (/bitbucket/i.test(url)) return "Bitbucket"
  if (/cnb\.cool/i.test(url)) return "CNB"
  if (/^git@/i.test(url)) return "SSH"
  return "Git"
}

export function AdminRepos({ session }: { session: Session }) {
  const [repos, setRepos] = useState<GitRepo[]>([])
  const [loading, setLoading] = useState(false)
  const [editing, setEditing] = useState<GitRepo | "new" | null>(null)
  const [status, setStatus] = useState<{ kind: "ok" | "err"; text: string } | null>(null)
  const [testingId, setTestingId] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      setRepos(await listRepos(session))
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    load()
  }, [load])

  async function runTest(repo: GitRepo) {
    setTestingId(repo.id)
    setStatus(null)
    try {
      const r = await testRepo(session, repo.id)
      setStatus({ kind: r.ok ? "ok" : "err", text: `「${repo.name}」${r.message}` })
      if (r.ok) await load()
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    } finally {
      setTestingId(null)
    }
  }

  async function remove(repo: GitRepo) {
    if (!window.confirm(`确定删除仓库「${repo.name}」？`)) return
    try {
      await deleteRepo(session, repo.id)
      setRepos((prev) => prev.filter((r) => r.id !== repo.id))
      setStatus({ kind: "ok", text: `已删除「${repo.name}」` })
    } catch (err) {
      setStatus({ kind: "err", text: (err as Error).message })
    }
  }

  return (
    <div className="h-full overflow-auto">
      <div className="mx-auto max-w-5xl px-5 py-6">
        <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-lg font-semibold text-foreground">代码仓库</h1>
            <p className="mt-1 text-sm text-muted-foreground text-pretty">
              关联任意 Git 仓库（GitHub / Gitee / GitLab / 自建均可），AI 部署时可直接选用；测试连接会从 gateway 校验远端分支。
            </p>
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={load}
              disabled={loading}
              className="flex items-center gap-1.5 rounded-lg border px-3 py-2 text-sm text-foreground transition-colors hover:bg-muted disabled:opacity-50"
            >
              <RefreshIcon width={15} height={15} className={loading ? "animate-spin" : ""} />
              刷新
            </button>
            <button
              onClick={() => setEditing("new")}
              className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              <PlusIcon width={15} height={15} />
              关联仓库
            </button>
          </div>
        </div>

        {status && (
          <div
            className={`mb-4 rounded-lg border px-3 py-2 text-sm ${
              status.kind === "ok"
                ? "border-primary/40 bg-primary/10 text-foreground"
                : "border-destructive/40 bg-destructive/10 text-destructive"
            }`}
          >
            {status.text}
          </div>
        )}

        {repos.length === 0 ? (
          <div className="rounded-xl border border-dashed bg-card p-10 text-center">
            <GitIcon width={28} height={28} className="mx-auto text-muted-foreground" />
            <p className="mt-3 text-sm text-muted-foreground">
              还没有关联任何仓库，点击「关联仓库」添加第一个。
            </p>
          </div>
        ) : (
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            {repos.map((repo) => (
              <div key={repo.id} className="flex flex-col rounded-xl border bg-card p-4">
                <div className="flex items-start gap-2">
                  <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
                    <GitIcon width={18} height={18} />
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="truncate text-sm font-medium text-foreground">{repo.name}</span>
                      <span className="shrink-0 rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">
                        {platformLabel(repo.url)}
                      </span>
                    </div>
                    {repo.description && (
                      <p className="mt-0.5 truncate text-xs text-muted-foreground">{repo.description}</p>
                    )}
                  </div>
                </div>

                <div className="mt-3 flex flex-col gap-1.5 text-xs">
                  <div className="flex items-center gap-2 text-muted-foreground">
                    <span className="shrink-0 text-foreground/60">地址</span>
                    <span className="truncate font-mono text-foreground">{repo.url}</span>
                  </div>
                  <div className="flex flex-wrap gap-x-4 gap-y-1 text-muted-foreground">
                    <span>
                      <span className="text-foreground/60">分支</span>{" "}
                      <span className="font-mono text-foreground">{repo.branch}</span>
                    </span>
                    <span>
                      <span className="text-foreground/60">认证</span>{" "}
                      {repo.has_password ? (
                        <span className="text-foreground">
                          {repo.username ? `${repo.username} ·` : ""} 密码已保存
                        </span>
                      ) : (
                        <span>无（公开 / SSH 密钥）</span>
                      )}
                    </span>
                    {repo.last_used_at && (
                      <span>
                        <span className="text-foreground/60">上次使用</span> {fmt(repo.last_used_at)}
                      </span>
                    )}
                  </div>
                </div>

                <div className="mt-3 flex flex-wrap items-center gap-2 border-t pt-3">
                  <button
                    onClick={() => runTest(repo)}
                    disabled={testingId === repo.id}
                    className="flex items-center gap-1 rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted disabled:opacity-50"
                  >
                    <PlugIcon width={13} height={13} />
                          {testingId === repo.id ? "测试中…" : "测试连接"}
                  </button>
                  <button
                    onClick={() => setEditing(repo)}
                    className="flex items-center gap-1 rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
                  >
                    <SettingsIcon width={13} height={13} />
                    编辑
                  </button>
                  {/^https?:\/\//i.test(repo.url) && (
                    <a
                      href={repo.url.replace(/\.git$/, "")}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="flex items-center gap-1 rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted"
                    >
                      <ExternalLinkIcon width={13} height={13} />
                      打开
                    </a>
                  )}
                  <button
                    onClick={() => remove(repo)}
                    className="ml-auto flex items-center gap-1 rounded-lg border px-2.5 py-1.5 text-xs text-destructive transition-colors hover:bg-destructive/10"
                  >
                    <TrashIcon width={13} height={13} />
                    删除
                  </button>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      {editing && (
        <RepoDialog
          session={session}
          repo={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={(repo, isNew) => {
            setRepos((prev) =>
              isNew ? [repo, ...prev] : prev.map((r) => (r.id === repo.id ? repo : r)),
            )
            setEditing(null)
            setStatus({ kind: "ok", text: isNew ? `已关联「${repo.name}」` : `已更新「${repo.name}」` })
          }}
        />
      )}
    </div>
  )
}

function RepoDialog({
  session,
  repo,
  onClose,
  onSaved,
}: {
  session: Session
  repo: GitRepo | null
  onClose: () => void
  onSaved: (repo: GitRepo, isNew: boolean) => void
}) {
  const isNew = !repo
  const [name, setName] = useState(repo?.name || "")
  const [url, setUrl] = useState(repo?.url || "")
  const [branch, setBranch] = useState(repo?.branch || "main")
  const [username, setUsername] = useState(repo?.username || "")
  const [password, setPassword] = useState("")
  const [showPassword, setShowPassword] = useState(false)
  const [description, setDescription] = useState(repo?.description || "")
  const [saving, setSaving] = useState(false)
  const [err, setErr] = useState("")

  async function submit() {
    if (!name.trim()) {
      setErr("请填写仓库名称")
      return
    }
    if (!url.trim()) {
      setErr("请填写仓库地址")
      return
    }
    setSaving(true)
    setErr("")
    const body: GitRepoInput = {
      name: name.trim(),
      url: url.trim(),
      branch: branch.trim() || "main",
      username: username.trim(),
      description: description.trim(),
    }
    if (password) body.password = password
    try {
      const saved = isNew
        ? await createRepo(session, body)
        : await updateRepo(session, repo!.id, body)
      onSaved(saved, isNew)
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4" onClick={onClose}>
      <div
        className="max-h-[88vh] w-full max-w-lg overflow-auto rounded-xl border bg-card p-5"
        onClick={(e) => e.stopPropagation()}
      >
        <h3 className="text-base font-semibold text-foreground">
          {isNew ? "关联代码仓库" : "编辑仓库"}
        </h3>
        <p className="mt-1 text-xs text-muted-foreground">
          支持任意 Git 服务，HTTPS 私有仓库填用户名 + 密码 / 访问令牌；公开仓库或 gateway 环境已有的 Git 凭据可留空。
        </p>

        <div className="mt-4 flex flex-col gap-3">
          <LabeledInput label="仓库名称" value={name} onChange={setName} placeholder="如：doops-app" />
          <LabeledInput
            label="仓库地址"
            value={url}
            onChange={setUrl}
            placeholder="https://github.com/org/repo.git 或 git@host:org/repo.git"
            mono
          />
          <LabeledInput label="默认分支" value={branch} onChange={setBranch} placeholder="main" mono />

          <div className="rounded-lg border bg-muted/30 p-3">
            <p className="mb-2 text-xs font-medium text-foreground">认证（私有仓库）</p>
            <div className="flex flex-col gap-3">
              <LabeledInput label="用户名" value={username} onChange={setUsername} placeholder="如：deploy-bot" />
              <label className="flex flex-col gap-1">
                <span className="text-xs font-medium text-foreground">
                  密码 / 访问令牌
                  {!isNew && repo?.has_password && (
                    <span className="ml-1 font-normal text-muted-foreground">（留空表示不修改）</span>
                  )}
                </span>
                <div className="relative">
                  <input
                    type={showPassword ? "text" : "password"}
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    placeholder={!isNew && repo?.has_password ? "••••••••（已保存）" : "ghp_xxx 或账号密码"}
                    className="input w-full pr-9 font-mono text-xs"
                  />
                  <button
                    type="button"
                    onClick={() => setShowPassword((v) => !v)}
                    className="absolute right-2 top-1/2 -translate-y-1/2 text-muted-foreground transition-colors hover:text-foreground"
                    aria-label={showPassword ? "隐藏密码" : "显示密码"}
                  >
                    {showPassword ? <EyeOffIcon width={15} height={15} /> : <EyeIcon width={15} height={15} />}
                  </button>
                </div>
                <span className="text-xs text-muted-foreground">
                  凭据在 gateway 本地加密存储，仅用于 Git 连接校验和后续拉取代码，不会回传明文。
                </span>
              </label>
            </div>
          </div>

          <label className="flex flex-col gap-1">
            <span className="text-xs font-medium text-foreground">备注（可选）</span>
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={2}
              placeholder="用途说明，例如：主应用 / 官网 / 基础设施"
              className="input text-xs"
            />
          </label>

          {err && <p className="text-xs text-destructive">{err}</p>}
        </div>

        <div className="mt-5 flex justify-end gap-2">
          <button
            onClick={onClose}
            className="rounded-lg border px-3 py-2 text-sm text-foreground transition-colors hover:bg-muted"
          >
            取消
          </button>
          <button
            onClick={submit}
            disabled={saving}
            className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-50"
          >
            <CheckIcon width={15} height={15} />
            {saving ? "保存中…" : isNew ? "关联" : "保存"}
          </button>
        </div>
      </div>
    </div>
  )
}

function LabeledInput({
  label,
  value,
  onChange,
  placeholder,
  mono,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  placeholder?: string
  mono?: boolean
}) {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-xs font-medium text-foreground">{label}</span>
      <input
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        className={`input ${mono ? "font-mono text-xs" : ""}`}
      />
    </label>
  )
}

function fmt(iso: string): string {
  const d = new Date(iso)
  const diff = Date.now() - d.getTime()
  const min = Math.floor(diff / 60000)
  if (min < 1) return "刚刚"
  if (min < 60) return `${min} 分钟前`
  const h = Math.floor(min / 60)
  if (h < 24) return `${h} 小时前`
  return `${Math.floor(h / 24)} 天前`
}
