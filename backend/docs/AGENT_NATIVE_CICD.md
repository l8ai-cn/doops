# Agent-Native CI/CD

DoOps 不实现 Agent 框架。DoOps keeps CI/CD on the existing Agent-native path:

```text
SemanticRelease -> doops push -> Ask -> doagent -> $doops-cicd -> runtime attestation
```

There is one Agent-native release path. It does not introduce another controller,
private deployment API, shell adapter, or external release event.

## Responsibilities

| Component | Responsibility |
| :--- | :--- |
| `doops` CLI | Resolve an explicit workflow file and target, push the repository snapshot, and issue one ordinary Ask |
| Gateway | Authenticate the target, enforce the session/resource boundary, route Ask, and bind the result to authoritative ACP tool-call events |
| `doops-agent` | Bind the doagent session to the pushed workspace and expose ordinary DoOps primitives |
| `doagent` | Select the Skill, coordinate multiple agents, call available modules, and report observed facts |
| `$doops-cicd` Skill | Read the declarative workflow, select existing DoOps modules, execute dry-run/apply within authorization, and write the Gateway-designated JSON `DeploymentRun` artifact |

DoOps does not implement planning, Skill routing, tool registries, retry loops,
or recovery policy in a second control plane. The Gateway only attests completed
runtime tool calls and computes the result digest. doagent
retains native context, planning, 多 Agent delegation, Skill composition, and
tool selection.

## CLI Contract

The only CI/CD entry is:

```text
doops -session <session> cicd run -f <workflow.yaml> -target <configured-target> \
  [--dry-run] [--set key=value ...]
```

The CLI must receive an explicit configured target. It never infers a cluster,
namespace, registry, release, or credentials from names or historical state.
Before Ask it pushes the repository into the session workspace and passes the
workspace commit to the Gateway and Skill as transport context. The Gateway
compares that value with the session `.doops-ready` commit before contacting
doagent; a mismatch fails closed. A failed push fails the operation; it does not
fall back to shell, kubectl, SSH, or a manual release path.

The CLI sends a small task envelope to ordinary `doops_agent_prompt`:

```json
{
  "task": "execute-doops-cicd-workflow",
  "skill": "$doops-cicd",
  "executionMode": "dry-run",
  "workflowPath": "backend/deploy/workflows/example.yaml",
  "workspaceCommit": "<pushed workspace commit>",
  "inputs": {}
}
```

`--dry-run` is read-only and requires mutation count zero. Without it, the run
is an explicit apply request and the Gateway selects the native doagent
mutation-capable mode. There is no separate mutation flag.

## Gateway And Agent

All ordinary prompts use the Gateway `ask` action and a session resource key.
The Gateway does not synthesize observations or convert a text response into
deployment evidence. It records completed ACP tool calls and rejects evidence
that does not reference one of those calls.

`response_format=json` remains a generic structured-result transport for callers
that need it. The bridge only accepts the artifact written by doagent, waits
for the authoritative `turn_finished` event, adds the runtime tool-call
attestation, and computes the final result digest.

Permission requests from doagent are fail-closed. DoOps never answers a
`permission.updated` request on behalf of the user or the Agent engine.

## Skill Result

For `response_format=json`, the Gateway supplies the sole result-artifact path.
`$doops-cicd` writes exactly one JSON result to that path:

```json
{
  "apiVersion": "doops.sh/v2",
  "kind": "DeploymentRun",
  "spec": {
    "mode": "dry-run"
  },
  "status": {
    "phase": "planned",
    "mutationCount": 0,
    "evidence": [
      {
        "subject": "source",
        "module": "source-observer",
        "toolCallId": "completed-runtime-call",
        "observedAt": "2026-07-15T00:00:00Z",
        "result": {
          "revision": "immutable-revision"
        }
      }
    ]
  }
}
```

Valid phases are `planned`, `converged`, `blocked`, `failed`, and
`outcome-unknown`. A successful result must contain evidence from real module
outputs. The literal text `admitted`, an Agent-authored attestation, or an Agent
claim without a completed runtime `toolCallId` is not evidence.

The Skill owns the workflow-specific schema and evidence requirements. It must
use existing DoOps modules and preserve credential boundaries: references and
metadata may be used, but Secret values are not read into prompts, logs, or
results.

## Retained Surface

The retained Go surface is ordinary Ask, workspace/session transport, generic
structured results, target authorization, audit, and existing primitive tools.

## Code Map

| Responsibility | Code |
| :--- | :--- |
| Thin CI/CD CLI | `backend/skills/doops-cli/cli/doops/workflow.go` |
| Workspace push | `backend/skills/doops-cli/cli/doops/push.go` |
| Ask routing and ACP event forwarding | `backend/agent/internal/server/handler_ws.go` |
| Gateway authorization and resource locking | `backend/agent/internal/server/tunnel_hub.go` |
| Workspace upload | `backend/agent/internal/server/workspace_upload.go` |
| Ask request types | `backend/agent/api/mcp.go` |
| CI/CD Skill | `backend/agent/skills/doops-cicd/SKILL.md` |
