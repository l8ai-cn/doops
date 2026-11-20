"use client"

import { useEffect, useState } from "react"
import { sessionIsAdmin, type Session, type Target } from "@/lib/client"
import { consoleFetch } from "@/lib/network-error"
import { Button, Input, Select } from "./ui"

type Asset = { asset_id: string; cluster: string; instance: string; cluster_id: string }
type Plan = {
  overdue: number
  executor_disabled: number
  plan_id: number
  name: string
  owner: string
  executor: string
  active: number
  next_due: number
  timezone: string
  weekday: number
  clock: string
  check_count: number
}
type Run = {
  trigger_kind: string
  run_id: number
  name: string
  due: number
  checked: number
  planned: number
  gaps: number
}
type Overview = { plans: Plan[]; runs: Run[]; assets: Asset[]; checks: Record<string, string> }
type Item = Asset & {
  item_id: number
  check_id: string
  status: string
  reason: string
  repair: string
  repair_by: string
  generation: number
}
type Receipt = {
  source: string
  evidence_category: string
  item_id: number
  generation: number
  status: string
  actor: string
  received: number
  evidence: unknown
  reason: string
  digest: string
}
type Report = {
  run: Run & { owner: string; executor: string }
  items: Item[]
  history: Receipt[]
  checks: Record<string, string>
}
type Preview = {
  scope: (Asset & { checks: string[]; authorized: boolean; online: boolean })[]
  next_due: number
}
const kinds: Record<string, string> = { manual: "手动", scheduled: "定时", reconciled: "漏期对账" }
const categories: Record<string, string> = {
  collector_summary: "受信采集方摘要",
  offline_advisory_comparison: "部署清单与公告的离线版本比对",
  collector_gateway_metric: "执行账号提交的 Gateway 指标",
  gap_record: "缺口记录",
}
const states: Record<string, string> = {
  pending: "待取证",
  missing: "漏检",
  insufficient: "证据不足",
  pass: "通过",
  finding: "发现问题",
}
const reasons: Record<string, string> = {
  executor_disabled: "执行账号已停用，周期已过期",
  target_offline: "Agent 离线",
  executor_grant_revoked: "执行账号已撤权",
  asset_binding_changed: "资产绑定已变更",
  scheduler_missed_window: "调度错过本期窗口",
  collection_deadline_elapsed: "到期未收到证据",
  no_registered_finding_baseline: "尚无已登记的修复基线",
  unsupported_or_ambiguous_version: "版本格式无法比较",
}
const date = (v: number) => new Date(v * 1000).toLocaleString("zh-CN", { hour12: false })
const assetKey = (a: Asset) => JSON.stringify([a.cluster, a.instance, a.asset_id])
async function request<T>(query: URLSearchParams, body?: object, signal?: AbortSignal): Promise<T> {
  const response = await consoleFetch(`/api/assets/inspections?${query}`, {
    method: body ? "POST" : "GET",
    cache: "no-store",
    signal,
    ...(body ? { headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) } : {}),
  })
  const value = await response.json()
  if (!response.ok) throw new Error(value.message || value.error || `HTTP ${response.status}`)
  return value
}
export { AssetWeeklySummary } from "./asset-weekly-summary"

export function AssetInspectionPanel(props: { session: Session; targets: Target[]; clusterId?: string; initialRunId?: number }) {
  const { session, clusterId } = props
  return (
    <InspectionWorkspace
      key={JSON.stringify([
        session.gateway,
        session.tokenId,
        session.username,
        session.isAdmin,
        session.token,
        clusterId,
        props.initialRunId,
      ])}
      {...props}
    />
  )
}
function InspectionWorkspace({
  session,
  targets,
  clusterId,
  initialRunId = 0,
}: {
  session: Session
  targets: Target[]
  clusterId?: string
  initialRunId?: number
}) {
  const admin = sessionIsAdmin(session) && !session.demo
  const [data, setData] = useState<Overview | null>(null),
    [report, setReport] = useState<Report | null>(null),
    [run, setRun] = useState(initialRunId),
    [revision, reload] = useState(0)
  const [error, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [create, setCreate] = useState(false),
    [memo, setMemo] = useState<Record<number, string>>({})
  const [preview, setPreview] = useState<Preview | null>(null),
    [checks, setChecks] = useState<string[]>([]),
    [selection, setSelection] = useState<string[]>([])
  const [draft, setDraft] = useState({
    name: "",
    owner: "",
    executor: "",
    weekday: 1,
    clock: "09:00",
    timezone: "Asia/Shanghai",
  })
  const query = (view: string, id?: number) =>
    new URLSearchParams({
      view,
      ...(clusterId ? { cluster_id: clusterId } : {}),
      ...(id ? { run_id: String(id) } : {}),
    })
  useEffect(() => {
    const ctrl = new AbortController()
    setData(null)
    setReport(null)
    setError("")
    if (session.demo) return
    const params = new URLSearchParams(clusterId ? { cluster_id: clusterId } : {})
    Promise.all([
      request<Overview>(params, undefined, ctrl.signal),
      run
        ? request<Report>(
            new URLSearchParams([...params, ["view", "report"], ["run_id", String(run)]]),
            undefined,
            ctrl.signal,
          )
        : null,
    ])
      .then(([overview, detail]) => {
        if (!ctrl.signal.aborted) {
          setData(overview)
          setReport(detail)
        }
      })
      .catch((e) => {
        if (!ctrl.signal.aborted) setError(e.message)
      })
    return () => ctrl.abort()
  }, [clusterId, run, revision, session.demo])
  async function action(body: object) {
    if (busy) return
    setBusy(true)
    setError("")
    try {
      await request(new URLSearchParams(), body)
      reload((v) => v + 1)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  async function plan(save: boolean) {
    if (!data || busy) return
    setBusy(true)
    setError("")
    try {
      const result = await request<Preview>(new URLSearchParams(), {
        ...draft,
        action: save ? "create" : "preview",
        scope: data.assets
          .filter((a) => selection.includes(assetKey(a)))
          .map(({ asset_id, cluster, instance }) => ({ asset_id, cluster, instance, checks })),
      })
      if (save) {
        setCreate(false)
        setPreview(null)
        reload((v) => v + 1)
      } else setPreview(result)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  async function exportReport() {
    setBusy(true)
    setError("")
    try {
      const result = await request<Report>(query("download", run))
      const href = URL.createObjectURL(
        new Blob([JSON.stringify(result, null, 2)], { type: "application/json" }),
      )
      const a = document.createElement("a")
      a.href = href
      a.download = `inspection-${run}.json`
      a.click()
      setTimeout(() => URL.revokeObjectURL(href), 1000)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  const flip = (list: string[], key: string) =>
    list.includes(key) ? list.filter((v) => v !== key) : [...list, key]
  if (session.demo)
    return (
      <section className="card-surface p-4">
        <h2>每周巡检</h2>
        <p>登录 Gateway 后可读取真实计划和采样证据。</p>
      </section>
    )
  return (
    <section aria-label="每周巡检" className="card-surface min-w-0 space-y-4 p-4">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-xl font-semibold">{run ? `巡检报告 #${run}` : "每周巡检"}</h2>
        <div className="flex flex-wrap gap-2">
          {run > 0 && <Button onClick={() => setRun(0)}>返回计划与运行</Button>}
          <Button disabled={busy} onClick={() => reload((v) => v + 1)}>
            刷新
          </Button>
          {!run && admin && (
            <Button
              variant="primary"
              onClick={() => {
                setCreate(!create)
                setPreview(null)
              }}
            >
              {create ? "关闭表单" : "新建周检计划"}
            </Button>
          )}
        </div>
      </header>
      {error && (
        <p role="alert" className="status-error">
          {error}
        </p>
      )}
      {!data && !error && <p role="status">正在读取巡检记录…</p>}
      {data && !run && (
        <>
          {create && admin && (
            <form
              className="space-y-3 rounded-lg border p-3"
              onChange={() => setPreview(null)}
              onSubmit={(e) => {
                e.preventDefault()
                void plan(!!preview)
              }}
            >
              <fieldset disabled={busy} className="space-y-3">
                <legend className="font-semibold">每周计划</legend>
                <div className="grid gap-2 sm:grid-cols-3">
                  {(
                    [
                      ["name", "计划名称"],
                      ["owner", "负责人"],
                      ["executor", "执行账号用户 ID"],
                      ["timezone", "IANA 时区"],
                      ["clock", "执行时间"],
                    ] as const
                  ).map(([key, label]) => (
                    <label key={key}>
                      {label}
                      <Input
                        required
                        type={key === "clock" ? "time" : "text"}
                        value={draft[key]}
                        onChange={(e) => setDraft({ ...draft, [key]: e.target.value })}
                      />
                    </label>
                  ))}
                  <label>
                    执行日
                    <Select
                      value={draft.weekday}
                      onChange={(e) => setDraft({ ...draft, weekday: Number(e.target.value) })}
                    >
                      {["日", "一", "二", "三", "四", "五", "六"].map((d, i) => (
                        <option key={d} value={i}>
                          周{d}
                        </option>
                      ))}
                    </Select>
                  </label>
                </div>
                <fieldset>
                  <legend>资产与目标</legend>
                  {!data.assets.length && <p>没有已绑定目标的可见资产。</p>}
                  <div className="flex flex-wrap gap-3">
                    {data.assets.map((a) => (
                      <label key={assetKey(a)}>
                        <input
                          type="checkbox"
                          checked={selection.includes(assetKey(a))}
                          onChange={() => setSelection(flip(selection, assetKey(a)))}
                        />{" "}
                        {a.asset_id} · {a.cluster}/{a.instance}
                        {targets.some(
                          (t) =>
                            t.cluster === a.cluster && t.instance === a.instance && t.status === "offline",
                        )
                          ? " · 离线"
                          : ""}
                      </label>
                    ))}
                  </div>
                </fieldset>
                <fieldset>
                  <legend>每个所选资产的应检项</legend>
                  <div className="flex flex-wrap gap-3">
                    {Object.entries(data.checks)
                      .sort()
                      .map(([id, label]) => (
                        <label key={id}>
                          <input
                            type="checkbox"
                            checked={checks.includes(id)}
                            onChange={() => setChecks(flip(checks, id))}
                          />{" "}
                          {label}
                        </label>
                      ))}
                  </div>
                </fieldset>
                {preview && (
                  <div role="status">
                    下次到期 {date(preview.next_due)}
                    {preview.scope.map((s) => (
                      <p key={assetKey(s)}>
                        {s.asset_id} · {s.checks.length} 项 · {s.authorized ? "已授权" : "缺少执行授权"} ·{" "}
                        {s.online ? "在线" : "离线"}
                      </p>
                    ))}
                  </div>
                )}
                <Button
                  variant="primary"
                  disabled={!selection.length || !checks.length || preview?.scope.some((s) => !s.authorized)}
                >
                  {preview ? "保存计划" : "预览范围与授权"}
                </Button>
              </fieldset>
            </form>
          )}
          <h3 className="font-semibold">计划</h3>
          {!data.plans.length && <p>当前范围暂无计划。</p>}
          {data.plans.map((p) => (
            <article className="rounded-lg border p-3" key={p.plan_id}>
              <strong>{p.name}</strong>
              <p>
                {p.owner} · 执行账号 {p.executor} · 每周{"日一二三四五六"[p.weekday]} {p.clock} {p.timezone}
              </p>
              <p>
                {p.check_count} 项 · {p.active ? `下次 ${date(p.next_due)}` : "已暂停"}
              </p>
              {admin && !!p.overdue && (
                <div role="status" className="rounded border p-2">
                  <p>
                    未对账漏期 · {date(p.next_due)}
                    {p.executor_disabled ? " · 执行账号已停用" : ""}。本期取证窗口已结束。
                  </p>
                  <Button
                    disabled={busy}
                    onClick={() => void action({ action: "reconcile", plan_id: p.plan_id, due: p.next_due })}
                  >
                    冻结本期漏检清单
                  </Button>
                </div>
              )}
              {admin && (
                <div className="flex gap-2">
                  <Button
                    disabled={busy || !p.active}
                    onClick={() => void action({ action: "run", plan_id: p.plan_id })}
                  >
                    执行本周巡检
                  </Button>
                  <Button
                    disabled={busy}
                    onClick={() => void action({ action: p.active ? "pause" : "resume", plan_id: p.plan_id })}
                  >
                    {p.active ? "暂停" : "恢复"}
                  </Button>
                </div>
              )}
            </article>
          ))}
          <h3 className="font-semibold">运行与报告</h3>
          {!data.runs.length && <p>尚未生成周期运行。</p>}
          {data.runs.map((r) => (
            <div key={r.run_id} className="flex flex-wrap items-center justify-between gap-2 border-b py-2">
              <span>
                {r.name} · {kinds[r.trigger_kind]} · {date(r.due)} · 已取证 {r.checked}/{r.planned} 项 · 缺口{" "}
                {r.gaps} 项
              </span>
              <Button onClick={() => setRun(r.run_id)}>查看报告 #{r.run_id}</Button>
            </div>
          ))}
        </>
      )}
      {report && (
        <>
          <div className="flex flex-wrap justify-between gap-2">
            <p>
              {clusterId ? `${clusterId} · ` : ""}{kinds[report.run.trigger_kind]} · {report.run.name} · 负责人 {report.run.owner} · 执行账号 {report.run.executor}
            </p>
            <Button disabled={busy} onClick={() => void exportReport()}>
              下载报告 JSON
            </Button>
          </div>
          {report.items.map((i) => (
            <article key={i.item_id} className="space-y-2 rounded-lg border p-3">
              <strong>
                {report.checks[i.check_id]} · {states[i.status]}
              </strong>
              <p>
                {i.asset_id} · {i.cluster}/{i.instance} · 第 {i.generation} 次
              </p>
              <p>{reasons[i.reason] || i.reason}</p>
              {report.history
                .filter((h) => h.item_id === i.item_id)
                .map((h) => (
                  <details key={h.generation}>
                    <summary>
                      第 {h.generation} 次证据 · {states[h.status]} · {date(h.received)}
                    </summary>
                    <p>
                      {categories[h.evidence_category]} · {h.source || "巡检台账"} · 提交人 {h.actor} ·{" "}
                      {h.reason}
                    </p>
                    <pre className="whitespace-pre-wrap break-all text-xs">
                      {JSON.stringify(h.evidence, null, 2)}
                    </pre>
                  </details>
                ))}
              {i.repair && (
                <p>
                  整改登记 · {i.repair_by} · {i.repair}
                </p>
              )}
              {((i.status === "finding" && !i.repair) || (admin && i.status === "pending")) && (
                <label>
                  处理说明
                  <Input
                    value={memo[i.item_id] || ""}
                    onChange={(e) => setMemo({ ...memo, [i.item_id]: e.target.value })}
                  />
                </label>
              )}
              <div className="flex flex-wrap gap-2">
                {[
                  { op: "repair", name: "登记修复", show: i.status === "finding" && !i.repair },
                  { op: "gap", name: "登记缺口", show: admin && i.status === "pending" },
                  {
                    op: "recheck",
                    name: "提交复检",
                    show: i.repair && ["finding", "insufficient"].includes(i.status),
                  },
                  {
                    op: "makeup",
                    name: "补检",
                    show: admin && ["missing", "insufficient"].includes(i.status),
                  },
                ]
                  .filter((a) => a.show)
                  .map((a) => (
                    <Button
                      key={a.op}
                      disabled={busy || (["repair", "gap"].includes(a.op) && !memo[i.item_id]?.trim())}
                      onClick={() =>
                        void action({
                          action: a.op,
                          item_id: i.item_id,
                          generation: i.generation,
                          memo: memo[i.item_id] || "",
                        })
                      }
                    >
                      {a.name}
                    </Button>
                  ))}
              </div>
            </article>
          ))}
        </>
      )}
    </section>
  )
}
