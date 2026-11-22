import assert from "node:assert/strict"
import test from "node:test"
import { createElement } from "react"
import { renderToStaticMarkup } from "react-dom/server"
import { AgentWorkspace, ToolRendererRegistry } from "@agent-cloud/agent-ui"

// Exercise the installed package's public component, including its pnpm patch.
// The synthetic runtime rejects all operations; these are rendered-output checks,
// not browser event-handler or live Gateway tests.
function fixture() {
  const tool = {
    id: "synthetic-tool",
    kind: "tool",
    identity: { namespace: "synthetic", semanticKey: "tool.custom", schemaVersion: "1" },
    title: "TOOL_CANARY",
    status: "failed",
    input: "INPUT_CANARY",
    output: "OUTPUT_CANARY",
    detail: "DETAIL_CANARY",
    failure: { message: "FAILURE_CANARY" },
    results: [{ id: "result", kind: "text", text: "RESULT_CANARY" }],
    actions: [{ actionId: "retry", actionType: "retry", confirmationRequired: false, label: "ACTION_CANARY", schemaVersion: "1" }],
    execution: { turnId: "SYNTHETIC_TURN" },
  }
  const interactions = ["artifact_action", "extension", "permission", "tool_action"].map((kind, index) => ({
    id: kind,
    kind,
    sourceId: `RECEIPT_${kind}`,
    commandId: `synthetic-command-${index}`,
    status: ["succeeded", "failed", "rejected", "running"][index],
    updatedAt: "2026-11-11T00:00:00Z",
  }))
  const snapshot = {
    sessionId: "synthetic-session",
    title: "Synthetic display probe",
    agentLabel: "Synthetic agent",
    status: "completed",
    connection: "connected",
    interactionMode: "acp",
    capabilities: {
      sendMessage: true, interrupt: true, resolvePermission: true,
      updateConfiguration: false, terminal: false, executeToolAction: true, extensions: [],
    },
    interactions,
    items: [
      { id: "user", kind: "message", role: "user", text: "USER_CANARY", status: "completed" },
      { id: "assistant", kind: "message", role: "assistant", text: "ASSISTANT_CANARY", status: "completed" },
      tool,
      { id: "reasoning", kind: "reasoning", title: "Reasoning", detail: "REASONING_CANARY", status: "completed" },
      { id: "error", kind: "error", title: "Error", detail: "ERROR_CANARY", status: "failed" },
    ],
    plan: [],
    permissions: [{ id: "approval", kind: "approval", title: "APPROVAL_CANARY", description: "PERMISSION_DESCRIPTION_CANARY" }],
    terminals: [],
    hasOlderItems: false,
    error: null,
  }
  const rejectOperation = () => assert.fail("SSR must not invoke a runtime operation")
  const runtime = {
    getSnapshot: () => snapshot,
    subscribe: () => () => {},
    open: rejectOperation, close: rejectOperation, sendMessage: rejectOperation,
    interrupt: rejectOperation, resolvePermission: rejectOperation,
    updateConfiguration: rejectOperation, executeToolAction: rejectOperation,
    loadOlder: rejectOperation,
  }
  function render(props = {}) {
    // The real snapshot also carries an execution projection of the same tool.
    // Include it so restoring duplicate trace rows cannot make these checks pass.
    snapshot.execution = {
      runs: [{
        id: "SYNTHETIC_TURN", title: "Turn SYNTHETIC_TURN", turnId: "SYNTHETIC_TURN",
        status: tool.status, childRunIds: [],
        steps: [{
          id: tool.id, kind: "tool", title: tool.title, status: tool.status,
          detail: tool.detail, failure: tool.failure, progress: tool.progress,
        }],
      }],
      activeRunId: tool.status === "running" ? "SYNTHETIC_TURN" : undefined,
      runningCount: tool.status === "running" ? 1 : 0,
      failedCount: tool.status === "failed" ? 1 : 0,
    }
    return renderToStaticMarkup(createElement(AgentWorkspace, {
      runtime, sessionId: snapshot.sessionId, presentation: "developer",
      defaultMode: "work",
      showFullscreen: false, locale: "en-US", ...props,
    }))
  }
  return { tool, snapshot, render }
}

function textContent(html) {
  // The ASCII canaries contain no HTML entities. Remove tags so accessible-name
  // attributes do not count as a second visible copy of progress or receipts.
  return html.replace(/<[^>]*>/g, "")
}

function appearsOnce(html, text) {
  assert.equal(textContent(html).split(text).length - 1, 1, `${text} must render exactly once`)
}

test("AgentWorkspace preserves messages, tool evidence, actions and approval", () => {
  const { render } = fixture()
  const html = render()
  for (const text of [
    "USER_CANARY", "ASSISTANT_CANARY", "INPUT_CANARY", "OUTPUT_CANARY",
    "DETAIL_CANARY", "FAILURE_CANARY", "RESULT_CANARY", "ACTION_CANARY",
    "APPROVAL_CANARY", "PERMISSION_DESCRIPTION_CANARY",
  ]) appearsOnce(html, text)
  // Reasoning and error text stay readable in the conversation. 0.3 previews the
  // detail on the activity row and repeats it in the expanded block, so only the
  // tool evidence itself is required to render exactly once.
  for (const text of ["REASONING_CANARY", "ERROR_CANARY"]) assert.ok(textContent(html).includes(text), `${text} must stay visible`)
  assert.match(html, /<button\b[^>]*>ACTION_CANARY<\/button>/)
})

test("AgentWorkspace keeps interaction receipts in the developer trace, not the conversation", () => {
  const { snapshot, render } = fixture()
  const html = render()
  // 0.3 moved execution evidence into the "Execution trace" drawer. The trigger
  // counts the four receipts plus the execution run; the receipts themselves are
  // not repeated in the conversation body.
  assert.match(html, /data-execution-trace-trigger="true"/)
  assert.match(html, /aria-label="Execution trace · 5"/)
  for (const interaction of snapshot.interactions) {
    assert.ok(!textContent(html).includes(interaction.sourceId), `${interaction.sourceId} must not repeat inline`)
  }
  assert.doesNotMatch(textContent(html), /Turn SYNTHETIC_TURN/)
})

test("AgentWorkspace still gates tool actions by runtime capability", () => {
  const { snapshot, render } = fixture()
  snapshot.capabilities.executeToolAction = false
  const html = render()
  assert.doesNotMatch(textContent(html), /ACTION_CANARY/)
  appearsOnce(html, "OUTPUT_CANARY")
  appearsOnce(html, "APPROVAL_CANARY")
})

for (const progress of [
  { stage: "STAGE_CANARY" },
  { message: "PROGRESS_CANARY" },
  { stage: "STAGE_CANARY", message: "PROGRESS_CANARY" },
]) {
  test(`AgentWorkspace keeps ${Object.keys(progress).join(" and ")} visible without a fraction`, () => {
    const { tool, snapshot, render } = fixture()
    tool.status = "running"
    tool.progress = progress
    snapshot.status = "running"
    const html = render()
    for (const text of Object.values(progress)) {
      assert.ok(textContent(html).includes(text), `${text} must stay visible`)
    }
    assert.doesNotMatch(html, /role="progressbar"/)
  })
}

test("AgentWorkspace preserves fractional progress and its text", () => {
  const { tool, snapshot, render } = fixture()
  tool.status = "running"
  tool.progress = { stage: "STAGE_CANARY", message: "PROGRESS_CANARY", fraction: 0.5 }
  snapshot.status = "running"
  const html = render()
  for (const text of ["STAGE_CANARY", "PROGRESS_CANARY"]) {
    assert.ok(textContent(html).includes(text), `${text} must stay visible`)
  }
  const bars = [...html.matchAll(/<[^>]+\brole="progressbar"[^>]*>/g)]
  assert.equal(bars.length, 1, "the tool has one accessible progress bar")
  assert.match(bars[0][0], /aria-valuemin="0"/)
  assert.match(bars[0][0], /aria-valuemax="1"/)
  assert.match(bars[0][0], /aria-valuenow="0\.5"/)
  assert.match(bars[0][0], /aria-label="PROGRESS_CANARY"/)
})

test("AgentWorkspace suppresses duplicate tool titles but preserves distinct titles and file links", () => {
  const { tool, snapshot, render } = fixture()
  const registry = new ToolRendererRegistry()
  tool.identity = { namespace: "synthetic", semanticKey: "filesystem.read", schemaVersion: "1" }
  tool.inputValue = { path: "src/path-canary.ts" }
  registry.register(tool.identity, { presentation: { label: "LABEL_CANARY", icon: () => null } }, "synthetic")
  snapshot.interactions = []
  // File links render when the workspace can reveal files: 0.3 gates them on a
  // codeWorkspace with list/read capabilities instead of patching the label.
  const codeWorkspace = {
    workspaceKey: "synthetic-workspace",
    workspaceRoot: "/workspace",
    capabilities: { list: true, read: true, diff: true },
  }
  for (const title of ["LABEL_CANARY", "src/path-canary.ts", "DISTINCT_TITLE_CANARY"]) {
    tool.title = title
    const html = render({ toolRenderers: registry, codeWorkspace })
    appearsOnce(html, "LABEL_CANARY")
    appearsOnce(html, "src/path-canary.ts")
    if (title === "DISTINCT_TITLE_CANARY") appearsOnce(html, title)
    assert.match(html, /<button\b[^>]*>src\/path-canary\.ts<\/button>/)
    appearsOnce(html, "INPUT_CANARY")
    appearsOnce(html, "OUTPUT_CANARY")
    appearsOnce(html, "FAILURE_CANARY")
  }
})
