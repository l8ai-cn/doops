import assert from "node:assert/strict"
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import test, { after } from "node:test"
import { fileURLToPath, pathToFileURL } from "node:url"
import TypeScript from "typescript"

const root = fileURLToPath(new URL("../../", import.meta.url))
const fixture = mkdtempSync(join(tmpdir(), "doops-same-origin-test-"))
after(() => rmSync(fixture, { recursive: true, force: true }))
const output = join(fixture, "same-origin.mjs")
writeFileSync(output, TypeScript.transpile(readFileSync(join(root, "lib/server/same-origin.ts"), "utf8"), {
  module: TypeScript.ModuleKind.ESNext,
  target: TypeScript.ScriptTarget.ES2022,
}))
const { rejectCrossOriginMutation } = await import(pathToFileURL(output).href)

function post(url, headers) {
  return new Request(url, { method: "POST", headers })
}

test("GET is never rejected", () => {
  assert.equal(rejectCrossOriginMutation(new Request("http://console.test/api/rpc")), null)
})

test("same origin passes and a sibling site is rejected", () => {
  delete process.env.DOOPS_CONSOLE_ORIGIN
  assert.equal(rejectCrossOriginMutation(post("http://console.test/api/rpc", { Origin: "http://console.test" })), null)
  assert.equal(rejectCrossOriginMutation(post("http://console.test/api/rpc", { Origin: "http://notebook.test" }))?.status, 403)
  assert.equal(rejectCrossOriginMutation(post("http://console.test/api/rpc", {}))?.status, 403)
})

test("forwarded headers alone cannot set the console origin", () => {
  delete process.env.DOOPS_CONSOLE_ORIGIN
  // Next.js standalone sees http://localhost:3000 while the browser is on https.
  const req = post("http://localhost:3000/api/rpc", {
    Host: "doops.example.com",
    "X-Forwarded-Proto": "https",
    Origin: "https://doops.example.com",
  })
  assert.equal(rejectCrossOriginMutation(req)?.status, 403)
  const forwardedHost = post("http://127.0.0.1:3000/api/rpc", {
    Host: "127.0.0.1:3000",
    "X-Forwarded-Host": "doops.example.com",
    "X-Forwarded-Proto": "https",
    Origin: "https://doops.example.com",
  })
  assert.equal(rejectCrossOriginMutation(forwardedHost)?.status, 403)
  const sibling = post("http://localhost:3000/api/rpc", {
    Host: "doops.example.com",
    "X-Forwarded-Proto": "https",
    Origin: "https://notebook.example.com",
  })
  assert.equal(rejectCrossOriginMutation(sibling)?.status, 403)
})

test("cross-site fetch metadata is rejected even with a matching origin", () => {
  delete process.env.DOOPS_CONSOLE_ORIGIN
  const req = post("http://console.test/api/rpc", { Origin: "http://console.test", "Sec-Fetch-Site": "cross-site" })
  assert.equal(rejectCrossOriginMutation(req)?.status, 403)
})

test("DOOPS_CONSOLE_ORIGIN pins the accepted origin", () => {
  process.env.DOOPS_CONSOLE_ORIGIN = "https://doops.example.com"
  try {
    assert.equal(rejectCrossOriginMutation(post("http://localhost:3000/api/rpc", { Origin: "https://doops.example.com" })), null)
    assert.equal(rejectCrossOriginMutation(post("http://console.test/api/rpc", { Host: "console.test", Origin: "http://console.test" }))?.status, 403)
  } finally {
    delete process.env.DOOPS_CONSOLE_ORIGIN
  }
})
