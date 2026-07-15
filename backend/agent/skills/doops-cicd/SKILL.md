---
name: doops-cicd
description: Generate, review, dry-run, and execute doops.sh/v2 DeploymentTemplate, SemanticRelease, and ServiceRelease YAML through the native doagent multi-agent engine and the DoOps modules exposed at runtime. Use for declarative build, artifact delivery, deployment, verification, rollback assessment, and release evidence tasks.
---

# DoOps CICD

Use one declarative `DeploymentTemplate` YAML as both the generated release
contract and the execution input. Let the native doagent multi-agent engine
plan, delegate, select Skills, and call the DoOps modules available at runtime.
Do not implement a second orchestration framework inside this Skill.

For applications with independently selectable services, use one
`SemanticRelease` composition and referenced `ServiceRelease` documents. The
composition defines semantic intents and dependencies; service documents define
artifact, workload binding and evidence requirements. Neither document contains
commands or fixed runtime tool choices.

## Select The Operation

- **generate**: create or update a `doops.sh/v2` `DeploymentTemplate` from the
  user's release intent and repository declarations.
- **review**: validate one template and its referenced configuration without
  contacting or mutating a target.
- **dry-run**: resolve inputs, inspect source and target facts, and report the
  mutations still required. Perform zero mutations.
- **apply**: execute the template only when the request explicitly authorizes
  mutation, then observe and verify the resulting state.

Read [references/deployment-template.md](references/deployment-template.md)
before generating or validating YAML. Read
[references/execution-contract.md](references/execution-contract.md) before
dry-run or apply.

## Generate YAML

1. Inspect the repository's existing workflow and environment declarations.
2. Preserve the existing `DeploymentTemplate` shape whenever it can express
   the request.
3. Declare parameters instead of embedding request-specific values.
4. Keep environment-specific target, credentials metadata, artifact mapping,
   executor settings, verification and rollback declarations in the referenced
   configuration source.
5. Validate YAML syntax and required fields before returning the file.

Do not add command lists, embedded shell scripts, hidden defaults, credentials,
or environment-specific behavior to the Skill.

For a single release unit, return exactly one `DeploymentTemplate` artifact.
For independently selectable services, return one `SemanticRelease` root and
the referenced `ServiceRelease` documents:

```json
{
  "apiVersion": "doops.sh/v2",
  "kind": "DeploymentTemplate",
  "metadata": {"name": "declared-name"},
  "spec": {"application": "declared-application"}
}
```

Every artifact must be valid YAML when materialized, and the response must
identify each repository-relative path. Do not return an imperative plan,
command list or textual-only proposal.

## Review Output

Return exactly one structured `DeploymentReview` result for `review`:

```json
{
  "apiVersion": "doops.sh/v2",
  "kind": "DeploymentReview",
  "metadata": {"workflow": "repository-relative-path"},
  "status": {
    "phase": "valid",
    "errors": [],
    "warnings": []
  }
}
```

`phase` must be `valid` or `invalid`. A review must not contact or mutate a
target, and warnings must not hide a validation error.

## Execute YAML

1. Load exactly one root `DeploymentTemplate` or `SemanticRelease`.
2. Resolve every `${inputs.<name>}` from explicit invocation inputs or declared
   defaults. Stop on missing or unknown inputs.
3. Load `configurationSource` relative to the repository containing the
   template. Stop on path escape, missing files, duplicate keys or invalid YAML.
4. Resolve the declared environment, release, artifact contract, executor,
   verification profile and rollback capability.
5. Discover the Skills, agents, tools and DoOps modules actually exposed by the
   runtime. Missing required capability is a blocking result.
6. Delegate independent source, artifact, deployment and verification work to
   native subagents when the runtime supports multi-agent execution.
7. Execute only the resolved declaration. Never infer another target,
   repository, namespace, release, credential or verification endpoint.
8. Re-observe real state after every mutation and write the single JSON
   `DeploymentRun` artifact described in the execution contract.

For a `SemanticRelease`:

1. Resolve every `serviceCatalog.<name>.uses` path relative to the composition
   document and load exactly one `ServiceRelease` from each path.
2. Reject path escape, duplicate service names, duplicate selections, unknown
   selections, empty selections and service identity mismatches.
3. Resolve the explicit selected-services set. `all` means every catalog entry;
   no other implicit selection is allowed.
4. Build a runtime graph from the declared intents, `requires`, `produces` and
   `satisfies` relationships.
5. Run selected-service artifact and verification intents concurrently when
   dependencies and runtime capabilities permit.
6. For `converge-shared-release`, observe every unselected service image
   identity and bind it as `preserve-observed`.
7. Aggregate selected image bindings and preserved identities into one shared
   release convergence. Service documents must not invoke the deployment
   executor independently.
8. Re-observe selected and unselected workloads and bind all observations to
   the final `DeploymentRun`.

The compatibility CLI adapter invokes the same chain without creating a second
control plane:

```text
doops -session <session> cicd run \
  -f <repository-relative-workflow.yaml> \
  -target <configured-doops-target> \
  --dry-run \
  --set <parameter>=<value>
```

The adapter first pushes the repository snapshot into the declared session,
then sends one `doops_agent_prompt` request selecting `$doops-cicd`. The Skill
loads the YAML only after the Gateway verifies the supplied workspace commit
against the session-ready commit, then returns one structured `DeploymentRun`
result with `status.mutationCount` and structured evidence. Without
`--dry-run`, `cicd run` is an explicit apply request and the Gateway selects the
native doagent mutation-capable mode; no separate mutation flag exists.
The adapter rejects missing workspace binding, capability snapshot, result
digest or evidence, unsupported phases, synthetic `admitted`, and any non-zero
dry-run mutation count.

Every evidence item must cite the completed runtime tool call that produced it.
The Gateway creates the runtime attestation from ACP tool-call events and
computes the final result digest; Agent-authored text cannot substitute for it.

When the Ask request requires `response_format=json`, the Gateway appends the
only permitted machine-result path to the prompt. Write exactly one JSON object
to that path using a temporary file and atomic rename. The JSON object must be
the same `DeploymentRun` reported to the coordinator; do not write YAML,
Markdown, a second result file, or a textual substitute.

The coordinator owns the final decision. A subagent or module response is
evidence only after the coordinator verifies that it belongs to this run and
the declared subject.

## Safety Rules

- Treat dry-run as read-only. Report planned mutations without performing them.
- Require explicit apply authorization before the first mutation.
- Stop when a required module, permission, credential reference or declaration
  is missing.
- Never use fallback, silent downgrade, guessed defaults, stale evidence or
  textual success.
- Never expose Secret values. Record only declared credential reference
  metadata and availability.
- Do not create deployment scripts as an alternate execution path.
- Do not activate the legacy `pipeline`, `shell`, `k8s`, or `image-build`
  orchestration Skills while executing this Skill. Use the runtime modules
  actually exposed to doagent; missing typed responsibility is blocking.
- Do not weaken tests, verification declarations or rollback requirements to
  make a run pass.
- Use generic DoOps Exec only when it is an explicitly selected runtime module
  and the YAML declaration authorizes that operation; record the exact action
  and result as mutation evidence.

## Completion

Return success only when every verification declared by the template and its
configuration source has current runtime evidence. Otherwise return a blocked,
failed, or outcome-unknown result with the exact unmet declaration.
