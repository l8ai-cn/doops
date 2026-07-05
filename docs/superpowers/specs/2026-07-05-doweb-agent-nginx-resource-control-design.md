# DoWeb Agent Nginx Resource Control Design

## Goal

DoWeb should become a natural-language control plane for Nginx-backed web
resource publishing. Users describe what they want to publish, which domain and
certificate to use, who may access it, and whether it should proxy or balance
traffic. DoWeb then converts the request into a structured resource
specification, renders candidate Nginx configuration, validates it, reloads
Nginx, checks the result, records evidence, and rolls back on failure.

The product reference is a mature application delivery security gateway. DoWeb
should not copy that gateway's UI first. It should absorb the resource model and
change discipline behind it so the Agent can manage Nginx safely.

## Current Context

The current DoWeb skill already has a useful deployment core:

- `install-nginx` bootstraps Docker Nginx by default and supports explicit host
  mode for an existing host Nginx.
- `domain-set` and `domain-list` maintain base-domain metadata.
- `deploy` publishes a customer-built `dist` directory, assigns domains, renders
  server blocks, validates with `nginx -t`, reloads, records state, and can run
  HTTP checks.
- `render`, `config-set`, `snippet-set`, `check`, and `rollback` provide direct
  Nginx operation paths with validation.
- The existing SiteSpec supports static sites, reverse proxy locations, upstream
  groups, and simple TLS certificate paths.

The next design step is to move from a deploy command with flags to a richer
resource control model that can represent certificate lifecycle, network
groups, path policies, load balancing, and advanced proxy options.

## Reference Capability Map

The reference gateway screenshots imply these core capabilities:

- Resource metadata: group, name, description, alert level, tags, publish state.
- Publish endpoint: protocol mode, domain, optional DNS automation, IPv4/IPv6
  mode.
- Backend binding: proxied URL, static site, or multiple backend servers.
- TLS lifecycle: existing certificate, local upload, free certificate, or
  self-signed certificate.
- Access policy: effective period, named policy, internal and external network
  access, and resource-level allow or deny behavior.
- Load balancing: enable switch, algorithm, backend protocol, active/passive
  health logic, timeout, retry count, backend list, status, and actions.
- Advanced proxy settings: host routing priority, strong TLS settings, SNI,
  keepalive, real client IP forwarding, WebSocket proxying, NTLM, stats,
  anti-hotlink, rewrite, human verification, timeout.
- Path access control: path, exact or fuzzy match, IP group, allow or block.
- Future acceleration: path/backend selection by IP group, cache, and
  acceleration settings.

## Design Principles

DoWeb must keep a structured model between natural language and Nginx. The
Agent may infer missing low-risk defaults, but it must ask for missing domains,
certificate intent, backend URLs, or artifact paths when those choices affect
production traffic.

DoWeb must not use silent fallback. If DNS, certificate validation, `nginx -t`,
reload, health checks, or rollback fails, the operation reports the exact
failure and stops. It must not silently switch protocol, disable TLS, remove an
access policy, or publish under a different domain.

The default path should be conservative and verifiable. Direct config editing is
allowed only through managed commands that preserve validation, reload, evidence,
and rollback behavior.

## Product Model

```text
Natural-language request
  -> intent extraction
  -> ResourceSpec
  -> validation and conflict checks
  -> rendered Nginx candidate
  -> diff and change evidence
  -> nginx -t
  -> reload
  -> HTTP/HTTPS checks
  -> state record or rollback
```

`ResourceSpec` is the primary model. `SiteSpec` becomes the Nginx-renderable
view derived from it.

## ResourceSpec

```yaml
resource:
  id: demo
  group: default
  name: Demo Site
  description: Customer-built frontend release
  alert_level: default
  tags: []
  enabled: true

publish:
  domain: demo.aihub.org.cn
  protocol: http_https        # http | https | http_https
  http_https_mode: redirect   # redirect | serve_both
  ip_family: ipv4             # ipv4 | dual_stack | ipv6
  auto_dns: false

backend:
  mode: static                # static | proxy | upstream
  static_root: /var/lib/doweb/sites/demo/current
  proxy_url: ""
  upstream_id: ""

tls:
  mode: existing              # none | existing | upload | self_signed | acme
  cert_id: cert-aihub
  cert_path: /etc/doweb/certs/cert-aihub/fullchain.pem
  key_path: /etc/doweb/certs/cert-aihub/privkey.pem

access:
  policies:
    - id: internal-admin
      scope: path             # resource | path
      path: /admin
      match: prefix           # exact | prefix
      ip_group: internal
      action: allow           # allow | deny
      effective_from: ""
      effective_to: ""

proxy_options:
  preserve_host: true
  real_ip: true
  websocket: false
  keepalive: false
  timeout_seconds: 60
  sni: false
  strong_tls: true
```

## Supporting Registries

DoWeb needs small local registries before adding a full Server UI:

- Domain registry: already started under `/var/lib/doweb/domains`.
- Resource registry: `/var/lib/doweb/resources/<resource_id>.json`.
- Certificate registry: `/var/lib/doweb/certs/<cert_id>.json`.
- Network group registry: `/var/lib/doweb/network-groups/<group_id>.json`.
- Access policy registry: `/var/lib/doweb/access-policies/<policy_id>.json`.
- Upstream registry: `/var/lib/doweb/upstreams/<upstream_id>.json`.

Each registry record must be plain JSON, deterministic, and safe to diff. Secret
material such as private keys is stored as files with restricted permissions and
is referenced by ID or path, not copied into resource state.

## Nginx Rendering Rules

Publish protocol maps to listeners:

- `http`: render port 80 only.
- `https`: render port 443 only and require valid TLS material.
- `http_https`: render both 80 and 443. The default is
  `http_https_mode: redirect`, where port 80 redirects to HTTPS. Use
  `http_https_mode: serve_both` only when the user explicitly wants both
  protocols to serve content directly.

IP family maps to `listen` directives:

- `ipv4`: IPv4 listeners only.
- `dual_stack`: IPv4 and IPv6 listeners.
- `ipv6`: IPv6 listeners only.

Backend mode maps to locations:

- `static`: `root`, `index`, and optional SPA fallback.
- `proxy`: root or path `proxy_pass` with explicit headers and timeout.
- `upstream`: named `upstream` block plus proxy locations.

Access policies map to `allow` and `deny` directives inside the relevant
`server` or `location` block. Path policies have higher priority than
resource-level policies because Nginx evaluates location-specific directives
more narrowly.

Advanced proxy options map to explicit directives:

- `preserve_host`: `proxy_set_header Host $host` or upstream host behavior.
- `real_ip`: `X-Real-IP` and `X-Forwarded-For`.
- `websocket`: `Upgrade` and `Connection` headers.
- `timeout_seconds`: `proxy_connect_timeout`, `proxy_send_timeout`, and
  `proxy_read_timeout`.
- `strong_tls`: managed SSL protocols and ciphers.
- `sni`: `proxy_ssl_server_name on` for HTTPS upstreams.

Unsupported advanced gateway features must remain visible as unsupported in the
Agent response. They must not be pretended through unrelated Nginx snippets.

## TLS Lifecycle

Certificate operations are first-class operations, not raw file edits.

Supported first-stage operations:

- Register an existing certificate and key path.
- Upload certificate/key files into the DoWeb certificate store.
- Generate a self-signed certificate for a development domain.
- Bind a certificate ID to a resource.
- Rotate a resource certificate.
- Inspect certificate subject, SANs, issuer, and expiration.

Validation requirements:

- Certificate and private key must match.
- Certificate must cover the requested domain unless the user explicitly marks
  the resource as development/self-signed.
- Files must be readable by Nginx and not world-writable.
- Rotation must render a candidate config, run `nginx -t`, reload, and check
  HTTPS before reporting success.

ACME or cloud-provider free certificate automation is a later stage. The model
reserves `tls.mode: acme`, but implementation should not fake it before a real
issuer flow exists.

## Network Groups and Access Policies

Network groups define reusable CIDR sets:

```yaml
id: internal
name: Internal Network
cidrs:
  - 10.0.0.0/8
  - 172.16.0.0/12
  - 192.168.0.0/16
```

Access policies reference network groups and may be attached to a whole
resource or to a path. Policies can be always active or time-bounded. The first
implementation should store effective dates and enforce the currently active
policy set during render and check operations. Guaranteed automatic policy
expiry requires a later scheduler or monitor.

Examples:

- "Only internal users can access `/admin`."
- "Public users can access the site, but block `/debug`."
- "Allow both internal and external access for this resource."

If a referenced network group does not exist, DoWeb must stop and ask for the
CIDRs instead of guessing.

## Load Balancing

Upstreams should become managed objects:

```yaml
id: api-pool
algorithm: least_conn         # round_robin | ip_hash | least_conn
backend_protocol: http        # http | https
passive_health:
  max_fails: 3
  fail_timeout_seconds: 30
endpoints:
  - name: api-1
    url: http://10.0.0.11:3000
    enabled: true
  - name: api-2
    url: http://10.0.0.12:3000
    enabled: true
```

First-stage load balancing should use Nginx OSS capabilities: round-robin,
`ip_hash`, `least_conn`, `max_fails`, and `fail_timeout`. Active health checks
are not available in open-source Nginx without extra modules, so they should be
implemented as DoWeb-side external probes or deferred. DoWeb must state this
clearly when users ask for active checks.

## Natural-Language Guidance

The Agent should normalize common user intents:

- "Publish this dist to aihub.org.cn" -> static resource with domain assignment.
- "Proxy this domain to an API" -> proxy backend with root or path location.
- "Use these two backends for load balancing" -> upstream resource.
- "Only internal can access admin" -> network group plus path policy.
- "Replace the HTTPS certificate" -> certificate validation and rotation.
- "Enable WebSocket" -> explicit proxy option.

The Agent should ask one focused question when a required production decision is
missing. Examples:

- Missing backend URL for a proxy.
- Missing domain or base domain.
- Missing certificate source for HTTPS-only publishing.
- Missing CIDR list for an unknown internal network group.

## Change Safety

Every write operation must produce evidence:

- Parsed intent and ResourceSpec.
- Candidate Nginx config path.
- Config diff or new config content.
- `nginx -t` result.
- Reload command result.
- HTTP/HTTPS health check result.
- State record path.
- Rollback result on failure.

Rollback must restore the previous Nginx config and previous site release or
certificate binding where applicable. If rollback cannot complete, DoWeb must
surface that failure as an operator action item.

## Phased Delivery

### Phase 1: Resource, TLS, and Access Core

- Add ResourceSpec registry.
- Add certificate registry and validation.
- Add network group registry.
- Add resource-level and path-level access policies.
- Render HTTP/HTTPS protocol modes and IP family.
- Render proxy options for real IP, WebSocket, timeout, and host behavior.
- Preserve the existing deploy, check, and rollback safety flow.

### Phase 2: Managed Load Balancing

- Add upstream registry.
- Render algorithms and passive health settings.
- Add backend enable/disable operations.
- Add DoWeb-side active probe reports without pretending they are Nginx OSS
  active health checks.

### Phase 3: Server API and UI

- Expose the resource, certificate, network group, access policy, and upstream
  registries through DoWeb Server.
- Add resource list, resource edit, certificate management, access policy, and
  load-balancing screens inspired by the reference product.
- Keep Agent operations and UI operations on the same ResourceSpec model.

### Phase 4: DNS and Certificate Automation

- Integrate provider DNS automation where credentials are available.
- Add ACME or provider-backed certificate issuance.
- Add renewal monitoring and certificate expiration alerts.

## Acceptance Scenarios

### Static frontend with HTTPS

Given a built `dist` directory and a registered certificate covering
`demo.aihub.org.cn`, when the user asks DoWeb to publish the site with HTTP and
HTTPS, then DoWeb records a resource, renders both listeners, validates Nginx,
reloads, checks the site, and records the release.

### Reverse proxy

Given `proxy-106.aihub.org.cn`, when the user asks DoWeb to proxy it to
`http://106.54.197.139/`, then DoWeb renders a root proxy location without a
duplicate static root location, validates, reloads, and checks the proxied site.

### Certificate rotation

Given a resource currently using certificate `cert-old`, when the user asks to
switch to `cert-new`, then DoWeb verifies the cert/key pair, verifies the domain
coverage, renders a candidate config, validates, reloads, checks HTTPS, and
keeps the previous binding available for rollback.

### Internal-only admin path

Given a network group `internal`, when the user asks that only internal users
may access `/admin`, then DoWeb renders a path-specific location policy with
allow/deny directives and keeps public access unchanged for other paths.

### Load-balanced upstream

Given two backend endpoints, when the user asks for least-connection load
balancing, then DoWeb creates an upstream object, renders `least_conn`, backend
servers, passive failure settings, and a proxy location pointing to that
upstream.

## Open Decisions

- ACME/free certificate automation depends on DNS or HTTP challenge ownership.
  This should not be implemented until the issuer flow is selected.
- Active health checks require either Nginx Plus, third-party modules, or
  DoWeb-side probes. The first OSS-compatible implementation should use passive
  Nginx health settings and external probes.
- Human verification, anti-hotlink, cache acceleration, and precise traffic
  statistics should be modeled later unless a concrete first customer scenario
  requires them.

## Verification Plan

The implementation plan must add tests that prove:

- ResourceSpec renders deterministic Nginx for static, proxy, and upstream
  resources.
- HTTPS-only resources fail without valid certificate material.
- Certificate rotation checks cert/key matching and domain coverage.
- Network group CIDRs render correct allow/deny rules.
- Path policy rules do not accidentally block unrelated paths.
- Load-balancing algorithms render only supported Nginx OSS directives.
- Failed `nginx -t`, reload, or health checks restore the previous state.
