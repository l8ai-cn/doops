# DeploymentTemplate Contract

## Semantic Composition

Use `SemanticRelease` when an application has independently selectable release
units that share an environment-level deployment boundary. A composition
references reusable `ServiceRelease` documents:

```yaml
apiVersion: doops.sh/v2
kind: SemanticRelease
metadata:
  name: example-production-release
spec:
  application: example
  environment: production
  configurationSource: deploy/environments.yaml
  parameters:
    revision:
      required: true
    version:
      required: true
    reason:
      required: true
    services:
      required: true
  serviceCatalog:
    api:
      uses: services/api.yaml
  flow:
    - intent: materialize-selected-artifacts
      forEach: selected-services
      produces:
        - source-artifact
    - intent: converge-shared-release
      requires:
        - source-target-digest-equal
      scope:
        selectedImageBindings: update
        unselectedImageBindings: preserve-observed
```

Each referenced file must be `doops.sh/v2 ServiceRelease` and must declare a
service identity, source context, artifact repository, deployment binding,
semantic intents and required evidence.

Resolve references relative to the composition file. Reject path escape,
duplicate keys, unknown or duplicate selections, mismatched service identity,
missing intents and unresolved dependencies. Semantic intents describe desired
outcomes; they must not contain commands, scripts or fixed tool names.

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
- Require `kind: DeploymentTemplate`, `SemanticRelease`, or `ServiceRelease`.
- Require `metadata.name`.
- For `DeploymentTemplate`, require `spec.application`, `spec.release`,
  `spec.environment` and `spec.configurationSource`.
- For `SemanticRelease`, require `spec.application`, `spec.release`,
  `spec.environment`, `spec.configurationSource`, `spec.serviceCatalog` and a
  non-empty semantic `spec.flow`.
- For `ServiceRelease`, require `spec.application`, `spec.service`,
  `spec.source`, `spec.artifact`, `spec.binding`, non-empty `spec.intents`, and
  non-empty required evidence.
- Permit existing parameter declarations with `required` and `default`.
- Resolve placeholders only in scalar values and only with exact
  `${inputs.<name>}` syntax.
- Reject unknown invocation inputs, unresolved placeholders, duplicate YAML
  keys and paths that escape the repository.
- For `DeploymentTemplate`, require exactly one of `release.source` or
  `release.manifest`.
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
