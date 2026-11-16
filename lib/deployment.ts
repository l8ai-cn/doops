import { gatewayAuthCode } from "./gateway-auth.ts"
import type { Target } from "./gateway"
import { AGENT_MODEL_MISSING_HINT, classifyAgentAdmission } from "./agent-error.ts"

export interface DeploymentInput {
  release: string
  namespace: string
  chart: string
  values: string
  message: string
  credentials: string
}

// Every field of the channel payload, in the order the console stores them and the server checks them.
export const DEPLOYMENT_FIELDS = ["release", "namespace", "chart", "values", "message", "credentials"] as const

// The session route refuses a larger `doops.operations/apply` form payload.
export const DEPLOYMENT_PAYLOAD_LIMIT = 64_000

// Mirrors the Gateway's inline credential check (credential_scope.go credentialScalarContainsInlineMaterial), so text
// it would refuse is stopped before the release starts and never reaches the prompt, a stored draft or the audit.
const credentialPatterns = [
  /\bgh[pousr]_[A-Za-z0-9]{20,255}\b/i, /\bgithub_pat_[A-Za-z0-9_]{20,255}\b/i, /\bglpat-[A-Za-z0-9_-]{20,255}\b/,
  /\b(?:AKIA|ASIA)[A-Z0-9]{16}\b/, /\bAIza[0-9A-Za-z_-]{35}\b/, /\bxox[baprs]-[A-Za-z0-9-]{10,255}\b/,
  /\b(?:sk|rk)_live_[A-Za-z0-9]{16,255}\b/, /\bnpm_[A-Za-z0-9]{36,255}\b/,
  /\bauthorization\s*:\s*(?:bearer|basic)\s+\S+/i, /\b(?:bearer|basic)\s+[A-Za-z0-9+/=_-]{8,}\b/i,
]
const secretKeys = new Set(["token", "password", "auth", "authorization", "cookie", "data", "stringdata", "secret", "sig", "clientsecret", "apikey", "privatekey", "registrypassword"])
const secretKeySuffixes = ["token", "password", "privatekey", "clientsecret", "apikey", "accesskey", "secretkey", "credential", "githubpat", "gitlabpat", "passphrase", "dockerconfigjson", "sshidentity", "signature"]

function secretLikeKey(key: string): boolean {
  const normalized = key.trim().toLowerCase().replace(/[_\-. ]/g, "")
  return secretKeys.has(normalized) || /(?:^|[^a-z0-9])pat$/i.test(key.trim()) ||
    /[a-z0-9](?:Pat|PAT)$/.test(key.trim()) || secretKeySuffixes.some((suffix) => normalized.endsWith(suffix))
}

function queryKeys(value: string): string[] {
  const query = value.split("#", 1)[0]
  const start = query.indexOf("?")
  if (start < 0) return []
  return query.slice(start + 1).split("&").flatMap((part) => {
    try { return [decodeURIComponent(part.split("=", 1)[0].replace(/\+/g, " "))] } catch { return [] }
  })
}

const urlUserInfo = /^[a-z][a-z0-9+.-]*:\/\/[^/?#\s]*@/i
const embeddedUrl = /\b[a-z][a-z0-9+.-]*:\/\/[^\s<>"'`，。；：、）」]+/gi

// The Gateway runs credentialScalarContainsInlineMaterial on the whole instruction (credential_runtime.go:202) when it
// resolves a credential for the release; it returns before that check when none is resolved (166, 190-192). The
// instruction has line breaks, so its url.Parse fails and only these checks take effect (credential_scope.go:182-192).
// The console applies them to every release, since such text must not reach the prompt or a stored draft either.
function containsInlineCredential(value: string): boolean {
  const lower = value.toLowerCase()
  if (lower.includes("-----begin ") && lower.includes("private key-----")) return true
  if (credentialPatterns.some((pattern) => pattern.test(value))) return true
  return false
}

export function containsCredentialMaterial(value: string): boolean {
  value = value.trim()
  if (!value) return false
  if (containsInlineCredential(value)) return true
  if (urlUserInfo.test(value)) return true
  return queryKeys(value).some(secretLikeKey)
}

function containsPromptCredentialMaterial(value: string): boolean {
  if (containsInlineCredential(value)) return true
  return [...value.matchAll(embeddedUrl)].some(([url]) =>
    urlUserInfo.test(url) || queryKeys(url).some(secretLikeKey))
}

export function deploymentRequest(target: Target, input: DeploymentInput) {
  for (const key of ["release", "namespace", "chart"] as const) {
    if (!input[key].trim()) throw new Error("请完整填写发布信息")
  }
  if (new TextEncoder().encode(JSON.stringify({ input })).length > DEPLOYMENT_PAYLOAD_LIMIT) {
    throw new Error("发布信息过长（超过 64 KB），请精简任务说明")
  }
  const name = /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/
  if (!name.test(input.release) || !name.test(input.namespace)) throw new Error("Release 和 namespace 必须为有效 Kubernetes 名称")
  if (input.release.length > 53) throw new Error("Helm release 名称不能超过 53 个字符")
  if (input.namespace.length > 63) throw new Error("Namespace 不能超过 63 个字符")
  if (containsCredentialMaterial(input.chart) || containsCredentialMaterial(input.values)) {
    throw new Error("Chart 或 Values 不能携带凭据内容")
  }
  const credential_refs = input.credentials.split(/[\s,]+/).filter(Boolean)
  if (credential_refs.some((name) => !/^[a-z0-9][a-z0-9-]*$/.test(name))) throw new Error("凭据只能引用资源名")
  // The Gateway refuses a repeated name (tunnel_hub.go validateExplicitCredentialNames).
  const repeated = credential_refs.find((name, index) => credential_refs.indexOf(name) !== index)
  if (repeated) throw new Error(`凭据资源名重复：${repeated}`)
  if (containsPromptCredentialMaterial(input.message)) throw new Error("发布说明不能携带凭据内容")
  const instruction = [
    input.message.trim(),
    `在 target ${target.cluster}/${target.instance} 处理 Helm release ${input.release}，namespace ${input.namespace}，Chart ${input.chart}.`,
    input.values.trim() ? `Values: ${input.values.trim()}.` : "",
  ].filter(Boolean).join("\n")
  return { instruction, operation: "apply", credential_refs }
}

export const DEPLOYMENT_EXTENSION = { namespace: "doops.operations", semanticType: "apply", schemaVersion: "1" } as const

// Builds the `doops.operations/apply` command the console sends; the server re-validates the same input.
export function deploymentExtensionCommand(target: Target, input: DeploymentInput) {
  deploymentRequest(target, input)
  return { ...DEPLOYMENT_EXTENSION, payload: { input: { ...input } } }
}

// The draft kept in the tab's sessionStorage, so switching tabs or reloading keeps what was typed. A draft with
// credential material is never stored.
export function deploymentDraftText(input: DeploymentInput): string | null {
  if (DEPLOYMENT_FIELDS.some((key) => key !== "credentials" &&
    (key === "message" ? containsPromptCredentialMaterial(input[key]) : containsCredentialMaterial(input[key])))) return null
  return JSON.stringify(Object.fromEntries(DEPLOYMENT_FIELDS.map((key) => [key, input[key]])))
}

export function parseDeploymentDraft(raw: string | null | undefined): DeploymentInput | undefined {
  if (!raw) return undefined
  let value: unknown
  try { value = JSON.parse(raw) } catch { return undefined }
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined
  const record = value as Record<string, unknown>
  if (Object.keys(record).length !== DEPLOYMENT_FIELDS.length || DEPLOYMENT_FIELDS.some((key) => typeof record[key] !== "string")) return undefined
  const draft = { ...record } as unknown as DeploymentInput
  return deploymentDraftText(draft) === null ? undefined : draft
}

// Runs one call at a time; a call made while another is in flight is dropped and resolves to false.
export function singleFlight<A extends unknown[]>(run: (...args: A) => Promise<unknown>): (...args: A) => Promise<boolean> {
  let running = false
  return async (...args: A) => {
    if (running) return false
    running = true
    try { await run(...args); return true } finally { running = false }
  }
}

// Connect and the session protocol prefix messages with "[code] "; users read the message only.
export function agentErrorText(raw: string | null | undefined): string {
  return (raw || "").replace(/^(?:\[[a-z_]+\]\s*)+/, "").trim()
}

function errorCode(raw: string | null | undefined): string | undefined {
  return /^\[([a-z_]+)\]/.exec(raw || "")?.[1]
}

// Agent and Helm errors can carry a whole stderr; summaries stay readable and the trace keeps the full text.
function clip(text: string, limit = 240): string {
  const trimmed = text.trim().replace(/[。.]$/, "")
  return trimmed.length > limit ? `${trimmed.slice(0, limit)}…` : trimmed
}

export type DeploymentNoticeAction = "open_credentials" | "reconnect" | "upgrade_agent"

export interface DeploymentNotice {
  kind: "connecting" | "no_model" | "model_unavailable" | "upgrade_agent" | "forbidden" | "signed_out" | "console_policy" | "offline" | "busy" | "unavailable"
  title: string
  detail: string
  actions: DeploymentNoticeAction[]
}

export type DeploymentAvailability =
  | { fieldsEditable: true; canSubmit: true }
  | { fieldsEditable: true; canSubmit: false; blockedReason: string; notice?: DeploymentNotice }

export interface DeploymentSessionState {
  connection: "connecting" | "connected" | "reconnecting" | "disconnected"
  error: string | null | undefined
  supportsApply: boolean
  running: boolean
}

// The channel state only decides whether this console can submit. Fields stay editable so a release
// can be drafted and checked locally while the target channel is unavailable.
export function deploymentAvailability(state: DeploymentSessionState): DeploymentAvailability {
  if (state.connection === "connected") {
    if (!state.supportsApply) {
      return blocked({ kind: "unavailable", title: "当前通道不支持 Helm 发布",
        detail: "目标 Agent 没有声明结构化 Helm 能力。请到「部署 → 升级 Agent」升级后再通过此通道提交。", actions: ["upgrade_agent"] })
    }
    // A connected snapshot's error belongs to the last release, not to the connection.
    if (state.running) return { fieldsEditable: true, canSubmit: false, blockedReason: "上一次发布正在执行，结束后才能再次提交" }
    return { fieldsEditable: true, canSubmit: true }
  }
  if (!agentErrorText(state.error) && state.connection !== "disconnected") {
    return blocked({ kind: "connecting", title: "正在连接目标 Agent", detail: "连接完成后即可发布；可以先填写和校验发布信息。", actions: [] })
  }
  return blocked(connectionNotice(state.error || ""))
}

// Which form controls are usable. Only an in-flight submission locks the fields; channel availability
// and local validation decide whether the submit action is available.
export function deploymentFormControls(state: { canSubmit: boolean; valid: boolean; submitting: boolean }) {
  const ready = state.canSubmit && state.valid && !state.submitting
  return { fieldsDisabled: state.submitting, submitDisabled: !ready }
}

function blocked(notice: DeploymentNotice): DeploymentAvailability {
  return { fieldsEditable: true, canSubmit: false, blockedReason: notice.title, notice }
}

const busyPattern = /target busy|target queue (?:full|timeout)|session has an active operation|运行中的任务|cancellation pending/
// Messages from the console's own Gateway allowlist (gateway-policy.ts), answered with 403 like a missing grant.
const consolePolicyPattern = /gateway (?:地址|协议)|未获控制台授权|生产 gateway|gateway allowlist|只允许访问本机 gateway/

// The console runtime rewrites the session error (agent-error.ts agentErrorMessage): the "[code] " prefix is gone and
// the missing-model text becomes AGENT_MODEL_MISSING_HINT. A refused /v1/rpc upgrade ends with the HTTP status
// (server/gateway-upgrade.ts). Each check below accepts both the raw and the rewritten form.
function modelMissing(message: string): boolean {
  return message === AGENT_MODEL_MISSING_HINT || /还没有可下发到该节点的模型|尚未连接大模型/.test(message)
}

function signedOut(code: string | undefined, message: string): boolean {
  return code === "unauthenticated" || Boolean(gatewayAuthCode(message)) || /缺少登录 token|（HTTP 401）$/.test(message)
}

function grantRefused(message: string): boolean {
  // HandleClientRPC answers a user without a grant with 403 before the WebSocket upgrade.
  return /^forbidden:|（HTTP 403）$/.test(message)
}

function targetOffline(message: string): boolean {
  return /target offline|（HTTP 404）$/.test(message)
}

function connectionNotice(raw: string): DeploymentNotice {
  const code = errorCode(raw)
  const message = agentErrorText(raw)
  if (modelMissing(message)) {
    return { kind: "no_model", title: "当前通道缺少 DoAgent 模型",
      detail: "发布由目标上的 DoAgent 执行，需要 Gateway 下发的模型。请联系平台管理员为该目标绑定 modelProvider 凭据，激活后点「重新连接」。可以先填写和校验发布信息。",
      actions: ["open_credentials", "reconnect"] }
  }
  if (/模型连接不可用|\bmodel_(?:provider_[a-z_]+|auth_failed|unavailable)\b/.test(message)) {
    return { kind: "model_unavailable", title: "当前通道的 DoAgent 模型不可用",
      detail: `Agent 报告：${clip(message)}。请联系平台管理员检查该目标绑定的 modelProvider 凭据，处理后点「重新连接」。`,
      actions: ["open_credentials", "reconnect"] }
  }
  if (/结构化 ACP|必须升级 Agent/.test(message)) {
    return { kind: "upgrade_agent", title: "当前通道不支持结构化执行",
      detail: "该 Agent 不支持控制台的结构化执行事件。请到「部署 → 升级 Agent」升级，完成后回到这里重新连接。", actions: ["upgrade_agent"] }
  }
  if (signedOut(code, message)) {
    return { kind: "signed_out", title: "登录已失效", detail: "Gateway 不再接受当前登录。请断开后重新登录。", actions: [] }
  }
  if (consolePolicyPattern.test(message)) {
    return { kind: "console_policy", title: "控制台不允许访问这个 Gateway",
      detail: `${clip(message)}。请联系控制台运维检查 DOOPS_CONSOLE_ALLOWED_GATEWAYS 配置。`, actions: [] }
  }
  if (grantRefused(message)) {
    return { kind: "forbidden", title: "当前账号没有该目标的使用权限",
      detail: "请联系平台管理员在「管理 → 权限」为当前账号授予该目标。", actions: [] }
  }
  if (targetOffline(message)) {
    return { kind: "offline", title: "目标 Agent 离线",
      detail: "Gateway 当前没有该目标的 Agent 连接。确认 Agent 正常运行后点「重新连接」。", actions: ["reconnect"] }
  }
  if (busyPattern.test(message)) {
    return { kind: "busy", title: "当前通道正在执行其它任务",
      detail: "目标上有其他变更在执行，或上一次停止还没有确认终态。可以在「审计」查看正在执行的操作，结束后点「重新连接」。", actions: ["reconnect"] }
  }
  // The last release's failure stays in the timeline; it says nothing about why the connection dropped.
  const reason = code === "doops_operation_failed" || code === "doops_apply_outcome_unknown" ? "" : clip(message)
  return { kind: "unavailable", title: "暂时连不上目标 Agent",
    detail: `${reason ? `连接失败：${reason}。` : "连接已断开。"}稍后点「重新连接」重试。`, actions: ["reconnect"] }
}

export interface DeploymentOutcome {
  kind: "idle" | "running" | "succeeded" | "unknown" | "cancelled" | "failed"
  title: string
  detail: string
  actions: DeploymentNoticeAction[]
}

const notStarted = "本次发布没有执行。"
const unchanged = "目标没有被修改。"

// What the last release in this session did to the target. Failure categories come from the Gateway, the Agent and the
// managed Helm tool (next.go, helm_release.go, NATURAL_LANGUAGE_DEPLOYMENT.md).
export function deploymentOutcome(state: { status: string; error: string | null | undefined }): DeploymentOutcome {
  const outcome = (kind: DeploymentOutcome["kind"], title: string, detail = "", actions: DeploymentNoticeAction[] = []): DeploymentOutcome =>
    ({ kind, title, detail, actions })
  // "launching" is the SDK's placeholder before any session snapshot arrives (emptySession); nothing was submitted.
  if (state.status === "running" || state.status === "waiting") {
    return outcome("running", "发布执行中…", "Helm release 的实际终态以 Agent 返回为准。")
  }
  if (state.status === "completed") {
    return outcome("succeeded", "发布已结束", "请按任务需要继续做业务健康和功能验收。")
  }
  if (state.status !== "failed") return outcome("idle", "填写发布信息后，执行过程显示在这里")
  const code = errorCode(state.error)
  const message = agentErrorText(state.error)
  const admission = classifyAgentAdmission(state.error, code)
  // A refused admission is not a release result: it says whether anything could have run at all.
  if (admission.kind === "queued") {
    return outcome("failed", "目标容量忙，本次发布没有执行",
      "目标当前没有空闲的执行容量，本次发布没有开始，也不需要核对 Helm 或 Deployment。可以点「新发布」换一个 session 重试，或等容量释放后重新提交。")
  }
  if (admission.kind === "queue_cleanup_unknown") {
    return outcome("unknown", "发布结果未确认",
      "Gateway 无法确认排队请求已从队列移除，后台可能仍会执行。请先在「审计」核对队列和本次操作，再到「Kubernetes」核对 release 状态；控制台不会自动重试。")
  }
  if (admission.kind === "no_prompt") {
    return outcome("failed", "发布任务内容为空", "Agent 没有收到任何任务内容，本次发布没有执行。请补全发布信息后重新提交。")
  }
  // Other categories (including doagent_prompt_rejected) keep their existing failure wording below.
  if (admission.needsMutationRecheck || /结果未确认|终态未确认/.test(message)) {
    return outcome("unknown", "发布结果未确认",
      "Agent 返回终态前连接中断，目标可能已经变更。请在「Kubernetes」核对 release 的 helm-history 和工作负载状态，或在「审计」查看本次操作。控制台不会自动重试。")
  }
  if (/operation canceled|已停止/.test(message)) {
    return outcome("cancelled", "发布已停止", "停止的发布不会自动回滚，Helm 可能已经写入新的 revision。请在「Kubernetes」查看 helm-history 核对 release 当前状态。")
  }
  if (/credential_reference_ambiguous|helm_credential_binding_ambiguous/.test(message)) {
    return outcome("failed", "目标绑定了多个 Helm 仓库凭据",
      `Gateway 无法判断这个 Chart 来自哪个仓库。请在「凭据资源名」填写该 Chart 仓库对应的 helmPull 凭据后重新发布。${notStarted}`, ["open_credentials"])
  }
  if (/helm_chart_requires_credential/.test(message)) {
    return outcome("failed", "Chart 仓库需要凭据",
      `Chart 仓库拒绝匿名下载。请为该目标绑定这个仓库的 helmPull 凭据，绑定后不必在表单中填写，重新发布即可。${unchanged}`, ["open_credentials"])
  }
  if (/credential_version_not_found/.test(message)) {
    return outcome("failed", "凭据没有激活的版本",
      `凭据已绑定，但没有验证并激活（promote）过的版本。请在凭据页完成验证和激活后重新发布。${notStarted}`, ["open_credentials"])
  }
  if (/credential_(?:grant_denied|forbidden|not_found|revoked|binding_target_mismatch|binding_scope_invalid)/.test(message)) {
    return outcome("failed", "填写的凭据在该目标上不可用",
      `请核对「凭据资源名」是否为绑定到该目标的凭据；留空时会自动使用目标绑定的 Helm 凭据。${notStarted}`)
  }
  if (busyPattern.test(message)) {
    return outcome("failed", "目标正忙", `目标上有其他变更在执行。${notStarted}可以在「审计」查看正在执行的操作，结束后重新提交。`)
  }
  if (grantRefused(message)) {
    return outcome("failed", "当前账号没有该目标的使用权限", `授权可能已被收回。${notStarted}请联系平台管理员在「管理 → 权限」授予该目标。`)
  }
  if (targetOffline(message)) {
    return outcome("failed", "目标 Agent 离线", `${notStarted}确认 Agent 正常运行后重新提交。`)
  }
  const helm = /\b(helm_[a-z]+(?:_[a-z]+)*(?:_\d{3})?)(?::\s*([\s\S]*))?/.exec(message)
  if (helm) {
    const reason = clip(helm[2] || helm[1])
    if (helm[1] === "helm_mutation_failed" && /invalid ownership metadata|cannot be imported into the current release/.test(message)) {
      return outcome("failed", "目标上已有同名对象，不属于本 release",
        `Helm 拒绝接管已存在且没有本 release 所有权元数据的对象：${reason}。${unchanged}如确需纳入本 release，请选择 take_ownership 后重新发布。`)
    }
    if (/^helm_(?:artifact_http_\d+|values_http_\d+|repository_[a-z_]+|values_[a-z_]+|chart_[a-z_]+|url_invalid|oci_unsupported)$/.test(helm[1])) {
      return outcome("failed", "无法获取 Chart 或 values", `${reason}。Agent 没有拿到发布资产，${unchanged}请确认地址可以从目标集群访问。`)
    }
    if (helm[1] === "helm_mutation_invalid_result") {
      return outcome("unknown", "Helm 没有返回本次发布的可信终态", `${reason}。目标可能已经变更，请在「Kubernetes」核对本次 release、revision 和 description。`)
    }
    if (helm[1] === "helm_mutation_failed") {
      return outcome("failed", "Helm 变更失败", `${reason}。Helm 可能已经写入失败的 revision，请在「Kubernetes」用 helm-history 查看 release 状态。`)
    }
    if (helm[1] === "helm_verification_failed") {
      return outcome("failed", "Helm release 终态校验未通过", `${reason}。请在「Kubernetes」核对本次 revision、description 和 release 状态。`)
    }
    if (helm[1] === "helm_interrupted") {
      return outcome("failed", "发布超时", `${reason}。请在「Kubernetes」核对 release 当前状态，再决定是否重新发布。`)
    }
  }
  if (/doagent_apply_tool_evidence_missing/.test(message)) {
    return outcome("failed", "Agent 没有返回可信的变更证据", "Agent 没有完成受管 Helm 发布，或证据没有通过校验，不能视为成功。请查看执行过程并核对目标状态。")
  }
  return outcome("failed", "发布失败", `${clip(message) || "Agent 没有返回原因"}。详情见执行过程。`)
}

// Why a submission was refused, and whether the release may have started anyway.
export function deploymentSubmitError(raw: string): string {
  const code = errorCode(raw)
  const message = agentErrorText(raw)
  const admission = classifyAgentAdmission(raw, code)
  if (admission.kind === "queued") return "目标容量忙，本次发布没有执行。请点「新发布」换一个 session 重试，或等容量释放后再提交。"
  if (admission.kind === "queue_cleanup_unknown") return "排队请求的清理结果未确认，后台可能仍会执行。请在「审计」核对队列与本次操作，并核对 release 状态后再决定。"
  if (admission.kind === "no_prompt") return "发布任务内容为空，本次没有提交。请补全发布信息后再提交。"
  // An unknown outcome may already have changed the target; it is never a safe retry. A rejected
  // admission keeps its existing failure wording below.
  if (admission.needsMutationRecheck) return "提交请求没有收到确认，发布可能已经开始。请查看执行过程或「审计」，先核对 Helm/Deployment 后再决定是否重新提交。"
  if (/运行中的任务/.test(message)) return "上一次发布还在执行，本次没有提交。请等它结束后再提交。"
  if (/禁止重放|不能复用此会话|结果未确认/.test(message)) return "上一次发布结果未确认，本次没有提交。请先在「Kubernetes」核对 release 状态，再点「新发布」。"
  if (/command_position_mismatch|cursor_/.test(message)) return "执行过程刚刚更新，本次没有提交。请核对当前结果后重新提交。"
  if (/agent_workbench_command_/.test(message)) return "Agent 没有接受本次提交，发布没有执行。详情见执行过程。"
  if (/agent_workbench_/.test(message)) return "与目标 Agent 的连接未就绪，本次没有提交。请等待连接完成后重试。"
  if (targetOffline(message)) return `目标 Agent 离线，${notStarted}`
  if (grantRefused(message)) return `当前账号没有该目标的使用权限，${notStarted}`
  if (modelMissing(message)) return `当前通道缺少 DoAgent 模型，${notStarted}`
  if (code === "unavailable" || code === "deadline_exceeded" || code === "unknown" || code === "internal" || /Failed to fetch|NetworkError/.test(message)) {
    return "提交请求没有收到确认，发布可能已经开始。请查看执行过程或「审计」，不要重复提交。"
  }
  return message || "提交失败"
}

// The Gateway serializes mutations within one session only (tunnel_hub.go exclusiveLockKey), so a new release
// session started after an unconfirmed result could run beside a release that is still going.
export function deploymentNewSessionWarning(kind: DeploymentOutcome["kind"]): string | undefined {
  if (kind !== "unknown") return undefined
  return "上一次发布结果未确认，目标可能仍在变更。新的发布会话不会等它结束，请先在「Kubernetes」核对 release 状态。"
}

// A stop only asks the Agent to stop; this says what happened to the release when it did not take effect.
export function deploymentStopFailure(raw: string): string {
  const message = agentErrorText(raw)
  if (/已请求停止，远端终态未确认/.test(message)) return "已请求停止，但 Agent 没有确认终态，发布可能仍在执行。请在「Kubernetes」核对 release 状态。"
  if (/任务已结束，取消未生效|当前没有运行中的任务/.test(message)) return "发布已经结束，停止没有生效。结果以执行过程为准。"
  if (/不能复用此会话|结果未确认/.test(message)) return "上一次发布结果未确认，无法停止。请在「Kubernetes」核对 release 状态。"
  if (/中断目标不是当前任务/.test(message)) return "要停止的不是当前发布，请刷新后重试。"
  return `停止请求失败：${message || "未知错误"}`
}
