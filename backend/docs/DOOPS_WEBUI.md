# Doops WebUI

## Current Gateway Console

The production application is the Next.js console at `/console`. Users sign in
with their existing Gateway account or user token, select an explicit target,
and execute operations under that identity. The console stores the Gateway
session in an HttpOnly cookie, restores and clears it through `/api/session`,
and does not expose the token to React state, localStorage or copied examples.
When the Gateway returns a token expiry, the cookie lifetime is capped at that
expiry and never outlives the token. A token with no expiry keeps the console's
local 90-day session cap; that cap is only browser storage policy, not token
validity. `GET /api/session` refreshes the cookie `Max-Age` after a successful
identity check, so a long-lived or sliding token stays in the browser. An
authentication 401 (expired, revoked, invalid) clears the cookie and returns
HTTP 200 with `session: null`, so the console returns to the login page instead
of showing a restore failure. A temporary Gateway 5xx keeps the local session
cookie so a transient outage does not force a new login. A 403 means permission
denial and keeps the login cookie.

Password login, token import and session restore all validate against
`GET /v1/auth/me`. The Gateway supplies the real username, platform-admin flag,
current token ID, issue time and optional expiry. The console does not infer
identity from target visibility or probe an admin-only endpoint. An absent or
malformed identity endpoint is an explicit error and does not create a cookie.
Token login therefore displays the real username and restores only that user's
target/session selection. The token page marks the current login token; revoking
it removes access for the console, CLI and any other clients using that same
token. Disconnect only clears the browser cookie;
revocation remains a separate explicit action. Password-login tokens are renewed
by the Gateway on authenticated use, and the console cookie is refreshed when
`GET /api/session` succeeds; administrator-issued service tokens are not renewed.

- AI conversation uses the shared Agent Cloud `AgentWorkspace`, v2 runtime,
  queue and store. The host supplies only Gateway transport, target/session
  routing and deployment forms. The session API requires the Agent's
  explicit structured ACP event capability and never downgrades to the old chat
  engine. This source change needs a new shared package publication and matching
  Agent/Web rollout; local fixture evidence is not production acceptance.
- AI diagnostics run on the Agent and query runtime metadata instead of reading
  settings or API keys. The console sends them, and the database and website pages
  use the same prompt RPC with `operation=ask` (the website page sends
  `operation=apply` when the user submits a change). The deployment form sends
  `operation=apply` with target, release, namespace, Chart, optional values, task
  text and managed credential references.
- After Gateway initialization succeeds, the console clears the 45-second
  connection timer. Gateway owns the normal prompt/apply execution deadline
  (currently 60 minutes); the console keeps a 65-minute watchdog margin so it
  does not cancel an authorized repair before Gateway returns its terminal receipt;
  waiting for its first output or a quiet tool does not cause an earlier timeout,
  and progress events do not reset the deadline. Metadata requests retain their
  separate 45-second limit. At the execution deadline, the console requests
  cancellation and waits up to 15 seconds for the original RPC's terminal result.
  A cancellation ACK is not completion; if no terminal result arrives, the outcome
  remains unknown and the mutation is not automatically replayed. See the
  [execution lifecycle](DOOPS_API_GUIDE.md#执行生命周期).
- Tool start/end status is progress only. Apply success or failure comes from the
  trusted structured tool receipt, not the model's final wording or an unrelated
  successful tool call. An explicitly unexecuted Helm validation/preparation
  failure can be followed by a corrected call for the same target, release and
  namespace; a real execution failure or unknown outcome remains unresolved.
- Helm success is the actual release terminal state: the release must be
  `deployed` and the revision and description must belong to that mutation.
  Business health and functional acceptance remain task-level checks. Native MCP
  `isError`, protocol errors and interrupted streams remain failures. Helm
  parameters, values behavior, credential origin selection and failure handling
  are defined in [NATURAL_LANGUAGE_DEPLOYMENT.md](NATURAL_LANGUAGE_DEPLOYMENT.md).
- While a terminal command is running, target/session and in-console navigation
  remain locked so the original output and Stop control stay available. A failed
  stop request keeps the lock until a remote terminal result arrives. Background
  target refreshes do not replace a running terminal's target. Disconnecting an
  idle session displays a pending screen so a new command cannot start mid-logout.
- Open **Kubernetes** in the console to browse Pods, Deployments, nodes and namespaces on the selected target. The initial view lists Pods across namespaces. Select a kind or filter by name or namespace to find a resource. Click its name to run a read-only `describe` in the operations panel. Refresh reloads the lists through `doops_k8s` and shows read errors for the affected kind.
- Kubernetes Execute stays in the same position and is disabled during a call.
  Stop is a separate control, so a double-click cannot cancel the submitted
  operation. Later clicks in the same double-click gesture cannot resubmit even
  after a fast result. Errors wrap below the controls without compressing their labels.
- Agents advertising `upgradeAudit` v1 have an image-reference upgrade/rollback form.
  The console runs preflight before apply, sends the source commit and CI URL, and
  joins all stages with one operation ID. Accepted is not completed: the page
  observes that Job, verifies the Gateway protocol and the expected release commit.
  A rollback uses the commit recorded in its historical receipt. Audit completion
  errors retain the accepted Job for verification and remain visible failures,
  including failures from the final initialize probe. Completed version verification
  releases the execution controls while the audit warning stays visible; the page
  does not wait for audit storage to recover. An unresolved Job still requires
  observation before another mutation. A definite final protocol or commit rejection
  starts one rollback and verifies the historical commit. Audit-only failures,
  network uncertainty and stopped observation do not start a rollback.
  The latest operation is kept in sessionStorage, scoped to Gateway, user and
  target, for resuming observation without resubmitting a mutation. The operation's
  audit records are available from the upgrade panel. First-upgrade and installation
  limits are defined in [BUILD_DEPLOY_SOP.md](BUILD_DEPLOY_SOP.md).
- Credential pages manage create, staged payload, target bindings, persisted version
  metadata, verify, promote, grant revocation and resource revocation. Payload
  input uses the credential endpoint only and never enters an AI task.
  The `凭据组` button creates a credential bundle and looks one up by name; a
  bundle holds credential IDs only. Steps and limits are in
  [CREDENTIAL_RESOURCES.md](CREDENTIAL_RESOURCES.md#bundle).
- The audit page exposes target/session/self/global scopes, action/status, operation
  ID and time filters, and cursor pagination. Routine `observing` upgrade polls are
  hidden by default; terminal observations remain visible. Each row shows its audit
  ID, operation ID, elapsed time, byte counts and available evidence.
  Users can read their own history; global audit requires platform admin.
  Credential owners also see events that reference their credentials. Audit
  query indexes are created when the Gateway starts.
- The **Database** tab opens the selected target’s `doops_database_inventory` list. Filter discovered instances by engine, instance name, database name or namespace. Select a row to inspect its discovery metadata. **带入只读查询** fills the database hint with that asset and opens the query form. **运行 DoSql** submits the request.
- The database operations form routes through the same `doops_agent_prompt` Gateway RPC and
  invokes the bundled DoSql skill. It reuses the selected target, session and
  Gateway audit; database connections and credentials are never stored in the
  browser. The page is read-only (query, discover, plan); changes go through
  `doops sql --mode apply --sql/--file`.
- The **网站** tab loads DoWeb-published sites using `doops_web` with `operation=site-list`. Filter by site ID or domain. Select a site to prefill a read-only inspection with its ID and domain. The listing reads release records from the target and requires an Agent with `site-list` support.
- The website operations form routes through the same `doops_agent_prompt` Gateway RPC and
  invokes the bundled DoWeb skill. It reuses the selected target and Gateway
  audit for SiteSpec, Nginx validation, reload, health and rollback evidence.
- Administrator mutations are audited with actor/token IDs, resource metadata,
  HTTP result and stable errors. Credential/configuration bodies are excluded.
  Unavailable audit storage blocks a mutation.
- 管理员实例页将在线、近期离线、历史和已归档记录分开计数。历史和已归档记录默认折叠；
  只有离线记录可归档或恢复，点击对应操作直接提交。归档不会删除 target 授权、令牌、凭据绑定或审计历史，
  Agent 重新连接时会自动恢复该记录。离线超过 90 天的实例记录由 Gateway 自动删除，授权仍保留。

Explicit execution and management buttons submit their operation directly,
without an extra confirmation dialog or checkbox. Invalid inputs, unavailable
targets and operations still running retain their actual state restrictions.
Creating a new session does not replay a previous command or resolve an unknown
outcome. The original receipt stays visible.

The complete Web/CLI/skill boundary and authorization matrix is documented in
`UNIFIED_OPERATIONS_WORKBENCH.md`.

Deploy Gateway and Web from matching published artifacts — GitHub Release assets for
Gateway and explicit operator-built image digests for Web — with the canonical host
scripts. Record the binary source commit, Web digest, health, reconnects and
rollback artifact. Local fixtures do not establish live Helm or production-user acceptance.

## What's New in the Console

The console answers "what changed in this release" from the repository's
`CHANGELOG.md`, not from a Gateway feed. `Dockerfile.web` copies that file into
the Web image; `lib/server/changelog.ts` reads it while the page is rendered and
`lib/changelog.ts` parses it, so `app/console/page.tsx` hands plain release
data to the shell. The dialog needs no Gateway RPC and no runtime HTTP request,
so it keeps working with an air-gapped Gateway.

- The current version is the newest versioned entry in `CHANGELOG.md`.
  `[Unreleased]` is parsed but never shown: it belongs to a release that does
  not exist yet. Empty releases, repeated version sections and headings without
  a version are ignored. A pending change becomes visible by renaming the
  heading to `## [<version>] — YYYY-MM-DD`; the date stays optional.
- Content is merged across the releases in range: sections are ordered as
  Added, Changed, Performance, Fixed, Security, Deprecated and Removed, and an
  unknown `###` heading falls into 其他. Every item keeps the version it
  arrived in, and an item repeated across releases is shown once.
- Inline formatting is limited to `code`, **bold** and http(s) links. Other
  Markdown, including raw HTML, is rendered as text; a non-http(s) link target
  stays plain text.
- The header entry is always available when releases exist, and disabled when
  the image carries no changelog. It shows a dot while the browser's last seen
  version is older than the current one; a first visit has no remembered
  version and therefore no dot.
- The dialog shows one merged view: a "v<current> 更新内容" title, a subtitle with
  the version count and range, one heading per non-empty category with an item
  count, and every item tagged with the version it arrived in. The automatic
  popup shows the releases since the browser's last visit. The manual entry can
  switch between "since last visit" and the latest 10, 20, 50 or 100 releases;
  when nothing is new since the recorded version it falls back to the latest 10
  releases and says so. It closes through the Close button, Escape or the
  overlay.
- After an upgrade the dialog opens once by itself, unless the browser asked not
  to be popped up again today ("今天不再弹出"). A first visit only records the
  current version, so a new user is not shown history. The automatic popup waits
  until the session is restored and the onboarding guide is closed, and opens at
  most once per page load. Opening the dialog, automatic or manual, records the
  current version and clears the dot, so a reload does not open it again. The
  quiet choice suppresses automatic popups for the current local day only; it
  never disables the header entry.
- State lives in browser localStorage only, under `LAST_VERSION_KEY` and
  `QUIET_DAY_KEY` (defined in `lib/whats-new.ts`). It stores version strings and
  a local day, never a token or identity, and nothing is reported to the
  Gateway. A browser that blocks storage behaves like a first visit and can
  still open the dialog manually.
- A missing, unreadable or empty `CHANGELOG.md` is not an error: the console
  renders, the dialog has no notes and its entry is disabled.
- A user-visible change updates `CHANGELOG.md` in the same change and moves its
  `[Unreleased]` entries under the release heading when the version is cut.

## 部署

- 生产控制台是仓库根目录的 Next.js 应用，由 `Dockerfile.web` 构建，由操作方以显式
  registry/repository/tag 推送，例如 `registry.example.com/doops-web:<tag>`。
- Gateway 主机上的正式发布使用 `backend/scripts/deploy-web-console.sh`，只接受
  immutable image digest，先在 `127.0.0.1:3001` 启动候选容器并验证，再替换
  `doops-web`；旧容器保留为 rollback。随后用
  `backend/scripts/deploy-gateway-tls-proxy.sh` 发布版本化 Nginx 配置。
- 生产容器必须设置 `DOOPS_CONSOLE_ALLOWED_GATEWAYS`，值为允许代理的 Gateway
  origin，多个值用英文逗号分隔，例如
  `DOOPS_CONSOLE_ALLOWED_GATEWAYS=https://doops.example.com`。控制台只接受精确
  origin，未配置时生产代理接口拒绝请求；除本机 loopback 外，生产 Gateway 必须使用
  HTTPS。
- 控制台的变更请求（POST/PUT/PATCH/DELETE）要求浏览器 `Origin` 与控制台同源，并拒绝
  `Sec-Fetch-Site` 为跨站的请求；同一可注册域下的兄弟站点也会被拒绝。默认按 `Host`
  或 `X-Forwarded-Host` 与 `X-Forwarded-Proto` 推断控制台来源，适用于仓库里的 TLS 反向代理
  配置。反向代理不传这些头、或控制台有多个对外地址时，设置
  `DOOPS_CONSOLE_ORIGIN=https://doops.example.com`（多个用英文逗号分隔），设置后只接受这些 origin。

## Console layout and shared controls

[`DESIGN.md`](../../DESIGN.md) defines console spacing, semantic tokens and control states.
Console views fill the available width with a 16px gutter; only public pages, dialogs
and document previews use bounded reading widths. Desktop and mobile share the same
components. Use `components/ui.tsx` for buttons, inputs, selects, textareas, segmented
controls and page tabs; keep business behavior and permission checks in the host panel.

The pinned `@agent-cloud/agent-ui@0.1.4` display patch is tracked in `patches/`
and applied by pnpm in local and Docker builds. Tool activity remains in the
conversation list; the developer trace retains independent interactions without
rendering the same execution/tool items again. Titles that repeat the workspace
path or translated tool label are omitted; tool results, actions and textual
progress stages/messages remain available, including progress without a fraction.
