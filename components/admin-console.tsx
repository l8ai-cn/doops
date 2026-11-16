"use client"

import { Button, Select, Input, PageTabs } from "./ui"
import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import type { Session } from "@/lib/client"
import {
  GRANT_ALL,
  grantInstanceChoices,
  grantScopeOptions,
  grantScopeWarning,
  validateGrantForm,
} from "@/lib/grant-form"
import {
  ACTIVE_INSTANCE_WINDOW_DAYS,
  TOKEN_STATE_LABELS,
  gatewayTime,
  groupInstances,
  groupTokensByUser,
  instanceClusterOptions,
  isTokenGroupCollapsed,
  offlineLabel,
  sortUsersForDisplay,
  summarizeInstanceScope,
  summarizeUsers,
  type TokenState,
} from "@/lib/admin-views"
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
  archiveInstance,
  unarchiveInstance,
  type AdminUser,
  type AdminGrant,
  type AdminToken,
  type AdminInstance,
} from "@/lib/admin"
import { AdminConfigDispatch } from "./admin-config-dispatch"
import { ProxyRoutesPanel } from "./proxy-routes-panel"
import { useAppDialog } from "./app-dialog"
import {
  ServerIcon,
  UsersIcon,
  ShieldIcon,
  KeyIcon,
  RefreshIcon,
  PlusIcon,
  TrashIcon,
  BanIcon,
  CheckIcon,
  CopyIcon,
  LayersIcon,
  PlugIcon,
} from "./icons"

type AdminTab =
  | "instances"
  | "config"
  | "users"
  | "grants"
  | "tokens"
  | "proxy"

const ADMIN_TABS: { id: AdminTab; label: string; icon: typeof ServerIcon }[] = [
  { id: "instances", label: "实例", icon: ServerIcon },
  { id: "config", label: "配置中心", icon: LayersIcon },
  { id: "users", label: "用户", icon: UsersIcon },
  { id: "grants", label: "权限", icon: ShieldIcon },
  { id: "tokens", label: "令牌", icon: KeyIcon },
  { id: "proxy", label: "代理路由", icon: PlugIcon },
]

function fmtTime(v?: string): string {
  if (!v) return "—"
  const d = new Date(v)
  if (Number.isNaN(d.getTime())) return v
  return d.toLocaleString()
}

export function AdminConsole({
  session,
  defaultTab = "instances",
}: {
  session: Session
  defaultTab?: AdminTab
}) {
  const [tab, setTab] = useState<AdminTab>(defaultTab)

  useEffect(() => {
    setTab(defaultTab)
  }, [defaultTab])

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <PageTabs
        ariaLabel="管理"
        value={tab}
        onChange={setTab}
        items={ADMIN_TABS.map(({ id, label, icon: Icon }) => ({
          id,
          label,
          icon: <Icon width={16} height={16} />,
        }))}
      />
      <div className="min-h-0 flex-1 overflow-auto">
        {tab === "instances" && <InstancesView session={session} />}
        {tab === "config" && <AdminConfigDispatch session={session} />}
        {tab === "users" && <UsersView session={session} />}
        {tab === "grants" && <GrantsView session={session} />}
        {tab === "tokens" && <TokensView session={session} />}
        {tab === "proxy" && <ProxyRoutesPanel session={session} />}
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
        <Button
          onClick={onRefresh}
          disabled={loading}
          size="sm"
        >
          <RefreshIcon width={14} height={14} className={loading ? "animate-spin" : ""} /> 刷新
        </Button>
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
  const [all, setAll] = useState<AdminInstance[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const [now, setNow] = useState(() => Date.now())
  const [cluster, setCluster] = useState("")
  const [showHistory, setShowHistory] = useState(false)
  const [showArchived, setShowArchived] = useState(false)
  const [busyKey, setBusyKey] = useState("")
  const archiveRef = useRef(false)

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      setAll(await listInstances(session, { includeHistory: true }))
      setNow(Date.now())
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    load()
  }, [load])

  const clusterOptions = useMemo(() => instanceClusterOptions(all), [all])
  const groups = useMemo(() => groupInstances(all, now, cluster), [all, now, cluster])
  const scope = useMemo(() => summarizeInstanceScope(all, now), [all, now])
  const historyKeys = new Set(groups.history.map((r) => `${r.cluster}/${r.instance}`))
  const archivedKeys = new Set(groups.archived.map((r) => `${r.cluster}/${r.instance}`))
  // Default to active rows. Historical and archived records remain discoverable
  // through separate explicit expanders so neither is mistaken for a live target.
  const rows = [
    ...groups.active,
    ...(showHistory ? groups.history : []),
    ...(showArchived ? groups.archived : []),
  ]

  async function changeArchive(row: AdminInstance) {
    if (archiveRef.current) return
    archiveRef.current = true
    const key = `${row.cluster}/${row.instance}`
    setBusyKey(key)
    setError("")
    try {
      if (row.archived) await unarchiveInstance(session, row)
      else await archiveInstance(session, row)
      await load()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      archiveRef.current = false
      setBusyKey("")
    }
  }

  return (
    <div>
      <Toolbar
        title="实例"
        desc={`在线 ${scope.online} · ${ACTIVE_INSTANCE_WINDOW_DAYS} 天内离线 ${scope.active - scope.online} · 历史 ${scope.history} · 已归档 ${scope.archived}`}
        onRefresh={load}
        loading={loading}
      >
        <Select
          aria-label="按集群筛选"
          value={cluster}
          onChange={(e) => setCluster(e.target.value)}
          className="w-56 text-xs"
        >
          <option value="">全部集群（{clusterOptions.length}）</option>
          {clusterOptions.map((option) => (
            <option key={option.name} value={option.name}>
              {option.name}（在线 {option.online}/{option.total}）
            </option>
          ))}
        </Select>
      </Toolbar>
      <ErrorBar msg={error} />
      <div className="overflow-x-auto p-4">
        <table className="w-full min-w-[760px] border-collapse text-sm">
          <thead>
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th className="px-3 py-2 font-medium">状态</th>
              <th className="px-3 py-2 font-medium">集群</th>
              <th className="px-3 py-2 font-medium">实例</th>
              <th className="px-3 py-2 font-medium">远端地址</th>
              <th className="px-3 py-2 font-medium">活动/排队</th>
              <th className="px-3 py-2 font-medium">最后心跳</th>
              <th className="px-3 py-2 text-right font-medium">操作</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr
                key={`${r.cluster}/${r.instance}`}
                className={`border-b border-border/50 ${historyKeys.has(`${r.cluster}/${r.instance}`) ? "opacity-60" : ""} ${archivedKeys.has(`${r.cluster}/${r.instance}`) ? "bg-muted/40 opacity-60" : ""}`}
              >
                <td className="px-3 py-2.5">
                  <span className="flex items-center gap-2">
                    <StatusDot status={r.status} />
                    <span className="whitespace-nowrap text-xs text-muted-foreground">
                      {r.archived ? "已归档" : r.status === "online" ? (r.busy ? "忙碌" : "在线") : offlineLabel(r, now)}
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
                <td className="px-3 py-2.5 text-right">
                  {r.status !== "online" && (
                    <button
                      type="button"
                      onClick={() => changeArchive(r)}
                      disabled={!!busyKey}
                      className="rounded-md border px-2 py-1 text-xs text-foreground transition-colors hover:bg-muted disabled:opacity-50"
                    >
                      {busyKey === `${r.cluster}/${r.instance}` ? "处理中…" : r.archived ? "取消归档" : "归档"}
                    </button>
                  )}
                </td>
              </tr>
            ))}
            {!rows.length && !loading && (
              <tr>
                <td colSpan={7} className="px-3 py-8 text-center text-sm text-muted-foreground">
                  {groups.history.length || groups.archived.length
                    ? `没有在线或 ${ACTIVE_INSTANCE_WINDOW_DAYS} 天内离线的实例；历史和已归档实例默认折叠`
                    : cluster
                      ? "该集群暂无注册实例"
                      : "暂无注册实例"}
                </td>
              </tr>
            )}
          </tbody>
        </table>
        <div className="mt-2 flex flex-wrap gap-1">
          {groups.history.length > 0 && (
            <button
              type="button"
              aria-expanded={showHistory}
              onClick={() => setShowHistory((v) => !v)}
              className="flex items-center gap-1.5 rounded-md px-2 py-1.5 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            >
              <span aria-hidden>{showHistory ? "▾" : "▸"}</span>
              {showHistory ? "收起" : "展开"}历史实例（{groups.history.length}）
              <span className="text-muted-foreground/70">
                · 离线超过 {ACTIVE_INSTANCE_WINDOW_DAYS} 天或没有心跳记录
              </span>
            </button>
          )}
          {groups.archived.length > 0 && (
            <button
              type="button"
              aria-expanded={showArchived}
              onClick={() => setShowArchived((v) => !v)}
              className="flex items-center gap-1.5 rounded-md px-2 py-1.5 text-xs text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
            >
              <span aria-hidden>{showArchived ? "▾" : "▸"}</span>
              {showArchived ? "收起" : "显示"}已归档实例（{groups.archived.length}）
            </button>
          )}
        </div>
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
  const { prompt, dialog } = useAppDialog()

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
    const pw = await prompt({
      title: "设置新密码",
      message: `为用户「${u.name}」设置新密码。`,
      label: "新密码",
      inputType: "password",
      confirmLabel: "保存",
    })
    if (!pw) return
    setError("")
    try {
      await setUserPassword(session, { user_id: u.id, password: pw })
      await load()
    } catch (e) {
      setError((e as Error).message)
    }
  }

  const userSummary = useMemo(() => summarizeUsers(rows), [rows])
  // 已停用用户排到最后并淡化显示。
  const displayRows = useMemo(() => sortUsersForDisplay(rows), [rows])

  return (
    <div>
      <Toolbar
        title={`用户（共 ${userSummary.total} · admin ${userSummary.admins}${userSummary.disabled ? ` · 已停用 ${userSummary.disabled}` : ""}）`}
        desc="管理可登录 gateway 的账号"
        onRefresh={load}
        loading={loading}
      >
        <Button
          onClick={() => setShowCreate((v) => !v)}
          variant="primary" size="sm"
        >
          <PlusIcon width={14} height={14} /> 新建用户
        </Button>
      </Toolbar>
      <ErrorBar msg={error} />

      {showCreate && (
        <div className="mx-4 mt-3 flex flex-wrap items-end gap-3 rounded-lg border bg-muted/40 p-3">
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            用户名
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="w-40"
              placeholder="ops-wang"
            />
          </label>
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            初始密码
            <Input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="w-40"
              placeholder="••••••••"
            />
          </label>
          <label className="flex items-center gap-1.5 py-2 text-xs text-foreground">
            <input type="checkbox" checked={admin} onChange={(e) => setAdmin(e.target.checked)} />
            授予 admin
          </label>
          <Button
            onClick={submit}
            disabled={busy || !name.trim() || !password.trim()}
            variant="primary" size="sm"
          >
            {busy ? "创建中…" : "创建"}
          </Button>
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
            {displayRows.map((u) => (
              <tr key={u.id} className={`border-b border-border/50 ${u.disabled ? "opacity-50" : ""}`}>
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
                    <Button
                      onClick={() => resetPassword(u)}
                      size="sm"
                    >
                      改密码
                    </Button>
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
      {dialog}
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
  // 集群和实例默认不选；Gateway 会把空值存成 "*"，所以未选择时不允许提交。
  const [cluster, setCluster] = useState("")
  const [instance, setInstance] = useState("")
  const [instances, setInstances] = useState<AdminInstance[]>([])
  const [instancesError, setInstancesError] = useState("")
  const [busy, setBusy] = useState(false)
  const busyRef = useRef(false)

  const scope = useMemo(() => grantScopeOptions(instances), [instances])
  const instanceChoices = grantInstanceChoices(scope, cluster)
  const validation = validateGrantForm({ userId, cluster, instance }, scope)
  const scopeWarning = grantScopeWarning(cluster, instance)

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    setInstancesError("")
    try {
      const [g, u, i] = await Promise.all([
        listGrants(session),
        listUsers(session),
        listInstances(session, { includeHistory: true }).catch((e: Error) => {
          setInstancesError(`查询实例列表失败：${e.message}`)
          return [] as AdminInstance[]
        }),
      ])
      setGrants(g)
      setUsers(u)
      setInstances(i)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setLoading(false)
    }
  }, [session])

  useEffect(() => {
    load()
  }, [load])

  function selectCluster(next: string) {
    setCluster(next)
    setInstance(next === GRANT_ALL ? GRANT_ALL : "")
  }

  async function submit() {
    if (busyRef.current) return
    const checked = validateGrantForm({ userId, cluster, instance }, scope)
    if (!checked.ok) {
      setError(checked.error)
      return
    }
    busyRef.current = true
    setBusy(true)
    setError("")
    try {
      await createGrant(session, checked.body)
      setShowCreate(false)
      setCluster("")
      setInstance("")
      await load()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      busyRef.current = false
      setBusy(false)
    }
  }

  async function remove(id: number) {
    if (busyRef.current) return
    busyRef.current = true
    setBusy(true)
    setError("")
    try {
      await deleteGrant(session, id)
      await load()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      busyRef.current = false
      setBusy(false)
    }
  }

  return (
    <div>
      <Toolbar
        title="权限"
        desc="授权：用户 × 集群/实例"
        onRefresh={load}
        loading={loading}
      >
        <Button
          onClick={() => setShowCreate((v) => !v)}
          variant="primary" size="sm"
        >
          <PlusIcon width={14} height={14} /> 新建授权
        </Button>
      </Toolbar>
      <ErrorBar msg={error} />

      {showCreate && (
        <div className="mx-4 mt-3 flex flex-col gap-3 rounded-lg border bg-muted/40 p-3">
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              用户
              <Select value={userId} onChange={(e) => setUserId(e.target.value)} className="w-44">
                <option value="">选择用户…</option>
                {users.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.name}
                  </option>
                ))}
              </Select>
            </label>
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              集群
              <Select aria-label="集群" value={cluster} onChange={(e) => selectCluster(e.target.value)} className="w-44">
                <option value="">选择集群…</option>
                {scope.map((item) => (
                  <option key={item.name} value={item.name}>
                    {item.name}
                  </option>
                ))}
                <option value={GRANT_ALL}>全部集群（*）</option>
              </Select>
            </label>
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              实例
              <Select
                aria-label="实例"
                value={instance}
                onChange={(e) => setInstance(e.target.value)}
                disabled={!cluster || cluster === GRANT_ALL}
                className="w-44 disabled:opacity-60"
              >
                {cluster === GRANT_ALL ? (
                  <option value={GRANT_ALL}>全部实例（*）</option>
                ) : (
                  <>
                    <option value="">{cluster ? "选择实例…" : "先选择集群"}</option>
                    {instanceChoices.map((name) => (
                      <option key={name} value={name}>
                        {name}
                      </option>
                    ))}
                    {cluster && <option value={GRANT_ALL}>该集群全部实例（*）</option>}
                  </>
                )}
              </Select>
            </label>
          </div>
          {instancesError && <p className="text-xs text-destructive">{instancesError}</p>}
          {!instancesError && !loading && scope.length === 0 && (
            <p className="text-xs text-muted-foreground">Gateway 还没有登记过实例，Agent 接入后才能按集群/实例授权。</p>
          )}
          {scopeWarning && (
            <p role="alert" className="rounded-md bg-warning/15 px-2.5 py-2 text-xs text-warning">
              {scopeWarning}
            </p>
          )}
          <p className="text-xs text-muted-foreground">
            授权后用户可使用该 target 的全部运维能力（含 Agent 升级）和它绑定的凭据；平台管理员和凭据管理不随 grant 下发。
          </p>
          <div>
            <Button
              onClick={submit}
              disabled={busy || !validation.ok}
              variant="primary" size="sm"
            >
              {busy ? "授权中…" : "授权"}
            </Button>
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
                  <div className="flex justify-end">
                    <Button
                      onClick={() => remove(g.id)}
                      disabled={busy}
                      size="sm" className="text-destructive"
                    >
                      <TrashIcon width={12} height={12} /> 删除
                    </Button>
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
  const [userName, setUserName] = useState("")
  const [name, setName] = useState("")
  const [busy, setBusy] = useState(false)
  const busyRef = useRef(false)
  const [created, setCreated] = useState("")
  const [showRevoked, setShowRevoked] = useState(false)
  const [now, setNow] = useState(() => Date.now())
  const [toggled, setToggled] = useState<ReadonlySet<string>>(() => new Set())

  const groups = useMemo(
    () => groupTokensByUser(rows, users, { now, showRevoked }),
    [rows, users, now, showRevoked],
  )
  const revokedTotal = rows.filter((t) => t.revoked).length

  function toggleGroup(key: string) {
    setToggled((current) => {
      const next = new Set(current)
      if (next.has(key)) next.delete(key)
      else next.add(key)
      return next
    })
  }

  const load = useCallback(async () => {
    setLoading(true)
    setError("")
    try {
      const [t, u] = await Promise.all([listTokens(session), listUsers(session)])
      setRows(t)
      setUsers(u)
      setNow(Date.now())
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
    if (busyRef.current) return
    busyRef.current = true
    setBusy(true)
    setError("")
    setCreated("")
    try {
      const res = await createToken(session, {
        user: userName,
        name: name.trim() || undefined,
      })
      setCreated(res.token)
      setName("")
      await load()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      busyRef.current = false
      setBusy(false)
    }
  }

  async function revoke(id: string) {
    if (busyRef.current) return
    busyRef.current = true
    setBusy(true)
    setError("")
    try {
      await revokeToken(session, id)
      await load()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      busyRef.current = false
      setBusy(false)
    }
  }

  return (
    <div>
      <Toolbar
        title="令牌"
        desc={`用户访问令牌 · ${groups.length} 个用户 · 共 ${rows.length} 个，已撤销 ${revokedTotal} 个`}
        onRefresh={load}
        loading={loading}
      >
        <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <input type="checkbox" checked={showRevoked} onChange={(e) => setShowRevoked(e.target.checked)} />
          显示已撤销（{revokedTotal}）
        </label>
        <Button
          onClick={() => {
            setShowCreate((v) => !v)
            setCreated("")
          }}
          variant="primary" size="sm"
        >
          <PlusIcon width={14} height={14} /> 签发令牌
        </Button>
      </Toolbar>
      <ErrorBar msg={error} />

      {showCreate && (
        <div className="mx-4 mt-3 flex flex-col gap-3 rounded-lg border bg-muted/40 p-3">
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              所属用户
              <Select value={userName} onChange={(e) => setUserName(e.target.value)} className="w-40">
                <option value="">选择用户…</option>
                {users.map((u) => (
                  <option key={u.id} value={u.name}>
                    {u.name}
                  </option>
                ))}
              </Select>
            </label>
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              备注名
              <Input value={name} onChange={(e) => setName(e.target.value)} className="w-36" placeholder="可选" />
            </label>
            <Button
              onClick={submit}
              disabled={busy || !userName}
              variant="primary" size="sm"
            >
              {busy ? "签发中…" : "签发"}
            </Button>
          </div>
          {created && (
            <div className="flex items-center gap-2 rounded-lg border border-success/40 bg-success/10 p-2.5">
              <span className="text-xs text-muted-foreground">请立即复制（仅显示一次）：</span>
              <code className="flex-1 truncate font-mono text-xs text-foreground">{created}</code>
              <Button
                onClick={() => navigator.clipboard?.writeText(created)}
                size="sm"
              >
                <CopyIcon width={12} height={12} /> 复制
              </Button>
            </div>
          )}
        </div>
      )}

      <div className="overflow-x-auto p-4">
        <table className="w-full min-w-[900px] border-collapse text-sm">
          <thead>
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th className="px-3 py-2 font-medium">备注名</th>
              <th className="px-3 py-2 font-medium">前缀</th>
              <th className="px-3 py-2 font-medium">状态</th>
              <th className="px-3 py-2 font-medium">创建时间</th>
              <th className="px-3 py-2 font-medium">过期时间</th>
              <th className="px-3 py-2 font-medium text-right">操作</th>
            </tr>
          </thead>
          {groups.map((group) => {
            const collapsed = isTokenGroupCollapsed(group, toggled)
            return (
              <tbody key={group.key || "unknown"}>
                <tr className={`border-b bg-muted/40 ${group.userDisabled ? "opacity-60" : ""}`}>
                  <td colSpan={6} className="px-3 py-2">
                    <button
                      type="button"
                      aria-expanded={!collapsed}
                      onClick={() => toggleGroup(group.key)}
                      className="flex w-full items-center gap-2 text-left text-xs"
                    >
                      <span aria-hidden className="text-muted-foreground">{collapsed ? "▸" : "▾"}</span>
                      <span className="font-medium text-foreground">{group.userName}</span>
                      {group.userDisabled && <span className="text-warning">用户已停用</span>}
                      <span className="text-muted-foreground">
                        令牌 {group.total} 个 · 可用 {group.usable}
                        {group.revoked > 0 ? ` · 已撤销 ${group.revoked}` : ""}
                      </span>
                    </button>
                  </td>
                </tr>
                {!collapsed &&
                  group.rows.map(({ token: t, state }) => (
                    <tr key={t.id} className={`border-b border-border/50 ${state === "active" ? "" : "opacity-70"}`}>
                      <td className="px-3 py-2.5 pl-8 text-xs text-foreground">
                        {t.name || "—"}
                        {t.id === session.tokenId && <span className="ml-2 text-primary">当前登录</span>}
                      </td>
                      <td className="px-3 py-2.5 font-mono text-xs text-muted-foreground">{t.prefix}…</td>
                      <td className="px-3 py-2.5">
                        <span className={`whitespace-nowrap text-xs ${TOKEN_STATE_TONE[state]}`}>
                          {TOKEN_STATE_LABELS[state]}
                        </span>
                      </td>
                      <td className="px-3 py-2.5 text-xs text-muted-foreground">{fmtTime(t.created_at)}</td>
                      <td className="px-3 py-2.5 text-xs text-muted-foreground">
                        {gatewayTime(t.expires_at) === null ? "不过期" : fmtTime(t.expires_at)}
                      </td>
                      <td className="px-3 py-2.5">
                        <div className="flex justify-end">
                          {!t.revoked && (
                            <Button
                              onClick={() => revoke(t.id)}
                              disabled={busy}
                              size="sm" className="text-destructive"
                            >
                              <BanIcon width={12} height={12} /> 撤销
                            </Button>
                          )}
                        </div>
                      </td>
                    </tr>
                  ))}
              </tbody>
            )
          })}
          {!groups.length && !loading && (
            <tbody>
              <tr>
                <td colSpan={6} className="px-3 py-8 text-center text-sm text-muted-foreground">
                  {rows.length ? "没有未撤销的令牌，已撤销令牌已隐藏" : "暂无令牌"}
                </td>
              </tr>
            </tbody>
          )}
        </table>
      </div>
    </div>
  )
}

const TOKEN_STATE_TONE: Record<TokenState, string> = {
  active: "text-success",
  revoked: "text-destructive",
  user_disabled: "text-warning",
  user_missing: "text-warning",
  expired: "text-muted-foreground",
}
