import type { Target } from "./gateway"
import { containsCredentialMaterial } from "./deployment.ts"

// A page reload must not silently move the user to another target or session.
// The selected target key and the current session id are kept per browser tab
// (sessionStorage) and per Gateway + user. Command output, credentials and
// tokens are never stored.

export type StorageLike = Pick<Storage, "getItem" | "setItem" | "removeItem">

export interface StorageScope {
  gateway: string
  username?: string
}

export interface ConsoleSelection {
  targetKey: string
  sessionId: string
}

const PREFIX = "doops.console.v1"

export function consoleScopeKey(scope: StorageScope): string {
  return `${encodeURIComponent(scope.gateway)}|${encodeURIComponent(scope.username ?? "")}`
}

function selectionKey(scope: StorageScope): string {
  return `${PREFIX}.selection:${consoleScopeKey(scope)}`
}

// sessionStorage can be missing or throw (private mode, disabled storage); the
// console then behaves as before and keeps nothing.
export function browserSessionStorage(): StorageLike | null {
  try {
    return typeof window === "undefined" ? null : window.sessionStorage
  } catch {
    return null
  }
}

export function readStored(storage: StorageLike | null, key: string): unknown {
  try {
    const raw = storage?.getItem(key)
    return raw ? JSON.parse(raw) : null
  } catch {
    return null
  }
}

export function writeStored(storage: StorageLike | null, key: string, value: unknown): void {
  try {
    if (value === null) storage?.removeItem(key)
    else storage?.setItem(key, JSON.stringify(value))
  } catch {
    // Storage is best effort; a failed write keeps the in-memory state.
  }
}

const CONTROL = /[\u0000-\u001f\u007f]/

// Same rule as the Gateway's validateSession: no path separators or "..".
export function validSessionId(value: unknown): value is string {
  return (
    typeof value === "string" &&
    value.length > 0 &&
    value.length <= 128 &&
    value.trim() === value &&
    value !== "." &&
    !value.includes("..") &&
    !/[/\\]/.test(value) &&
    !CONTROL.test(value)
  )
}

export function validTargetKey(value: unknown): value is string {
  return typeof value === "string" && value.length > 0 && value.length <= 256 && !CONTROL.test(value)
}

// A token login carries no username, so every token user on one Gateway would
// share the same key and inherit the previous user's target and session. Such
// a login keeps nothing.
export function persistsScope(scope: StorageScope): boolean {
  return Boolean(scope.username?.trim())
}

export function saveSelection(storage: StorageLike | null, scope: StorageScope, selection: ConsoleSelection): void {
  if (!persistsScope(scope)) return
  if (!validTargetKey(selection.targetKey) || !validSessionId(selection.sessionId)) return
  writeStored(storage, selectionKey(scope), { targetKey: selection.targetKey, sessionId: selection.sessionId })
}

export function loadSelection(storage: StorageLike | null, scope: StorageScope): ConsoleSelection | null {
  if (!persistsScope(scope)) return null
  const value = readStored(storage, selectionKey(scope))
  if (!value || typeof value !== "object" || Array.isArray(value)) return null
  const { targetKey, sessionId } = value as Record<string, unknown>
  if (!validTargetKey(targetKey) || !validSessionId(sessionId)) return null
  return { targetKey, sessionId }
}

export interface RestoredSelection {
  selected: Target | null
  // Set only when the saved session belongs to the restored target.
  sessionId: string | null
  // Shown once when the saved target is no longer listed.
  notice: string
}

export function restoreSelection(saved: ConsoleSelection | null, targets: readonly Target[]): RestoredSelection {
  const first = targets[0] ?? null
  if (!saved) return { selected: first, sessionId: null, notice: "" }
  const match = targets.find((target) => target.key === saved.targetKey)
  if (match) return { selected: match, sessionId: saved.sessionId, notice: "" }
  const reason = `上次选中的目标 ${saved.targetKey} 当前不在可用目标列表中（可能已离线或已取消授权）`
  return {
    selected: first,
    sessionId: null,
    notice: first ? `${reason}，已切换到 ${first.key} 并开启新会话` : `${reason}，当前没有可用目标`,
  }
}

// A command the terminal or the AI exec bar started is recorded while it runs
// and cleared when it ends. A record still present after a reload means the
// reload cut the command off and its outcome is unknown. The record holds the
// executable name only, never arguments, output, credentials or tokens, and
// nothing is ever replayed from it.

export type CommandSource = "terminal" | "exec"

export interface CommandRecord {
  id: string
  targetKey: string
  sessionId: string
  summary: string
  startedAt: string
  source: CommandSource
}

export interface CommandStart {
  targetKey: string
  sessionId: string
  command: string
  source: CommandSource
}

export const UNLOAD_WARNING = "刷新或关闭会中断正在执行的命令"

export function beforeUnloadWarning(activeCommands: number): string {
  return activeCommands > 0 ? UNLOAD_WARNING : ""
}

const SUMMARY_LIMIT = 80
const MAX_RECORDS = 20
const ENV_ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/
const EXECUTABLE = /^[A-Za-z0-9._/+-]{1,64}$/

// A token pasted into the terminal by mistake is the first word itself. A word
// with letters and digits and a run of 16 or more characters between / . _ -
// looks like one (an AWS secret key contains "/"); executable names such as
// x86_64-linux-gnu-gcc or terraform-provider-aws_v5.31.0 are short runs.
function tokenLike(word: string): boolean {
  return containsCredentialMaterial(word) ||
    (/[A-Za-z]/.test(word) && /\d/.test(word) && word.split(/[/._-]/).some((segment) => segment.length >= 16))
}

// The summary is the executable name of the first line and nothing else:
// arguments can carry a password in more forms than any pattern catches
// (mysql -pS3cret, curl -u admin:pw, echo pw | sudo -S). Leading VAR=value
// assignments are skipped; anything after the name becomes " …". A first word
// that is not a plain name, or looks like a token, is stored as "命令".
export function commandSummary(raw: string): string {
  const words = raw.trim().split(/\s+/).filter(Boolean)
  let index = 0
  while (index < words.length && ENV_ASSIGNMENT.test(words[index])) index++
  const word = words[index] ?? ""
  const name = EXECUTABLE.test(word) && !tokenLike(word) ? word : "命令"
  return index + 1 < words.length || index > 0 ? `${name} …` : name
}

function formatStartedAt(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString("zh-CN", { hour12: false })
}

export function interruptedCommandNotice(
  record: CommandRecord,
  current: { targetKey: string; sessionId: string } | null,
): string {
  const where = record.source === "exec" ? "AI 助手执行栏" : "终端"
  const detail = `（${where}命令：${record.summary}，开始于 ${formatStartedAt(record.startedAt)}）`
  if (current && current.targetKey === record.targetKey && current.sessionId === record.sessionId) {
    return `上次命令在页面刷新时被中断，结果未知；请先核对目标实际状态，可在审计页按本会话查询${detail}`
  }
  return `上次命令在页面刷新时被中断，结果未知；请先核对目标 ${record.targetKey} 的实际状态，可在审计页选择该目标后按会话 ${record.sessionId} 查询${detail}`
}

const COMMAND_ID = /^cmd_[a-z0-9]{1,40}$/

function commandRecord(value: unknown): CommandRecord | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null
  const { id, targetKey, sessionId, summary, startedAt, source } = value as Record<string, unknown>
  if (
    typeof id !== "string" || !COMMAND_ID.test(id) ||
    !validTargetKey(targetKey) || !validSessionId(sessionId) ||
    typeof summary !== "string" || summary.length > SUMMARY_LIMIT + 1 || CONTROL.test(summary) ||
    typeof startedAt !== "string" || Number.isNaN(Date.parse(startedAt)) ||
    (source !== "terminal" && source !== "exec")
  ) {
    return null
  }
  return { id, targetKey, sessionId, summary, startedAt, source }
}

function commandsKey(scope: StorageScope): string {
  return `${PREFIX}.commands:${consoleScopeKey(scope)}`
}

export class CommandJournal {
  readonly storage: () => StorageLike | null
  readonly now: () => Date
  // Commands this page started and has not seen end.
  readonly active = new Set<string>()
  unloading = false

  constructor(storage: () => StorageLike | null, now: () => Date = () => new Date()) {
    this.storage = storage
    this.now = now
  }

  begin(scope: StorageScope, start: CommandStart): string {
    const id = `cmd_${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`
    this.active.add(id)
    // An id the Gateway would reject cannot be looked up later; still count it
    // as running so the unload warning covers it.
    if (persistsScope(scope) && validTargetKey(start.targetKey) && validSessionId(start.sessionId)) {
      const record: CommandRecord = {
        id,
        targetKey: start.targetKey,
        sessionId: start.sessionId,
        summary: commandSummary(start.command),
        startedAt: this.now().toISOString(),
        source: start.source,
      }
      this.write(scope, [...this.read(scope), record].slice(-MAX_RECORDS))
    }
    return id
  }

  // While the page unloads, the aborted stream ends the command; its record
  // stays so the next page can report it.
  end(scope: StorageScope, id: string): void {
    this.active.delete(id)
    if (this.unloading) return
    this.write(scope, this.read(scope).filter((record) => record.id !== id))
  }

  activeCount(): number {
    return this.active.size
  }

  setUnloading(unloading: boolean): void {
    this.unloading = unloading
  }

  // Returns the records an earlier page left behind, once. Commands this page
  // is still running are kept and not reported.
  takeInterrupted(scope: StorageScope): CommandRecord[] {
    if (!persistsScope(scope)) return []
    const records = this.read(scope)
    this.write(scope, records.filter((record) => this.active.has(record.id)))
    return records.filter((record) => !this.active.has(record.id))
  }

  read(scope: StorageScope): CommandRecord[] {
    const value = readStored(this.storage(), commandsKey(scope))
    if (!Array.isArray(value)) return []
    return value.map(commandRecord).filter((record): record is CommandRecord => record !== null)
  }

  write(scope: StorageScope, records: CommandRecord[]): void {
    writeStored(this.storage(), commandsKey(scope), records.length ? records : null)
  }
}

// One journal per page, shared by the terminal, the exec bar and the shell.
export const commandJournal = new CommandJournal(browserSessionStorage)

// The release page keeps, per target, its release session (so a running apply
// stays visible after switching views) and the unsent form draft. The draft
// text comes from deploymentDraftText, which never contains credential material.
export interface DeployState {
  sessionId: string | null
  draft: string | null
  previousRelease?: DeployReceipt
}

// Keep only the last unknown release's identity and status; load its evidence from the hosted session.
export interface DeployReceipt {
  sessionId: string
  status: "unknown"
}

interface DeployEntry {
  targetKey: string
  sessionId?: string
  draft?: string
  previousRelease?: DeployReceipt
}

const MAX_DEPLOY_TARGETS = 20
const MAX_DRAFT_LENGTH = 128_000

function deployKey(scope: StorageScope): string {
  return `${PREFIX}.deploy:${consoleScopeKey(scope)}`
}

function deployEntry(value: unknown): DeployEntry | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null
  const { targetKey, sessionId, draft, previousRelease } = value as Record<string, unknown>
  if (!validTargetKey(targetKey)) return null
  const entry: DeployEntry = { targetKey }
  if (validSessionId(sessionId)) entry.sessionId = sessionId
  if (typeof draft === "string" && draft.length <= MAX_DRAFT_LENGTH) entry.draft = draft
  const receipt = deployReceipt(previousRelease)
  if (receipt) entry.previousRelease = receipt
  return entry
}

function deployReceipt(value: unknown): DeployReceipt | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined
  const { sessionId, status } = value as Record<string, unknown>
  if (!validSessionId(sessionId) || status !== "unknown") return undefined
  return { sessionId, status }
}

// A token login stores nothing (persistsScope), yet a running release must stay visible after switching views. Its
// entries live in memory only, keyed by that login's session object, so the next login in the tab, by any account,
// starts empty (the same approach as createK8sFormStore in k8s.ts).
const memoryDeployEntries = new WeakMap<object, DeployEntry[]>()

function readDeployEntries(storage: StorageLike | null, scope: StorageScope): DeployEntry[] {
  if (!persistsScope(scope)) return memoryDeployEntries.get(scope) ?? []
  const value = readStored(storage, deployKey(scope))
  if (!Array.isArray(value)) return []
  return value.map(deployEntry).filter((entry): entry is DeployEntry => entry !== null)
}

function writeDeployEntries(storage: StorageLike | null, scope: StorageScope, entries: DeployEntry[]): void {
  const kept = entries.slice(-MAX_DEPLOY_TARGETS)
  if (persistsScope(scope)) writeStored(storage, deployKey(scope), kept.length ? kept : null)
  else if (kept.length) memoryDeployEntries.set(scope, kept)
  else memoryDeployEntries.delete(scope)
}

// Most recent target last; an entry with neither a session nor a draft is dropped.
function updateDeployEntry(storage: StorageLike | null, scope: StorageScope, targetKey: string, change: Partial<DeployEntry>): void {
  if (!validTargetKey(targetKey)) return
  const entries = readDeployEntries(storage, scope)
  const previous = entries.find((entry) => entry.targetKey === targetKey)
  const next: DeployEntry = { ...previous, ...change, targetKey }
  if (next.sessionId === undefined) delete next.sessionId
  if (next.draft === undefined) delete next.draft
  const rest = entries.filter((entry) => entry.targetKey !== targetKey)
  writeDeployEntries(storage, scope, next.sessionId || next.draft || next.previousRelease ? [...rest, next] : rest)
}

export function loadDeployState(storage: StorageLike | null, scope: StorageScope, targetKey: string): DeployState {
  const entry = readDeployEntries(storage, scope).find((candidate) => candidate.targetKey === targetKey)
  return {
    sessionId: entry?.sessionId ?? null,
    draft: entry?.draft ?? null,
    ...(entry?.previousRelease ? { previousRelease: entry.previousRelease } : {}),
  }
}

export function saveDeploySession(storage: StorageLike | null, scope: StorageScope, targetKey: string, sessionId: string, previousRelease?: DeployReceipt): void {
  if (!validSessionId(sessionId)) return
  const receipt = deployReceipt(previousRelease)
  if (previousRelease !== undefined && !receipt) return
  updateDeployEntry(storage, scope, targetKey, { sessionId, ...(receipt ? { previousRelease: receipt } : {}) })
}

export function saveDeployDraft(storage: StorageLike | null, scope: StorageScope, targetKey: string, draft: string | null): void {
  if (draft !== null && draft.length > MAX_DRAFT_LENGTH) draft = null
  updateDeployEntry(storage, scope, targetKey, { draft: draft ?? undefined })
}

// Earlier console versions kept the release session and draft under these
// keys without a user, so they are removed at logout too.
const LEGACY_DEPLOY_PREFIXES = ["doops.deploy-session.", "doops.deploy-draft."]

function removeLegacyDeployKeys(storage: StorageLike | null): void {
  const listable = storage as (StorageLike & Partial<Pick<Storage, "key" | "length">>) | null
  if (!listable || typeof listable.key !== "function" || typeof listable.length !== "number") return
  try {
    const keys: string[] = []
    for (let index = 0; index < listable.length; index++) {
      const key = listable.key(index)
      if (key && LEGACY_DEPLOY_PREFIXES.some((prefix) => key.startsWith(prefix))) keys.push(key)
    }
    for (const key of keys) listable.removeItem(key)
  } catch {
    // Storage is best effort.
  }
}

// Logging out removes everything this tab kept for that Gateway and user, so
// the next login in the same tab starts from nothing.
export function clearConsoleState(storage: StorageLike | null, scope: StorageScope): void {
  writeStored(storage, selectionKey(scope), null)
  writeStored(storage, commandsKey(scope), null)
  writeStored(storage, deployKey(scope), null)
  memoryDeployEntries.delete(scope)
  removeLegacyDeployKeys(storage)
}
