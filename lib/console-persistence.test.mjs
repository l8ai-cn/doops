import assert from "node:assert/strict"
import test from "node:test"
import {
  clearConsoleState,
  consoleScopeKey,
  loadDeployState,
  loadSelection,
  restoreSelection,
  saveDeployDraft,
  saveDeploySession,
  saveSelection,
  validSessionId,
} from "./console-persistence.ts"

function memoryStorage() {
  const data = new Map()
  return {
    data,
    getItem: (key) => (data.has(key) ? data.get(key) : null),
    setItem: (key, value) => data.set(key, String(value)),
    removeItem: (key) => data.delete(key),
  }
}

const scope = { gateway: "https://doops.l8ai.cn", username: "ops-zhang" }
const zyj = { cluster: "doops-zyj", instance: "zyj", key: "doops-zyj/zyj" }
const test1 = { cluster: "doops-zyj", instance: "test", key: "doops-zyj/test" }

test("Given a saved selection, When targets load, Then the saved target and session are restored", () => {
  const storage = memoryStorage()
  saveSelection(storage, scope, { targetKey: zyj.key, sessionId: "console-abc12345" })
  const restored = restoreSelection(loadSelection(storage, scope), [test1, zyj])
  assert.equal(restored.selected, zyj)
  assert.equal(restored.sessionId, "console-abc12345")
  assert.equal(restored.notice, "")
})

test("Given the saved target is gone, When targets load, Then the first target is chosen with a new session and one notice", () => {
  for (const [name, targets, selected, notice] of [
    ["another target remains", [test1], test1, /doops-zyj\/zyj.*不在可用目标列表中.*已切换到 doops-zyj\/test/],
    ["no target remains", [], null, /doops-zyj\/zyj.*当前没有可用目标/],
  ]) {
    const storage = memoryStorage()
    saveSelection(storage, scope, { targetKey: zyj.key, sessionId: "console-abc12345" })
    const restored = restoreSelection(loadSelection(storage, scope), targets)
    assert.equal(restored.selected, selected, name)
    assert.equal(restored.sessionId, null, name)
    assert.match(restored.notice, notice, name)
  }
})

test("Given nothing saved, When targets load, Then the first target is chosen without a notice", () => {
  const restored = restoreSelection(loadSelection(memoryStorage(), scope), [test1, zyj])
  assert.deepEqual(restored, { selected: test1, sessionId: null, notice: "" })
})

test("Given another Gateway or user, When the selection is loaded, Then the other scope's selection is not used", () => {
  const storage = memoryStorage()
  saveSelection(storage, scope, { targetKey: zyj.key, sessionId: "console-abc12345" })
  for (const other of [
    { gateway: "https://other.example.test", username: "ops-zhang" },
    { gateway: scope.gateway, username: "ops-li" },
    { gateway: scope.gateway },
  ]) {
    assert.equal(loadSelection(storage, other), null, JSON.stringify(other))
  }
  assert.notEqual(consoleScopeKey({ gateway: "a|b", username: "c" }), consoleScopeKey({ gateway: "a", username: "b|c" }))
})

test("Given a stored value that is malformed or unsafe, When it is loaded, Then it is ignored", () => {
  for (const raw of [
    "not json",
    "[]",
    JSON.stringify({ targetKey: zyj.key }),
    JSON.stringify({ targetKey: zyj.key, sessionId: "../other" }),
    JSON.stringify({ targetKey: zyj.key, sessionId: "a/b" }),
    JSON.stringify({ targetKey: "", sessionId: "console-1" }),
    JSON.stringify({ targetKey: zyj.key, sessionId: 42 }),
  ]) {
    const storage = memoryStorage()
    storage.setItem(`doops.console.v1.selection:${consoleScopeKey(scope)}`, raw)
    assert.equal(loadSelection(storage, scope), null, raw)
  }
  assert.equal(loadSelection(null, scope), null)
})

test("Given a session id, When validated, Then only Gateway-safe ids pass", () => {
  for (const [value, want] of [
    ["console-abc12345", true],
    ["workload-x.y_z", true],
    ["", false],
    [" console", false],
    ["..", false],
    ["a/b", false],
    ["a\\b", false],
    ["a\nb", false],
    ["x".repeat(129), false],
  ]) {
    assert.equal(validSessionId(value), want, JSON.stringify(value))
  }
})

test("Given storage that throws, When the selection is saved or loaded, Then the console keeps working", () => {
  const broken = {
    getItem: () => { throw new Error("denied") },
    setItem: () => { throw new Error("quota") },
    removeItem: () => { throw new Error("denied") },
  }
  saveSelection(broken, scope, { targetKey: zyj.key, sessionId: "console-1" })
  assert.equal(loadSelection(broken, scope), null)
})

test("Given a selection, When it is saved, Then only the target key and session id are stored", () => {
  const storage = memoryStorage()
  saveSelection(storage, scope, { targetKey: zyj.key, sessionId: "console-abc12345", token: "synthetic-token" })
  const [value] = [...storage.data.values()]
  assert.deepEqual(JSON.parse(value), { targetKey: zyj.key, sessionId: "console-abc12345" })
})

test("Given a token login without a username, When the selection is saved, Then nothing is stored or restored", () => {
  const storage = memoryStorage()
  for (const tokenScope of [{ gateway: scope.gateway }, { gateway: scope.gateway, username: "" }, { gateway: scope.gateway, username: " " }]) {
    saveSelection(storage, tokenScope, { targetKey: zyj.key, sessionId: "console-abc12345" })
    assert.equal(storage.data.size, 0, JSON.stringify(tokenScope))
    assert.equal(loadSelection(storage, tokenScope), null, JSON.stringify(tokenScope))
  }
})

test("Given a user logs out, When the console state is cleared, Then the saved selection is gone", () => {
  const storage = memoryStorage()
  saveSelection(storage, scope, { targetKey: zyj.key, sessionId: "console-abc12345" })
  clearConsoleState(storage, scope)
  assert.equal(loadSelection(storage, scope), null)
  assert.equal(storage.data.size, 0)
})

// R4-5: the release page keeps its release session and unsent draft per Gateway + user, never for a token login,
// and logout removes them, so the next person in the same tab does not inherit them.
const draft = JSON.stringify({ release: "synthetic-app", reason: "synthetic reason" })

test("Given a release session and draft, When another scope loads them, Then only the same Gateway and user see them", () => {
  for (const [name, reader, expected] of [
    ["same user", scope, { sessionId: "deploy-abc12345", draft }],
    ["another user on the same Gateway", { gateway: scope.gateway, username: "ops-li" }, { sessionId: null, draft: null }],
    ["the same user on another Gateway", { gateway: "https://other.example.test", username: "ops-zhang" }, { sessionId: null, draft: null }],
    ["a token login", { gateway: scope.gateway }, { sessionId: null, draft: null }],
  ]) {
    const storage = memoryStorage()
    saveDeploySession(storage, scope, zyj.key, "deploy-abc12345")
    saveDeployDraft(storage, scope, zyj.key, draft)
    assert.deepEqual(loadDeployState(storage, reader, zyj.key), expected, name)
    assert.deepEqual(loadDeployState(storage, scope, test1.key), { sessionId: null, draft: null }, `${name}: other target`)
  }
})

test("Given a token login without a username, When the release page saves, Then nothing is stored", () => {
  for (const tokenScope of [{ gateway: scope.gateway }, { gateway: scope.gateway, username: "" }, { gateway: scope.gateway, username: " " }]) {
    const storage = memoryStorage()
    saveDeploySession(storage, tokenScope, zyj.key, "deploy-abc12345")
    saveDeployDraft(storage, tokenScope, zyj.key, draft)
    assert.equal(storage.data.size, 0, JSON.stringify(tokenScope))
  }
})

// R6-4: a token login keeps nothing in sessionStorage, but its release session and draft stay in memory for that login,
// so leaving the release page and coming back still shows the running release.
test("Given a token login, When the release page comes back, Then its release session and draft come from memory only", () => {
  for (const tokenScope of [{ gateway: scope.gateway }, { gateway: scope.gateway, username: "" }, { gateway: scope.gateway, username: " " }]) {
    const name = JSON.stringify(tokenScope)
    const storage = memoryStorage()
    saveDeploySession(storage, tokenScope, zyj.key, "deploy-abc12345")
    saveDeployDraft(storage, tokenScope, zyj.key, draft)
    assert.equal(storage.data.size, 0, `${name}: nothing reaches sessionStorage`)
    assert.deepEqual(loadDeployState(storage, tokenScope, zyj.key), { sessionId: "deploy-abc12345", draft }, `${name}: same login`)
    assert.deepEqual(loadDeployState(storage, tokenScope, test1.key), { sessionId: null, draft: null }, `${name}: other target`)
    assert.deepEqual(loadDeployState(null, tokenScope, zyj.key), { sessionId: "deploy-abc12345", draft }, `${name}: no storage at all`)
    // The next token login on the same Gateway is another session object and starts empty.
    assert.deepEqual(loadDeployState(storage, { ...tokenScope }, zyj.key), { sessionId: null, draft: null }, `${name}: another login`)
    saveDeploySession(storage, tokenScope, zyj.key, "../escape")
    assert.equal(loadDeployState(storage, tokenScope, zyj.key).sessionId, "deploy-abc12345", `${name}: an unsafe id is ignored`)
    clearConsoleState(storage, tokenScope)
    assert.deepEqual(loadDeployState(storage, tokenScope, zyj.key), { sessionId: null, draft: null }, `${name}: logout drops it`)
  }
})

test("Given a saved draft, When it is cleared, Then the release session stays and an empty entry is dropped", () => {
  const storage = memoryStorage()
  saveDeploySession(storage, scope, zyj.key, "deploy-abc12345")
  saveDeployDraft(storage, scope, zyj.key, draft)
  saveDeployDraft(storage, scope, zyj.key, null)
  assert.deepEqual(loadDeployState(storage, scope, zyj.key), { sessionId: "deploy-abc12345", draft: null })
  const other = memoryStorage()
  saveDeployDraft(other, scope, zyj.key, draft)
  saveDeployDraft(other, scope, zyj.key, null)
  assert.equal(other.data.size, 0)
})

test("an unknown release receipt survives new sessions and drafts within only its Gateway, user and target", () => {
  const storage = memoryStorage()
  const previousRelease = { sessionId: "deploy-unknown1", status: "unknown" }
  saveDeploySession(storage, scope, zyj.key, "deploy-new1", { ...previousRelease, output: "SYNTHETIC_SECRET" })
  saveDeployDraft(storage, scope, zyj.key, draft)
  saveDeploySession(storage, scope, zyj.key, "deploy-new2")
  assert.deepEqual(loadDeployState(storage, scope, zyj.key), { sessionId: "deploy-new2", draft, previousRelease })
  assert.doesNotMatch([...storage.data.values()].join(""), /SYNTHETIC_SECRET|output/)
  for (const [reader, key] of [
    [{ ...scope, username: "another-user" }, zyj.key],
    [{ ...scope, gateway: "https://other.example.test" }, zyj.key],
    [scope, test1.key],
  ]) assert.equal(loadDeployState(storage, reader, key).previousRelease, undefined)
  saveDeployDraft(storage, scope, zyj.key, null)
  assert.deepEqual(loadDeployState(storage, scope, zyj.key).previousRelease, previousRelease)
  clearConsoleState(storage, scope)
  assert.equal(loadDeployState(storage, scope, zyj.key).previousRelease, undefined)
})

test("a token login keeps the unknown receipt only in its own memory and clears it on logout", () => {
  const storage = memoryStorage()
  const tokenScope = { gateway: scope.gateway }
  const previousRelease = { sessionId: "deploy-unknown1", status: "unknown" }
  saveDeploySession(storage, tokenScope, zyj.key, "deploy-new1", previousRelease)
  assert.deepEqual(loadDeployState(storage, tokenScope, zyj.key).previousRelease, previousRelease)
  assert.equal(storage.data.size, 0)
  assert.equal(loadDeployState(storage, { ...tokenScope }, zyj.key).previousRelease, undefined)
  clearConsoleState(storage, tokenScope)
  assert.equal(loadDeployState(storage, tokenScope, zyj.key).previousRelease, undefined)
})

test("invalid previous receipt metadata is rejected on save and ignored on read", () => {
  for (const previousRelease of [
    { sessionId: "../bad", status: "unknown" },
    { sessionId: "deploy-old1", status: "succeeded" },
    { sessionId: "deploy-old1" },
  ]) {
    const storage = memoryStorage()
    saveDeploySession(storage, scope, zyj.key, "deploy-new1", previousRelease)
    assert.equal(storage.data.size, 0)
    storage.setItem(`doops.console.v1.deploy:${consoleScopeKey(scope)}`, JSON.stringify([
      { targetKey: zyj.key, sessionId: "deploy-new1", previousRelease },
    ]))
    assert.deepEqual(loadDeployState(storage, scope, zyj.key), { sessionId: "deploy-new1", draft: null })
  }
})

test("Given unsafe or malformed release state, When it is saved or loaded, Then it is ignored", () => {
  const storage = memoryStorage()
  saveDeploySession(storage, scope, zyj.key, "../escape")
  assert.equal(storage.data.size, 0, "a session id the Gateway rejects is not stored")
  const key = `doops.console.v1.deploy:${consoleScopeKey(scope)}`
  for (const [name, raw] of [
    ["not JSON", "{"], ["empty list", "[]"],
    ["object instead of a list", JSON.stringify({ [zyj.key]: { sessionId: "deploy-abc12345" } })],
    ["entry not an object", JSON.stringify(["deploy-abc12345"])],
    ["unsafe session id", JSON.stringify([{ targetKey: zyj.key, sessionId: "../x" }])],
    ["draft not a string", JSON.stringify([{ targetKey: zyj.key, draft: { release: "x" } }])],
  ]) {
    storage.data.set(key, raw)
    assert.deepEqual(loadDeployState(storage, scope, zyj.key), { sessionId: null, draft: null }, name)
  }
})

test("Given many targets, When release state is saved, Then only the most recent targets are kept", () => {
  const storage = memoryStorage()
  for (let i = 0; i < 25; i++) saveDeploySession(storage, scope, `c/t${i}`, `deploy-s${i}`)
  assert.equal(loadDeployState(storage, scope, "c/t0").sessionId, null, "the oldest target drops out")
  assert.equal(loadDeployState(storage, scope, "c/t24").sessionId, "deploy-s24")
  saveDeployDraft(storage, scope, "c/t5", draft)
  saveDeploySession(storage, scope, "c/t25", "deploy-s25")
  assert.equal(loadDeployState(storage, scope, "c/t5").draft, draft, "a target used again moves to the end")
})

test("Given a user logs out, When the console state is cleared, Then release sessions and drafts are gone, old unscoped keys too", () => {
  const data = new Map()
  const storage = {
    data,
    get length() { return data.size },
    key: (index) => [...data.keys()][index] ?? null,
    getItem: (key) => (data.has(key) ? data.get(key) : null),
    setItem: (key, value) => data.set(key, String(value)),
    removeItem: (key) => data.delete(key),
  }
  saveDeploySession(storage, scope, zyj.key, "deploy-abc12345")
  saveDeployDraft(storage, scope, zyj.key, draft)
  // Written by earlier console versions without a user in the key.
  data.set(`doops.deploy-session.${zyj.key}`, "deploy-old00001")
  data.set(`doops.deploy-draft.${zyj.key}`, draft)
  data.set("unrelated.key", "kept")
  clearConsoleState(storage, scope)
  assert.deepEqual(loadDeployState(storage, scope, zyj.key), { sessionId: null, draft: null })
  assert.deepEqual([...data.keys()], ["unrelated.key"])
  // Storage that cannot list its keys still clears the scoped entry.
  const plain = memoryStorage()
  saveDeploySession(plain, scope, zyj.key, "deploy-abc12345")
  clearConsoleState(plain, scope)
  assert.equal(plain.data.size, 0)
})
