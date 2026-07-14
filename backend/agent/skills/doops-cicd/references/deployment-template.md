# DeploymentTemplate Contract

## Compatible Shape

Continue to accept the existing contract:

```yaml
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: example-release
  description: Release one declared application
spec:
  parameters:
    version:
      required: true
    reason:
      required: true
  application: example
  release:
    source:
      repository: https://example.invalid/org/repo.git
      revision: ${inputs.version}
      branch: main
  environment: production
  configurationSource: deploy/environments.yaml
```

The template is intentionally small. It identifies release intent and points to
the repository-owned environment declaration. Do not expand it into an
imperative step list.

## Validation

- Require `apiVersion: doops.sh/v2`.
- Require `kind: DeploymentTemplate`.
- Require `metadata.name`.
- Require `spec.application`, `spec.release`, `spec.environment` and
  `spec.configurationSource`.
- Permit existing parameter declarations with `required` and `default`.
- Resolve placeholders only in scalar values and only with exact
  `${inputs.<name>}` syntax.
- Reject unknown invocation inputs, unresolved placeholders, duplicate YAML
  keys and paths that escape the repository.
- Require exactly one of `release.source` or `release.manifest`.
- Treat repository revision, manifest digest and image digest declarations as
  immutable identities when present.

## Configuration Source

The referenced configuration owns:

- artifact contract and source-to-target mapping;
- physical target identity;
- executor type and settings;
- credential reference metadata;
- verification profiles;
- rollback capability and boundaries.

The Skill must not guess missing configuration from an environment name,
hostname, previous run, current cluster context or chat history.
