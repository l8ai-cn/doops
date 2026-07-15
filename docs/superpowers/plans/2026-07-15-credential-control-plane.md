# DoOps Credential Control Plane Implementation Plan

## Outcome

DoOps manages versioned personal and platform credentials as first-class
Gateway resources. A `doops.sh/v2` deployment references credentials by name.
The Gateway authorizes each reference against the authenticated actor and
declared deployment context. The Agent materializes only the authorized
purpose on the selected target and returns non-sensitive evidence.

No credential value enters Git, a workspace file, a command argument, an Agent
prompt, an ACP result, an audit tail, or a CLI history record.

## Trust Boundaries

1. The CLI and deployment repository are untrusted declarations.
2. The Gateway is the credential policy decision point and encrypted payload
   store.
3. The authenticated Gateway-to-Agent WebSocket is the only payload transport.
4. The Agent is trusted only for the target to which it is registered.
5. Kubernetes, OCI, Helm, and Git endpoints are external verification systems.

The Gateway must not trust deployment context supplied only by CLI flags.
After the workspace push, it asks the selected Agent to parse the bound
workspace commit and return credential reference metadata. The Gateway checks
that metadata against grants before any payload leaves the credential store.

## Data Model

- `CredentialResource`: stable identity, name, scope (`personal` or
  `platform`), owner, type, lifecycle state, fixed materialization policy.
- `CredentialVersion`: immutable encrypted payload, payload digest, state
  (`staged`, `active`, `superseded`, `revoked`), creator and timestamps.
- `CredentialGrant`: resource, target cluster/instance, project, environment,
  template, namespace, allowed use, creator, revocation state.
- `CredentialBundle`: stable named collection of resource references and uses;
  it contains no payload.
- `CredentialVerification`: version, target context, materialized resource
  identity, status, resourceVersion/digest/error category and timestamp.

Database foreign keys and uniqueness constraints enforce one active version,
stable names within ownership scope, and idempotent grants.

## Key Management

`DOOPS_GATEWAY_SECRET_KEY` is mandatory when credential payloads or encrypted
Git repository passwords exist. Startup and every secret operation fail closed
when the key is absent or invalid. The Gateway must not auto-generate a key
beside `gateway.db`.

Ciphertext uses versioned AES-256-GCM envelopes with associated data binding the
credential resource ID, version ID, type, and payload digest. A ciphertext
copied to another resource or version must fail authentication.

## Permission Model

New actions:

- `credential:create`
- `credential:metadata`
- `credential:grant`
- `credential:use`
- `credential:rotate`
- `credential:revoke`
- `credential:audit`

Personal credential owners can update and rotate their own payload, but can
grant only deployment scopes for which they already have target permission.
Platform resources and grants require `credential:grant` or `admin`.

Use requires both:

1. the deployment action permission for the selected target; and
2. a matching active credential grant for actor, target, project, environment,
   template, namespace, credential use, and credential state.

## Runtime Protocol

1. `doops cicd run` pushes the workspace and receives its immutable commit.
2. CLI calls the Gateway credential prepare endpoint with target, session,
   workflow path, workspace commit, and mode.
3. Gateway calls an internal Agent tool to parse the bound workflow and
   configuration source. The tool rejects path escape, duplicate YAML keys,
   inline secret-like fields, unknown credential fields, and unresolved refs.
4. Agent returns only deployment context and credential reference metadata.
5. Gateway resolves bundles and active versions, evaluates permissions and
   grants, and records an audit start.
6. Dry-run returns planned materializations with mutation count zero.
7. Apply sends one purpose-bound materialization request per resolved
   credential to the registered Agent. The payload is never forwarded to the
   client or doagent.
8. Agent validates the type schema, materializes the target representation,
   performs type-specific verification, and returns structured evidence.
9. Gateway persists redacted verification and audit records and returns a
   `CredentialRun`.
10. CLI includes only the CredentialRun ID and materialized resource metadata
    in the ordinary doops-cicd prompt.

Failure stops the deployment before the doops-cicd apply prompt. There is no
fallback to an existing Secret, inline YAML, shell, SSH, or another target.

## Typed Materializers

- `registry`: create `kubernetes.io/dockerconfigjson`, optionally patch the
  declared workload `imagePullSecrets`, verify exact type/key set and OCI
  manifest digest.
- `tls`: create `kubernetes.io/tls`, validate certificate/private-key match,
  return fingerprint and expiry.
- `opaque`: create `Opaque` with the resource's allowlisted exact key set.
- `helmRepository`: create purpose-bound `Opaque` repository credentials and
  verify the repository index without returning response bodies.
- `gitToken`: create purpose-bound `Opaque` Git credentials and verify remote
  metadata using an in-memory/temporary askpass channel with guaranteed cleanup.

Kubernetes objects receive DoOps ownership labels and annotations containing
resource/version IDs and payload digest, never payload values.

## Rotation And Revocation

Rotation creates a staged immutable version. It is promoted only after all
required selected grants have current successful verification. If a target
mutation succeeds and a later target fails, the Gateway rematerializes the
previous active version on successful targets and records rollback evidence.
An unknown rollback result leaves the staged version unpromoted and reports
`outcome-unknown`.

Gateway serializes credential target mutations across prepare, verification,
promotion, and revocation so concurrent requests cannot leave the active
database version different from the materialized target version.

An individual grant can be revoked without revoking the Credential Resource.
Grant revocation blocks future authorization immediately; target objects remain
owned by the Credential Resource and are removed only by resource revocation.

Revocation blocks new use before Agent contact, marks the version/resource
revoked, and schedules or explicitly performs removal of DoOps-owned target
objects. A target object whose ownership metadata does not match is never
deleted.

## Verification

Allowed evidence:

- credential resource/version IDs;
- target, namespace, generated resource name;
- Secret type and sorted key names;
- Kubernetes resourceVersion;
- OCI manifest digest;
- TLS fingerprint and expiration;
- Helm/Git endpoint identity and status;
- error category and correlation ID.

Tests must scan API responses, CLI output, logs, audits, `DeploymentRun`,
workspace files, and serialized errors for seeded canary values.

## Delivery Gates

1. Store and API tests fail before implementation and pass afterward.
2. Permission tests cover owner/admin/user, granted/ungranted target,
   namespace, environment, template, and use.
3. Agent tests use fake `kubectl`, OCI, Helm, and Git endpoints and assert exact
   requests plus zero payload leakage.
4. Gateway-Agent integration proves payload transport is not forwarded to the
   client or audit.
5. CLI functional tests prove stdin-only payload input and workflow preflight.
6. Full Go, Python contract, TypeScript, and production web builds pass.
7. Independent security and code review have no unresolved blocking findings.
8. Commit is pushed, CNB CI succeeds, immutable images are published.
9. GitOps deployment succeeds, Gateway/Agent health is green, and a synthetic
   authenticated registry on a non-production namespace proves create, pull,
   rotate, revoke, cleanup, audit redaction, and failure behavior.
