import { gatewayAuthMessage } from "./gateway-auth.ts"
import { classifyAgentAdmission } from "./agent-error.ts"
// AI 助手回复里的 shell 代码块只作为“建议命令”交给用户；执行一律由用户主动提交后
// 走终端页同一条 doops_shell 路径。这里只做纯文本解析和参数构造，不拼接凭据。

const SHELL_LANGUAGES = new Set(["bash", "sh", "shell", "zsh", "console"])
const FENCE = /^ {0,3}(`{3,}|~{3,})[ \t]*([^\s`]*)[^\n]*$/
const PROMPT = /^\s*\$ /
const CONTINUATION = /^\s*> ?/

export interface ShellBlock {
  language: string
  command: string
}

export interface AssistantMessageLike {
  kind: string
  role?: string
  text?: string
  status?: string
}

// extractShellBlocks 返回 markdown 中 ```bash/```sh/```shell 代码块的内容，
// 未闭合的代码块（流式输出尚未结束）不返回，避免执行半截命令。
export function extractShellBlocks(markdown: string): ShellBlock[] {
  const blocks: ShellBlock[] = []
  const lines = markdown.replace(/\r\n?/g, "\n").split("\n")
  let open: { fence: string; language: string; body: string[] } | null = null
  for (const line of lines) {
    if (!open) {
      const match = FENCE.exec(line)
      if (match) open = { fence: match[1], language: match[2].toLowerCase(), body: [] }
      continue
    }
    const trimmed = line.trim()
    if (trimmed.startsWith(open.fence[0].repeat(open.fence.length)) && /^[`~]+$/.test(trimmed)) {
      if (SHELL_LANGUAGES.has(open.language)) {
        const command = normalizeShellCommand(open.body.join("\n"), open.language)
        if (command) blocks.push({ language: open.language, command })
      }
      open = null
      continue
    }
    open.body.push(line)
  }
  return blocks
}

// 带 “$ ” 提示符的代码块是一段终端记录：只保留提示符后的命令行和它的 “> ” 续行，
// 丢掉夹在中间的输出。console 代码块总是按终端记录处理。
function normalizeShellCommand(body: string, language: string): string {
  const lines = body.split("\n")
  if (language !== "console" && !lines.some((line) => PROMPT.test(line))) return body.trim()
  const commands: string[] = []
  let continuing = false
  for (const line of lines) {
    if (PROMPT.test(line)) {
      const command = line.replace(PROMPT, "")
      commands.push(command)
      continuing = command.endsWith("\\")
    } else if (continuing) {
      const command = line.replace(CONTINUATION, "")
      commands.push(command)
      continuing = command.endsWith("\\")
    }
  }
  return commands.join("\n").trim()
}

// latestAssistantShellBlocks 取最近一条已完成助手消息里的 shell 代码块，去重。
export function latestAssistantShellBlocks(items: readonly AssistantMessageLike[]): ShellBlock[] {
  for (let i = items.length - 1; i >= 0; i--) {
    const item = items[i]
    if (item.kind !== "message" || item.role !== "assistant") continue
    if (item.status !== "completed") return []
    const seen = new Set<string>()
    return extractShellBlocks(item.text ?? "").filter((block) => {
      if (seen.has(block.command)) return false
      seen.add(block.command)
      return true
    })
  }
  return []
}

// shellToolArguments 构造 doops_shell 参数：只有会话和用户提交的原始命令。
export function shellToolArguments(sessionId: string, command: string): { session_id: string; command: string } {
  const trimmed = command.trim()
  if (!sessionId) throw new Error("缺少会话")
  if (!trimmed) throw new Error("命令为空")
  return { session_id: sessionId, command: trimmed }
}

// TerminalPrefill 是“填入终端”跨标签传递的命令；nonce 让同一条命令可以重复预填。
export interface TerminalPrefill {
  command: string
  nonce: number
}

export function nextTerminalPrefill(previous: TerminalPrefill | null, command: string): TerminalPrefill | null {
  const trimmed = command.trim()
  if (!trimmed) return previous
  return { command: trimmed, nonce: (previous?.nonce ?? 0) + 1 }
}

// prefillToApply 判断终端是否要把预填命令写入输入框：每个 nonce 只写一次，不自动执行。
export function prefillToApply(prefill: TerminalPrefill | null, appliedNonce: number): string | null {
  if (!prefill || prefill.nonce <= appliedNonce) return null
  return prefill.command
}

// shellSegments 把命令按未加引号的 ; & | 换行 和子 shell 括号切成简单命令，
// 每段是去掉引号后的词。它只用于识别破坏性命令，不用于执行。
function shellSegments(command: string): string[][] {
  const segments: string[][] = []
  let words: string[] = []
  let word = ""
  let inWord = false
  let quote: "'" | '"' | null = null
  const endWord = () => {
    if (inWord) words.push(word)
    word = ""
    inWord = false
  }
  const endSegment = () => {
    endWord()
    if (words.length) segments.push(words)
    words = []
  }
  for (let i = 0; i < command.length; i++) {
    const ch = command[i]
    if (quote) {
      if (ch === quote) quote = null
      else if (ch === "\\" && quote === '"' && i + 1 < command.length) word += command[++i]
      else word += ch
      continue
    }
    if (ch === "'" || ch === '"') {
      quote = ch
      inWord = true
    } else if (ch === "\\" && i + 1 < command.length) {
      const next = command[++i]
      if (next !== "\n") {
        word += next
        inWord = true
      }
    } else if ("\n;&|()`".includes(ch)) {
      endSegment()
    } else if (ch === " " || ch === "\t" || ch === "\r") {
      endWord()
    } else {
      word += ch
      inWord = true
    }
  }
  endSegment()
  return segments
}

// 包装命令及其带值参数：sudo -u postgres psql、xargs -n 1 rm、timeout 5 kill 等，
// 真正执行的是包装命令后面的那个命令。
const WRAPPER_VALUE_FLAGS = new Map<string, ReadonlySet<string>>([
  ["sudo", new Set(["-u", "-g", "-h", "-p", "-C", "-D", "-r", "-t", "-U", "--user", "--group"])],
  ["doas", new Set(["-u", "-C"])],
  ["env", new Set(["-u", "-C", "-S", "--unset", "--chdir"])],
  ["nohup", new Set()],
  ["time", new Set()],
  ["nice", new Set(["-n"])],
  ["ionice", new Set(["-c", "-n"])],
  ["timeout", new Set(["-s", "-k", "--signal", "--kill-after"])],
  ["xargs", new Set(["-I", "-n", "-P", "-L", "-d", "-E", "-s", "-a"])],
  ["stdbuf", new Set(["-i", "-o", "-e"])],
  ["exec", new Set()],
  ["command", new Set()],
  ["builtin", new Set()],
])

function stripWrappers(words: readonly string[]): string[] {
  let i = 0
  while (i < words.length) {
    if (/^[A-Za-z_][A-Za-z0-9_]*=/.test(words[i])) {
      i++
      continue
    }
    const name = commandName(words[i])
    const valueFlags = WRAPPER_VALUE_FLAGS.get(name)
    if (!valueFlags) break
    i++
    while (i < words.length && words[i].startsWith("-")) {
      if (words[i] === "--") {
        i++
        break
      }
      if (valueFlags.has(words[i])) i++
      i++
    }
    if (name === "timeout" && i < words.length) i++
  }
  return words.slice(i)
}

function commandName(word: string): string {
  return word.slice(word.lastIndexOf("/") + 1)
}

// positionals 跳过选项（含带值选项的值），遇到 “--” 停止。
function positionals(args: readonly string[], valueFlags: ReadonlySet<string>): string[] {
  const out: string[] = []
  for (let i = 0; i < args.length; i++) {
    const arg = args[i]
    if (arg === "--") break
    if (arg.startsWith("-")) {
      if (valueFlags.has(arg)) i++
      continue
    }
    out.push(arg)
  }
  return out
}

const RM_COMMANDS = new Set(["rm", "rmdir", "unlink", "shred", "srm"])
const DISK_COMMANDS = new Set(["mkswap", "fdisk", "sfdisk", "gdisk", "parted", "wipefs", "blkdiscard"])
const POWER_COMMANDS = new Set(["reboot", "shutdown", "halt", "poweroff"])
const KILL_COMMANDS = new Set(["kill", "pkill", "killall"])
const SHELL_COMMANDS = new Set(["bash", "sh", "zsh", "dash", "ash"])
const SQL_COMMANDS = new Set(["psql", "mysql", "mariadb", "sqlite3", "clickhouse-client", "clickhouse", "cqlsh", "mongo", "mongosh"])
const SQL_DESTRUCTIVE = /\b(drop\s+(table|database|schema|index|view|user|role|collection)|truncate\s+(table\s+)?["`\w]|delete\s+from|alter\s+table\s+\S+\s+drop)\b|\.drop\(|dropdatabase\(|deletemany\(/i
const KUBECTL_VALUE_FLAGS = new Set([
  "-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user", "-s", "--server",
  "-l", "--selector", "-f", "--filename", "-c", "--container", "-o", "--output", "--field-selector",
])
const KUBECTL_DESTRUCTIVE = new Set(["delete", "drain", "cordon", "taint", "replace", "scale"])
const HELM_VALUE_FLAGS = new Set(["-n", "--namespace", "--kube-context", "--kubeconfig"])
const HELM_DESTRUCTIVE = new Set(["uninstall", "delete", "del", "un", "rollback"])
const CONTAINER_COMMANDS = new Set(["docker", "podman", "nerdctl", "crictl"])
const CONTAINER_VALUE_FLAGS = new Set([
  "-H", "--host", "--context", "-c", "--config", "-f", "--file", "-p", "--project-name",
  "-u", "--user", "-w", "--workdir", "-e", "--env", "--env-file", "--name", "-v", "--volume",
  "--network", "--entrypoint", "-l", "--log-level",
])
const CONTAINER_DESTRUCTIVE = new Set(["rm", "rmi", "rmp", "kill", "stop", "restart", "prune", "down"])
const SYSTEMCTL_VALUE_FLAGS = new Set(["-H", "--host", "-M", "--machine", "-t", "--type", "-p", "--property", "-s", "--signal"])
const SYSTEMCTL_DESTRUCTIVE = new Set([
  "stop", "restart", "try-restart", "reload-or-restart", "kill", "disable", "mask", "isolate",
  "reboot", "poweroff", "halt", "suspend", "hibernate", "rescue", "emergency",
])
const SERVICE_DESTRUCTIVE = new Set(["stop", "restart", "force-reload"])
const GIT_VALUE_FLAGS = new Set(["-C", "-c", "--git-dir", "--work-tree"])
const FIND_EXEC = new Set(["-exec", "-execdir", "-ok", "-okdir"])
const NO_FLAGS: ReadonlySet<string> = new Set()

// destructiveCommandReason 返回命令里第一个会删除、停止或覆盖数据的简单命令的
// 说明（用于命令旁的风险提示），安全命令返回 null。它不拦截执行，也不是安全边界。
export function destructiveCommandReason(command: string): string | null {
  return commandReason(command)
}

function commandReason(command: string): string | null {
  for (const segment of shellSegments(command)) {
    const reason = segmentReason(stripWrappers(segment))
    if (reason) return reason
  }
  return null
}

function segmentReason(words: readonly string[]): string | null {
  if (words.length === 0) return null
  const name = commandName(words[0])
  const args = words.slice(1)
  if (RM_COMMANDS.has(name)) return `删除文件（${name}）`
  if (name.startsWith("mkfs") || DISK_COMMANDS.has(name)) return `格式化或改写磁盘（${name}）`
  if (POWER_COMMANDS.has(name)) return `重启或关机（${name}）`
  if (KILL_COMMANDS.has(name)) return `终止进程（${name}）`
  if (name === "truncate") return "清空文件（truncate）"
  if (name === "userdel" || name === "groupdel") return `删除系统账号（${name}）`
  if (name === "dd" && args.some((arg) => arg.startsWith("of="))) return "用 dd 覆盖写入设备或文件"
  if (name === "init" && ["0", "6"].includes(args[0] ?? "")) return `重启或关机（init ${args[0]}）`
  if (name === "crontab" && args.includes("-r")) return "删除全部定时任务（crontab -r）"
  if ((name === "chmod" || name === "chown" || name === "chgrp") &&
    args.some((arg) => arg === "--recursive" || /^-[A-Za-z]*R/.test(arg))) {
    return `递归修改权限（${name} -R）`
  }
  if (name === "find") {
    if (args.includes("-delete")) return "用 find 删除匹配的文件"
    const exec = args.findIndex((arg) => FIND_EXEC.has(arg))
    if (exec >= 0) return segmentReason(stripWrappers(args.slice(exec + 1)))
    return null
  }
  if (SHELL_COMMANDS.has(name)) {
    const script = args.indexOf("-c")
    return script >= 0 && script + 1 < args.length ? commandReason(args[script + 1]) : null
  }
  if (SQL_COMMANDS.has(name)) return SQL_DESTRUCTIVE.test(args.join(" ")) ? `执行删除类 SQL（${name}）` : null
  if (name === "redis-cli") {
    return args.some((arg) => ["flushall", "flushdb"].includes(arg.toLowerCase())) ? "清空 Redis 数据" : null
  }
  if (name === "kubectl") {
    const [sub, action] = positionals(args, KUBECTL_VALUE_FLAGS)
    if (sub === "exec") {
      const separator = args.indexOf("--")
      return separator >= 0 ? segmentReason(stripWrappers(args.slice(separator + 1))) : null
    }
    if (KUBECTL_DESTRUCTIVE.has(sub) || (sub === "rollout" && (action === "restart" || action === "undo"))) {
      return `修改或删除 Kubernetes 资源（kubectl ${sub}）`
    }
    return null
  }
  if (name === "helm") {
    const [sub] = positionals(args, HELM_VALUE_FLAGS)
    return HELM_DESTRUCTIVE.has(sub) ? `卸载或回滚 Helm release（helm ${sub}）` : null
  }
  if (CONTAINER_COMMANDS.has(name)) {
    const subs = positionals(args, CONTAINER_VALUE_FLAGS)
    if (subs[0] === "exec" || subs[0] === "run") return segmentReason(subs.slice(2))
    const sub = subs.slice(0, 2).find((value) => CONTAINER_DESTRUCTIVE.has(value))
    return sub ? `删除或停止容器资源（${name} ${sub}）` : null
  }
  if (name === "systemctl") {
    const [sub] = positionals(args, SYSTEMCTL_VALUE_FLAGS)
    return SYSTEMCTL_DESTRUCTIVE.has(sub) ? `停止或重启服务（systemctl ${sub}）` : null
  }
  if (name === "service") {
    const [, action] = positionals(args, NO_FLAGS)
    return SERVICE_DESTRUCTIVE.has(action) ? `停止或重启服务（service ${action}）` : null
  }
  if (name === "git") {
    const [sub] = positionals(args, GIT_VALUE_FLAGS)
    const rewrites =
      (sub === "reset" && args.includes("--hard")) ||
      (sub === "push" && args.some((arg) => ["-f", "--force", "--force-with-lease", "--delete"].includes(arg))) ||
      (sub === "clean" && args.some((arg) => /^-[a-z]*f/.test(arg))) ||
      (sub === "branch" && args.includes("-D"))
    return rewrites ? `改写 Git 历史或工作区（git ${sub}）` : null
  }
  return null
}

export interface ShellOutputState {
  output: string
  progress: string
}

const ANSI_SEQUENCE = /\u001b\[[0-9;?]*[ -/]*[@-~]/g

// appendShellOutput 合并一段 doops_shell 流式输出。Agent 的心跳以 “\r” 开头且没有
// 换行，只更新进度提示；普通输出去掉 ANSI 控制序列后追加。
export function appendShellOutput(state: ShellOutputState, chunk: string): ShellOutputState {
  const text = chunk.replace(ANSI_SEQUENCE, "")
  if (text.startsWith("\r") && !text.includes("\n")) {
    return { output: state.output, progress: text.replace(/\r/g, "").trim() }
  }
  const lines = text.replace(/\r\n/g, "\n").split("\n").map((line) => line.slice(line.lastIndexOf("\r") + 1))
  return { output: state.output + lines.join("\n"), progress: state.progress }
}

const SHELL_SUCCESS_MARKER = "Operation complete."

// shellResultOutcome 解读 doops_shell 的最终结果文本：成功标记不当作输出，
// 命令没能启动时 Agent 也返回成功响应，这里改判为失败。
export function shellResultOutcome(text: string): { ok: boolean; text: string } {
  const trimmed = text.trim()
  if (trimmed === SHELL_SUCCESS_MARKER) return { ok: true, text: "" }
  if (trimmed.startsWith("Error starting command:")) return { ok: false, text: trimmed }
  return { ok: true, text }
}

export type ShellFailureKind = "exit" | "signal" | "canceled" | "interrupted" | "forbidden" | "offline" | "auth" | "unknown" | "blocked" | "other"

// info：用户自己停止且命令干净结束；warn：用户停止但命令可能执行了一部分；error：其余失败。
export type ShellFailureTone = "info" | "warn" | "error"

export interface ShellFailure {
  kind: ShellFailureKind
  tone: ShellFailureTone
  text: string
  exitCode?: number
}

// /api/rpc 在 Gateway 拒绝 WebSocket 升级时给出“…（HTTP n）”形式的中文原因；
// gatewayUpgradeStatus 取出其中的状态码，不是这种格式时返回 null。
export function gatewayUpgradeStatus(raw: string): string | null {
  return /（HTTP (\d{3})）$/.exec(raw.trim())?.[1] ?? null
}

// Gateway 对执行中被打断的变更命令（doops_shell 等）不能确认干净停止，会返回这条原因。
const INTERRUPTED_MUTATION = "mutation was interrupted before it finished"

// describeShellFailure 把 Gateway/Agent 的错误翻译成运维能直接处理的说明，并给出展示样式。
export function describeShellFailure(
  message: string,
  opts: { stopRequested: boolean; target: { cluster: string; instance: string } },
): ShellFailure {
  const failure = classifyShellFailure(message, opts)
  const tone: ShellFailureTone = failure.kind === "interrupted" ? "warn"
    : failure.kind === "canceled" && opts.stopRequested ? "info" : "error"
  return { ...failure, tone }
}

function classifyShellFailure(
  message: string,
  opts: { stopRequested: boolean; target: { cluster: string; instance: string } },
): Omit<ShellFailure, "tone"> {
  const raw = message.trim()
  const where = `${opts.target.cluster}/${opts.target.instance}`
  if (classifyAgentAdmission(raw).kind === "unknown") {
    // 用户点了停止、Gateway 报告命令在执行中被打断：这是用户自己的停止，但不能当作干净结束。
    if (opts.stopRequested && raw.includes(INTERRUPTED_MUTATION)) {
      return { kind: "interrupted", text: "已停止：命令在执行中被打断，可能已经执行了一部分。请先核对实际状态，再决定是否重新执行" }
    }
    return { kind: "unknown", text: `远端终态未确认：请先核对实际状态，不要直接重放原命令（${raw}）` }
  }
  const blocked = /blocked_command\(([^)]+)\):\s*(.*)/.exec(raw)
  if (blocked) return { kind: "blocked", text: `Agent 拒绝执行（${blocked[1]}）：${blocked[2]}` }
  const upgrade = gatewayUpgradeStatus(raw)
  const authMessage = gatewayAuthMessage(raw)
  if (authMessage) return { kind: "auth", text: authMessage }
  if (/^forbidden\b/i.test(raw) || upgrade === "403") return { kind: "forbidden", text: `无权在 ${where} 上执行命令：请联系管理员授权该目标` }
  if (/^target offline\b/i.test(raw) || upgrade === "404") {
    return { kind: "offline", text: `目标离线：${where} 当前未连接 Gateway，请确认 Agent 在线后重试` }
  }
  if (upgrade === "401" || raw.includes("未建立 Gateway 会话") || raw.includes("缺少 gateway 地址或 token")) {
    return { kind: "auth", text: "登录已失效，请重新登录后再执行" }
  }
  if (raw.includes("operation canceled") || raw.includes("请求派发前已停止")) {
    return { kind: "canceled", text: opts.stopRequested ? "已停止" : "命令被中止：可能超过执行时限，或会话被取消" }
  }
  const exit = /exit status (\d+)/.exec(raw)
  if (exit) {
    const exitCode = Number(exit[1])
    return { kind: "exit", exitCode, text: `命令失败，退出码 ${exitCode}` }
  }
  const signal = /signal: ([\w ]+)/.exec(raw)
  if (signal) return { kind: "signal", text: `命令被信号终止（${signal[1].trim()}）` }
  return { kind: "other", text: raw }
}

export interface TargetSwitchState {
  skillRunning: boolean
  agentBusy: boolean
  execRunning: boolean
}

// targetSwitchBlocker 返回不能切换目标的原因；可以切换时返回 null。执行栏命令还在跑时
// 切换会让面板重新挂载，用户就看不到这条命令的输出和终态了。
export function targetSwitchBlocker(state: TargetSwitchState): string | null {
  if (state.skillRunning) return "请先等待当前操作结束，再切换目标"
  if (state.agentBusy) return "请先停止当前 AI 任务，再切换目标"
  if (state.execRunning) return "AI 助手执行栏里的命令还在运行，请等它结束或停止后再切换目标"
  return null
}
