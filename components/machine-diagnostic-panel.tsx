"use client"

import { useEffect, useState } from "react"
import { assetRequest } from "@/lib/assets"
import { Button, Input, Select } from "./ui"

type App = { system_id: string; namespace: string; application: string }
type Process = { pid: number; start_ticks: number; name: string; rss_bytes: number; deployment?: App }
type Reading = { at_ms: number; cpu_total_ticks?: number; cpu_idle_ticks?: number; memory_total_bytes?: number; memory_used_bytes?: number }
type Sample = Reading & { source: string; os: string; version: string; kernel: string; boot_id: string; boot_at: number; oom_kills: number | null; exited: number; processes: Process[] }
type Metric = { metric: string; device: string; at_ms: number; status: string; unit: string; value: number | null }
type Report = { from_ms: number; to_ms: number; sample: Sample | null; evidence_mode: "tunnel" | "mount"; generation?: number; comparison?: { previous?: Reading; status: string; cpu_percent?: number }; gap?: string; application?: App & { system_name: string; verified_at: string; databases: string[]; series: Metric[] } }
type Focus = { pid: number; start_ticks: number; at_ms: number }
const stamp = (ms: number) => new Date(ms).toLocaleString("zh-CN", { hour12: false })
const mib = (bytes: number) => (bytes / 1048576).toLocaleString("zh-CN", { maximumFractionDigits: 2 })

function ResourceComparison({ sample, comparison }: { sample: Sample; comparison: NonNullable<Report["comparison"]> }) {
  const description: Record<string, string> = {
    first_sample: "等待第二次采样。同一启动周期的两条读数可计算 CPU 使用率。",
    reboot: "两次取样之间宿主已重启，CPU 计数已重置。",
    gap: "两次取样间隔超过 5 分钟，采样断流。",
    unknown: "快照缺少 CPU 计数，区间使用率未知。",
    counter_discontinuity: "CPU 计数不连续，区间使用率未知。",
  }
  return <section aria-label="宿主 CPU 与内存" className="space-y-2">
    <h4 className="text-sm font-medium">宿主 CPU 与内存</h4>
    <p className="text-sm">{comparison.cpu_percent !== undefined ? `区间 CPU 使用率 ${comparison.cpu_percent.toFixed(1)}%` : description[comparison.status]}</p>
    {Date.now() - sample.at_ms > 300_000 && <p className="text-xs text-muted-foreground">最近样本距今超过 5 分钟，当前读数未知。</p>}
    <div className="overflow-x-auto"><table className="w-full text-left text-xs"><thead><tr><th>宿主采集时间</th><th>CPU 忙碌 / 总计（ticks）</th><th>内存已用 / 总量（MiB）</th></tr></thead><tbody>
      {(comparison.previous ? [comparison.previous, sample] : [sample]).map(r => <tr key={r.at_ms}><td>{stamp(r.at_ms)}</td><td>{r.cpu_total_ticks == null || r.cpu_idle_ticks == null ? "未知" : `${r.cpu_total_ticks - r.cpu_idle_ticks} / ${r.cpu_total_ticks}`}</td><td>{r.memory_used_bytes == null || r.memory_total_bytes == null ? "未知" : `${mib(r.memory_used_bytes)} / ${mib(r.memory_total_bytes)}`}</td></tr>)}
    </tbody></table></div>
  </section>
}

export function MachineDiagnosticPanel({ assetId, cluster, instance, isAdmin }: { isAdmin: boolean; assetId: string; cluster?: string; instance?: string }) {
  const [range, setRange] = useState("1h")
  const [end, setEnd] = useState<number>()
  const [focus, setFocus] = useState<Focus>()
  const [sort, setSort] = useState("memory")
  const [limit, setLimit] = useState(50)
  const [fingerprint, setFingerprint] = useState("")
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<{ text: string; failed?: boolean }>()
  const [revision, setRevision] = useState(0)
  const [response, setResponse] = useState<{ key: string; report?: Report; error?: string }>()
  const params = new URLSearchParams({ asset_id: assetId, cluster: cluster ?? "", instance: instance ?? "", range })
  if (end) params.set("to_ms", String(end))
  if (focus) for (const [k, v] of Object.entries(focus)) params.set(k, String(v))
  const query = params.toString(), key = `${revision}/${query}`
  useEffect(() => {
    if (!cluster || !instance) return
    let active = true
    assetRequest<Report>("machine-diagnostics", undefined, new URLSearchParams(query)).then(report => {
      if (active) setResponse({ key, report })
    }).catch(cause => { if (active) setResponse({ key, error: cause instanceof Error ? cause.message : String(cause) }) })
    return () => { active = false }
  }, [cluster, instance, query, key])
  const result = response?.key === key ? response : undefined
  const report = result?.report, s = report?.sample, a = report?.application
  function restart(value: string) { setRange(value); setFocus(undefined); setEnd(undefined); setLimit(50); setRevision(v => v + 1) }
  async function collect(action: "bind" | "refresh") {
    setBusy(true)
    setNotice(undefined)
    try {
      const receipt = await assetRequest<{ at_ms: number; new_sample: boolean }>("machine-diagnostics", {
        action, asset_id: assetId, cluster, instance, ...(action === "bind" ? { host_sha256: fingerprint } : {}),
      })
      setNotice({ text: action === "bind" ? "宿主指纹已核验并绑定。" : receipt.new_sample ? "已获取新样本。" : "宿主尚未产生更新的样本。" })
      setFingerprint("")
      restart(range)
    } catch (cause) {
      const detail = cause instanceof Error ? cause.message : String(cause)
      setNotice({ text: action === "bind" ? `核验并绑定失败，请核对宿主指纹和已领取目标。${detail}` : `从宿主取样失败。${detail}`, failed: true })
      setRevision(v => v + 1)
    } finally { setBusy(false) }
  }
  const rows = s ? (sort === "pid" ? [...s.processes].sort((x, y) => x.pid - y.pid) : s.processes) : []
  return <section aria-label="机器诊断" className="min-w-0 space-y-3 rounded-lg border p-3">
    <header className="flex flex-wrap items-center justify-between gap-2"><h4 className="font-semibold">{focus ? "所属应用诊断" : "宿主系统与进程"}</h4>
      {cluster && instance && <div className="flex flex-wrap items-center gap-2"><label className="text-xs">时间范围 <Select aria-label="诊断时间范围" value={range} onChange={ev => restart(ev.target.value)}>{["1h", "6h", "24h", "7d"].map(r => <option key={r}>{r}</option>)}</Select></label>
        {focus ? <Button size="sm" onClick={() => setFocus(undefined)}>返回进程列表</Button> : <Button size="sm" onClick={() => restart(range)}>重读记录</Button>}</div>}</header>
    {notice && <p role={notice.failed ? "alert" : "status"} className={notice.failed ? "status-error" : "text-sm"}>{notice.text}</p>}
    {!cluster || !instance ? <p className="text-sm text-muted-foreground">机器尚未绑定 Agent 目标。</p> : result?.error ? <p role="alert" className="status-error">{result.error}</p> : !report ? <p role="status" className="text-sm">正在读取诊断…</p> : <>
      <p className="break-all text-xs text-muted-foreground">{cluster}/{instance} · {assetId} · {stamp(report.from_ms)} 至 {stamp(report.to_ms)}</p>
      {!focus && report.evidence_mode === "tunnel" && (report.gap === "binding_missing" ? isAdmin && <form className="space-y-2" onSubmit={ev => { ev.preventDefault(); void collect("bind") }}>
        <label className="block text-sm">已知宿主指纹（SHA-256）<Input value={fingerprint} autoComplete="off" spellCheck={false} maxLength={64} onChange={ev => setFingerprint(ev.target.value.trim().toLowerCase())} /></label>
        <p className="text-xs text-muted-foreground">从宿主管理控制台或已核验的资产记录取得指纹。</p>
        <Button type="submit" size="sm" disabled={busy || !/^[a-f0-9]{64}$/.test(fingerprint)}>{busy ? "核验中…" : "核验并绑定"}</Button>
      </form> : <Button size="sm" disabled={busy} onClick={() => void collect("refresh")}>{busy ? "取样中…" : "从宿主取样"}</Button>)}
      {!s ? <p role="status">{report.gap === "binding_missing" ? "尚未核验该机器的宿主指纹。" : "所选窗口内没有可用宿主样本。"}</p> : <>
        <p className="text-xs text-muted-foreground">{report.evidence_mode === "mount" ? "只读挂载" : `隧道样本 · 连接代次 ${report.generation}`} · {s.source} · {stamp(s.at_ms)}</p>
        {a ? <><h5 className="break-all font-semibold">{a.application} · {a.namespace}</h5><p className="break-all text-sm">{a.system_name}（{a.system_id}） · PID {focus?.pid} · 部署核验于 {stamp(Date.parse(a.verified_at))}</p>
          <p className="break-all text-sm">依赖数据库 · {a.databases.join("、") || "未登记"}</p><h5 className="text-sm font-semibold">应用指标 · 窗口内最近 exporter 采样</h5>
          {a.series.length ? <ul className="space-y-1 text-sm">{a.series.map(m => <li key={`${m.metric}/${m.device}`}>{m.metric} {m.device} · {m.value == null ? (m.status === "not_applicable" ? "不适用" : "缺数据") : `${m.value} ${m.unit}`} · {stamp(m.at_ms)}</li>)}</ul> : <p className="text-sm text-muted-foreground">本窗口没有该应用的指标样本。</p>}</> : <>
          {report.comparison && <ResourceComparison sample={s} comparison={report.comparison} />}
          <dl className="grid gap-3 text-sm sm:grid-cols-2">{[["操作系统", `${s.os} ${s.version}`], ["运行内核", s.kernel], ["最近启动", stamp(s.boot_at * 1000)], ["OOM 杀进程累计（本次启动）", s.oom_kills ?? "内核未提供计数"]].map(([label, value]) => <div key={label}><dt className="text-xs text-muted-foreground">{label}</dt><dd className="break-all">{value}</dd></div>)}</dl>
          <div className="flex flex-wrap items-center justify-between gap-2"><h5 className="text-sm font-semibold">进程 · {rows.length} 项</h5><Select aria-label="进程排序" value={sort} onChange={ev => setSort(ev.target.value)}><option value="memory">常驻内存从高到低</option><option value="pid">PID 从小到大</option></Select></div>
          {s.exited > 0 && <small>采样期间另有 {s.exited} 个 PID 退出或被复用。</small>}
          <div className="overflow-x-auto"><table className="w-full text-left text-sm"><thead><tr>{["PID", "进程", "RSS (MiB)", "所属应用"].map(v => <th key={v} scope="col" className="p-1">{v}</th>)}</tr></thead><tbody className="divide-y">{rows.slice(0, limit).map(p => <tr key={p.pid}><td className="p-1">{p.pid}</td><td className="break-all p-1">{p.name}</td><td className="p-1">{mib(p.rss_bytes)}</td><td className="p-1">{p.deployment ? <button type="button" className="break-all text-left text-primary underline" onClick={() => { setEnd(report.to_ms); setFocus({ pid: p.pid, start_ticks: p.start_ticks, at_ms: s.at_ms }) }}>{p.deployment.application}</button> : "未核验"}</td></tr>)}</tbody></table></div>
          {rows.length > limit && <Button size="sm" onClick={() => setLimit(v => v + 50)}>显示更多进程</Button>}</>}
      </>}
    </>}
  </section>
}
