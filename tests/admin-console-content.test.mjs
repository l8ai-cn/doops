import assert from "node:assert/strict"
import { readFileSync } from "node:fs"

const source = readFileSync(new URL("../components/admin-console.tsx", import.meta.url), "utf8")

assert.ok(source.includes("isMissingAdminApi"), "admin console should classify missing gateway admin APIs")
assert.ok(source.includes("gateway 版本未包含管理接口"), "admin console should show a clear gateway version mismatch message")
assert.ok(source.includes("return null"), "empty-state rendering should be suppressed while an error is shown")
