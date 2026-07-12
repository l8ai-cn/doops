# Remote Multi-Ops CI/CD Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development or executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace local CI/CD plan rendering, private-key signing, image building, and single-target reconciliation with a remote control plane that coordinates separately declared DoOps roles and records immutable evidence.

**Architecture:** The CLI submits only an immutable source reference, a workflow path, parameter values, and an explicit mutation decision to a remote control plane. The control plane resolves the one repository-owned environment registry, compiles and signs the DeploymentPlan remotely, then dispatches source verification, image build, artifact attestation, deployment, health observation, and rollback as explicit role operations. Each role returns structured evidence; a release cannot converge from text output or a successful command alone.

**Tech Stack:** Go, JSON-RPC/MCP, DoOps Gateway reverse tunnel, Git, BuildKit, Helm, Kubernetes, OCI image digests, Ed25519.

---

### Task 1: Release Request Boundary

**Files:**
- Modify: `backend/agent/api/mcp.go`
- Modify: `backend/skills/doops-cli/cli/doops/cicd.go`
- Modify: `backend/skills/doops-cli/cli/doops/main.go`
- Test: `backend/skills/doops-cli/cli/doops/cicd_remote_test.go`

- [ ] **Step 1: Write a failing CLI test**

Require `doops cicd submit` to construct this request without loading a local YAML file, resolving `deploy/environments.yaml`, or reading any local signing material:

```go
ReleaseRequest{
    APIVersion: "doops.sh/v3",
    Kind: "ReleaseRequest",
    RepositoryID: "repo_zhiyong",
    Revision: "0123456789abcdef0123456789abcdef01234567",
    WorkflowPath: "deploy/workflows/test.yaml",
    Inputs: map[string]string{"reason": "release"},
    AllowMutate: true,
}
```

- [ ] **Step 2: Run the focused test and observe failure**

Run:

```bash
cd backend/skills/doops-cli && go test ./cli/doops -run TestCICDSubmitDoesNotReadLocalDeploymentState -count=1
```

Expected: failure because the CLI still resolves a local deployment plan instead of submitting a typed release request.

- [ ] **Step 3: Add the minimal request-only CLI path**

Add the typed `ReleaseRequest` and a `doops_cicd_submit` client call. `cicd submit` requires `--target`, `--repository-id`, `--revision`, `--workflow`, and `-session`; it may accept `--set`, `--dry-run`, and `--allow-mutate`. It must reject local `-f` input and must not access any deployment key or local registry.

- [ ] **Step 4: Re-run the focused CLI tests**

Run:

```bash
cd backend/skills/doops-cli && go test ./cli/doops -run 'TestCICDSubmit|TestCICDRun' -count=1
```

Expected: `PASS`, and legacy local `cicd run -f ...` returns an explicit migration error rather than silently executing the old path.

### Task 2: Control-Plane Role Contract

**Files:**
- Modify: `backend/agent/api/mcp.go`
- Create: `backend/agent/internal/server/cicd_multiops.go`
- Test: `backend/agent/internal/server/cicd_multiops_test.go`

- [ ] **Step 1: Write failing role-graph tests**

Require a compiled release to include exactly these roles and dependencies:

```go
[]CICDOperation{
    {Role: "source-verifier"},
    {Role: "image-builder", DependsOn: []string{"source-verifier"}},
    {Role: "artifact-attestor", DependsOn: []string{"image-builder"}},
    {Role: "deployment-executor", DependsOn: []string{"artifact-attestor"}},
    {Role: "health-observer", DependsOn: []string{"deployment-executor"}},
    {Role: "rollback-controller", DependsOn: []string{"deployment-executor", "health-observer"}},
}
```

The test must also reject a graph missing the rollback controller, duplicate role names, unbound role targets, direct shell commands, and an image operation without OCI output digest requirements.

- [ ] **Step 2: Run the focused test and observe failure**

Run:

```bash
cd backend/agent && go test ./internal/server -run TestCICDOperationGraph -count=1
```

Expected: failure because no multi-Ops release graph exists.

- [ ] **Step 3: Add typed operation graph validation**

Implement typed operation, target binding, artifact, and evidence declarations. The compiler may only create a graph after resolving the repository-owned environment registry. The graph must require distinct logical roles, immutable source and image references, post-deploy log evidence, and a rollback transition on any failed health gate.

- [ ] **Step 4: Re-run the focused control-plane tests**

Run:

```bash
cd backend/agent && go test ./internal/server -run 'TestCICDOperationGraph|TestCICDReleaseRequest' -count=1
```

Expected: `PASS`.

### Task 3: Gateway Submission and Auditing

**Files:**
- Modify: `backend/agent/internal/server/gateway_store.go`
- Modify: `backend/agent/internal/server/tunnel_hub.go`
- Modify: `backend/agent/internal/server/admin_http.go`
- Test: `backend/agent/internal/server/gateway_protocol_test.go`

- [ ] **Step 1: Write a failing gateway test**

Require `doops_cicd_submit` to be accepted only with the `cicd:submit` permission, to create a release audit record, and to reject an unconfigured compiler role before any deployment target is contacted.

- [ ] **Step 2: Run the focused test and observe failure**

Run:

```bash
cd backend/agent && go test ./internal/server -run TestGatewayCICDSubmit -count=1
```

Expected: failure because the Gateway has not yet recognized the remote release submission action.

- [ ] **Step 3: Add the remote submission route**

Add `doops_cicd_submit` as a Gateway-owned action. It validates the request, starts a parent release audit, invokes the remote compiler role through the reverse tunnel, and writes child role outcomes to the same release session. No private key, local filesystem path, raw command, or caller-selected environment target is accepted from the CLI.

- [ ] **Step 4: Re-run the focused Gateway tests**

Run:

```bash
cd backend/agent && go test ./internal/server -run 'TestGatewayCICDSubmit|TestGateway.*Audit' -count=1
```

Expected: `PASS`.

### Task 4: Remote Compiler and Immutable Attestation

**Files:**
- Modify: `backend/agent/internal/server/handler_ws.go`
- Create: `backend/agent/internal/server/cicd_compiler.go`
- Test: `backend/agent/internal/server/handler_ws_test.go`

- [ ] **Step 1: Write a failing compiler test**

Require the compiler to clone only the registered repository at the requested 40-character commit, load the requested workflow and `deploy/environments.yaml` from that clone, validate the role graph, generate the canonical plan digest, and sign it with a key available only in the compiler process.

- [ ] **Step 2: Run the focused test and observe failure**

Run:

```bash
cd backend/agent && go test ./internal/server -run TestRemoteCICDCompiler -count=1
```

Expected: failure because current compilation and signing occur in `doops` CLI.

- [ ] **Step 3: Move compilation and signing behind the compiler role**

Implement a structured compiler tool that emits a signed `DeploymentPlan` plus source identity evidence. The compiler must reject a source URL supplied by the caller, a mutable ref, a workflow path outside the repository root, an unknown environment, an unsigned registry result, and any fallback to local state.

- [ ] **Step 4: Re-run compiler tests**

Run:

```bash
cd backend/agent && go test ./internal/server -run 'TestRemoteCICDCompiler|TestGatewayCICDSubmit' -count=1
```

Expected: `PASS`.

### Task 5: Remote BuildKit, Deployment, Observation, and Rollback

**Files:**
- Modify: `backend/agent/internal/server/cicd_multiops.go`
- Modify: `backend/agent/internal/server/tunnel_hub.go`
- Modify: `backend/agent/docs/cicd-reconcile-protocol.md`
- Test: `backend/agent/internal/server/cicd_multiops_test.go`

- [ ] **Step 1: Write failing execution-gate tests**

Require the image builder to return image digests, the artifact attestor to bind those digests to the source tree, the deployment executor to use only Helm/GitOps declarations, the health observer to return public, workload, endpoint, and post-deploy log evidence, and the rollback controller to execute when any required health evidence is absent.

- [ ] **Step 2: Run the focused test and observe failure**

Run:

```bash
cd backend/agent && go test ./internal/server -run TestMultiOpsReleaseExecution -count=1
```

Expected: failure because current reconciliation delegates the entire release to one target and accepts only one report.

- [ ] **Step 3: Implement role dispatch and fail-closed gates**

Dispatch each typed operation to its declared target over the Gateway tunnel. Preserve the last known good revision before mutation. A failed build, attest, rollout, endpoint check, public contract check, or log scan must collect required failure evidence and invoke the rollback controller. Convergence requires all role attestations; a text success message is invalid.

- [ ] **Step 4: Re-run role execution tests**

Run:

```bash
cd backend/agent && go test ./internal/server -run 'TestMultiOpsReleaseExecution|TestGatewayCICDSubmit' -count=1
```

Expected: `PASS`.

### Task 6: Canonical Agent Image and Legacy Release Removal

**Files:**
- Modify: `backend/Dockerfile`
- Delete: `backend/agent/Dockerfile`
- Delete: `backend/deploy.sh`
- Modify: `.cnb.yml`
- Modify: `backend/README.md`
- Modify: `backend/docs/BUILD_DEPLOY_SOP.md`
- Modify: `backend/test/test_sandbox_image_contract.py`
- Test: `backend/agent/internal/server/agent_upgrade_test.go`

- [ ] **Step 1: Write failing image-entrypoint tests**

Require `backend/Dockerfile` to be the only production Agent Dockerfile, to install and verify pinned `kubectl`, and require all release documentation to point to remote multi-Ops BuildKit rather than CNB or `deploy.sh`.

- [ ] **Step 2: Run the focused test and observe failure**

Run:

```bash
python3 -m pytest backend/test/test_sandbox_image_contract.py -q
cd backend/agent && go test ./internal/server -run TestAgentImage -count=1
```

Expected: failure because duplicate Dockerfiles, CNB tag release, and `deploy.sh` remain authoritative.

- [ ] **Step 3: Consolidate only after Task 5 passes**

Move the verified kubectl installation to `backend/Dockerfile`, delete the duplicate Dockerfile and shell release script, remove CNB image-release stages, and update documentation to name the remote source verifier, builder, attestor, executor, observer, and rollback controller. Preserve `Dockerfile.base.light` only as a declared remote base-runtime artifact with an immutable digest.

- [ ] **Step 4: Verify static contracts and remote canary**

Run:

```bash
python3 -m pytest backend/test/test_sandbox_image_contract.py -q
cd backend/agent && go test ./...
cd ../skills/doops-cli && go test ./...
```

Then run the remote multi-Ops canary against the declared non-production control plane. The canary must return source, image, attestation, rollout, health, log-scan, and rollback-path evidence before any production promotion is enabled.

**Coverage review:** Tasks 1-4 remove local deployment authority and establish the remote signed plan boundary. Task 5 implements the required multi-Ops execution and antifragile release gates. Task 6 removes duplicate and legacy build/release paths only after the replacement has passed remote canary validation.
