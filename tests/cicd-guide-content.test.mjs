import assert from "node:assert/strict"
import { readFileSync } from "node:fs"

const source = readFileSync(new URL("../components/cicd-guide.tsx", import.meta.url), "utf8")

assert.ok(source.includes("doops -session"), "CI/CD snippets must use the supported doops CLI release path")
assert.ok(!source.includes("/api/rpc"), "CI/CD snippets must not point at the Next.js proxy route")
assert.ok(!source.includes('${keyDisplay}'), "CI/CD snippets must not render the live session token")
assert.ok(source.includes("doops add"), "snippets should create a gateway target from CI secrets")
