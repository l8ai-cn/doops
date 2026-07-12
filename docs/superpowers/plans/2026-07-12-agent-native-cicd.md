# Agent-Native CI/CD Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `doops cicd run` synchronize the repository to the declared target and initiate a doagent `Ask` task that executes and verifies the deployment goal.

**Architecture:** `DeploymentTemplate` still compiles into an immutable `DeploymentPlan`. The CLI resolves the declared target, calls `doops push` to create the session workspace, then sends one generated semantic deployment instruction through `doops_agent_prompt`/Ask. The doagent chooses operational commands, performs the release, validates every declared acceptance condition, and returns its evidence in the Ask result. There is no CI/CD stage list, command list, or separate reconcile MCP tool.

**Tech Stack:** Go CLI, existing DoOps Git workspace transport, Gateway WebSocket, doagent ACP Ask.

---

### Task 1: Define the Agent-Native Execution Contract

**Files:**
- Create: `backend/skills/doops-cli/cli/doops/cicd_agentic.go`
- Test: `backend/skills/doops-cli/cli/doops/cicd_agentic_test.go`

- [ ] **Step 1: Write the failing orchestration test**

```go
func TestAgenticDeploymentPushesBeforeAsk(t *testing.T) {
    calls := []string{}
    runner := agenticDeploymentRunner{
        pushWorkspace: func(Server, string, string) error {
            calls = append(calls, "push")
            return nil
        },
        ask: func(string) (string, error) {
            calls = append(calls, "ask")
            return "Converged", nil
        },
    }

    _, err := runner.Run(context.Background(), validAgenticDeploymentPlan(t), CICDAgenticRunRequest{
        SourceDirectory: t.TempDir(),
        SessionID:       "cicd-agentic-test",
    })
    if err != nil {
        t.Fatal(err)
    }
    if diff := cmp.Diff([]string{"push", "ask"}, calls); diff != "" {
        t.Fatalf("unexpected CI/CD operation order (-want +got):\n%s", diff)
    }
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./cli/doops -run TestAgenticDeploymentPushesBeforeAsk -count=1`

Expected: failure because `agenticDeploymentRunner` does not exist.

- [ ] **Step 3: Add the minimal Agent-native runner**

Implement a runner that:

```go
type CICDAgenticRunRequest struct {
    SourceDirectory string
    SessionID       string
    DryRun          bool
    AllowMutate     bool
}

type CICDAgenticRun struct {
    PlanDigest string `json:"planDigest"`
    Target     string `json:"target"`
    Workspace  string `json:"workspace"`
    Outcome    string `json:"outcome"`
}
```

Its only external sequence is:

```go
Push(server, sourceDirectory, "", false, nil, sessionID)
client.CallAndCapture("doops_agent_prompt", map[string]interface{}{
    "instruction": buildAgenticDeploymentInstruction(plan, request),
})
```

`dryRun` is a requirement inside the semantic Ask instruction, not a reason to skip source synchronization.

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./cli/doops -run TestAgenticDeploymentPushesBeforeAsk -count=1`

Expected: PASS.

### Task 2: Generate the Semantic Ask Instruction

**Files:**
- Modify: `backend/skills/doops-cli/cli/doops/cicd_agentic.go`
- Test: `backend/skills/doops-cli/cli/doops/cicd_agentic_test.go`

- [ ] **Step 1: Write the failing instruction-contract test**

The test must assert that the generated instruction:

```go
assertContains(t, instruction, "DeploymentPlan")
assertContains(t, instruction, plan.Digest)
assertContains(t, instruction, "Validate every requiredEvidence")
assertContains(t, instruction, "restore the last known good revision")
assertNotContains(t, instruction, "deploy.sh")
assertNotContains(t, instruction, "stage")
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./cli/doops -run TestAgenticDeploymentInstructionCarriesGoalAndAcceptance -count=1`

Expected: failure because the instruction generator does not exist.

- [ ] **Step 3: Implement the generator**

The instruction must tell doagent to:

1. Treat the resolved profile and artifact contract in the plan as authoritative.
2. Inspect the synchronized `/root/ws/<session>` workspace and the actual target.
3. Reach the declared desired state using its available tools.
4. Validate every declared success evidence item before reporting success.
5. On failure, preserve evidence, restore the last known good revision, and report the blocking fact.
6. Avoid generated deployment scripts, stage lists, and command replay.

- [ ] **Step 4: Run the test and verify it passes**

Run: `go test ./cli/doops -run TestAgenticDeploymentInstructionCarriesGoalAndAcceptance -count=1`

Expected: PASS.

### Task 3: Replace the Dedicated Reconcile Entry Point

**Files:**
- Modify: `backend/skills/doops-cli/cli/doops/cicd.go`
- Modify: `backend/skills/doops-cli/cli/doops/main.go`
- Delete: `backend/skills/doops-cli/cli/doops/cicd_v2_reconcile.go`
- Modify: `backend/skills/doops-cli/cli/doops/cicd_v2_test.go`

- [ ] **Step 1: Write the failing command-level test**

```go
func TestCICDRunDoesNotRequirePlanSigningKey(t *testing.T) {
    t.Setenv("DOOPS_CICD_PLAN_SIGNING_KEY", "")
    // A fake Agent-native runner is sufficient; run must reach it.
    err := runCICDCommand(context.Background(), args, newFakeAgenticRunner)
    if err != nil {
        t.Fatalf("cicd run should use push and Ask, not a plan-signing gate: %v", err)
    }
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./cli/doops -run TestCICDRunDoesNotRequirePlanSigningKey -count=1`

Expected: failure because `cicd run` currently requires `DOOPS_CICD_PLAN_SIGNING_KEY`.

- [ ] **Step 3: Route `cicd run` to the Agent-native runner**

Remove signing and dedicated reconcile construction from `runCICDCommand`. In `main.go`, resolve the plan target, require `-session`, derive the repository root from the template path, construct an Agent-native runner, and return its JSON result.

- [ ] **Step 4: Remove the unused dedicated reconcile CLI path**

Delete `cicd_v2_reconcile.go` and remove types/functions that only supported `doops_cicd_reconcile`.

- [ ] **Step 5: Run focused CLI tests**

Run: `go test ./cli/doops -run 'TestAgenticDeployment|TestCICDRunDoesNotRequirePlanSigningKey' -count=1`

Expected: PASS.

### Task 4: Remove the Dedicated Gateway Reconcile Protocol

**Files:**
- Modify: `backend/agent/api/mcp.go`
- Modify: `backend/agent/internal/server/handler_ws.go`
- Modify: `backend/agent/internal/server/tunnel_hub.go`
- Modify: `backend/agent/internal/server/handler_ws_test.go`
- Modify: `backend/agent/internal/server/gateway_protocol_test.go`
- Delete: `backend/agent/docs/cicd-reconcile-protocol.md`

- [ ] **Step 1: Write failing tests that reject the removed tool**

```go
result := callToolResult(t, conn, "doops_cicd_reconcile", map[string]interface{}{})
if result["error"] == nil {
    t.Fatal("dedicated reconcile tool must not remain after CI/CD moves to Ask")
}
```

- [ ] **Step 2: Run the test and verify it fails**

Run: `go test ./internal/server -run TestDedicatedCICDReconcileIsNotAnAgentEntryPoint -count=1`

Expected: failure because the dedicated tool still exists.

- [ ] **Step 3: Delete the protocol-specific tool and retain Ask**

Remove `CICDReconcileParams`, `doops_cicd_reconcile` dispatch, plan attestation verification, structured report collection, route-specific reconcile handling, and reconcile-only audit logic. Keep standard Gateway route binding, `push`, and `ask` permission checks.

- [ ] **Step 4: Run the focused server test**

Run: `go test ./internal/server -run TestDedicatedCICDReconcileIsNotAnAgentEntryPoint -count=1`

Expected: PASS.

### Task 5: Update User-Facing Workflow and Verify

**Files:**
- Modify: `backend/agent/skills/system_prompt.md`
- Modify: `backend/skills/doops-cli/cli/doops/main.go`
- Modify: `deploy/README.md` in `/Users/wwyz/Documents/code/zhiyong`

- [ ] **Step 1: Describe the only deployment flow**

Document:

```text
DeploymentTemplate -> DeploymentPlan -> doops push -> doops ask -> doagent execution -> doagent validation
```

Do not mention signing keys, `doops_cicd_reconcile`, stage lists, or a generated deployment script.

- [ ] **Step 2: Run all affected tests**

Run:

```bash
cd backend/skills/doops-cli && go test ./... -count=1
cd backend/agent && go test ./... -count=1
cd /Users/wwyz/Documents/code/zhiyong && python3 -m unittest deploy.tests.test_new_deploy_contract deploy.tests.test_new_deploy_build_tool deploy.tests.test_release_manifest_tool deploy.tests.test_deployment_health_contract deploy.tests.test_permission_topology_contract
```

Expected: all pass.

- [ ] **Step 3: Verify the generated plan and CLI path**

Run:

```bash
doops -session semantic-cicd-verify cicd lint -f /Users/wwyz/Documents/code/zhiyong/deploy/workflows/test.yaml
doops -session semantic-cicd-verify cicd plan -f /Users/wwyz/Documents/code/zhiyong/deploy/workflows/test.yaml --set releaseId=<40-character-commit> --set reason=verification
```

Expected: lint succeeds and the plan contains a resolved target profile and acceptance requirements.

- [ ] **Step 4: Commit and push only task files**

Commit DoOps and Zhiyong separately. Do not include unrelated worktree changes. Push both branches and verify their remote refs contain the commits.
