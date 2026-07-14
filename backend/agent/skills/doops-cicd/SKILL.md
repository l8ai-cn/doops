---
name: doops-cicd
description: Generate, review, dry-run, and execute doops.sh/v2 DeploymentTemplate YAML through the native doagent multi-agent engine and the DoOps modules exposed at runtime. Use for declarative build, artifact delivery, deployment, verification, rollback assessment, and release evidence tasks.
---

# DoOps CICD

Use one declarative `DeploymentTemplate` YAML as both the generated release
contract and the execution input. Let the native doagent multi-agent engine
plan, delegate, select Skills, and call the DoOps modules available at runtime.
Do not implement a second orchestration framework inside this Skill.

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

## Execute YAML

1. Load exactly one `DeploymentTemplate`.
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
8. Re-observe real state after every mutation and write the run result described
   in the execution contract.

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
loads the YAML from that session and returns one structured `DeploymentRun`
result with `status.mutationCount` and structured evidence. Use `--allow-mutate`
only for an explicitly authorized apply request.
The adapter rejects missing evidence, unsupported phases, synthetic
`admitted`, and any non-zero dry-run mutation count.

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
- Do not weaken tests, verification declarations or rollback requirements to
  make a run pass.
- Use generic DoOps Exec only when it is an explicitly selected runtime module
  and the YAML declaration authorizes that operation; record the exact action
  and result as mutation evidence.

## Completion

Return success only when every verification declared by the template and its
configuration source has current runtime evidence. Otherwise return a blocked,
failed, or outcome-unknown result with the exact unmet declaration.
