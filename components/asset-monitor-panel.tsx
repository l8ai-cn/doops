"use client"

import { useEffect, useState, type FormEvent } from "react"
import { sessionIsAdmin, type Session, type Target } from "@/lib/client"
import { consoleFetch } from "@/lib/network-error"
import { Button, Input, Select } from "./ui"

const kinds: Record<string, string> = {
  machine: "机器",
  os: "操作系统",
  kernel: "内核",
  process: "进程",
  app: "应用",
  database: "数据库",
}
const metrics: Record<string, string> = {
  cpu: "CPU",
  memory: "内存",
  gpu: "GPU",
  network: "网络吞吐",
  requests: "请求速率",
  storage_total: "存储总量",
  storage: "存储已用",
  storage_available: "存储可用",
}
const units: Record<string, string> = {
  cpu: "cores",
  memory: "bytes",
  gpu: "%",
  network: "bytes/s",
  requests: "req/s",
  storage_total: "bytes",
  storage: "bytes",
  storage_available: "bytes",
}
const states: Record<string, string> = {
  missing: "缺数据",
  not_applicable: "不适用",
  open: "异常",
  recovered: "恢复",
  interrupted: "采集中断",
  reconfigured: "规则已更新",
}
type Asset = { type: string; asset_id: string; namespace: string; origin: string }
type Point = { at_ms: number; status: string; value: number | null }
type Rule = { threshold: number; duration_seconds: number }
type Alert = Rule & { opened_ms: number; ended_ms: number | null; state: string }
type Series = {
  device_id?: string
  metric: string
  unit: string
  latest: Point
  stale: boolean
  points: Point[]
  rule: Rule | null
  alerts: Alert[]
}
type Detail = { from_ms: number; to_ms: number; max_gap_ms: number; series: Series[] }
type MonitorRange = "1h" | "6h" | "24h" | "7d"
const rangeMs: Record<MonitorRange, number> = {
  "1h": 3600000,
  "6h": 21600000,
  "24h": 86400000,
  "7d": 604800000,
}
export type AssetMonitorSelection = Asset & {
  cluster: string
  instance: string
  cluster_id?: string
  metric: string
  device_id?: string
  range: MonitorRange
  end_ms: number
}
type Props = {
  session: Session
  targets: Target[]
  clusterId?: string
  initialSelection?: AssetMonitorSelection
}
const stamp = (at: number) => new Date(at).toLocaleString("zh-CN", { hour12: false })
const sourceName = (origin: string) => (origin === "k8s_top" ? "Agent · kubectl top" : "可信 exporter")
const assetKey = (a: Asset) => JSON.stringify([a.type, a.asset_id, a.namespace, a.origin])

function deviceLabel(metric: string, id?: string) {
  if (metric === "network") return id ? `网卡 ${id}` : "未指定网卡"
  if (metric === "storage" || metric === "storage_total" || metric === "storage_available")
    return id ? `挂载点 ${id}` : "未指定挂载点"
  return id ? `GPU UUID ${id}` : ""
}

async function request<T>(url: string, signal?: AbortSignal, body?: object): Promise<T> {
  const res = await consoleFetch(url, {
    cache: "no-store",
    signal,
    ...(body
      ? { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) }
      : {}),
  })
  if (!res.ok) {
    const data = await res.json()
    throw new Error(`${res.status} · ${data.message || data.error}`)
  }
  return res.json()
}
function useMonitorRead<T>(url: string, revision = 0) {
  const key = `${revision}:${url}`
  const [result, setResult] = useState<{ key: string; data?: T; error?: string }>()
  useEffect(() => {
    if (!url) return
    const ctrl = new AbortController()
    request<T>(url, ctrl.signal)
      .then((data) => {
        if (!ctrl.signal.aborted) setResult({ key, data })
      })
      .catch((err) => {
        if (!ctrl.signal.aborted) setResult({ key, error: String(err.message) })
      })
    return () => ctrl.abort()
  }, [key, url])
  return result?.key === key ? result : { key }
}
function History({ s, d }: { s: Series; d: Detail }) {
  let max = 1
  for (const p of s.points) if (p.value !== null) max = Math.max(p.value, max)
  const parts: string[] = []
  const dots: { x: number; y: number }[] = []
  let segment: string[] = [],
    last = 0,
    first = { x: 0, y: 0 }
  function finish() {
    if (segment.length > 0) dots.push(first)
    if (segment.length > 1) parts.push(segment.join(" "))
    segment = []
  }
  for (const p of s.points) {
    if (p.status !== "ok" || p.value === null) {
      finish()
      continue
    }
    if (p.at_ms - last > d.max_gap_ms) finish()
    const x = 4 + ((p.at_ms - d.from_ms) / (d.to_ms - d.from_ms)) * 392,
      y = 72 - (p.value / max) * 65
    if (!segment.length) first = { x, y }
    segment.push(`${x.toFixed(2)},${y.toFixed(2)}`)
    last = p.at_ms
  }
  finish()
  return (
    <div className="mt-2">
      <small className="text-muted-foreground">
        0–{max.toLocaleString()} {s.unit}
      </small>
      <svg
        viewBox="0 0 400 80"
        className="block w-full text-primary"
        role="img"
        aria-label={`${metrics[s.metric]} ${deviceLabel(s.metric, s.device_id)}历史曲线，${s.points.length} 个样本`}
      >
        <path d="M4 73 H396" stroke="currentColor" opacity=".2" />
        {parts.map((points, i) => (
          <polyline key={i} points={points} fill="none" stroke="currentColor" strokeWidth="1.5" />
        ))}
        {dots.map((p) => (
          <circle key={p.x} cx={p.x} cy={p.y} r="2" fill="currentColor" />
        ))}
      </svg>
      <small className="text-muted-foreground">
        {stamp(d.from_ms)} — {stamp(d.to_ms)} · {s.points.length} 个样本
      </small>
    </div>
  )
}
function MetricCard({
  metric,
  series: s,
  detail,
  admin,
  save,
}: {
  metric: string
  series?: Series
  detail: Detail
  admin: boolean
  save: (metric: string, device: string | undefined, rule: Rule) => Promise<void>
}) {
  const [busy, setBusy] = useState(false),
    [feedback, setFeedback] = useState("")
  async function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    const fields = new FormData(e.currentTarget)
    setBusy(true)
    setFeedback("")
    try {
      await save(metric, s?.device_id, {
        threshold: Number(fields.get("threshold")),
        duration_seconds: Number(fields.get("seconds")),
      })
      setFeedback("规则已保存")
    } catch (e) {
      setFeedback((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  return (
    <article className="min-w-0 rounded-lg border bg-card p-3">
      <h4 className="text-sm font-semibold">
        {metrics[metric]} <small className="text-muted-foreground">{s?.unit ?? units[metric]}</small>
      </h4>
      {s && deviceLabel(metric, s.device_id) && (
        <p className="break-all font-mono text-xs">
          {deviceLabel(metric, s.device_id)}
        </p>
      )}
      {!s ? (
        <p className="mt-2 text-sm text-muted-foreground">尚无采集样本</p>
      ) : (
        <>
          <p className="mt-1 text-sm">
            {s.latest.status === "ok"
              ? `${s.latest.value?.toLocaleString(undefined, { maximumSignificantDigits: 7 })} ${s.unit}`
              : states[s.latest.status]}
            {s.stale && " · 采样已过期"}
          </p>
          <small className="text-muted-foreground">最近观测 {stamp(s.latest.at_ms)}</small>
          <History s={s} d={detail} />
          {s.rule && (
            <p className="mt-2 text-xs">
              规则 · 高于 {s.rule.threshold} {s.unit}，持续 {s.rule.duration_seconds} 秒
            </p>
          )}
          {s.alerts.length > 0 && (
            <ul className="mt-2 space-y-1 text-xs" aria-label="告警记录">
              {s.alerts.map((a) => (
                <li key={a.opened_ms}>
                  {states[a.state]} · {stamp(a.opened_ms)}
                  {a.ended_ms && ` 至 ${stamp(a.ended_ms)}`} · &gt; {a.threshold} {s.unit} /{" "}
                  {a.duration_seconds}s
                </li>
              ))}
            </ul>
          )}
          {admin && (
            <details className="mt-2 text-sm">
              <summary className="cursor-pointer">设置告警规则</summary>
              <form className="mt-2 flex flex-wrap items-end gap-2" onSubmit={submit}>
                <label>
                  阈值（{s.unit}）
                  <Input
                    name="threshold"
                    type="number"
                    min="0"
                    max={metric === "gpu" ? 100 : undefined}
                    step="any"
                    required
                    defaultValue={s.rule?.threshold}
                  />
                </label>
                <label>
                  持续秒数
                  <Input
                    name="seconds"
                    type="number"
                    min="1"
                    max="604800"
                    step="1"
                    required
                    defaultValue={s.rule?.duration_seconds}
                  />
                </label>
                <Button disabled={busy} type="submit">
                  {busy ? "保存中" : "保存规则"}
                </Button>
              </form>
            </details>
          )}
          {feedback && <p role="status">{feedback}</p>}
        </>
      )}
    </article>
  )
}
function SourcePanel({
  session,
  target,
  clusterId,
}: {
  session: Session
  target: Target
  clusterId?: string
}) {
  const [kind, setKind] = useState(""),
    [selected, setSelected] = useState(""),
    [range, setRange] = useState("1h"),
    [revision, refresh] = useState(0)
  const [ns, setNs] = useState(""),
    [busy, setBusy] = useState(false),
    [notice, setNotice] = useState("")
  const route = {
    cluster: target.cluster,
    instance: target.instance,
    ...(clusterId ? { cluster_id: clusterId } : {}),
  }
  const endpoint = "/api/assets/monitor",
    listQuery = new URLSearchParams({ ...route, ...(kind ? { type: kind } : {}) })
  const catalog = useMonitorRead<{ assets: Asset[] }>(`${endpoint}?${listQuery}`, revision)
  const assets = catalog.data?.assets,
    asset = assets?.find((a) => assetKey(a) === selected) ?? assets?.[0]
  const detailUrl = asset ? `${endpoint}?${new URLSearchParams({ ...route, ...asset, range })}` : ""
  const detail = useMonitorRead<Detail>(detailUrl, revision)
  async function collect(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setNotice("")
    try {
      const res = await request<{ accepted: number }>(endpoint, undefined, {
        ...route,
        action: "collect",
        namespace: ns,
      })
      setNotice(`已完成观测，接收 ${res.accepted} 个指标样本`)
      refresh((x) => x + 1)
    } catch (e) {
      setNotice((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  async function save(metric: string, device_id: string | undefined, rule: Rule) {
    await request(endpoint, undefined, { ...route, ...asset, action: "rule", metric, device_id, ...rule })
    refresh((x) => x + 1)
    setNotice("规则已保存，从后续样本开始判定。")
  }
  return (
    <div className="space-y-4">
      <form onSubmit={collect} className="flex flex-wrap items-end gap-2">
        <label className="text-sm">
          K8s 命名空间
          <Input
            required
            value={ns}
            maxLength={63}
            pattern="[a-z0-9]([a-z0-9\-]*[a-z0-9])?"
            onChange={(e) => setNs(e.target.value)}
          />
        </label>
        <Button type="submit" disabled={busy || target.status === "offline"}>
          {busy ? "正在采集" : "采集 Pod CPU / 内存"}
        </Button>
        <Button type="button" onClick={() => refresh((x) => x + 1)}>
          刷新观测
        </Button>
      </form>
      {notice && (
        <p role="status" className="text-sm">
          {notice}
        </p>
      )}
      <div className="flex flex-wrap items-end gap-2 text-sm">
        <label>
          资产类型
          <Select
            value={kind}
            onChange={(e) => {
              setKind(e.target.value)
              setSelected("")
            }}
          >
            <option value="">全部类型</option>
            {Object.entries(kinds).map(([k, l]) => (
              <option key={k} value={k}>
                {l}
              </option>
            ))}
          </Select>
        </label>
        {asset && (
          <label>
            资产与来源
            <Select value={assetKey(asset)} onChange={(e) => setSelected(e.target.value)}>
              {assets?.map((a) => (
                <option value={assetKey(a)} key={assetKey(a)}>
                  {kinds[a.type]} · {a.namespace && a.namespace + "/"}
                  {a.asset_id} · {sourceName(a.origin)}
                </option>
              ))}
            </Select>
          </label>
        )}
        <label>
          时间范围
          <Select value={range} onChange={(e) => setRange(e.target.value)}>
            {["1h", "6h", "24h", "7d"].map((r) => (
              <option key={r}>{r}</option>
            ))}
          </Select>
        </label>
      </div>
      {catalog.error || detail.error ? (
        <p role="alert">{catalog.error || detail.error}</p>
      ) : !assets ? (
        <p role="status">正在读取授权来源…</p>
      ) : !asset ? (
        <p>当前来源和类型尚无监控样本。</p>
      ) : !detail.data ? (
        <p role="status">正在读取时间序列…</p>
      ) : (
        <>
          <p className="text-xs text-muted-foreground">
            {sourceName(asset.origin)} · {target.cluster}/{target.instance} ·{" "}
            {asset.origin === "k8s_top" ? "Gateway 观测时间" : "采集器观测时间"} · 缺样或间隔超过 5
            分钟时曲线断开
          </p>
          <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
            {Object.keys(metrics).flatMap((metric) => {
              const matches = detail.data!.series.filter((series) => series.metric === metric)
              const cards: Array<Series | undefined> = matches.length ? matches : [undefined]
              return cards.map((series) => (
                <MetricCard
                  key={JSON.stringify([assetKey(asset), metric, series?.device_id])}
                  metric={metric}
                  series={series}
                  detail={detail.data!}
                  admin={sessionIsAdmin(session)}
                  save={save}
                />
              ))
            })}
          </div>
        </>
      )}
    </div>
  )
}
function MonitorSources({ session, targets, clusterId }: Props) {
  const [choice, setChoice] = useState("")
  const clusters = useMonitorRead<{ clusters: { cluster: string; instance: string; asset_id: string }[] }>(
    clusterId ? "/api/assets/clusters" : "",
  )
  const available = clusterId
    ? targets.filter((t) =>
        clusters.data?.clusters.some(
          (s) => s.asset_id === clusterId && s.cluster === t.cluster && s.instance === t.instance,
        ),
      )
    : targets
  const key = (t: Target) => JSON.stringify([t.cluster, t.instance])
  const target = available.find((t) => key(t) === choice) ?? available[0]
  return (
    <section className="space-y-4 rounded-lg border bg-card p-4">
      <h3 className="font-semibold">资产监控</h3>
      {clusters.error ? (
        <p role="alert">{clusters.error}</p>
      ) : clusterId && !clusters.data ? (
        <p role="status">正在核对集群来源…</p>
      ) : !target ? (
        <p>当前范围没有授权来源。</p>
      ) : (
        <>
          <label className="block max-w-md text-sm">
            采集目标
            <Select value={key(target)} onChange={(e) => setChoice(e.target.value)}>
              {available.map((t) => (
                <option key={key(t)} value={key(t)}>
                  {t.cluster}/{t.instance}
                </option>
              ))}
            </Select>
          </label>
          <SourcePanel key={key(target)} target={target} session={session} clusterId={clusterId} />
        </>
      )}
    </section>
  )
}
type AlertState = "open" | "missing"
type Incident = Asset & {
  cluster: string
  instance: string
  connection: string
  metric: string
  device_id?: string
  reason: "threshold" | "missing" | "stale"
  opened_ms?: number
  last_sample_ms: number
}
type Summary = {
  open: number
  missing: number
  sources: number
  unmonitored_sources: number
  to_ms: number
  page_size: number
  items: Incident[]
}

function WindowSelect({ value, onChange }: { value: MonitorRange; onChange: (r: MonitorRange) => void }) {
  return (
    <label className="text-sm">
      曲线时间范围
      <Select value={value} onChange={(e) => onChange(e.target.value as MonitorRange)}>
        {Object.keys(rangeMs).map((r) => (
          <option key={r}>{r}</option>
        ))}
      </Select>
    </label>
  )
}
// Recheck scope and freshness when returning to the console. A visible duty
// summary also advances when a source quietly stops sending samples.
function useAlertRefresh() {
  const [revision, setRevision] = useState(0)
  useEffect(() => {
    const check = () => {
      if (document.visibilityState === "visible") setRevision((v) => v + 1)
    }
    const timer = window.setInterval(check, 60000)
    document.addEventListener("visibilitychange", check)
    return () => {
      window.clearInterval(timer)
      document.removeEventListener("visibilitychange", check)
    }
  }, [])
  return { revision, refresh: () => setRevision((v) => v + 1) }
}
type SummaryProps = { clusterId?: string; onOpen: (selection: AssetMonitorSelection) => void }
function DutySummary({ clusterId, onOpen }: SummaryProps) {
  const [state, setState] = useState<AlertState>(),
    [page, setPage] = useState(1),
    [range, setRange] = useState<MonitorRange>("1h")
  const { revision, refresh } = useAlertRefresh()
  const q = new URLSearchParams({ view: "alerts" })
  if (clusterId !== undefined) q.set("cluster_id", clusterId)
  if (state) {
    q.set("state", state)
    q.set("page", String(page))
  }
  const { data, error } = useMonitorRead<Summary>(`/api/assets/monitor?${q}`, revision)
  function select(next: AlertState) {
    setPage(1)
    setState((value) => (value === next ? undefined : next))
  }
  function open(item: Incident, end: number) {
    const { type, asset_id, namespace, origin, cluster, instance, metric, device_id } = item
    onOpen({
      type,
      asset_id,
      namespace,
      origin,
      cluster,
      instance,
      metric,
      device_id,
      cluster_id: clusterId,
      range,
      end_ms: end,
    })
  }
  return (
    <section className="card-surface space-y-3 p-4" aria-label="值班告警">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-semibold">值班告警</h3>
        <Button size="sm" onClick={refresh}>
          刷新告警
        </Button>
      </div>
      {error ? (
        <p role="alert">{error}</p>
      ) : !data ? (
        <p role="status">正在核对可见告警…</p>
      ) : (
        <>
          <div className="flex flex-wrap gap-2">
            {(["open", "missing"] as const).map((k) => (
              <Button key={k} aria-expanded={state === k} onClick={() => select(k)}>
                {k === "open" ? "未恢复异常" : "采集中断"} · {data[k]}
              </Button>
            ))}
          </div>
          <p className="text-xs text-muted-foreground">
            按可见指标序列计数 · {data.sources} 个授权来源，{data.unmonitored_sources} 个尚未接入监控 ·{" "}
            {stamp(data.to_ms)}
          </p>
          {state && (
            <>
              <div className="flex flex-wrap items-end gap-3">
                <WindowSelect value={range} onChange={setRange} />
                <small>
                  {state === "open" ? "当前仍超阈值的序列" : "当前缺数或超过 5 分钟未更新的适用序列"}
                </small>
              </div>
              {data.items.length === 0 ? (
                <p>本页没有匹配的告警。</p>
              ) : (
                <ul className="divide-y">
                  {data.items.map((item) => (
                    <li
                      key={JSON.stringify([
                        item.cluster,
                        item.instance,
                        assetKey(item),
                        item.metric,
                        item.device_id,
                      ])}
                      className="flex flex-wrap items-center justify-between gap-2 py-2"
                    >
                      <div className="min-w-0 break-all">
                        <strong>
                          {kinds[item.type]} · {item.asset_id} · {metrics[item.metric]}
                        </strong>{" "}
                        <code>{deviceLabel(item.metric, item.device_id)}</code>
                        <p className="text-xs text-muted-foreground">
                          {item.cluster}/{item.instance} · {sourceName(item.origin)}
                          {item.namespace && ` · ${item.namespace}`} ·{" "}
                          {item.connection === "online" ? "来源在线" : "来源离线"}
                        </p>
                        <p className="text-xs">
                          {item.reason === "threshold"
                            ? `持续超阈值 · ${stamp(item.opened_ms!)}`
                            : item.reason === "missing"
                              ? "采集器上报缺数"
                              : "采样已过期"}{" "}
                          · 最近观测 {stamp(item.last_sample_ms)}
                        </p>
                      </div>
                      <div className="flex flex-wrap gap-2">
                        <Button size="sm" onClick={() => open(item, data.to_ms)}>
                          查看 {range} 曲线
                        </Button>
                        {item.last_sample_ms < data.to_ms - rangeMs[range] && (
                          <Button size="sm" onClick={() => open(item, item.last_sample_ms)}>
                            最近样本前 {range}
                          </Button>
                        )}
                      </div>
                    </li>
                  ))}
                </ul>
              )}
              {(page > 1 || data[state] > data.page_size) && (
                <div className="flex items-center gap-2">
                  <Button disabled={page === 1} onClick={() => setPage((p) => p - 1)}>
                    上一页
                  </Button>
                  <small>第 {page} 页</small>
                  <Button
                    disabled={page * data.page_size >= data[state]}
                    onClick={() => setPage((p) => p + 1)}
                  >
                    下一页
                  </Button>
                </div>
              )}
            </>
          )}
        </>
      )}
    </section>
  )
}
export function AssetAlertSummary(props: SummaryProps) {
  return <DutySummary key={props.clusterId} {...props} />
}

function SelectedCurve({
  selection,
  clusterId,
  browse,
}: {
  selection: AssetMonitorSelection
  clusterId?: string
  browse: () => void
}) {
  const [range, setRange] = useState<MonitorRange>(selection.range)
  const { revision, refresh } = useAlertRefresh()
  const q = new URLSearchParams({
    cluster: selection.cluster,
    instance: selection.instance,
    type: selection.type,
    asset_id: selection.asset_id,
    namespace: selection.namespace,
    origin: selection.origin,
    metric: selection.metric,
    range,
    end_ms: String(selection.end_ms),
  })
  const cluster = clusterId ?? selection.cluster_id
  if (cluster !== undefined) q.set("cluster_id", cluster)
  q.set("device_id", selection.device_id ?? "")
  const { data, error } = useMonitorRead<Detail>(`/api/assets/monitor?${q}`, revision)
  return (
    <section className="card-surface space-y-3 p-4" aria-label="告警曲线">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="font-semibold">告警曲线</h3>
        <div className="flex gap-2">
          <Button size="sm" onClick={refresh}>
            刷新曲线
          </Button>
          <Button size="sm" onClick={browse}>
            浏览资产监控
          </Button>
        </div>
      </div>
      {error ? (
        <p role="alert">{error}</p>
      ) : !data ? (
        <p role="status">正在核对来源权限并读取历史…</p>
      ) : !data.series.length ? (
        <p>此时窗截止前尚无该指标的观测。</p>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-3">
            <WindowSelect value={range} onChange={setRange} />
            <p className="break-all text-sm">
              {kinds[selection.type]} · {selection.asset_id} · {selection.cluster}/{selection.instance} ·{" "}
              {sourceName(selection.origin)} {selection.namespace}
            </p>
          </div>
          {data.series.map((s) => (
            <article key={s.device_id ?? s.metric} className="max-w-3xl rounded-lg border p-3">
              <h4>
                {metrics[s.metric]} · {s.unit}
              </h4>
              {deviceLabel(s.metric, s.device_id) && <p className="break-all font-mono text-xs">{deviceLabel(s.metric, s.device_id)}</p>}
              <p className="text-sm">
                窗末最近观测 {stamp(s.latest.at_ms)} ·{" "}
                {s.latest.status === "ok" ? `${s.latest.value} ${s.unit}` : states[s.latest.status]}
                {s.stale ? " · 采样已过期" : ""}
              </p>
              <History s={s} d={data} />
              {!s.points.length && <p>所选时窗内没有样本。</p>}
            </article>
          ))}
        </>
      )}
    </section>
  )
}
function MonitorEntry(props: Props) {
  const [browse, setBrowse] = useState(false)
  if (props.initialSelection && !browse)
    return (
      <SelectedCurve
        selection={props.initialSelection}
        clusterId={props.clusterId}
        browse={() => setBrowse(true)}
      />
    )
  return <MonitorSources {...props} />
}
export function AssetMonitorPanel(props: Props) {
  if (props.session.demo) return <p>真实资产监控需要 Gateway 会话。</p>
  return (
    <MonitorEntry
      key={JSON.stringify([
        props.session.gateway,
        props.session.username,
        props.session.tokenId,
        props.session.issuedAt,
        props.session.isAdmin,
        props.clusterId,
        props.initialSelection,
      ])}
      {...props}
    />
  )
}
