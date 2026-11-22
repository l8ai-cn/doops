import assert from "node:assert/strict"
import test from "node:test"
import { parseAgentPaths } from "./agent-paths.ts"
import {
  joinWorkspacePath,
  workspaceAbsolutePath,
  workspaceRelativePath,
  workspaceRootPath,
} from "./workspace-path.ts"

test("Agent paths use defaults only for absent configuration", () => {
  assert.deepEqual(parseAgentPaths("{}"), { workspace_root: "/root/ws", kb_root: "/root/kb" })
  assert.deepEqual(parseAgentPaths('{"workspace_root":"/home/ops/ws/","kb_root":"/data/knowledge"}'), {
    workspace_root: "/home/ops/ws", kb_root: "/data/knowledge",
  })
  for (const text of ["", "null", '{"workspace_root":""}', '{"kb_root":"relative"}', '{"kb_root":"/data/../root"}']) {
    assert.throws(() => parseAgentPaths(text))
  }
})

test("custom workspace roots apply to navigation and containment", () => {
  const base = "/home/ops/workspaces/"
  assert.equal(workspaceRootPath("session-a", base), "/home/ops/workspaces/session-a")
  assert.equal(workspaceRelativePath("session-a", "/home/ops/workspaces/session-a/file", base), "file")
  assert.equal(workspaceRelativePath("session-a", "/root/ws/session-a/file", base), null)
  assert.equal(joinWorkspacePath("session-a", "/home/ops/workspaces/session-a", "nested/file", base), "/home/ops/workspaces/session-a/nested/file")
  assert.equal(joinWorkspacePath("session-a", "/home/ops/workspaces/session-a", "../file", base), null)
  assert.equal(workspaceAbsolutePath("session-a", "", base), "/home/ops/workspaces/session-a")
})

test("workspace file paths use relative Agent paths inside the session root", () => {
  assert.equal(workspaceRootPath("session-a"), "/root/ws/session-a")
  assert.equal(
    workspaceRelativePath("session-a", "/root/ws/session-a/src/main.ts"),
    "src/main.ts",
  )
  assert.equal(
    workspaceAbsolutePath("session-a", "src/main.ts"),
    "/root/ws/session-a/src/main.ts",
  )
})

test("workspace paths reject absolute, traversal, sibling, and invalid session paths", () => {
  assert.equal(workspaceRelativePath("session-a", "/root/ws/session-b/secret.txt"), null)
  assert.equal(workspaceRelativePath("session-a", "/root/ws/session-a/../secret.txt"), null)
  assert.equal(workspaceAbsolutePath("session-a", "/etc/passwd"), null)
  assert.equal(workspaceAbsolutePath("session-a", "../secret.txt"), null)
  assert.equal(workspaceRootPath("../session-a"), null)
  assert.equal(workspaceRootPath("session/a"), null)
})

test("new and uploaded files remain under the current workspace directory", () => {
  assert.equal(
    joinWorkspacePath("session-a", "/root/ws/session-a/src", "nested/file.ts"),
    "/root/ws/session-a/src/nested/file.ts",
  )
  assert.equal(
    joinWorkspacePath("session-a", "/root/ws/session-a/src", "../outside.ts"),
    null,
  )
  assert.equal(
    joinWorkspacePath("session-a", "/root/ws/session-a/src", "/tmp/outside.ts"),
    null,
  )
})

test("literal file names are not silently rewritten or interpreted as shell", () => {
  for (const name of ["folder name/file.txt", "prefix$(printf marker)", "quote'file", "a\\b"]) {
    assert.equal(workspaceAbsolutePath("session-a", name), `/root/ws/session-a/${name}`)
    assert.equal(workspaceRelativePath("session-a", `/root/ws/session-a/${name}`), name)
  }
  assert.equal(workspaceAbsolutePath("session-a", " file.txt"), null)
  assert.equal(workspaceAbsolutePath("session-a", "file.txt "), null)
})
