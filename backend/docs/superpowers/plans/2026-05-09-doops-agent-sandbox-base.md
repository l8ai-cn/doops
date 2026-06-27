# Doops Agent Sandbox Base Image Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebase the doops-agent runtime image from the internal WebIDE image to agent-infra AIO Sandbox while preserving doops exec, doagent ask, kubectl access, and BuildKit image-build capability.

**Architecture:** Keep the existing Go gateway and doagent binary composition, but introduce a sandbox-specific final runtime image and entrypoint. Build and deploy it under a non-latest tag first, then promote only after exec/ask/runtime checks pass.

**Tech Stack:** Docker/BuildKit, Go doops-agent gateway, Rust do-agent binary, AIO Sandbox container image, bash entrypoint, Kubernetes/nerdctl runtime mounts.

---

### Task 1: Add Structural Guard Tests

**Files:**
- Create: `test/test_sandbox_image_contract.py`

- [ ] Write tests that assert the sandbox Dockerfile uses `agent-infra/sandbox` or the China mirror, copies `/usr/local/bin/do-agent`, copies `/app/doops-agent`, runs `do-agent --help`, and does not reference legacy base/runtime/protocol/auth paths.
- [ ] Run: `python3 -m pytest test/test_sandbox_image_contract.py -q`.
- [ ] Expected before implementation: fails because `agent/Dockerfile.sandbox` and `agent/sandbox-entrypoint.sh` do not exist.

### Task 2: Add Sandbox Dockerfile And Entrypoint

**Files:**
- Create: `agent/Dockerfile.sandbox`
- Create: `agent/sandbox-entrypoint.sh`

- [ ] Implement a multi-stage build: Go builder, doagent binary source image, final AIO Sandbox runtime.
- [ ] Install minimum runtime additions: `tini`, `curl`, `ca-certificates`, BuildKit v0.21.1 if missing.
- [ ] Copy doops-agent, doops skills, self-docs, do-agent, and canonical doagent skills.
- [ ] Add build gate: `/usr/local/bin/do-agent --help >/dev/null`.
- [ ] Entrypoint starts sandbox base service when discoverable, configures `/root/.agent/settings.json`, syncs skills, starts doagent ACP HTTP on 9000, starts buildkitd, writes registry auth, and runs `/app/doops-agent` under `tini -s`.

### Task 3: Wire Docs And Build Defaults

**Files:**
- Modify: `README.md`
- Modify: `docs/BUILD_DEPLOY_SOP.md`
- Modify: `scripts/build_oci_image.py`

- [ ] Document sandbox candidate image and immutable promotion path.
- [ ] Keep current production latest documented until sandbox tag passes remote tests.
- [ ] Ensure build scripts can select `agent/Dockerfile.sandbox` without replacing the legacy Dockerfile prematurely.

### Task 4: Verify Locally And Build Remotely

**Files:**
- No source changes unless verification reveals a root cause.

- [ ] Run Python contract tests.
- [ ] Run Go tests for `agent` and `skills/doops-cli/cli/doops`.
- [ ] Build remote image tag `registry.example.com/lab/doops-agent:sandbox-20260509` using BuildKit.
- [ ] Run image self-checks for `/app/doops-agent -help`, `/usr/local/bin/do-agent --help`, and absence of the old token gateway port form.

### Task 5: Deploy Candidate And E2E Test

**Files:**
- No source changes unless verification reveals a root cause.

- [ ] Run candidate container under `doops-agent-sandbox-live` first.
- [ ] Verify ports 42222 and 9000.
- [ ] Run `doops exec` readonly check.
- [ ] Run `doops ask` readonly complex check.
- [ ] If all pass, update README with candidate digest and next promotion note.
