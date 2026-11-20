"use client"

import { useRef, useState } from "react"
import { assetRequest } from "@/lib/assets"
import { consoleFetch } from "@/lib/network-error"
import { Button, Input, Select } from "./ui"

const metadata = "machine_metadata"
const withLogins = "machine_and_credentials"
const header = "asset_id,name,kind,address,cpu_count,memory_gib,disk_gib,host_asset_id"
const statusLabel = { new: "可新增", imported: "已新增", duplicate: "已有", error: "行错误" }
const loginStatusLabel: Record<string, string> = { pending: "待保存", saved: "已保存", existing: "已保存过", duplicate: "批内重复", error: "需修正" }
const loginErrorLabel: Record<string, string> = {
  machine_not_registered: "资产编号尚未登记，请先登记机器或填写新机器的名称和类型。",
  invalid_machine_username: "登录账号为空或首尾有空格，请核对。",
  machine_login_required: "缺少登录密码。",
  machine_login_conflict: "同一机器、同一账号的密码有冲突，本批不会覆盖已有密码。",
}
type CsvResult = {
  scope: string
  credential_result: { status: string; saved: number; expected?: number; existing?: number }
  rows: { line: number; asset_id: string; status: keyof typeof statusLabel; code: string; message: string; credential_status?: string }[]
  batch_id: string
  preview_token?: string
}
type Receipt = { action: "preview" | "import"; result: CsvResult }
type Machine = { asset_id: string; name: string; address?: string }

function checkReceipt(result: CsvResult, action: Receipt["action"], scope?: string) {
  const cred = result.credential_result
  if (!Array.isArray(result.rows) || !result.batch_id || !cred || (scope && result.scope !== scope)) throw new Error("批次回执不完整，请重新查询。")
  if (result.scope === metadata && cred.status === "not_imported" && cred.saved === 0) return
  if (result.scope === withLogins && cred.status === (action === "preview" ? "preview" : "saved") &&
      [cred.saved, cred.expected, cred.existing].every(n => typeof n === "number" && Number.isSafeInteger(n) && n >= 0) &&
      (action === "preview" ? cred.saved === 0 : cred.saved === cred.expected)) return
  throw new Error("凭据保存结果与本批次不一致，请核对批次回执。")
}

export function MachineCsvImport({ machines, machine, onClose, onReadback }: {
  machines: Machine[]; machine?: Machine; onClose: () => void; onReadback: () => void
}) {
  const [scope, setScope] = useState(machine ? withLogins : "")
  const [file, setFile] = useState<File | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)
  const username = useRef<HTMLInputElement>(null)
  const password = useRef<HTMLInputElement>(null)
  const [receipt, setReceipt] = useState<Receipt | null>(null)
  const [error, setError] = useState("")
  const [busy, setBusy] = useState<Receipt["action"] | "lookup" | null>(null)
  const [batchId, setBatchId] = useState("")
  const [skipped, setSkipped] = useState<number[]>([])
  const rows = receipt?.result.rows ?? []
  const counts = { new: 0, imported: 0, duplicate: 0, error: 0 }
  for (const row of rows) counts[row.status]++
  const loginMode = scope === withLogins
  const credentials = receipt?.result.credential_result
  const changed = new Set(rows.filter(row => row.status === "new" || row.status === "imported").map(row => row.asset_id))
  const existingMachines = new Set(rows.filter(row => row.status === "duplicate" && !changed.has(row.asset_id)).map(row => row.asset_id))
  const pending = receipt?.action === "preview" && (receipt.result.scope === withLogins
    ? !counts.error && (credentials?.expected ?? 0) > 0 : counts.new > 0)

  function resetResult() { setReceipt(null); setError(""); setSkipped([]) }
  async function readCsv() {
    if (machine) {
      const user = username.current?.value ?? "", secret = password.current?.value ?? ""
      if (!user || !secret) throw new Error("请填写登录账号和密码。")
      const fields = [machine.asset_id, user, secret].map(value => `"${value.replaceAll('"', '""')}"`)
      return `asset_id,username,password\r\n${fields.join(",")}\r\n`
    }
    if (!file) throw new Error("请选择要导入的文件。")
    if (loginMode && /\.xlsx$/i.test(file.name)) {
      const { machineLoginWorkbookCsv } = await import("@/lib/machine-login-workbook")
      const parsed = await machineLoginWorkbookCsv(file, machines)
      setSkipped(parsed.skippedRows)
      return parsed.csv
    }
    if (file.size > 1024 * 1024 + 3) throw new Error("单个 CSV 文件最大为 1 MiB。")
    return new TextDecoder("utf-8", { fatal: true }).decode(await file.arrayBuffer())
  }

  async function run(action: Receipt["action"]) {
    if (busy || !scope || (action === "import" && !pending)) return
    const token = receipt?.result.preview_token
    setBusy(action); setError(""); setReceipt(null)
    try {
      const csv = await readCsv()
      const response = await consoleFetch("/api/assets/machines", { method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action, scope, csv, preview_token: action === "import" ? token : undefined }) })
      const result = await response.json()
      if (!response.ok) throw new Error(result.message || result.error || `请求失败（HTTP ${response.status}）`)
      checkReceipt(result, action, scope)
      if (action === "preview" && !/^[a-f0-9]{64}$/.test(result.preview_token ?? "")) throw new Error("预览凭证无效，请重新预览。")
      setReceipt({ action, result }); setBatchId(result.batch_id)
      if (action === "import") {
        if (password.current) password.current.value = ""
        if (fileInput.current) fileInput.current.value = ""
        setFile(null)
      }
    } catch (cause) { setError(cause instanceof Error ? cause.message : "导入请求失败") }
    finally { if (action === "import") onReadback(); setBusy(null) }
  }
  async function lookup() {
    if (busy) return
    setBusy("lookup"); resetResult()
    try {
      const result = await assetRequest<CsvResult>("machines", undefined, new URLSearchParams({ batch_id: batchId.trim() }))
      checkReceipt(result, "import"); setReceipt({ action: "import", result }); onReadback()
    } catch (cause) { setError(cause instanceof Error ? cause.message : "获取批次回执失败") }
    finally { setBusy(null) }
  }

  return <div className="space-y-4">
    <div className="flex flex-wrap items-center justify-between gap-2"><h3 className="font-semibold">{machine ? `添加登录凭据 · ${machine.name}` : "机器导入"}</h3>
      {!machine && scope && <a className="btn btn-sm" download={loginMode ? "machine-logins.csv" : "machine-assets.csv"}
        href={`data:text/csv;charset=utf-8,${encodeURIComponent((loginMode ? "asset_id,username,password" : header) + "\r\n")}`}>下载 CSV 模板</a>}
    </div>
    {machine ? <div className="grid max-w-xl gap-3 sm:grid-cols-2">
      <label className="grid gap-1.5 text-sm">登录账号<Input ref={username} disabled={!!busy} autoComplete="off" onChange={resetResult} /></label>
      <label className="grid gap-1.5 text-sm">登录密码<Input ref={password} disabled={!!busy} type="password" autoComplete="new-password" onChange={resetResult} /></label>
    </div> : <>
      <label className="grid max-w-xl gap-1.5 text-sm">导入内容<Select value={scope} disabled={!!busy} onChange={(e) => { setScope(e.target.value); setFile(null); resetResult() }}>
        <option value="">请选择要导入的内容</option><option value={metadata}>机器台账</option><option value={withLogins}>机器和登录凭据</option>
      </Select></label>
      {scope && <label className="grid max-w-xl gap-1.5 text-sm">{loginMode ? "选择 XLSX 或 UTF-8 CSV 文件" : "选择 UTF-8 CSV 文件"}
        <input key={scope} ref={fileInput} type="file" accept={loginMode ? ".xlsx,.csv" : ".csv"} disabled={!!busy} onChange={e => { setFile(e.target.files?.[0] ?? null); resetResult() }} />
      </label>}
      {loginMode && <p className="text-xs text-muted-foreground">已有机器可用资产编号、账号、密码三列补录。Excel 也可用管理 IP 匹配已有机器。新增机器的 CSV 还需填写 name、kind。</p>}
    </>}
    {skipped.length > 0 && <details className="text-sm"><summary>Excel 中有 {skipped.length} 个未匹配的源行，将跳过</summary><p className="break-words">行号 {skipped.join("、")}</p></details>}
    {error && <p role="alert" className="status-error">{error}</p>}
    {receipt && credentials && <div className="space-y-2"><div role="status" className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm">
      <h4 className="font-semibold">{receipt.action === "preview" ? "导入预览" : "导入结果"}</h4>
      {receipt.result.scope === withLogins ? <span>机器{receipt.action === "preview" ? "可新增" : "已新增"} {changed.size} 台 · 已登记 {existingMachines.size} 台 · 行错误 {counts.error}</span>
        : <span>{receipt.action === "preview" ? `可新增 ${counts.new}` : `已新增 ${counts.imported}`} · 重复跳过 {counts.duplicate} · 行错误 {counts.error}</span>}
      {receipt.result.scope === withLogins ? <span>{receipt.action === "preview" ? `待保存登录凭据 ${credentials.expected}` : `已保存登录凭据 ${credentials.saved}`} 条 · 已有 {credentials.existing} 条</span> : <span>本批保存登录凭据 0 条</span>}
      <span className="break-all font-mono text-xs text-muted-foreground">批次号 {receipt.result.batch_id}</span>
    </div>
    <div className="max-h-72 overflow-auto rounded-md border"><table className="w-full text-left text-sm"><thead className="bg-muted"><tr>{["导入行号","资产编号","机器状态",...(receipt.result.scope === withLogins ? ["凭据状态"] : []),"说明"].map(label=><th key={label} scope="col" className="px-3 py-2">{label}</th>)}</tr></thead>
      <tbody>{rows.map(row=><tr key={row.line} className="border-t"><td className="px-3 py-2">{row.line}</td><td className="px-3 py-2 font-mono">{row.asset_id || "—"}</td><td className="px-3 py-2">{statusLabel[row.status]}</td>{receipt.result.scope === withLogins && <td className="px-3 py-2">{loginStatusLabel[row.credential_status ?? ""] ?? "—"}</td>}<td className="px-3 py-2">{loginErrorLabel[row.code] ?? row.message}</td></tr>)}</tbody>
    </table></div></div>}
    <div className="flex flex-wrap gap-2"><Button size="sm" disabled={!!busy || !scope || (!machine && !file)} onClick={() => void run("preview")}>{busy === "preview" ? "正在预览…" : loginMode ? "预览导入" : receipt || error ? "重新预览" : "预览 CSV"}</Button>
      {receipt?.action === "preview" && <Button size="sm" variant="primary" disabled={!!busy || !pending} onClick={() => void run("import")}>{loginMode ? "确认保存凭据" : `仅导入可新增的 ${counts.new} 行`}</Button>}
      <Button size="sm" variant="ghost" disabled={!!busy} onClick={onClose}>{machine ? "返回机器详情" : "返回台账"}</Button></div>
    {!machine && <details className="border-t pt-2 text-sm"><summary className="cursor-pointer">按批次号查导入回执</summary>
      <form className="mt-2 flex flex-wrap gap-2" onSubmit={e=>{e.preventDefault();void lookup()}}><Input aria-label="导入批次号" className="max-w-lg" value={batchId} required pattern="[a-f0-9]{64}" disabled={!!busy} onChange={e=>setBatchId(e.target.value)} /><Button type="submit" size="sm" disabled={!!busy || !batchId.trim()}>查询回执</Button></form>
    </details>}
  </div>
}
