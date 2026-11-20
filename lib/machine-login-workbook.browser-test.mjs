// node lib/machine-login-workbook.browser-test.mjs. Set CHROME_BIN to use another Chromium installation.
// The local HTTP server exposes only the test, parser, and zip.js modules. No workbook or CSV is written to disk.
import assert from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createServer } from 'node:http'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { stripTypeScriptTypes } from 'node:module'
import { once } from 'node:events'

async function runCases() {
  const { machineLoginWorkbookCsv: parse } = await import('/parser.js')
  const { ZipWriter } = await import('/zip/lib/zip-core-writer.js')
  const sheetNS = 'http://schemas.openxmlformats.org/spreadsheetml/2006/main'
  const ns = 'http://schemas.openxmlformats.org/package/2006/content-types'
  const mime = 'application/vnd.openxmlformats-officedocument.spreadsheetml.'
  const escape = s => s.replaceAll('&', '&amp;').replaceAll('<', '&lt;')
  const formula = { xml: '<f>1+1</f><v>2</v>' }
  const shared = { xml: '<v>0</v>', type: 's' }
  function sheet(rows, merges = []) {
    return `<worksheet xmlns="${sheetNS}"><sheetData>` + rows.map((cells, i) => `<row r="${i + 1}">` + cells.map((v,j) => {
      if (v == null) return ''
      const ref = String.fromCharCode(65 + j) + (i + 1)
      return typeof v === 'object' ? `<c r="${ref}" t="${v.type ?? 'n'}">${v.xml}</c>` : `<c r="${ref}" t="inlineStr"><is><t xml:space="preserve">${escape(v)}</t></is></c>`
    }).join('') + '</row>').join('') + '</sheetData><mergeCells>' + merges.map(ref => `<mergeCell ref="${ref}"/>`).join('') + '</mergeCells></worksheet>'
  }
  async function book(sheets, extra = {}) {
    const relNS = 'http://schemas.openxmlformats.org/package/2006/relationships'
    const office = 'http://schemas.openxmlformats.org/officeDocument/2006/relationships'
    const parts = { '_rels/.rels': `<Relationships xmlns="${relNS}"><Relationship Id="root" Type="${office}/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
      'xl/workbook.xml': `<workbook xmlns="${sheetNS}" xmlns:r="${office}"><sheets>` + sheets.map((_, i) => `<sheet name="Sheet${i+1}" sheetId="${i+1}" r:id="r${i+1}"/>`).join('') + '</sheets></workbook>',
      'xl/_rels/workbook.xml.rels': `<Relationships xmlns="${relNS}">` + sheets.map((_,i) => `<Relationship Id="r${i+1}" Type="${office}/worksheet" Target="worksheets/sheet${i+1}.xml"/>`).join('') + `<Relationship Id="sst" Type="${office}/sharedStrings" Target="sharedStrings.xml"/></Relationships>`,
      'xl/sharedStrings.xml': `<sst xmlns="${sheetNS}"><si><r><t>shared</t></r><r><t xml:space="preserve"> S&amp; </t></r><rPh><t>noise</t></rPh></si></sst>`,
      '[Content_Types].xml': `<Types xmlns="${ns}"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Override PartName="/xl/workbook.xml" ContentType="${mime}sheet.main+xml"/><Override PartName="/xl/sharedStrings.xml" ContentType="${mime}sharedStrings+xml"/>` + sheets.map((_,i) => `<Override PartName="/xl/worksheets/sheet${i+1}.xml" ContentType="${mime}worksheet+xml"/>`).join('') + '</Types>', ...extra }
    sheets.forEach((s,i) => { parts[`xl/worksheets/sheet${i+1}.xml`] = s })
    const chunks = [], zip = new ZipWriter(new WritableStream({ write(chunk) { chunks.push(chunk) } }), { useWebWorkers: false })
    for (const [name, xml] of Object.entries(parts)) await zip.add(name, new Blob([xml]).stream())
    await zip.close()
    return new File(chunks, 'synthetic.xlsx')
  }
  const machines = Array.from({length:74}, (_,i) => ({ asset_id: `m${i+1}`, address: `192.0.2.${i+1}`, name: `known${i+1}` }))
  const chinese = ['业务系统名称','IP地址','操作系统','服务器初始帐户/密码','服务器描述']
  const std = ['资产编号','username','password']
  const pass = '  aB&,"/字  '
  const row = (ip, pair, name = 'other') => [name, ip, null, pair]
  const checks = []
  const check = (name, ok) => { if (!ok) throw new Error('FAILED ' + name); checks.push(name) }
  async function rejects(name, file, re, inventory = machines) {
    try { await parse(await file, inventory) } catch (err) {
      check(name, re.test(err.message) && !err.message.includes(pass) && !err.message.includes('canary')); return
    }
    check(name, false)
  }
  const manual = sheet([['安装说明',formula],['密码字段示意']])
  const inventory = Array.from({length:397}, (_,i) => i < 74 ? row(machines[i].address, `帐户：admin\n密码：${pass}`) : row('198.51.100.1','canary/ambiguous/ignored'))
  inventory[74] = row(null, null, '标题说明')
  inventory[75] = row('198.51.100.1-254', 'unknown')
  const mix = await parse(await book([manual, sheet([chinese, ...inventory], ['A2:A3']), sheet([['Linux']]), manual]), machines)
  check('four sheets, 397 rows, exact 74 matches', mix.matchedRows === 74 && mix.csv.split('\n').length === 75)
  check('323 exact 1-based skipped rows', JSON.stringify(mix.skippedRows) === JSON.stringify(Array.from({length:323},(_,i)=>76+i)))
  check('password whitespace, case, punctuation and CSV quoting', mix.csv.split('\n')[1] === '"m1","admin","  aB&,""/字  "')
  const dup = await parse(await book([sheet([std,['m1','admin',shared],['m1','secondary',pass],['m1','admin',shared]])]), machines)
  check('multiple accounts and identical repeat preserved', dup.matchedRows === 3 && dup.csv.endsWith('"m1","admin","shared S& "'))
  const lineBreak = await parse(await book([sheet([std,['m1','admin', 'a_x000D_\nb_x005F_x000A_']])]), machines)
  check('password CRLF and literal Excel escape preserved', lineBreak.csv.endsWith('"a\r\nb_x000A_"'))
  const paired = await parse(await book([sheet([["机器清单"], chinese,row(machines[0].address,'admin/  MiX  '),row(machines[1].address,'admin\npass/word')], ['A1:E1'])]), machines)
  check('merged title, slash pair and two lines', paired.skippedRows[0] === 1 && paired.csv.includes('"  MiX  "') && paired.csv.endsWith('"pass/word"'))
  const ipv6 = await parse(await book([sheet([chinese, row('2001:0db8::1','admin/yes'),row('2001:db8:2::/64', null)])]), [{asset_id:'v6',address:'2001:db8::1'}])
  check('IPv6 canonical address and disjoint prefix', ipv6.matchedRows === 1 && ipv6.skippedRows[0] === 3)
  await rejects('partial id and IP conflict',book([sheet([[...std,'IP地址'],['m1','admin',pass,machines[1].address]])]), /第 2 行.*同一台/)
  await rejects('password conflict includes both source rows', book([sheet([std,['m1','admin',pass],['m1','admin','canary']])]), /第 3 行.*第 2 行.*冲突/)
  await rejects('no qualifying sheet', book([manual,manual]), /工作表选择错误.*没有/)
  await rejects('multiple qualifying sheets', book([sheet([std]),manual,sheet([chinese])]), /工作表选择错误.*多个/)
  await rejects('ambiguous pair blocks entire batch', book([sheet([chinese, row(machines[0].address,'admin/OK'), row(machines[1].address,'admin/canary/ambiguous')])]), /第 3 行.*唯一拆分/)
  await rejects('multiple inventory matches', book([sheet([chinese, row(machines[0].address,'admin/OK')])]), /第 2 行.*多个匹配/, [machines[0],{...machines[1], address:machines[0].address}])
  for (const addr of ['192.0.2.0-99','192.0.2.0/24','198.51.100.1,192.0.2.1']) {
    await rejects('overlapping address '+addr, book([sheet([chinese,row(addr,'canary')])]), /第 2 行.*可能涉及/)
  }
  await rejects('known name with missing address',book([sheet([chinese,row('', 'admin/canary', machines[0].name)])]), /第 2 行.*单一 IP/)
  await rejects('matched formula rejected', book([sheet([std,['m1','admin',formula]])]), /第 2 行.*公式/)
  await rejects('empty matched secret rejected', book([sheet([std,['m1','admin',null]])]), /第 2 行.*为空/)
  await rejects('merged credential rejected', book([sheet([std,['m1','admin',pass]],['C2:C3'])]), /第 2 行.*合并/)
  await rejects('unmatched only reports rows', book([sheet([chinese,row('198.51.100.8','canary')])]), /源行号：2/)
  await rejects('macro part rejected', book([manual],{'xl/vbaProject.bin':'synthetic'}), /含宏/)
  await rejects('inflated entry limit', book([manual],{ 'xl/data.xml':' '.repeat(8*1024*1024+1) }), /解压大小/)
  await rejects('cell limit', book([sheet([std,['m1','admin','a'.repeat(32768)]])]), /32,767/)
  return checks.length
}

const zipRoot = new URL('../node_modules/@zip.js/zip.js/', import.meta.url)
const parser = stripTypeScriptTypes(await readFile(new URL('./machine-login-workbook.ts',import.meta.url),'utf8')).replace('@zip.js/zip.js/lib/', '/zip/lib/')
let finish
const done = new Promise(resolve => { finish = resolve })
const server = createServer(async (req, res) => {
  if (req.url === '/result' && req.method === 'POST') { let data=''; for await (const part of req) data+=part; finish(JSON.parse(data)); res.end(); return }
  try {
    if (req.url === '/') { res.setHeader('Content-Type','text/html; charset=utf-8'); res.end(`<script type="module">(${runCases})().then(count=>({count}),e=>({error:e.message})).then(r=>fetch('/result',{method:'POST',body:JSON.stringify(r)}))</script>`); return }
    res.setHeader('Content-Type','text/javascript')
    if (req.url === '/parser.js') { res.end(parser); return }
    if (!/^\/zip\/lib\/[\w./-]+\.js$/.test(req.url) || req.url.includes('..')) throw Error()
    res.end(await readFile(new URL(req.url.slice(5),zipRoot)))
  } catch { res.writeHead(404); res.end() }
})
await new Promise(resolve => server.listen(0,'127.0.0.1', resolve))
const profile = await mkdtemp(join(tmpdir(),'machine-workbook-browser-'))
const chrome = process.env.CHROME_BIN ?? (process.platform === 'darwin' ? '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' : 'chromium')
const child = spawn(chrome,['--headless','--disable-gpu','--disable-background-networking','--disable-extensions','--no-first-run',`--user-data-dir=${profile}`,`http://127.0.0.1:${server.address().port}/`],{stdio:'ignore'})
const closed = once(child,'close')
try {
  const result = await Promise.race([done, closed.then(()=>{ throw Error('Browser exited') }), new Promise((_,reject)=>setTimeout(()=>reject(Error('Browser timeout')),30000).unref())])
  assert.ifError(result.error ? new Error(result.error) : null)
  console.log(`${result.count} browser checks passed`)
} finally {
  child.kill(); await closed; server.closeAllConnections(); server.close(); await rm(profile,{recursive:true,force:true,maxRetries:3})
}
