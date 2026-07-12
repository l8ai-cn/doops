# Semantic CI/CD Reconcile Protocol

`doops_cicd_reconcile` is the only CI/CD execution boundary. It accepts one
immutable `doops.sh/v2` `DeploymentPlan`; it does not accept workflow stages,
task names, shell commands, or deployment scripts.

## Plan Attestation

`cicd plan` only renders a plan for review. `cicd run` requires
`DOOPS_CICD_PLAN_SIGNING_KEY`, a base64 Ed25519 private key held only by the
approved CI compiler. The compiler signs the immutable plan digest with issuer
`doops-cicd-compiler`.

The Gateway requires the matching base64 Ed25519 public key in
`DOOPS_CICD_PLAN_PUBLIC_KEY` and verifies the signature before capability
negotiation, session creation, or route relay. A caller holding only the
`reconcile` permission cannot manufacture a deployable plan.

The Agent listener binds to loopback by default. Production reconciliation
travels through the authenticated reverse Gateway tunnel, where the resolved
plan `cluster`/`instance` must exactly match the connected Agent route.

## Capability

Before a reconcile request, the gateway calls the doagent `initialize` method.
The result must advertise:

```json
{
  "capabilities": {
    "cicdStructuredReport": true
  }
}
```

Without this capability the gateway rejects the request before creating an
agent session. The gateway never converts agent text into a deployment result.

## Input

The MCP tool input is:

```json
{
  "session_id": "release-session",
  "dry_run": false,
  "plan": {
    "apiVersion": "doops.sh/v2",
    "kind": "DeploymentPlan",
    "digest": "sha256:...",
    "spec": {
      "release": {
        "source": {
          "repository": "https://example.test/zhiyong.git",
          "revision": "0123456789abcdef0123456789abcdef01234567"
        }
      },
      "target": {
        "environment": "test",
        "executionTarget": "gw-oilan-node",
        "profileDigest": "sha256:...",
        "profile": {
          "target": "gw-oilan-node",
          "cluster": "doops-oilan",
          "instance": "oilan-node"
        }
      },
      "artifactContract": {
        "sourceRepository": "https://example.test/zhiyong.git",
        "sourceBranch": "main",
        "services": ["zhiyong-exam-api"],
        "imageReferenceFormat": "repository@digest",
        "manifestRepository": "registry.example.test/releases"
      },
      "desiredState": {
        "application": "zhiyong",
        "delivery": "build-immutable-release",
        "configurationSource": "deploy/environments.yaml",
        "authorization": "reconcile"
      },
      "acceptance": {
        "requiredEvidence": ["source-identity", "runtime-state"],
        "requiredFailureEvidence": ["rollout-status", "rollback-state"]
      },
      "policy": {
        "mutation": "require-explicit-approval",
        "convergence": "until-verified",
        "failureMode": "restore-last-known-good"
      }
    }
  }
}
```

The gateway recomputes `digest` with the plan `digest` field cleared and rejects
any mismatch. It also recomputes `profileDigest`, requires a 40-character Git
commit for source releases, and requires `sha256:<64 hex>` for promoted manifest
releases. The gateway route must exactly match the resolved profile
`cluster`/`instance`; a configured CLI target must make the same match before
opening the gateway connection.

The complete environment profile and artifact contract are part of the
immutable plan. The reconciler must verify the plan's source, target profile,
and evidence before reporting convergence.

## Output

The doagent completes a reconciliation only by emitting this ACP SSE event:

```json
{
  "jsonrpc": "2.0",
  "method": "session/update",
  "params": {
    "update": {
      "sessionUpdate": "agent_report",
      "report": {
        "planDigest": "sha256:...",
        "status": "Reconciling",
        "evidence": [
          {
            "kind": "source-identity",
            "reference": "git:0123456789abcdef0123456789abcdef01234567"
          }
        ],
        "violations": []
      }
    }
  }
}
```

`status` is one of `Pending`, `Reconciling`, `Converged`, `Blocked`, or
`Failed`. Every evidence entry requires non-empty `kind` and `reference`.
Every violation requires non-empty `code` and `message`.

Evidence must be emitted by deterministic checks against the declared target,
such as Git identity, OCI manifest inspection, Kubernetes rollout state, or
HTTP acceptance checks. Agent-generated prose is not evidence. The current
Gateway validates the evidence contract and retains every terminal report in
its audit trail; the doagent capability remains responsible for producing
truthful target observations.

`agent_message`, a textual JSON block, `PASS`, and a generated `deploy.sh`
are never valid completion signals.

For `Converged`, the report must contain every `requiredEvidence` kind. For
`Blocked` or `Failed`, it must contain every `requiredFailureEvidence` kind and
at least one violation. Gateway audit preserves the plan digest, structured
evidence, and violations for every terminal report; `Blocked` and `Failed` are
recorded as failed operations.

## Bounded Reconciliation

The CLI evaluates required evidence against the plan. It continues only while
the evidence or violations change. It stops with `Blocked` after the configured
maximum no-progress count or iteration count, and with `Failed` for protocol or
execution errors. Only complete required evidence produces `Converged`.
