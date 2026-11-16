// 文件页「检查部署」「清理工作区」的参数拼装、本地校验和结果文案。
// 参数与 Agent 的 doops_check_deployment / doops_clean_workspace 一致
// （backend/agent/internal/server/workspace_upload.go）。

export type MaintenanceKind = "check" | "clean"

export interface CheckDeploymentInput {
  namespace: string
  deployment: string
  image: string
}

export const EMPTY_CHECK_INPUT: CheckDeploymentInput = { namespace: "", deployment: "", image: "" }

// Kubernetes 命名空间是 DNS-1123 label，Deployment 名称是 DNS-1123 subdomain。
const DNS_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
const DNS_SUBDOMAIN = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/

// 镜像引用：[registry[:port]/]path[:tag][@digest]，与 distribution/reference 的语法一致。
const DOMAIN_COMPONENT = "(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])"
const DOMAIN = `${DOMAIN_COMPONENT}(?:\\.${DOMAIN_COMPONENT})*(?::[0-9]+)?`
const PATH_COMPONENT = "[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*"
const IMAGE_NAME = `(?:${DOMAIN}/)?${PATH_COMPONENT}(?:/${PATH_COMPONENT})*`
const IMAGE_TAG = "[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}"
const IMAGE_DIGEST = "[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,}"
const IMAGE_REFERENCE = new RegExp(`^(${IMAGE_NAME})(?::${IMAGE_TAG})?(?:@${IMAGE_DIGEST})?$`)

export function isImageReference(value: string): boolean {
  if (value.length > 512) return false
  const match = IMAGE_REFERENCE.exec(value)
  return match !== null && match[1].length <= 255
}

// 校验三个必填项并拼出 doops_check_deployment 参数；命名空间不做默认值。
export function checkDeploymentRequest(sessionId: string, input: CheckDeploymentInput): Record<string, string> {
  const namespace = input.namespace.trim()
  const deployment = input.deployment.trim()
  const image = input.image.trim()
  if (!namespace) throw new Error("请填写命名空间")
  if (namespace.length > 63 || !DNS_LABEL.test(namespace)) {
    throw new Error("命名空间只能包含小写字母、数字和 -，以字母或数字开头和结尾，最长 63 个字符")
  }
  if (!deployment) throw new Error("请填写 Deployment 名称")
  if (deployment.length > 253 || !DNS_SUBDOMAIN.test(deployment)) {
    throw new Error("Deployment 名称只能包含小写字母、数字、- 和 .，以字母或数字开头和结尾，最长 253 个字符")
  }
  if (!image) throw new Error("请填写期望镜像")
  if (!isImageReference(image)) {
    throw new Error("期望镜像格式无效，例如 registry.example.com/team/app:1.2.3 或 app@sha256:<digest>")
  }
  return { session_id: sessionId, namespace, deployment, image }
}

// 与 workspace-path.ts 和 /api/rpc 的会话 ID 规则一致。
const SESSION_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/

// session_id 用于审计和 RPC 归属，workspace 是 Agent 实际删除的工作区名。
export function cleanWorkspaceRequest(sessionId: string): Record<string, string> {
  if (!SESSION_ID.test(sessionId) || sessionId.includes("..")) throw new Error("当前会话无效，无法清理工作区")
  return { session_id: sessionId, workspace: sessionId }
}

type Rewrite = [RegExp, (m: RegExpExecArray) => string]

const CHECK_ERRORS: Rewrite[] = [
  [/^namespace, deployment and image are required$/, () => "命名空间、Deployment 和期望镜像均为必填"],
  [/^kubectl get deploy (\S+) failed: ([\s\S]*)$/, (m) => `查询 Deployment ${m[1]} 失败（kubectl get deploy）：${m[2]}`],
  [/^empty image from kubectl for (\S+)$/, (m) => `Deployment ${m[1]} 未返回镜像`],
  [/^image mismatch: want "(.*)" got "(.*)"$/, (m) => `镜像不一致：期望 ${m[1]}，实际 ${m[2]}`],
  [/^invalid check deployment params: ([\s\S]*)$/, (m) => `检查参数格式无效：${m[1]}`],
]

const CLEAN_ERRORS: Rewrite[] = [
  [/^unsafe session id: ([\s\S]*)$/, (m) => `工作区会话无效：${m[1]}`],
  [/^invalid clean workspace params: ([\s\S]*)$/, (m) => `清理参数格式无效：${m[1]}`],
]

const RESULTS: Record<MaintenanceKind, Rewrite[]> = {
  check: [[/^check ok: (\S+) -> ([\s\S]+)$/, (m) => `检查通过：Deployment ${m[1]} 当前镜像为 ${m[2]}`]],
  clean: [[/^Cleaned workspace and bare repo: ([\s\S]+)$/, (m) => `已删除工作区和 Git 裸仓库：${m[1]}`]],
}

function rewrite(rules: Rewrite[], raw: string): string {
  const text = raw.trim()
  for (const [pattern, render] of rules) {
    const match = pattern.exec(text)
    if (match) return render(match)
  }
  return text
}

// Agent 的英文报错改写成中文；不认识的报错（Gateway 授权、连接失败等）原样展示。
export function maintenanceErrorText(kind: MaintenanceKind, raw: string): string {
  return rewrite(kind === "check" ? CHECK_ERRORS : CLEAN_ERRORS, raw)
}

export function maintenanceResultText(kind: MaintenanceKind, raw: string): string {
  return rewrite(RESULTS[kind], raw)
}

export interface MaintenanceLog {
  lines: string[]
  errors: string[]
}

export const EMPTY_MAINTENANCE_LOG: MaintenanceLog = { lines: [], errors: [] }

export function maintenanceOutput(log: MaintenanceLog, text: string): MaintenanceLog {
  return text ? { ...log, lines: [...log.lines, text] } : log
}

// callTool 先推 error 事件再抛出同一条错误，同一条错误只记一次。
export function maintenanceFailure(log: MaintenanceLog, kind: MaintenanceKind, raw: string): MaintenanceLog {
  const text = maintenanceErrorText(kind, raw) || "执行失败"
  return log.errors.includes(text) ? log : { ...log, errors: [...log.errors, text] }
}
