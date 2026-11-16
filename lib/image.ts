import { gatewayAuthMessage } from "./gateway-auth.ts"
import { classifyAgentAdmission } from "./agent-error.ts"
// 镜像构建、复制与 Registry 同步的表单到 doops_shell 参数映射。
// 命令沿用 CLI backend/cli/cli/doops/image.go、registry_sync.go 的工具和参数，另外：
// - 每个失败点打印一行 doops_image_error=<类别> 标记并保留原退出码，界面据此给出原因；
// - 开启 pipefail，inspect 失败不会被当成两侧 digest 相同；
// - 结尾打印 manifest digest，便于界面展示 Agent 返回的结果。
// 私有 Registry 凭据只经 Gateway 下发到目标的 authfile，这里从不接收凭据内容。

export const BUILDKIT_SOCKET = "unix:///run/buildkit/buildkitd.sock"
const BUILDKIT_SOCKET_PATH = "/run/buildkit/buildkitd.sock"
const REGISTRY_TOKEN = /^[A-Za-z0-9._:/-]+$/
// Docker tag 语法；tag 拼进引用，斜杠或冒号会让引用指向别的仓库。
const TAG = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$/
const CONTROL_CHARS = /[\u0000-\u001f\u007f]/
const PLATFORM = /^[A-Za-z0-9_]+(?:\/[A-Za-z0-9_.-]+){0,2}$/
// buildctl 按 CSV 解析 --output，镜像引用里的逗号会追加输出选项（如 push=true）。
const IMAGE_REFERENCE = /^[A-Za-z0-9][A-Za-z0-9._/:@-]*$/
// skopeo 的传输方式；引用必须以其中之一开头，所以不会被 skopeo 当成选项。
const TRANSPORTS = new Set(["containers-storage", "dir", "docker", "docker-archive", "docker-daemon", "oci", "oci-archive", "ostree", "sif"])
const MAX_TIMEOUT_SECONDS = 86400
// Agent 单次工具执行上限的默认值（DOOPS_TOOL_TIMEOUT），超过后命令被中止。
export const AGENT_TIME_LIMIT_SECONDS = 30 * 60
// 零字节的 sha256：inspect 成功却没有输出 manifest 时得到的值。
const EMPTY_SHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

export type ImageMode = "build" | "copy" | "sync"

export interface ImageBuildInput {
  context: string
  dockerfile: string
  platform: string
  output: "oci" | "image"
  outputPath: string
  image: string
  push: boolean
}

export interface ImageCopyInput {
  source: string
  destination: string
  sourceAuthfile: string
  destinationAuthfile: string
  all: boolean
  verify: boolean
  timeout: number
}

export interface RegistrySyncInput {
  sourceRegistry: string
  targetRegistry: string
  tag: string
  images: string
  sourceAuthfile: string
  targetAuthfile: string
  timeout: number
}

export interface ShellToolRequest {
  tool: "doops_shell"
  arguments: { session_id: string; command: string }
}

export function shellQuote(value: string): string {
  return "'" + value.replaceAll("'", `'"'"'`) + "'"
}

function shellJoin(values: string[]): string {
  return values.map(shellQuote).join(" ")
}

// Go path.Clean 的 POSIX 实现，保证默认 Dockerfile 路径与 CLI 相同。
export function cleanPath(value: string): string {
  if (!value) return "."
  const rooted = value.startsWith("/")
  const out: string[] = []
  for (const part of value.split("/")) {
    if (!part || part === ".") continue
    if (part === "..") {
      if (out.length && out[out.length - 1] !== "..") out.pop()
      else if (!rooted) out.push("..")
      continue
    }
    out.push(part)
  }
  const joined = out.join("/")
  return rooted ? "/" + joined : joined || "."
}

function dirName(value: string): string {
  const clean = cleanPath(value)
  const idx = clean.lastIndexOf("/")
  if (idx < 0) return "."
  return idx === 0 ? "/" : clean.slice(0, idx)
}

function baseName(value: string): string {
  if (!value) return "."
  const trimmed = value.replace(/\/+$/, "")
  if (!trimmed) return "/"
  return trimmed.slice(trimmed.lastIndexOf("/") + 1)
}

function requireText(value: string, message: string): string {
  const trimmed = value.trim()
  if (!trimmed) throw new Error(message)
  if (CONTROL_CHARS.test(trimmed)) throw new Error("参数不能包含换行或控制字符")
  return trimmed
}

function optionalText(value: string): string {
  const trimmed = value.trim()
  if (CONTROL_CHARS.test(trimmed)) throw new Error("参数不能包含换行或控制字符")
  return trimmed
}

function validTimeout(timeout: number): boolean {
  return Number.isInteger(timeout) && timeout > 0 && timeout <= MAX_TIMEOUT_SECONDS
}

function requireTimeout(timeout: number): number {
  if (!validTimeout(timeout)) throw new Error(`单步超时必须是 1 到 ${MAX_TIMEOUT_SECONDS} 之间的整数秒`)
  return timeout
}

function optionalAuthfile(value: string, label: string): string {
  const path = optionalText(value)
  if (path && !path.startsWith("/")) throw new Error(`${label} authfile 必须是目标上的绝对路径`)
  return path
}

function transportReference(value: string, label: string): string {
  const ref = requireText(value, `请填写${label}引用（含传输方式）`)
  const colon = ref.indexOf(":")
  const transport = colon > 0 ? ref.slice(0, colon) : ""
  const valid = TRANSPORTS.has(transport) && colon < ref.length - 1 && !/\s/.test(ref) &&
    (transport !== "docker" || (ref.startsWith("docker://") && ref.length > "docker://".length))
  if (!valid) throw new Error(`${label}必须以传输方式开头（如 docker://、oci-archive:），且不能包含空白`)
  return ref
}

function buildPlatform(value: string): string {
  const items = optionalText(value).split(",").map((item) => item.trim()).filter(Boolean)
  for (const item of items) {
    if (!PLATFORM.test(item)) throw new Error(`平台格式无效：${item}（示例 linux/amd64,linux/arm64）`)
  }
  return items.join(",")
}

function shellRequest(sessionId: string, command: string): ShellToolRequest {
  if (!sessionId.trim()) throw new Error("缺少操作 session")
  return { tool: "doops_shell", arguments: { session_id: sessionId, command } }
}

// 以下 shell 片段里内联的标记值只来自常量或已校验的 Registry token；
// 任意路径一律经 shellQuote 传入。

function stopWith(marker: string, code = 1): string {
  return `{ echo '${marker}' >&2; exit ${code}; }`
}

function stopWithPath(marker: string, path: string): string {
  return `{ printf '%s\\n' '${marker} path='${shellQuote(path)} >&2; exit 1; }`
}

function stepFailed(step: string, ref = ""): string {
  return `|| { rc=$?; echo "doops_image_error=step_failed rc=$rc step=${step}${ref ? " ref=" + ref : ""}" >&2; exit "$rc"; }`
}

// 前两行保持 CLI 的开头；工具都在后才开启 pipefail。
function preamble(tools: string[]): string[] {
  return [
    "set -eu",
    ...tools.map((tool) => `command -v ${tool} >/dev/null 2>&1 || ${stopWith(`doops_image_error=missing_tool tool=${tool}`, 127)}`),
    "set -o pipefail",
  ]
}

function authfileCheck(role: string, path: string): string {
  const quoted = shellQuote(path)
  return `{ test -f ${quoted} && test -r ${quoted}; } || ${stopWithPath(`doops_image_error=authfile_missing role=${role}`, path)}`
}

function verifyLines(srcInspect: string, dstInspect: string, timeout: number, ref: string, success: string): string[] {
  const refPart = ref ? ` ref=${ref}` : ""
  return [
    `src_digest=$(timeout ${timeout}s ${srcInspect} | sha256sum) ${stepFailed("inspect_source", ref)}`,
    `dst_digest=$(timeout ${timeout}s ${dstInspect} | sha256sum) ${stepFailed("inspect_destination", ref)}`,
    "src_digest=${src_digest%% *}",
    "dst_digest=${dst_digest%% *}",
    `test "\${src_digest}" != ${EMPTY_SHA256} || { echo "doops_image_error=empty_manifest role=source${refPart}" >&2; exit 1; }`,
    `test "\${dst_digest}" != ${EMPTY_SHA256} || { echo "doops_image_error=empty_manifest role=destination${refPart}" >&2; exit 1; }`,
    `test "\${src_digest}" = "\${dst_digest}" || { echo "doops_image_error=digest_mismatch source=sha256:\${src_digest} destination=sha256:\${dst_digest}${refPart}" >&2; exit 1; }`,
    success,
  ]
}

export function imageBuildCommand(input: ImageBuildInput): string {
  const context = requireText(input.context, "请填写远端构建上下文目录")
  if (!context.startsWith("/")) throw new Error("构建上下文目录必须是目标上的绝对路径")
  if (input.output !== "oci" && input.output !== "image") throw new Error("输出方式只能是 oci 或 image")
  const outputPath = optionalText(input.outputPath)
  const image = optionalText(input.image)
  const platform = buildPlatform(input.platform)
  if (input.output === "oci" && !outputPath) throw new Error("输出为 OCI 归档时必须填写归档路径")
  if (input.output === "image" && !image) throw new Error("输出为镜像时必须填写镜像引用")
  if (input.output === "oci" && input.push) throw new Error("只有输出为镜像时才能推送")
  // --output 按 CSV 解析：逗号会追加输出选项，双引号会破坏解析。
  if (input.output === "oci" && (!outputPath.startsWith("/") || /[,"]/.test(outputPath))) {
    throw new Error("OCI 归档路径必须是目标上的绝对路径，且不能包含逗号或双引号")
  }
  if (input.output === "image" && !IMAGE_REFERENCE.test(image)) {
    throw new Error("镜像引用格式无效：只能包含字母、数字和 . _ - / : @，不能有空白或逗号")
  }
  const dockerfile = optionalText(input.dockerfile)
  if (dockerfile && !dockerfile.startsWith("/")) throw new Error("Dockerfile 必须是目标上的绝对路径")
  const dockerfilePath = dockerfile || cleanPath(context + "/Dockerfile")
  const dockerfileDir = dirName(dockerfilePath)
  const dockerfileName = baseName(dockerfilePath)
  if (dockerfileName === "." || dockerfileName === "/" || dockerfileName === "") throw new Error("Dockerfile 必须指向一个文件")

  let build = `buildctl --addr ${shellQuote(BUILDKIT_SOCKET)} build --progress=plain --frontend dockerfile.v0 --local context=${shellQuote(context)} --local dockerfile=${shellQuote(dockerfileDir)} --opt filename=${shellQuote(dockerfileName)}`
  if (platform) build += " --opt " + shellQuote("platform=" + platform)
  if (input.output === "oci") build += " --output " + shellQuote("type=oci,dest=" + outputPath)
  else build += " --output " + shellQuote(`type=image,name=${image},push=${input.push ? "true" : "false"}`)
  return [
    ...preamble(["buildctl"]),
    `test -d ${shellQuote(context)} || ${stopWithPath("doops_image_error=path_missing role=context", context)}`,
    `test -f ${shellQuote(dockerfilePath)} || ${stopWithPath("doops_image_error=path_missing role=dockerfile", dockerfilePath)}`,
    `test -S ${BUILDKIT_SOCKET_PATH} || ${stopWith(`doops_image_error=buildkit_socket_missing path=${BUILDKIT_SOCKET_PATH}`)}`,
    `${build} ${stepFailed("build")}`,
  ].join("\n")
}

export function imageCopyCommand(input: ImageCopyInput): string {
  const source = transportReference(input.source, "源镜像")
  const destination = transportReference(input.destination, "目标镜像")
  const sourceAuthfile = optionalAuthfile(input.sourceAuthfile, "源")
  const destinationAuthfile = optionalAuthfile(input.destinationAuthfile, "目标")
  const timeout = requireTimeout(input.timeout)
  const parts = preamble(input.verify ? ["timeout", "skopeo", "sha256sum"] : ["timeout", "skopeo"])
  if (sourceAuthfile) parts.push(authfileCheck("source", sourceAuthfile))
  if (destinationAuthfile) parts.push(authfileCheck("destination", destinationAuthfile))
  const copyArgs = ["skopeo", "copy"]
  if (input.all) copyArgs.push("--all")
  copyArgs.push("--retry-times", "3")
  if (sourceAuthfile) copyArgs.push("--src-authfile", sourceAuthfile)
  if (destinationAuthfile) copyArgs.push("--dest-authfile", destinationAuthfile)
  copyArgs.push(source, destination)
  parts.push(`timeout ${timeout}s ${shellJoin(copyArgs)} ${stepFailed("copy")}`)
  if (input.verify) {
    const srcInspect = ["skopeo", "inspect", "--raw", ...(sourceAuthfile ? ["--authfile", sourceAuthfile] : []), source]
    const dstInspect = ["skopeo", "inspect", "--raw", ...(destinationAuthfile ? ["--authfile", destinationAuthfile] : []), destination]
    parts.push(...verifyLines(shellJoin(srcInspect), shellJoin(dstInspect), timeout, "", `echo "manifest_digest=sha256:\${dst_digest}"`))
  }
  return parts.join("\n")
}

// 重复的镜像名只同步一次。
export function registryImages(images: string): string[] {
  return [...new Set(images.split(/[\s,]+/).filter(Boolean))]
}

function registryPath(value: string, label: string): string {
  if (!REGISTRY_TOKEN.test(value)) throw new Error(`${label} 包含不支持的字符`)
  if (value.startsWith("/") || value.endsWith("/") || value.includes("//")) {
    throw new Error(`${label} 格式无效：不能以 / 开头或结尾，也不能包含 //`)
  }
  return value
}

export function registrySyncCommand(input: RegistrySyncInput): string {
  const sourceRegistry = input.sourceRegistry.trim()
  const targetRegistry = input.targetRegistry.trim()
  const tag = input.tag.trim()
  const images = registryImages(input.images)
  if (!sourceRegistry || !targetRegistry || !tag || images.length === 0) {
    throw new Error("请填写源 Registry、目标 Registry、tag 和至少一个镜像名")
  }
  const timeout = requireTimeout(input.timeout)
  registryPath(sourceRegistry, "源 Registry")
  registryPath(targetRegistry, "目标 Registry")
  if (!TAG.test(tag)) throw new Error("tag 格式无效：只能包含字母、数字和 _ . -，且不以 . 或 - 开头")
  for (const image of images) {
    if (image.includes(":") || image.includes("@")) throw new Error(`镜像名不能带 tag 或 digest：${image}（tag 请单独填写）`)
    if (!REGISTRY_TOKEN.test(image) || image.startsWith("/") || image.endsWith("/")) throw new Error(`镜像名包含不支持的字符：${image}`)
  }
  const sourceAuthfile = optionalAuthfile(input.sourceAuthfile, "源")
  const targetAuthfile = optionalAuthfile(input.targetAuthfile, "目标")
  const srcAuthArgs = sourceAuthfile ? ["--src-authfile", shellQuote(sourceAuthfile)] : []
  const dstAuthArgs = targetAuthfile ? ["--dest-authfile", shellQuote(targetAuthfile)] : []
  const srcInspectArgs = sourceAuthfile ? ["--authfile", shellQuote(sourceAuthfile)] : []
  const dstInspectArgs = targetAuthfile ? ["--authfile", shellQuote(targetAuthfile)] : []
  const parts = preamble(["timeout", "sha256sum", "skopeo"])
  if (sourceAuthfile) parts.push(authfileCheck("source", sourceAuthfile))
  if (targetAuthfile) parts.push(authfileCheck("target", targetAuthfile))
  for (const image of images) {
    const ref = `${image}:${tag}`
    const sourceRef = shellQuote(`docker://${sourceRegistry}/${ref}`)
    const targetRef = shellQuote(`docker://${targetRegistry}/${ref}`)
    const copyArgs = ["skopeo", "copy", "--all", "--retry-times", "3", ...srcAuthArgs, ...dstAuthArgs, sourceRef, targetRef]
    parts.push(`timeout ${timeout}s ${copyArgs.join(" ")} ${stepFailed("copy", ref)}`)
    const srcInspect = ["skopeo", "inspect", "--raw", ...srcInspectArgs, sourceRef].join(" ")
    const dstInspect = ["skopeo", "inspect", "--raw", ...dstInspectArgs, targetRef].join(" ")
    parts.push(...verifyLines(srcInspect, dstInspect, timeout, ref, `echo "${ref} manifest_digest=sha256:\${dst_digest}"`))
  }
  return parts.join("\n")
}

export function imageBuildRequest(sessionId: string, input: ImageBuildInput): ShellToolRequest {
  return shellRequest(sessionId, imageBuildCommand(input))
}

export function imageCopyRequest(sessionId: string, input: ImageCopyInput): ShellToolRequest {
  return shellRequest(sessionId, imageCopyCommand(input))
}

export function registrySyncRequest(sessionId: string, input: RegistrySyncInput): ShellToolRequest {
  return shellRequest(sessionId, registrySyncCommand(input))
}

// 复制带校验时每个镜像最多三步（copy、两次 inspect），各自受单步超时约束；
// 合计超过 Agent 单次执行上限时，命令会在中途被中止。构建没有单步上限，不估算。
export function durationWarning(mode: ImageMode, input: ImageBuildInput | ImageCopyInput | RegistrySyncInput): string {
  if (mode === "build") return ""
  const timeout = (input as ImageCopyInput | RegistrySyncInput).timeout
  if (!validTimeout(timeout)) return ""
  const steps = mode === "copy"
    ? ((input as ImageCopyInput).verify ? 3 : 1)
    : 3 * registryImages((input as RegistrySyncInput).images).length
  const total = steps * timeout
  if (total <= AGENT_TIME_LIMIT_SECONDS) return ""
  const advice = mode === "sync" ? "调小单步超时，或把镜像分批同步" : "调小单步超时"
  return `最坏情况下各步骤合计可达 ${Math.ceil(total / 60)} 分钟，超过 Agent 单次执行上限（默认 30 分钟）；超出上限时命令会被中止。可${advice}。`
}

// ---- 执行输出 ----
// Agent 每行输出推送一次；心跳以 “\r” 开头且没有换行，只更新进度提示。
// 界面只保留最近 OUTPUT_MAX_LINES 行，digest、失败标记和 Registry 报错另行记录，
// 被挤出可见区域后仍能用于结果和失败说明。

export const OUTPUT_MAX_LINES = 2000
const OUTPUT_TRIM_BATCH = 200
const LINE_MAX_CHARS = 4000
const SIGNAL_MAX_LINES = 20
const DIGEST_MAX_LINES = 1000
const ANSI_SEQUENCE = /\u001b\[[0-9;?]*[ -/]*[@-~]/g
const DIGEST_LINE = /(?:^|\s)manifest_digest=sha256:[a-f0-9]{64}$|^#\d+ exporting manifest (?:list )?sha256:[a-f0-9]{64}\b/
const REGISTRY_UNAUTHORIZED = /unauthorized|authentication required|401 Unauthorized/i
const REGISTRY_DENIED = /\bdenied: |requested access to the resource is denied|403 Forbidden|insufficient_scope/i
const REGISTRY_NOT_FOUND = /manifest unknown|name unknown|repository name not known/i
const REGISTRY_RATE_LIMITED = /toomanyrequests|429 Too Many Requests/i
const NETWORK_FAILURE = /x509:|no such host|connection refused|i\/o timeout|TLS handshake timeout|dial tcp/i
const SIGNAL_PATTERNS = [REGISTRY_UNAUTHORIZED, REGISTRY_DENIED, REGISTRY_NOT_FOUND, REGISTRY_RATE_LIMITED, NETWORK_FAILURE]

export interface ImageOutput {
  lines: string[]
  omitted: number
  progress: string
  pending: string
  pendingDropped: number
  digestLines: string[]
  signals: string[]
  marker: string
}

export function createImageOutput(): ImageOutput {
  return { lines: [], omitted: 0, progress: "", pending: "", pendingDropped: 0, digestLines: [], signals: [], marker: "" }
}

function appendPending(state: ImageOutput, piece: string) {
  const room = Math.max(LINE_MAX_CHARS - state.pending.length, 0)
  if (piece.length <= room) {
    state.pending += piece
    return
  }
  state.pending += piece.slice(0, room)
  state.pendingDropped += piece.length - room
}

function recordLine(state: ImageOutput, line: string) {
  if (DIGEST_LINE.test(line)) {
    state.digestLines.push(line)
    if (state.digestLines.length > DIGEST_MAX_LINES) state.digestLines.shift()
  }
  if (line.startsWith("doops_image_error=")) state.marker = line
  if (SIGNAL_PATTERNS.some((pattern) => pattern.test(line))) {
    state.signals.push(line)
    if (state.signals.length > SIGNAL_MAX_LINES) state.signals.shift()
  }
  state.lines.push(line)
  if (state.lines.length > OUTPUT_MAX_LINES) {
    const drop = state.lines.length - OUTPUT_MAX_LINES + OUTPUT_TRIM_BATCH
    state.lines.splice(0, drop)
    state.omitted += drop
  }
}

function completeLine(state: ImageOutput) {
  let line = state.pending
  line = line.slice(line.lastIndexOf("\r") + 1)
  if (state.pendingDropped) line += `…（本行已截断 ${state.pendingDropped} 个字符）`
  state.pending = ""
  state.pendingDropped = 0
  recordLine(state, line)
}

export function appendImageOutput(state: ImageOutput, chunk: string): ImageOutput {
  const text = chunk.replace(ANSI_SEQUENCE, "")
  if (text.startsWith("\r") && !text.includes("\n")) {
    state.progress = text.replace(/\r/g, "").trim()
    return state
  }
  const pieces = text.replace(/\r\n/g, "\n").split("\n")
  pieces.forEach((piece, index) => {
    appendPending(state, piece)
    if (index < pieces.length - 1) completeLine(state)
  })
  return state
}

// 执行结束后把没有换行的最后一行也计入。
export function finishImageOutput(state: ImageOutput): ImageOutput {
  if (state.pending || state.pendingDropped) completeLine(state)
  return state
}

export function imageOutputText(state: ImageOutput): string {
  let text = state.lines.join("\n")
  if (state.pending) text += (text ? "\n" : "") + state.pending
  return state.omitted ? `…已省略前 ${state.omitted} 行…\n${text}` : text
}

export interface ImageDigest {
  ref: string
  digest: string
  kind: "manifest" | "manifest list"
}

export interface ImageMarker {
  kind: string
  fields: Record<string, string>
}

function parseMarker(line: string): ImageMarker | null {
  const match = /^doops_image_error=([a-z_]+)(?: (.*))?$/.exec(line)
  if (!match) return null
  const fields: Record<string, string> = {}
  let rest = match[2] ?? ""
  // path 总在最后，且可能含空格。
  const path = /(?:^| )path=(.*)$/.exec(rest)
  if (path) {
    fields.path = path[1]
    rest = rest.slice(0, path.index)
  }
  for (const token of rest.split(" ").filter(Boolean)) {
    const eq = token.indexOf("=")
    if (eq > 0) fields[token.slice(0, eq)] = token.slice(eq + 1)
  }
  return { kind: match[1], fields }
}

// 结果只列 manifest digest：复制和同步来自命令自己打印的校验结果；构建来自
// buildctl 的 exporting manifest 行，多平台时只列 manifest list。blob 和 config digest 不列。
export function imageResults(mode: ImageMode, state: ImageOutput): { digests: ImageDigest[]; failure: ImageMarker | null } {
  const seen = new Set<string>()
  const digests: ImageDigest[] = []
  const add = (ref: string, digest: string, kind: ImageDigest["kind"]) => {
    const key = `${ref} ${digest}`
    if (seen.has(key)) return
    seen.add(key)
    digests.push({ ref, digest, kind })
  }
  if (mode === "build") {
    const lists: string[] = []
    const manifests: string[] = []
    for (const line of state.digestLines) {
      const match = /^#\d+ exporting manifest (list )?(sha256:[a-f0-9]{64})\b/.exec(line)
      if (match) (match[1] ? lists : manifests).push(match[2])
    }
    if (lists.length) lists.forEach((digest) => add("", digest, "manifest list"))
    else manifests.forEach((digest) => add("", digest, "manifest"))
  } else {
    const pattern = mode === "sync" ? /^(\S+) manifest_digest=(sha256:[a-f0-9]{64})$/ : /^()manifest_digest=(sha256:[a-f0-9]{64})$/
    for (const line of state.digestLines) {
      const match = pattern.exec(line)
      if (match) add(match[1], match[2], "manifest")
    }
  }
  return { digests, failure: parseMarker(state.marker) }
}

const SHELL_SUCCESS_MARKER = "Operation complete."

// doops_shell 的最终结果：成功标记不当作输出；命令没能启动时 Agent 也返回成功响应，这里改判为失败。
export function imageCompletion(text: string): { ok: boolean; text: string } {
  const trimmed = text.trim()
  if (trimmed === SHELL_SUCCESS_MARKER) return { ok: true, text: "" }
  if (trimmed.startsWith("Error starting command:")) return { ok: false, text: trimmed }
  return { ok: true, text }
}

export type ImageFailureKind =
  | "unknown" | "stopped" | "canceled" | "blocked" | "forbidden" | "auth" | "offline" | "busy" | "start"
  | "missing_tool" | "buildkit" | "authfile" | "path" | "digest_mismatch" | "empty_manifest" | "timeout"
  | "registry_auth" | "registry_denied" | "not_found" | "rate_limited" | "network" | "step" | "other"

export interface ImageFailure {
  kind: ImageFailureKind
  text: string
}

const STEP_LABELS: Record<string, string> = {
  build: "构建",
  copy: "复制",
  inspect_source: "读取源 manifest",
  inspect_destination: "读取目标 manifest",
}

const ROLE_LABELS: Record<string, string> = { source: "源", destination: "目标", target: "目标" }

function markerFailure(marker: ImageMarker, signals: string[], timeout?: number): ImageFailure | null {
  const f = marker.fields
  const role = ROLE_LABELS[f.role] || f.role || ""
  const refSuffix = f.ref ? `（${f.ref}）` : ""
  switch (marker.kind) {
    case "missing_tool":
      return { kind: "missing_tool", text: `目标缺少 ${f.tool}：标准 DoOps Agent 镜像自带 buildctl、skopeo 和 coreutils，请升级该目标的 Agent 镜像或换用已具备镜像工具链的目标；不会在线安装` }
    case "buildkit_socket_missing":
      return { kind: "buildkit", text: `目标上的 BuildKit 未运行：找不到 ${f.path}，请确认 Agent 内的 buildkitd 已启动` }
    case "authfile_missing":
      return { kind: "authfile", text: `${role} authfile 不存在或不可读：${f.path}。留空即使用 Gateway 为本目标下发的 authfile` }
    case "path_missing":
      return { kind: "path", text: f.role === "dockerfile" ? `Dockerfile 不存在：${f.path}` : `构建上下文目录不存在：${f.path}` }
    case "digest_mismatch":
      return { kind: "digest_mismatch", text: `源和目标的 manifest digest 不一致${refSuffix}：源 ${f.source}，目标 ${f.destination}。目标传输方式可能转换了 manifest 格式，或目标 tag 已被其他内容覆盖；核对两侧后再决定，不要直接重试` }
    case "empty_manifest":
      return { kind: "empty_manifest", text: `${role} manifest 为空${refSuffix}：inspect 没有返回内容，无法校验 digest` }
    case "step_failed": {
      const step = STEP_LABELS[f.step] || f.step || "命令"
      if (f.rc === "124") {
        return { kind: "timeout", text: `${step}${refSuffix}超时：超过单步超时${timeout ? ` ${timeout} 秒` : ""}后被终止。目标可能已写入部分内容，可调大单步超时后重新执行` }
      }
      const prefix = f.ref ? `${f.ref}：` : ""
      const has = (pattern: RegExp) => signals.some((line) => pattern.test(line))
      if (has(REGISTRY_UNAUTHORIZED)) return { kind: "registry_auth", text: `${prefix}Registry 拒绝认证（401）：确认该目标已授权这个 Registry 的凭据；若填写了 authfile，确认其中有该 Registry 的登录信息` }
      if (has(REGISTRY_DENIED)) return { kind: "registry_denied", text: `${prefix}Registry 拒绝访问（403）：凭据有效，但没有该仓库的拉取或推送权限` }
      if (has(REGISTRY_NOT_FOUND)) return { kind: "not_found", text: `${prefix}镜像不存在（404）：核对 Registry 路径、镜像名和 tag；源仓库的 tag 可能已被保留策略清理` }
      if (has(REGISTRY_RATE_LIMITED)) return { kind: "rate_limited", text: `${prefix}Registry 限流（429）：稍后再试，或使用已登录的凭据` }
      if (has(NETWORK_FAILURE)) return { kind: "network", text: `${prefix}网络连接失败：检查目标到 Registry（或构建中访问的地址）的 DNS、网络和证书` }
      return { kind: "step", text: `${step}${f.ref ? ` ${f.ref}` : ""} 失败，退出码 ${f.rc}，详见下方输出` }
    }
    default:
      return null
  }
}

// explainImageFailure 把 Gateway、Agent 和命令自身的失败标记翻译成运维能直接处理的说明。
// /api/rpc 在 Gateway 拒绝 WebSocket 升级时给出“…（HTTP n）”。格式与 lib/shell-blocks.ts 的
// gatewayUpgradeStatus 相同；本文件不引用其他模块，测试可以直接加载，两处由 shell-blocks.test.mjs 校验一致。
function gatewayUpgradeStatus(raw: string): string | null {
  return /（HTTP (\d{3})）$/.exec(raw.trim())?.[1] ?? null
}

export function explainImageFailure(
  message: string,
  output: ImageOutput,
  opts: { stopRequested: boolean; target: { cluster: string; instance: string }; timeout?: number },
): ImageFailure {
  const raw = message.trim()
  const where = `${opts.target.cluster}/${opts.target.instance}`
  if (classifyAgentAdmission(raw).kind === "unknown") {
    return { kind: "unknown", text: `远端终态未确认：请先核对目标路径或 Registry 的实际状态，不要直接重新执行（${raw}）` }
  }
  if (raw.includes("请求派发前已停止") || raw.includes("before dispatch")) {
    return opts.stopRequested
      ? { kind: "stopped", text: "已停止：命令还没有派发到目标" }
      : { kind: "canceled", text: "请求在派发前被中止，命令没有在目标上执行" }
  }
  if (raw.includes("operation canceled")) {
    return opts.stopRequested
      ? { kind: "stopped", text: "已停止：Agent 已终止命令。目标路径或 Registry 可能已写入部分内容，核对后再决定是否重新执行" }
      : { kind: "canceled", text: "命令被中止：可能超过 Agent 单次执行上限（默认 30 分钟，由 DOOPS_TOOL_TIMEOUT 配置），或会话被取消。目标路径或 Registry 可能已写入部分内容" }
  }
  const blocked = /blocked_command\(([^)]+)\):\s*(.*)/.exec(raw)
  if (blocked) return { kind: "blocked", text: `Agent 拒绝执行（${blocked[1]}）：${blocked[2]}` }
  const upgrade = gatewayUpgradeStatus(raw)
  const authMessage = gatewayAuthMessage(raw)
  if (authMessage) return { kind: "auth", text: authMessage }
  if (/^forbidden\b/i.test(raw) || upgrade === "403" || raw.includes("Unexpected server response: 403")) {
    return { kind: "forbidden", text: `无权在 ${where} 上执行：请联系平台管理员授权该目标` }
  }
  if (upgrade === "401" || raw.includes("Unexpected server response: 401") || raw.includes("未建立 Gateway 会话") || raw.includes("缺少 gateway 地址或 token")) {
    return { kind: "auth", text: "登录已失效，请重新登录后再执行" }
  }
  if (/^target offline\b/i.test(raw) || upgrade === "404") {
    return { kind: "offline", text: `目标离线：${where} 当前未连接 Gateway，确认 Agent 在线后再执行` }
  }
  if (raw.includes("session has an active operation")) {
    return { kind: "busy", text: "当前会话正忙：已有操作在执行，等它结束后再试，或换一个 session" }
  }
  if (/target queue (?:full|timeout)|target busy/.test(raw)) {
    return { kind: "busy", text: `目标正忙：${where} 上排队的操作已满或等待超时，稍后再试` }
  }
  if (raw.startsWith("Error starting command:")) {
    return { kind: "start", text: `命令没能在目标上启动：${raw.slice("Error starting command:".length).trim()}` }
  }
  const marker = parseMarker(output.marker)
  const explained = marker && markerFailure(marker, output.signals, opts.timeout)
  if (explained) return explained
  const exit = /exit status (\d+)/.exec(raw)
  if (exit) return { kind: "other", text: `命令失败，退出码 ${exit[1]}，详见下方输出` }
  const signal = /signal: ([\w ]+)/.exec(raw)
  if (signal) return { kind: "other", text: `命令被信号终止（${signal[1].trim()}）` }
  return { kind: "other", text: raw || "工具执行失败" }
}
