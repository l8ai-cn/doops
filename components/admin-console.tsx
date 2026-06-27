"use client"

import { useCallback, useEffect, useState } from "react"
import type { Session } from "@/lib/client"
import {
  listUsers,
  createUser,
  setUserPassword,
  setUserDisabled,
  listGrants,
  createGrant,
  deleteGrant,
  listTokens,
  createToken,
  revokeToken,
  listInstances,
  listOperations,
  cancelOperation,
  ALL_ACTIONS,
  type AdminUser,
  type AdminGrant,
  type AdminToken,
  type AdminInstance,
  type AdminOperation,
} from "@/lib/admin"
import { AdminConfigDispatch } from "./admin-config-dispatch"
import { AdminJobs } from "./admin-jobs"
import {
  ServerIcon,
  UsersIcon,
  ShieldIcon,
  ActivityIcon,
  KeyIcon,
  RefreshIcon,
  PlusIcon,
  TrashIcon,
  BanIcon,
  CheckIcon,
  CopyIcon,
  LayersIcon,
  HistoryIcon,
} from "./icons"

type AdminTab = "instances" | "config" | "jobs" | "users" | "grants" | "tokens" | "operations"

const ADMIN_TABS: { id: AdminTab; label: string; icon: typeof ServerIcon }[] = [
  { id: "instances", label: "实例", icon: ServerIcon },
  { id: "config", label: "配置中心", icon: LayersIcon },
  { id: "jobs", label: "巡检任务", icon: HistoryIcon },
  { id: "users", label: "用户", icon: UsersIcon },
  { id: "grants", label: "权限", icon: ShieldIcon },
  { id: "tokens", label: "令牌", icon: KeyIcon },
  { id: "operations", label: "运行中操作", icon: ActivityIcon },
]

function fmtTime(v?: string): string {
  if (!v) return "—"
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return v
  return d.toLocaleString()
}

export function AdminConsole({ session }: { session: Session }) {
  const [tab, setTab] = useState<AdminTab>("instances")

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <nav className="flex shrink-0 gap-1 overflow-x-auto border-b bg-card px-2">
        {ADMIN_TABS.map((t) => {
          const Icon = t.icon
          const active = tab === t.id
          return (
            <button
              key={t.id}
              onClick={() => setTab(t.id)}
              className={`-mb-px flex shrink-0 items-center gap-1.5 border-b-2 px-3 py-2.5 text-sm transition-colors ${
                active
                  ? "border-primary text-foreground"
                  : "border-transparent text-muted-foreground hover:text-foreground"
              }`}
            >
              <Icon width={16} height={16} />
              {t.label}
            </button>
          )
        })}
      </nav>
      <div className="min-h-0 flex-1 overflow-auto">
        {tab === "instances" && <InstancesView session={session} />}
        {tab === "config" && <AdminConfigDispatch session={session} />}
        {tab === "jobs" && <AdminJobs session={session} />}
        {tab === "users" && <UsersView session={session} />}
        {tab === "grants" && <GrantsView session={session} />}
        {tab === "tokens" && <TokensView session={session} />}
        {tab === "operations" && <OperationsView session={session} />}
      </div>
    </div>
  )
}

function Toolbar({
  title,
  desc,
  onRefresh,
  loading,
  children,
}: {
  title: string
  desc: string
  onRefresh: () => void
  loading: boolean
  children?: React.ReactNode
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-2 border-b bg-card/50 px-4 py-3">
      <div>
        <h2 className="text-sm font-semibold text-foreground">{title}</h2>
        <p className="text-xs text-muted-foreground">{desc}</p>
      </div>
      <div className="flex items-center gap-2">
        {children}
        <button
          onClick={onRefresh}
          disabled={loading}
          className="flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted disabled:opacity-50"
        >
          <RefreshIcon width={14} height={14} className={loading ? "animate-spin" : ""} /> 刷新
        </button>
      </div>
    </div>
  )
}

function ErrorBar({ msg }: { msg: string }) {
  if (!msg) return null
  return (
    <div className="mx-4 mt-3 rounded-lg border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs text-destructive">
      {msg}
    </div>
  )
}

function StatusDot({ status }: { status: string }) {
  const online = status === "online"
  return (
    <span
      className={`inline-block h-2 w-2 rounded-full ${online ? "bg-success" : "bg-muted-foreground/40"}`}
      aria-hidden
    />
  )
}

/* ============================ 实例 ============================ */
function InstancesView({ session }: { session: Session }) {
  const [rows, setRows] = useState<AdminInstance[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      setRows(await listInstances(session))
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    load()
  }, [load])

  return (
    <div>
      <Toolbar
        title="实例"
        desc="已注册到 gateway 的边缘节点及其连接状态"
        onRefresh={load}
        loading={loading}
      />
      <ErrorBar msg={error} />
      <div className="overflow-x-auto p-4">
        <table className="w-full min-w-[640px] border-collapse text-sm">
          <thead>
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th className="px-3 py-2 font-medium">状态</th>
              <th className="px-3 py-2 font-medium">集群</th>
              <th className="px-3 py-2 font-medium">实例</th>
              <th className="px-3 py-2 font-medium">远端地址</th>
              <th className="px-3 py-2 font-medium">活动/排队</th>
              <th className="px-3 py-2 font-medium">最后心跳</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={`${r.cluster}/${r.instance}`} className="border-b border-border/50">
                <td className="px-3 py-2.5">
                  <span className="flex items-center gap-2">
                    <StatusDot status={r.status} />
                    <span className="text-xs text-muted-foreground">
                      {r.status === "online" ? (r.busy ? "忙碌" : "在线") : "离线"}
                    </span>
                  </span>
                </td>
                <td className="px-3 py-2.5 font-mono text-xs text-muted-foreground">{r.cluster}</td>
                <td className="px-3 py-2.5 font-medium text-foreground">{r.instance}</td>
                <td className="px-3 py-2.5 font-mono text-xs text-muted-foreground">
                  {r.remote || "—"}
                </td>
                <td className="px-3 py-2.5 text-xs">
                  <span className="text-primary">{r.active_ops}</span>
                  <span className="text-muted-foreground"> / {r.queued_ops}</span>
                </td>
                <td className="px-3 py-2.5 text-xs text-muted-foreground">{fmtTime(r.last_seen)}</td>
              </tr>
            ))}
            {!rows.length && !loading && (
              <tr>
                <td colSpan={6} className="px-3 py-8 text-center text-sm text-muted-foreground">
                  暂无注册实例
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}

/* ============================ 用户 ============================ */
function UsersView({ session }: { session: Session }) {
  const [rows, setRows] = useState<AdminUser[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const [showCreate, setShowCreate] = useState(false)
  const [name, setName] = useState("")
  const [password, setPassword] = useState("")
  const [admin, setAdmin] = useState(false)
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      setRows(await listUsers(session))
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    load()
  }, [load])

  async function submit() {
    if (!name.trim() || !password.trim()) return
    setBusy(true)
    setError("")
    try {
      await createUser(session, { name: name.trim(), password, admin })
      setName("")
      setPassword("")
      setAdmin(false)
      setShowCreate(false)
      await load()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  async function toggleDisabled(u: AdminUser) {
    setError("")
    try {
      await setUserDisabled(session, { user_id: u.id, disabled: !u.disabled })
      await load()
    } catch (e) {
      setError((e as Error).message)
    }
  }

  async function resetPassword(u: AdminUser) {
    const pw = window.prompt(`为用户「${u.name}」设置新密码：`)
    if (!pw) return
    setError("")
    try {
      await setUserPassword(session, { user_id: u.id, password: pw })
      await load()
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <div>
      <Toolbar title="用户" desc="管理可登录 gateway 的账号" onRefresh={load} loading={loading}>
        <button
          onClick={() => setShowCreate((v) => !v)}
          className="flex items-center gap-1.5 rounded-lg bg-primary px-2.5 py-1.5 text-xs font-medium text-primary-foreground transition-opacity hover:opacity-90"
        >
          <PlusIcon width={14} height={14} /> 新建用户
        </button>
      </Toolbar>
      <ErrorBar msg={error} />

      {showCreate && (
        <div className="mx-4 mt-3 flex flex-wrap items-end gap-3 rounded-lg border bg-muted/40 p-3">
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            用户名
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="input w-40"
              placeholder="ops-wang"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            初始密码
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="input w-40"
              placeholder="••••••••"
            />
          </label>
          <label className="flex items-center gap-1.5 py-2 text-xs text-foreground">
            <input type="checkbox" checked={admin} onChange={(e) => setAdmin(e.target.checked)} />
            授予 admin
          </label>
          <button
            onClick={submit}
            disabled={busy || !name.trim() || !password.trim()}
            className="rounded-lg bg-primary px-3 py-2 text-xs font-medium text-primary-foreground disabled:opacity-40"
          >
            {busy ? "创建中…" : "创建"}
          </button>
        </div>
      )}

      <div className="overflow-x-auto p-4">
        <table className="w-full min-w-[620px] border-collapse text-sm">
          <thead>
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th className="px-3 py-2 font-medium">用户名</th>
              <th className="px-3 py-2 font-medium">角色</th>
              <th className="px-3 py-2 font-medium">授权数</th>
              <th className="px-3 py-2 font-medium">状态</th>
              <th className="px-3 py-2 font-medium">创建时间</th>
              <th className="px-3 py-2 font-medium text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((u) => (
              <tr key={u.id} className="border-b border-border/50">
                <td className="px-3 py-2.5 font-medium text-foreground">{u.name}</td>
                <td className="px-3 py-2.5">
                  {u.is_admin ? (
                    <span className="rounded bg-primary/15 px-1.5 py-0.5 text-xs font-medium text-primary">
                      admin
                    </span>
                  ) : (
                    <span className="text-xs text-muted-foreground">user</span>
                  )}
                </td>
                <td className="px-3 py-2.5 text-xs text-muted-foreground">{u.grant_count}</td>
                <td className="px-3 py-2.5">
                  <span
                    className={`text-xs ${u.disabled ? "text-destructive" : "text-success"}`}
                  >
                    {u.disabled ? "已停用" : "正常"}
                  </span>
                </td>
                <td className="px-3 py-2.5 text-xs text-muted-foreground">{fmtTime(u.created_at)}</td>
                <td className="px-3 py-2.5">
                  <div className="flex items-center justify-end gap-1.5">
                    <button
                      onClick={() => resetPassword(u)}
                      className="rounded-md border px-2 py-1 text-xs text-foreground transition-colors hover:bg-muted"
                    >
                      改密码
                    </button>
                    <button
                      onClick={() => toggleDisabled(u)}
                      className={`flex items-center gap-1 rounded-md border px-2 py-1 text-xs transition-colors hover:bg-muted ${
                        u.disabled ? "text-success" : "text-destructive"
                      }`}
                    >
                      {u.disabled ? (
                        <>
                          <CheckIcon width={12} height={12} /> 启用
                        </>
                      ) : (
                        <>
                          <BanIcon width={12} height={12} /> 停用
                        </>
                      )}
                    </button>
                  </div>
                </td>
              </tr>
            ))}
            {!rows.length && !loading && (
              <tr>
                <td colSpan={6} className="px-3 py-8 text-center text-sm text-muted-foreground">
                  暂无用户
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}

/* ============================ 权限 ============================ */
function GrantsView({ session }: { session: Session }) {
  const [grants, setGrants] = useState<AdminGrant[]>([])
  const [users, setUsers] = useState<AdminUser[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const [showCreate, setShowCreate] = useState(false)
  const [userId, setUserId] = useState("")
  const [cluster, setCluster] = useState("*")
  const [instance, setInstance] = useState("*")
  const [actions, setActions] = useState<string[]>(["exec", "ask", "read"])
  const [busy, setBusy] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      const [g, u] = await Promise.all([listGrants(session), listUsers(session)])
      setGrants(g)
      setUsers(u)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    load()
  }, [load])

  function toggleAction(a: string) {
    setActions((prev) => (prev.includes(a) ? prev.filter((x) => x !== a) : [...prev, a]))
  }

  async function submit() {
    if (!userId) return
    setBusy(true)
    setError("")
    try {
      await createGrant(session, { user_id: userId, cluster, instance, actions })
      setShowCreate(false)
      setActions(["exec", "ask", "read"])
      await load()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  async function remove(id: number) {
    if (!window.confirm("确认删�����该授权？")) return
    setError("")
    try {
      await deleteGrant(session, id)
      await load()
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <div>
      <Toolbar
        title="权限"
        desc="RBAC 授权：用户 × 集群/实例 × 动作"
        onRefresh={load}
        loading={loading}
      >
        <button
          onClick={() => setShowCreate((v) => !v)}
          className="flex items-center gap-1.5 rounded-lg bg-primary px-2.5 py-1.5 text-xs font-medium text-primary-foreground transition-opacity hover:opacity-90"
        >
          <PlusIcon width={14} height={14} /> 新建授权
        </button>
      </Toolbar>
      <ErrorBar msg={error} />

      {showCreate && (
        <div className="mx-4 mt-3 flex flex-col gap-3 rounded-lg border bg-muted/40 p-3">
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              用户
              <select value={userId} onChange={(e) => setUserId(e.target.value)} className="input w-44">
                <option value="">选择用户…</option>
                {users.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.name}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              集群
              <input value={cluster} onChange={(e) => setCluster(e.target.value)} className="input w-32" placeholder="* 或 prod-cn" />
            </label>
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              实例
              <input value={instance} onChange={(e) => setInstance(e.target.value)} className="input w-32" placeholder="* 或 web-01" />
            </label>
          </div>
          <div className="flex flex-col gap-1.5">
            <span className="text-xs text-muted-foreground">动作</span>
            <div className="flex flex-wrap gap-1.5">
              {ALL_ACTIONS.map((a) => {
                const on = actions.includes(a)
                return (
                  <button
                    key={a}
                    onClick={() => toggleAction(a)}
                    className={`rounded-md border px-2 py-1 font-mono text-xs transition-colors ${
                      on
                        ? "border-primary bg-primary/15 text-primary"
                        : "text-muted-foreground hover:bg-muted"
                    }`}
                  >
                    {a}
                  </button>
                )
              })}
            </div>
          </div>
          <div>
            <button
              onClick={submit}
              disabled={busy || !userId}
              className="rounded-lg bg-primary px-3 py-2 text-xs font-medium text-primary-foreground disabled:opacity-40"
            >
              {busy ? "授权中…" : "授权"}
            </button>
          </div>
        </div>
      )}

      <div className="overflow-x-auto p-4">
        <table className="w-full min-w-[680px] border-collapse text-sm">
          <thead>
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th className="px-3 py-2 font-medium">用户</th>
              <th className="px-3 py-2 font-medium">集群</th>
              <th className="px-3 py-2 font-medium">实例</th>
              <th className="px-3 py-2 font-medium">动作</th>
              <th className="px-3 py-2 font-medium text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            {grants.map((g) => (
              <tr key={g.id} className="border-b border-border/50">
                <td className="px-3 py-2.5 font-medium text-foreground">{g.user_name || g.user_id}</td>
                <td className="px-3 py-2.5 font-mono text-xs text-muted-foreground">{g.cluster}</td>
                <td className="px-3 py-2.5 font-mono text-xs text-muted-foreground">{g.instance}</td>
                <td className="px-3 py-2.5">
                  <div className="flex flex-wrap gap-1">
                    {g.actions.map((a) => (
                      <span
                        key={a}
                        className={`rounded px-1.5 py-0.5 font-mono text-xs ${
                          a === "admin"
                            ? "bg-primary/15 text-primary"
                            : "bg-muted text-muted-foreground"
                        }`}
                      >
                        {a}
                      </span>
                    ))}
                  </div>
                </td>
                <td className="px-3 py-2.5">
                  <div className="flex justify-end">
                    <button
                      onClick={() => remove(g.id)}
                      className="flex items-center gap-1 rounded-md border px-2 py-1 text-xs text-destructive transition-colors hover:bg-muted"
                    >
                      <TrashIcon width={12} height={12} /> 删除
                    </button>
                  </div>
                </td>
              </tr>
            ))}
            {!grants.length && !loading && (
              <tr>
                <td colSpan={5} className="px-3 py-8 text-center text-sm text-muted-foreground">
                  暂无授权
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}

/* ============================ 令牌 ============================ */
function TokensView({ session }: { session: Session }) {
  const [rows, setRows] = useState<AdminToken[]>([])
  const [users, setUsers] = useState<AdminUser[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const [showCreate, setShowCreate] = useState(false)
  const [kind, setKind] = useState("user")
  const [userName, setUserName] = useState("")
  const [name, setName] = useState("")
  const [cluster, setCluster] = useState("")
  const [instance, setInstance] = useState("")
  const [busy, setBusy] = useState(false)
  const [created, setCreated] = useState("")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      const [t, u] = await Promise.all([listTokens(session), listUsers(session)])
      setRows(t)
      setUsers(u)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    load()
  }, [load])

  async function submit() {
    setBusy(true)
    setError("")
    setCreated("")
    try {
      const res = await createToken(session, {
        kind,
        user: kind === "user" ? userName : undefined,
        name: name.trim() || undefined,
        cluster: kind === "agent" ? cluster.trim() : undefined,
        instance: kind === "agent" ? instance.trim() : undefined,
      })
      setCreated(res.token)
      setName("")
      await load()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  async function revoke(id: string) {
    if (!window.confirm("确认撤销该令牌？撤销后立即失效。")) return
    setError("")
    try {
      await revokeToken(session, id)
      await load()
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <div>
      <Toolbar
        title="令牌"
        desc="用户访问令牌与 agent 注册令牌"
        onRefresh={load}
        loading={loading}
      >
        <button
          onClick={() => {
            setShowCreate((v) => !v)
            setCreated("")
          }}
          className="flex items-center gap-1.5 rounded-lg bg-primary px-2.5 py-1.5 text-xs font-medium text-primary-foreground transition-opacity hover:opacity-90"
        >
          <PlusIcon width={14} height={14} /> 签发令牌
        </button>
      </Toolbar>
      <ErrorBar msg={error} />

      {showCreate && (
        <div className="mx-4 mt-3 flex flex-col gap-3 rounded-lg border bg-muted/40 p-3">
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              类型
              <select value={kind} onChange={(e) => setKind(e.target.value)} className="input w-32">
                <option value="user">用户令牌</option>
                <option value="agent">Agent 令牌</option>
              </select>
            </label>
            {kind === "user" ? (
              <label className="flex flex-col gap-1 text-xs text-muted-foreground">
                所属用户
                <select value={userName} onChange={(e) => setUserName(e.target.value)} className="input w-40">
                  <option value="">选择用户…</option>
                  {users.map((u) => (
                    <option key={u.id} value={u.name}>
                      {u.name}
                    </option>
                  ))}
                </select>
              </label>
            ) : (
              <>
                <label className="flex flex-col gap-1 text-xs text-muted-foreground">
                  集群
                  <input value={cluster} onChange={(e) => setCluster(e.target.value)} className="input w-32" placeholder="prod-cn" />
                </label>
                <label className="flex flex-col gap-1 text-xs text-muted-foreground">
                  实例
                  <input value={instance} onChange={(e) => setInstance(e.target.value)} className="input w-32" placeholder="web-03" />
                </label>
              </>
            )}
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              备注名
              <input value={name} onChange={(e) => setName(e.target.value)} className="input w-36" placeholder="可选" />
            </label>
            <button
              onClick={submit}
              disabled={busy || (kind === "user" && !userName) || (kind === "agent" && (!cluster.trim() || !instance.trim()))}
              className="rounded-lg bg-primary px-3 py-2 text-xs font-medium text-primary-foreground disabled:opacity-40"
            >
              {busy ? "签发中…" : "签发"}
            </button>
          </div>
          {created && (
            <div className="flex items-center gap-2 rounded-lg border border-success/40 bg-success/10 p-2.5">
              <span className="text-xs text-muted-foreground">请立即复制（仅显示一次）：</span>
              <code className="flex-1 truncate font-mono text-xs text-foreground">{created}</code>
              <button
                onClick={() => navigator.clipboard?.writeText(created)}
                className="flex items-center gap-1 rounded-md border px-2 py-1 text-xs text-foreground transition-colors hover:bg-muted"
              >
                <CopyIcon width={12} height={12} /> 复制
              </button>
            </div>
          )}
        </div>
      )}

      <div className="overflow-x-auto p-4">
        <table className="w-full min-w-[720px] border-collapse text-sm">
          <thead>
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th className="px-3 py-2 font-medium">类型</th>
              <th className="px-3 py-2 font-medium">归属</th>
              <th className="px-3 py-2 font-medium">前缀</th>
              <th className="px-3 py-2 font-medium">状态</th>
              <th className="px-3 py-2 font-medium">创建时间</th>
              <th className="px-3 py-2 font-medium text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((t) => (
              <tr key={t.id} className="border-b border-border/50">
                <td className="px-3 py-2.5">
                  <span
                    className={`rounded px-1.5 py-0.5 text-xs ${
                      t.kind === "agent" ? "bg-accent/15 text-accent" : "bg-muted text-muted-foreground"
                    }`}
                  >
                    {t.kind === "agent" ? "agent" : "user"}
                  </span>
                </td>
                <td className="px-3 py-2.5 text-xs text-foreground">
                  {t.kind === "agent"
                    ? `${t.cluster || "?"}/${t.instance || "?"}`
                    : t.user_name || t.user_id || "—"}
                  {t.name ? <span className="text-muted-foreground"> · {t.name}</span> : null}
                </td>
                <td className="px-3 py-2.5 font-mono text-xs text-muted-foreground">{t.prefix}…</td>
                <td className="px-3 py-2.5">
                  <span className={`text-xs ${t.revoked ? "text-destructive" : "text-success"}`}>
                    {t.revoked ? "已撤销" : "有效"}
                  </span>
                </td>
                <td className="px-3 py-2.5 text-xs text-muted-foreground">{fmtTime(t.created_at)}</td>
                <td className="px-3 py-2.5">
                  <div className="flex justify-end">
                    {!t.revoked && (
                      <button
                        onClick={() => revoke(t.id)}
                        className="flex items-center gap-1 rounded-md border px-2 py-1 text-xs text-destructive transition-colors hover:bg-muted"
                      >
                        <BanIcon width={12} height={12} /> 撤销
                      </button>
                    )}
                  </div>
                </td>
              </tr>
            ))}
            {!rows.length && !loading && (
              <tr>
                <td colSpan={6} className="px-3 py-8 text-center text-sm text-muted-foreground">
                  暂无令牌
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}

/* ============================ 运行中操作 ============================ */
function OperationsView({ session }: { session: Session }) {
  const [rows, setRows] = useState<AdminOperation[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      setRows(await listOperations(session))
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    load()
    const id = setInterval(load, 5000)
    return () => clearInterval(id)
  }, [load])

  async function cancel(id: string) {
    if (!window.confirm("确认取消该运行中操作？")) return
    setError("")
    try {
      await cancelOperation(session, id)
      await load()
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <div>
      <Toolbar
        title="运行中操作"
        desc="当前正在各实例上执行的任务（每 5 秒刷新）"
        onRefresh={load}
        loading={loading}
      />
      <ErrorBar msg={error} />
      <div className="overflow-x-auto p-4">
        <table className="w-full min-w-[720px] border-collapse text-sm">
          <thead>
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th className="px-3 py-2 font-medium">目标</th>
              <th className="px-3 py-2 font-medium">用户</th>
              <th className="px-3 py-2 font-medium">动作</th>
              <th className="px-3 py-2 font-medium">摘要</th>
              <th className="px-3 py-2 font-medium">已运行</th>
              <th className="px-3 py-2 font-medium text-right">操作</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((o) => (
              <tr key={o.id} className="border-b border-border/50">
                <td className="px-3 py-2.5 text-xs">
                  <span className="font-medium text-foreground">{o.instance}</span>
                  <span className="text-muted-foreground"> · {o.cluster}</span>
                </td>
                <td className="px-3 py-2.5 text-xs text-muted-foreground">{o.user_id}</td>
                <td className="px-3 py-2.5">
                  <span className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">
                    {o.action}
                  </span>
                </td>
                <td className="px-3 py-2.5 max-w-xs truncate font-mono text-xs text-muted-foreground">
                  {o.command_summary || o.kind}
                </td>
                <td className="px-3 py-2.5 text-xs text-muted-foreground">{o.age_seconds}s</td>
                <td className="px-3 py-2.5">
                  <div className="flex justify-end">
                    <button
                      onClick={() => cancel(o.id)}
                      className="flex items-center gap-1 rounded-md border px-2 py-1 text-xs text-destructive transition-colors hover:bg-muted"
                    >
                      <BanIcon width={12} height={12} /> 取消
                    </button>
                  </div>
                </td>
              </tr>
            ))}
            {!rows.length && !loading && (
              <tr>
                <td colSpan={6} className="px-3 py-8 text-center text-sm text-muted-foreground">
                  当前无运行中操作
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
