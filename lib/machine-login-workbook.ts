import { ZipReader, type FileEntry } from "@zip.js/zip.js/lib/zip-core-reader.js"

type Machine = { asset_id: string; address?: string; name?: string }
type WorkbookCsv = { csv: string; skippedRows: number[]; matchedRows: number }
type Row = { line: number; cells: Map<number, string | null> }

const MiB = 1024 * 1024
const sheetNS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
const sheetType = "application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"
const workbookType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
const relNS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
const packageRelNS = "http://schemas.openxmlformats.org/package/2006/relationships"
const stringsType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"

// Only errors constructed here cross the public boundary. XML and ZIP error messages can contain input text.
class WorkbookError extends Error {}
function fail(message: string): never { throw new WorkbookError(message) }
function rowError(line: number, message: string): never { return fail(`Excel 第 ${line} 行：${message}`) }

async function xmlPart(entries: Map<string, FileEntry>, path: string, root: string, ns = sheetNS) {
  const entry = entries.get(path)
  if (!entry) fail("XLSX 缺少所需的工作簿部件。")
  const bytes = await entry.arrayBuffer({ useWebWorkers: false, checkSignature: true })
  const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes)
  if (/<!DOCTYPE|<!ENTITY/i.test(text)) fail("工作簿含有不支持的 XML 文档类型或实体声明。")
  let markup = 0
  for (let pos = text.indexOf("<"); pos >= 0; pos = text.indexOf("<", pos + 1)) {
    if (++markup > 250_000) fail("工作簿 XML 节点过多。")
  }
  const doc = new DOMParser().parseFromString(text, "application/xml")
  if (doc.documentElement.localName !== root || doc.documentElement.namespaceURI !== ns || doc.getElementsByTagName("parsererror").length) {
    fail("工作簿 XML 格式不正确。")
  }
  return doc.documentElement
}

function children(el: Element, name: string) {
  return Array.from(el.children).filter(child => child.namespaceURI === el.namespaceURI && child.localName === name)
}

function excelText(el: Element) {
  // Rich-text runs contain the displayed text. Phonetic guides (rPh) are annotations.
  const runs = children(el, "r")
  const nodes = runs.length ? runs.flatMap(run => children(run, "t")) : children(el, "t")
  const text = nodes.map(node => (node.textContent ?? "").replace(/_x([\da-f]{4})_/gi, (_, code: string) =>
    String.fromCharCode(parseInt(code, 16)))).join("")
  if (text.length > 32767) fail("工作簿单元格超过 32,767 个字符。")
  return text
}

function coordinate(ref: string) {
  const match = /^([A-Z]{1,3})([1-9]\d{0,6})$/.exec(ref)
  if (!match) fail("工作表的单元格坐标无效。")
  let column = 0
  for (const ch of match[1]) column = column * 26 + ch.charCodeAt(0) - 64
  const line = Number(match[2])
  if (column > 16384 || line > 1048576) fail("工作表的单元格坐标超出范围。")
  return { column, line }
}

// null marks a non-text value, including a cached formula. Do not convert numbers, dates or booleans to secrets.
function cellValue(cell: Element, strings: readonly string[], line: number): string | null {
  if (children(cell, "f").length) return null
  const kind = cell.getAttribute("t")
  const value = children(cell, "v")[0]?.textContent
  if (kind === "inlineStr") {
    const inline = children(cell, "is")[0]
    return inline ? excelText(inline) : ""
  }
  if (kind === "s") {
    if (!value || !/^\d+$/.test(value) || Number(value) >= strings.length) rowError(line, "共享字符串引用无效。")
    return strings[Number(value)]
  }
  return value == null ? "" : null
}

function sheetRows(sheet: Element, strings: readonly string[]) {
  const data = children(sheet, "sheetData")
  if (data.length !== 1) fail("工作表数据区域无效。")
  const rows: Row[] = []
  let last = 0, count = 0, chars = 0
  for (const row of children(data[0], "row")) {
    const line = Number(row.getAttribute("r") ?? last + 1)
    if (!Number.isInteger(line) || line <= last || line > 1048576) fail("工作表行号重复或无效。")
    last = line
    const cells: Row["cells"] = new Map()
    let column = 0
    for (const cell of children(row, "c")) {
      if (++count > 50_000) fail("工作表超过 50,000 个单元格。")
      const ref = cell.getAttribute("r")
      const pos = ref ? coordinate(ref) : { column: column + 1, line }
      if (pos.line !== line || pos.column <= column || pos.column > 16384) rowError(line, "单元格坐标重复或无效。")
      column = pos.column
      const value = cellValue(cell, strings, line)
      if (value !== "") cells.set(column, value)
      chars += value?.length ?? 0
      if (chars > 8 * MiB) fail("工作表展开后的单元格文本过大。")
    }
    if (cells.size) rows.push({ line, cells })
  }
  return rows
}

function ipKey(input: string | undefined) {
  const ip = input?.trim() ?? ""
  if (/^(?:0|[1-9]\d{0,2})(?:\.(?:0|[1-9]\d{0,2})){3}$/.test(ip) && ip.split(".").every(o => Number(o) <= 255)) return ip
  if (ip.includes(":") && /^[a-f\d:.]+$/i.test(ip)) {
    try { return new URL(`http://[${ip}]/`).hostname } catch { /* Invalid IPv6 literal. */ }
  }
}

const idLabels = ["asset_id", "资产编号"]
const pairLabels = ["服务器初始帐户/密码", "服务器初始账户/密码"]
const labelOf = (value: string | null) => value?.trim().toLowerCase().replaceAll("／", "/") ?? ""

function isHeader(row: Row) {
  const labels = new Set(Array.from(row.cells.values(), labelOf))
  return (idLabels.some(label => labels.has(label)) && labels.has("username") && labels.has("password")) ||
    (labels.has("ip地址") && pairLabels.some(label => labels.has(label)))
}

function columns(header: Row) {
  const labels = [...header.cells].map(([index, value]) => [index, labelOf(value)] as const)
  function find(...names: string[]) {
    const hits = labels.filter(([, label]) => label && names.includes(label))
    if (hits.length > 1) rowError(header.line, "表头有重复的标识或凭据列。")
    return hits[0]?.[0]
  }
  const id = find(...idLabels), user = find("username"), pass = find("password")
  const ip = find("ip地址"), pair = find(...pairLabels)
  if (id && user && pass && !pair) return { id, user, pass, ip }
  if (ip && pair && !id && !user && !pass) return { ip, pair, name: find("业务系统名称") }
  rowError(header.line, "需提供资产编号、username、password 三列，或 IP地址、服务器初始帐户/密码两列。")
}

function credentials(pair: string, line: number): [string, string] {
  const lines = pair.split(/\r\n|\n|\r/)
  if (lines.length === 2) {
    const user = /^(?:用户名|[账帐][号户]|username)[:：](.*)$/i.exec(lines[0])
    const pass = /^(?:密码|password)[:：](.*)$/i.exec(lines[1])
    if (user && pass) return [user[1], pass[1]]
    if (!user && !pass && /^[\w.@\\-]+$/.test(lines[0])) return [lines[0], lines[1]]
  } else if (lines.length === 1) {
    const parts = pair.split("/")
    if (parts.length === 2 && /^[\w.@\\-]+$/.test(parts[0])) return [parts[0], parts[1]]
  }
  return rowError(line, "无法唯一拆分帐户和密码。请改用独立的 username、password 文本列，或分两行明确标注“帐户：”和“密码：”。")
}

// Ranges and lists can only exclude a row or flag an ambiguity; they never identify a machine for import.
function ipNumber(key: string): bigint {
  if (!key.startsWith("[")) return key.split(".").reduce((v, octet) => v * 256n + BigInt(octet), 0n)
  const [left, right] = key.slice(1, -1).split("::").map(side => side ? side.split(":") : [])
  const words = right ? [...left, ...Array(8 - left.length - right.length).fill("0"), ...right] : left
  return BigInt("0x" + words.map(word => word.padStart(4, "0")).join(""))
}

function outsideInventory(text: string, selected: ReadonlyMap<string, Machine[]>) {
  const targets = [...selected.keys()]
  const tokens = text.replace(/\s*([-~～至/])\s*/g, "$1").split(/[\s,，;；、]+/).filter(Boolean)
  return tokens.length > 0 && tokens.every(token => {
    const single = ipKey(token)
    if (single) return !selected.has(single)
    const range = /^([^/~～至-]+)([-~～至/])([^/~～至-]+)$/.exec(token)
    if (!range) return false
    const start = ipKey(range[1])
    if (!start) return false
    const ipv6 = start.startsWith("[")
    let low = ipNumber(start), high: bigint
    if (range[2] === "/") {
      const bits = ipv6 ? 128 : 32, prefix = Number(range[3])
      if (!/^\d{1,3}$/.test(range[3]) || prefix > bits) return false
      const block = 1n << BigInt(bits - prefix)
      low = low / block * block
      high = low + block - 1n
    } else {
      const last = !ipv6 && /^\d{1,3}$/.test(range[3]) ? start.replace(/\d+$/, range[3]) : range[3]
      const end = ipKey(last)
      if (!end || end.startsWith("[") !== ipv6) return false
      high = ipNumber(end)
      if (high < low) return false
    }
    return targets.every(ip => ip.startsWith("[") !== ipv6 || ipNumber(ip) < low || ipNumber(ip) > high)
  })
}

function convert(sheet: Element, rows: Row[], header: Row, machines: readonly Machine[]): WorkbookCsv {
  const cols = columns(header)
  const importedColumns = [cols.id, cols.ip, cols.user, cols.pass, cols.pair].filter((n): n is number => n !== undefined)
  const merges = Array.from(sheet.getElementsByTagNameNS(sheetNS, "mergeCell"))
  if (merges.length > 2000) fail("工作表的合并区域过多。")
  const blocked = merges.flatMap(merge => {
    const refs = (merge.getAttribute("ref") ?? "").split(":")
    if (refs.length !== 2) fail("工作表合并区域无效。")
    const a = coordinate(refs[0]), b = coordinate(refs[1])
    if (a.column > b.column || a.line > b.line) fail("工作表合并区域无效。")
    return importedColumns.some(c => c >= a.column && c <= b.column) ? [{ first: a.line, last: b.line }] : []
  })
  const ids = new Map<string, Machine[]>(), ips = new Map<string, Machine[]>()
  function index(map: Map<string, Machine[]>, key: string, m: Machine) {
    const group = map.get(key)
    if (group) group.push(m)
    else map.set(key, [m])
  }
  for (const m of machines) {
    index(ids, m.asset_id, m)
    const key = ipKey(m.address)
    if (key) index(ips, key, m)
  }
  const names = new Set(machines.map(m => m.name?.trim()).filter(Boolean))
  const seen = new Map<string, { pass: string; line: number }>()
  const skippedRows: number[] = []
  const csv = ["asset_id,username,password"]
  for (const row of rows) {
    const { line, cells } = row
    if (line < header.line) { skippedRows.push(line); continue }
    const merged = blocked.some(range => line >= range.first && line <= range.last)
    if (row === header) {
      if (merged) rowError(line, "表头的标识或凭据列涉及合并区域。")
      continue
    }
    function read(col: number) {
      const value = cells.get(col)
      if (value === null) rowError(line, "标识和凭据必须为文本，不能使用公式、数值或错误值。")
      if (!value) rowError(line, "标识或凭据为空，请补齐。")
      return value
    }
    let matches: Machine[] | undefined
    if (cols.ip) {
      const raw = cells.get(cols.ip)
      if (raw === null) rowError(line, "IP 地址含公式或非文本值，无法排除与已选机器匹配。")
      const text = raw?.trim() ?? ""
      const ip = ipKey(text)
      if (!ip) {
        const noAddress = /^(?:[-—–/]|无|暂无|无IP|N\/A)?$/i.test(text)
        const name = cols.name ? cells.get(cols.name)?.trim() : undefined
        if (!cols.id && ((noAddress && !names.has(name) && !(merged && cells.has(cols.pair!))) ||
          (!noAddress && outsideInventory(text, ips)))) { skippedRows.push(line); continue }
        rowError(line, "地址可能涉及已选机器，需提供完整的单一 IP 地址。")
      }
      matches = ips.get(ip)
      if (!matches && !cols.id) { skippedRows.push(line); continue }
    }
    if (cols.id) {
      const byId = ids.get(read(cols.id))
      if (!byId) rowError(line, "资产编号不在已有台账中。")
      if (cols.ip && (matches?.length !== 1 || matches[0] !== byId[0])) rowError(line, "资产编号和 IP 未指向同一台唯一机器。")
      matches = byId
    }
    if (matches?.length !== 1 || ids.get(matches[0].asset_id)?.length !== 1) rowError(line, "台账中有多个匹配项，无法唯一确定机器。")
    if (merged) rowError(line, "标识或凭据列涉及 Excel 合并区域，请逐行逐列填写。")
    const id = matches[0].asset_id
    const [user, pass] = cols.pair ? credentials(read(cols.pair), line) : [read(cols.user!), read(cols.pass!)]
    if (!user || user.trim() !== user || /[\r\n]/.test(user) || !pass) rowError(line, "帐户或密码为空，或帐户含首尾空白、换行。")
    // The JSON request cannot transport unpaired UTF-16 surrogates losslessly.
    if (/[\uD800-\uDFFF]/u.test(user) || /[\uD800-\uDFFF]/u.test(pass)) rowError(line, "凭据包含预览接口无法原样保留的字符。")
    const key = JSON.stringify([id, user])
    const previous = seen.get(key)
    if (previous && previous.pass !== pass) rowError(line, `与第 ${previous.line} 行的同一机器、同一帐户密码冲突。`)
    if (!previous) seen.set(key, { pass, line })
    csv.push([id, user, pass].map(v => `"${v.replaceAll('"', '""')}"`).join(","))
  }
  const matchedRows = csv.length - 1
  if (!matchedRows) fail(`没有匹配到已有机器。${skippedRows.length ? `待跳过源行共 ${skippedRows.length} 行，源行号：${skippedRows.join("、")}。` : "请检查表头下的数据行。"}`)
  const result = csv.join("\n")
  if (matchedRows > 2000 || new TextEncoder().encode(result).length > MiB) fail("匹配数据超过预览上限（2,000 行或 1 MiB），请分批导入。")
  return { csv: result, skippedRows, matchedRows }
}

/** Parse locally. Only csv is sent to the server preview; skippedRows are original 1-based worksheet row numbers. */
export async function machineLoginWorkbookCsv(file: File, machines: readonly Machine[]): Promise<WorkbookCsv> {
  if (!/\.xlsx$/i.test(file.name) || !file.size || file.size > 8 * MiB) fail("请选择 8 MiB 以内的非空 .xlsx 工作簿。")
  const zip = new ZipReader(file.stream(), { strictness: "strict", useWebWorkers: false, checkSignature: true })
  try {
    const entries = new Map<string, FileEntry>()
    let total = 0, count = 0
    for await (const entry of zip.getEntriesGenerator()) {
      total += entry.uncompressedSize
      if (++count > 512 || entry.uncompressedSize > 8 * MiB || total > 24 * MiB) fail("工作簿 ZIP 部件数量或解压大小超过限制。")
      if (entry.encrypted || /(?:vbaProject|macrosheets\/)/i.test(entry.filename)) fail("不支持加密或含宏的工作簿。")
      if (!entry.directory) {
        if (entries.has(entry.filename)) fail("工作簿含重复的 ZIP 部件。")
        entries.set(entry.filename, entry)
      }
    }
    const manifest = await xmlPart(entries, "[Content_Types].xml", "Types", "http://schemas.openxmlformats.org/package/2006/content-types")
    if (Array.from(manifest.children).some(e => /macro|vbaproject/i.test(e.getAttribute("ContentType") ?? ""))) fail("不支持含宏的工作簿。")
    const overrides = children(manifest, "Override")
    const parts = (type: string) => overrides.filter(n => n.getAttribute("ContentType") === type).map(n => (n.getAttribute("PartName") ?? "").slice(1))
    const workbooks = parts(workbookType)
    if (workbooks.length !== 1) fail("文件不是标准 XLSX 工作簿。")
    const workbookPath = workbooks[0], workbook = await xmlPart(entries, workbookPath, "workbook")
    const slash = workbookPath.lastIndexOf("/") + 1
    const relFile = workbookPath.slice(0, slash) + "_rels/" + workbookPath.slice(slash) + ".rels"
    const rels = children(await xmlPart(entries, relFile, "Relationships", packageRelNS), "Relationship")
    const knownSheets = new Set(parts(sheetType))
    const paths = new Set<string>()
    for (const item of children(workbook, "sheets").flatMap(el => children(el, "sheet"))) {
      const id = item.getAttributeNS(relNS, "id")
      const matches = rels.filter(rel => rel.getAttribute("Id") === id)
      if (!id || matches.length !== 1) fail("工作表关系无效。")
      const rel = matches[0]
      if (rel.getAttribute("Type") !== relNS + "/worksheet") continue // e.g. a chart sheet
      const target = rel.getAttribute("Target")
      if (!target || rel.getAttribute("TargetMode") === "External" || /[:?#\\]/.test(target) || target.startsWith("//")) fail("工作表引用必须位于工作簿内。")
      const path = new URL(target, new URL(workbookPath, "https://package.invalid/")).pathname.slice(1)
      if (!knownSheets.has(path) || paths.has(path)) fail("工作表引用重复或与工作簿清单不一致。")
      paths.add(path)
    }
    const stringsParts = parts(stringsType)
    if (stringsParts.length > 1) fail("工作簿有多个共享字符串表。")
    const strings = stringsParts.length ? children(await xmlPart(entries, stringsParts[0], "sst"), "si").map(excelText) : []
    let candidate: { sheet: Element; rows: Row[]; header: Row } | undefined
    for (const path of paths) {
      const sheet = await xmlPart(entries, path, "worksheet")
      const rows = sheetRows(sheet, strings)
      const headers = rows.filter(isHeader)
      if (!headers.length) continue
      if (candidate) fail("工作表选择错误：有多个工作表符合凭据表头，请只保留目标凭据表。")
      if (headers.length > 1) rowError(headers[1].line, "同一工作表有多个凭据表头，请单独导入一个数据区域。")
      candidate = { sheet, rows, header: headers[0] }
    }
    if (!candidate) fail("工作表选择错误：没有工作表符合凭据表头（资产编号、username、password，或 IP地址、服务器初始帐户/密码）。")
    return convert(candidate.sheet, candidate.rows, candidate.header, machines)
  } catch (err) {
    if (err instanceof WorkbookError) throw err
    fail("无法安全读取 XLSX，请检查文件是否损坏、加密或使用了不支持的编码。")
  } finally {
    await zip.close()
  }
}
