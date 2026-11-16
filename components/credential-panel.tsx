"use client"

import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import type { Session, Target } from "@/lib/client"
import { mapWithConcurrency } from "@/lib/console-refresh"
import { randomSession } from "@/lib/gateway"
import {
  createCredential,
  bindCredentialToTarget,
  buildBindingScope,
  listCredentialBindings,
  listCredentialVersions,
  listCredentials,
  promoteCredentialVersion,
  putCredentialPayload,
  revokeCredentialBinding,
  revokeCredential,
  verifyCredentialVersion,
  canImportFromSecret,
  type CredentialBinding,
  type CredentialBindingScope,
  type CredentialResource,
  type CredentialScope,
  type CredentialType,
  type CredentialUse,
  type CredentialVerificationResponse,
  type CredentialVersion,
} from "@/lib/credentials"
import { SegmentedControl, Button, Select, Input, Textarea } from "./ui"
import {
  BanIcon,
  CheckIcon,
  KeyIcon,
  PlusIcon,
  RefreshIcon,
  SaveIcon,
  ShieldIcon,
} from "./icons"
import { SecretImportDialog } from "./credential-secret-import"
import { CredentialBundleButton } from "./credential-bundle-dialog"

const TYPE_LABELS: Record<CredentialType, string> = {
  registry: "OCI Registry",
  tls: "TLS",
  opaque: "Opaque Secret",
  helmRepository: "Helm Repository",
  gitToken: "Git Token",
  modelProvider: "Model Provider",
}

const TYPE_USES: Record<CredentialType, CredentialUse> = {
  registry: "imagePull",
  tls: "tls",
  opaque: "opaqueSecret",
  helmRepository: "helmPull",
  gitToken: "gitCheckout",
  modelProvider: "modelProvider",
}

// Two requests per managed resource; at most this many resources load at once.
const MANAGED_DETAIL_CONCURRENCY = 4

type Action =
  | { kind: "payload"; credential: CredentialResource }
  | { kind: "binding"; credential: CredentialResource }
  | { kind: "import"; credential: CredentialResource }
  | { kind: "version"; credential: CredentialResource; version: CredentialVersion }
  | {
      kind: "verify"
      credential: CredentialResource
      version: CredentialVersion
      binding: CredentialBinding
    }
  | null

interface VerificationStatus {
  status: string
  evidence: number
}

function fmtTime(value?: string) {
  if (!value) return "-"
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString("zh-CN", { hour12: false })
}

export function CredentialPanel({ session, target }: { session: Session; target: Target }) {
  const [view, setView] = useState<"target" | "managed">("target")
  const [rows, setRows] = useState<CredentialResource[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState("")
  const [notice, setNotice] = useState("")
  const [mutating, setMutating] = useState(false)
  const mutationRef = useRef(false)
  const [showCreate, setShowCreate] = useState(false)
  const [action, setAction] = useState<Action>(null)
  const [versionsByCredential, setVersionsByCredential] = useState<
    Record<string, CredentialVersion[]>
  >({})
  const [selectedVersionIDs, setSelectedVersionIDs] = useState<Record<string, string>>({})
  const [verificationStatuses, setVerificationStatuses] = useState<
    Record<string, VerificationStatus>
  >({})

  const loadController = useRef<AbortController | null>(null)

  // Loads run on a view or target change and on a user action, never on the
  // console's background target poll. A newer load aborts the older one.
  const load = useCallback(async () => {
    loadController.current?.abort()
    const controller = new AbortController()
    loadController.current = controller
    const { signal } = controller
    setLoading(true)
    setError("")
    try {
      let nextRows: CredentialResource[]
      const versionEntries: Array<[string, CredentialVersion[]]> = []
      if (session.demo) {
        nextRows = await listCredentials(session, target, view)
      } else if (view === "managed") {
        // Gateway has no batch endpoint for versions and grants, so each
        // resource takes two requests; keep only a few resources in flight.
        const managedRows = await listCredentials(session, target, "managed", signal)
        nextRows = await mapWithConcurrency(
          managedRows,
          MANAGED_DETAIL_CONCURRENCY,
          async (resource) => {
            const [versions, bindings] = await Promise.all([
              listCredentialVersions(session, resource.id, signal),
              listCredentialBindings(session, resource.id, signal),
            ])
            versionEntries.push([resource.id, versions])
            return { ...resource, bindings }
          },
          signal,
        )
      } else {
        // Target visibility is authoritative for operators. Do not load the
        // management view here: its owner/admin boundary is independent and
        // must not erase a successful target credential response.
        nextRows = await listCredentials(session, target, "target", signal)
      }
      if (signal.aborted) return
      const nextVersions = Object.fromEntries(versionEntries)
      setRows(nextRows)
      setVersionsByCredential(nextVersions)
      setSelectedVersionIDs((current) => {
        const next = { ...current }
        for (const [credentialID, versions] of Object.entries(nextVersions)) {
          if (!versions.some((version) => version.id === next[credentialID])) {
            next[credentialID] =
              versions.find((version) => version.state === "staged")?.id ??
              versions.find((version) => version.state === "active")?.id ??
              versions[0]?.id ??
              ""
          }
        }
        return next
      })
    } catch (err) {
      if (signal.aborted) return
      setRows([])
      setVersionsByCredential({})
      setSelectedVersionIDs({})
      setError((err as Error).message)
    } finally {
      if (loadController.current === controller) setLoading(false)
    }
    // The target poll replaces the object on every heartbeat; reload only when
    // the target itself changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [session, target.cluster, target.instance, view])

  useEffect(() => {
    load()
    return () => loadController.current?.abort()
  }, [load])

  function completed(message: string) {
    setNotice(message)
    window.setTimeout(() => setNotice(""), 4000)
  }

  async function revoke(resource: CredentialResource) {
    if (mutationRef.current) return
    mutationRef.current = true
    setMutating(true)
    setError("")
    try {
      await revokeCredential(session, resource.id)
      completed(`已吊销 ${resource.name}`)
      await load()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      mutationRef.current = false
      setMutating(false)
    }
  }

  async function revokeBinding(resource: CredentialResource, binding: CredentialBinding) {
    if (mutationRef.current) return
    mutationRef.current = true
    setMutating(true)
    setError("")
    try {
      await revokeCredentialBinding(session, resource.id, binding.id)
      completed(`已解除 ${resource.name} 的一条目标绑定`)
      await load()
    } catch (err) {
      setError((err as Error).message)
    } finally {
      mutationRef.current = false
      setMutating(false)
    }
  }

  async function promote(resource: CredentialResource, version: CredentialVersion) {
    if (version.state !== "staged" || mutationRef.current) return
    mutationRef.current = true
    setMutating(true)
    setError("")
    try {
      await promoteCredentialVersion(session, resource.id, version.id)
      setAction(null)
      await load()
      completed(`已激活 ${resource.name} 的版本 ${version.id}`)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      mutationRef.current = false
      setMutating(false)
    }
  }

  function selectVersion(credentialID: string, versionID: string) {
    setSelectedVersionIDs((current) => ({ ...current, [credentialID]: versionID }))
  }

  function rememberVersion(credentialID: string, version: CredentialVersion) {
    setVersionsByCredential((current) => ({
      ...current,
      [credentialID]: [
        version,
        ...(current[credentialID] ?? []).filter((candidate) => candidate.id !== version.id),
      ],
    }))
    selectVersion(credentialID, version.id)
  }

  function rememberVerification(
    resource: CredentialResource,
    version: CredentialVersion,
    result: CredentialVerificationResponse,
  ) {
    setVerificationStatuses((current) => ({
      ...current,
      [`${resource.id}:${version.id}`]: {
        status: result.status,
        evidence: result.evidence.length,
      },
    }))
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b bg-card px-4 py-3">
        <div>
          <div className="flex items-center gap-2">
            <KeyIcon width={17} height={17} className="text-primary" />
            <h2 className="text-sm font-semibold text-foreground">凭据资源</h2>
          </div>
          <p className="mt-0.5 text-xs text-muted-foreground">
            {target.cluster}/{target.instance} · 操作权限包含凭据使用权限
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <SegmentedControl
            value={view}
            onChange={setView}
            ariaLabel="凭据范围"
            options={[
              { value: "target", label: "当前目标可用" },
              { value: "managed", label: "我管理的" },
            ]}
          />
          <button
            onClick={load}
            disabled={loading}
            title="刷新凭据"
            className="flex h-8 w-8 items-center justify-center rounded-md border text-muted-foreground hover:bg-muted hover:text-foreground disabled:opacity-50"
          >
            <RefreshIcon width={15} height={15} className={loading ? "animate-spin" : ""} />
          </button>
          <CredentialBundleButton session={session} target={target} />
          <button
            onClick={() => {
              setView("managed")
              setShowCreate(true)
            }}
            disabled={session.demo}
            title={session.demo ? "演示模式不执行凭据写操作" : "新建凭据"}
            className="flex items-center gap-1.5 rounded-md bg-primary px-3 py-2 text-xs font-medium text-primary-foreground disabled:cursor-not-allowed disabled:opacity-50"
          >
            <PlusIcon width={14} height={14} /> 新建凭据
          </button>
        </div>
      </div>

      {error && (
        <div className="border-b border-destructive/30 bg-destructive/10 px-4 py-2 text-xs text-destructive">
          {error}
        </div>
      )}
      {notice && (
        <div className="flex items-center gap-2 border-b border-success/30 bg-success/10 px-4 py-2 text-xs text-success">
          <CheckIcon width={14} height={14} /> {notice}
        </div>
      )}
      {showCreate && (
        <CreateCredentialForm
          session={session}
          onCancel={() => setShowCreate(false)}
          onCreated={async (resource) => {
            setShowCreate(false)
            completed(`已创建 ${resource.name}`)
            await load()
          }}
          onError={setError}
        />
      )}

      <div className="min-h-0 flex-1 overflow-auto">
        {/* Fixed column widths keep the table inside a 1200px window. The
            action column wraps its buttons and stays pinned to the right, so
            a narrow window may scroll the table but never hides an action. */}
        <table className="w-full min-w-[900px] table-fixed border-collapse text-sm">
          <thead className="sticky top-0 z-20 bg-card">
            <tr className="border-b text-left text-xs text-muted-foreground">
              <th className="w-[240px] px-4 py-2.5 font-medium">名称</th>
              <th className="w-[120px] px-3 py-2.5 font-medium">类型</th>
              <th className="w-[64px] px-3 py-2.5 font-medium">归属</th>
              <th className="w-[84px] px-3 py-2.5 font-medium">状态</th>
              <th className="px-3 py-2.5 font-medium">
                {view === "target" ? "目标绑定" : "版本 / 目标绑定"}
              </th>
              <th className="sticky right-0 w-[220px] border-l bg-card px-3 py-2.5 text-right font-medium">操作</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((resource) => {
              const versions = versionsByCredential[resource.id] ?? []
              const version = versions.find(
                (candidate) => candidate.id === selectedVersionIDs[resource.id],
              )
              const verification = version
                ? verificationStatuses[`${resource.id}:${version.id}`]
                : undefined
              return (
              <tr key={resource.id} className="border-b border-border/60 align-top">
                <td className="px-4 py-3">
                  <div className="break-words font-medium text-foreground">{resource.name}</div>
                  <div className="mt-0.5 break-all font-mono text-[11px] text-muted-foreground">{resource.id}</div>
                </td>
                <td className="px-3 py-3 text-xs text-foreground">{TYPE_LABELS[resource.type]}</td>
                <td className="px-3 py-3 text-xs text-muted-foreground">
                  {resource.scope === "platform" ? "平台" : "个人"}
                </td>
                <td className="px-3 py-3">
                  <span
                    className={`rounded px-1.5 py-0.5 text-xs ${
                      resource.state === "active"
                        ? "bg-success/15 text-success"
                        : "bg-muted text-muted-foreground"
                    }`}
                  >
                    {resource.state}
                  </span>
                </td>
                <td className="min-w-0 break-words px-3 py-3 text-xs text-muted-foreground">
                  {view === "target" ? (
                    <div className="flex flex-col gap-2">
                      <BindingSummary
                        resource={resource}
                        version={version}
                        disabled
                        onVerify={(binding) =>
                          version &&
                          setAction({ kind: "verify", credential: resource, version, binding })
                        }
                        onRevoke={(binding) => revokeBinding(resource, binding)}
                      />
                      {versions.length > 0 && (
                        <VersionSummary
                          updatedAt={resource.updated_at}
                          versions={versions}
                          selectedVersion={version}
                          verification={verification}
                          onSelect={(versionID) => selectVersion(resource.id, versionID)}
                        />
                      )}
                    </div>
                  ) : (
                    <div className="flex flex-col gap-2">
                      <VersionSummary
                        updatedAt={resource.updated_at}
                        versions={versions}
                        selectedVersion={version}
                        verification={verification}
                        onSelect={(versionID) => selectVersion(resource.id, versionID)}
                      />
                      <BindingSummary
                        resource={resource}
                        version={version}
                        disabled={Boolean(session.demo)}
                        busy={mutating}
                        onVerify={(binding) =>
                          version &&
                          setAction({ kind: "verify", credential: resource, version, binding })
                        }
                        onRevoke={(binding) => revokeBinding(resource, binding)}
                      />
                    </div>
                  )}
                </td>
                <td className="sticky right-0 z-10 border-l border-border/60 bg-background px-3 py-3">
                  <div className="flex flex-wrap justify-end gap-1.5 [&>button]:whitespace-nowrap">
                    {view === "managed" && resource.state === "active" && canImportFromSecret(resource.type) && (
                      <Button
                        onClick={() => setAction({ kind: "import", credential: resource })}
                        disabled={mutating}
                        size="sm"
                      >
                        从 Secret 导入
                      </Button>
                    )}
                    {view === "managed" && resource.state === "active" && !session.demo && (
                      <>
                        <Button
                          onClick={() => setAction({ kind: "payload", credential: resource })}
                          disabled={mutating}
                          size="sm"
                        >
                          写入版本
                        </Button>
                        {version && (
                          <>
                            <Button
                              onClick={() => setAction({ kind: "version", credential: resource, version })}
                              size="sm"
                            >
                              查看版本
                            </Button>
                            {version.state === "staged" && (
                              <>
                                <button
                                  onClick={() => promote(resource, version)}
                                  disabled={mutating}
                                  className="rounded-md bg-primary px-2 py-1 text-xs font-medium text-primary-foreground hover:opacity-90 disabled:opacity-50"
                                >
                                  激活
                                </button>
                              </>
                            )}
                          </>
                        )}
                        <Button
                          onClick={() => setAction({ kind: "binding", credential: resource })}
                          disabled={mutating}
                          size="sm"
                        >
                          <ShieldIcon width={12} height={12} /> 绑定目标
                        </Button>
                        <button
                          onClick={() => revoke(resource)}
                          disabled={mutating}
                          title="吊销凭据"
                          className="flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground hover:bg-destructive/15 hover:text-destructive disabled:opacity-50"
                        >
                          <BanIcon width={14} height={14} />
                        </button>
                      </>
                    )}
                  </div>
                </td>
              </tr>
              )
            })}
            {!rows.length && !loading && (
              <tr>
                <td colSpan={6} className="px-4 py-16 text-center text-sm text-muted-foreground">
                  {error
                    ? "凭据加载失败，请查看错误并刷新"
                    : view === "target"
                    ? "当前目标没有已配置的可用凭据"
                    : "当前账号没有可管理的凭据"}
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>

      {action?.kind === "payload" && (
        <PayloadDialog
          session={session}
          credential={action.credential}
          onClose={() => setAction(null)}
          onDone={(version) => {
            rememberVersion(action.credential.id, version)
            setAction(null)
            completed(`已写入暂存版本 ${version.id}`)
          }}
          onError={setError}
        />
      )}
      {action?.kind === "import" && (
        <SecretImportDialog
          session={session}
          target={target}
          credential={action.credential}
          onClose={() => setAction(null)}
          onDone={(version) => {
            rememberVersion(action.credential.id, version)
            setAction(null)
            completed(`已从 Secret 导入暂存版本 ${version.id}`)
          }}
          onError={setError}
        />
      )}
      {action?.kind === "binding" && (
        <BindingDialog
          session={session}
          target={target}
          credential={action.credential}
          onClose={() => setAction(null)}
          onDone={async (message) => {
            setAction(null)
            completed(message)
            await load()
          }}
          onError={setError}
        />
      )}
      {action?.kind === "version" && (
        <VersionDialog
          credential={action.credential}
          version={action.version}
          busy={mutating}
          verification={verificationStatuses[`${action.credential.id}:${action.version.id}`]}
          onClose={() => setAction(null)}
          onPromote={() => promote(action.credential, action.version)}
        />
      )}
      {action?.kind === "verify" && (
        <VerifyDialog
          session={session}
          credential={action.credential}
          version={action.version}
          binding={action.binding}
          onClose={() => setAction(null)}
          onDone={(result) => {
            rememberVerification(action.credential, action.version, result)
            setAction(null)
            completed(
              `版本 ${action.version.id} 已验证，返回 ${result.evidence.length} 条 metadata evidence`,
            )
          }}
          onError={setError}
        />
      )}
    </div>
  )
}

function VersionDialog({
  credential,
  version,
  busy,
  verification,
  onClose,
  onPromote,
}: {
  credential: CredentialResource
  version: CredentialVersion
  busy: boolean
  verification?: VerificationStatus
  onClose: () => void
  onPromote: () => void
}) {
  return (
    <DialogFrame title={`凭据版本 · ${credential.name}`} onClose={onClose}>
      <div className="space-y-3 p-4 text-xs">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <ReadOnlyField label="Version ID" value={version.id} />
          <ReadOnlyField label="状态" value={version.state} />
          <ReadOnlyField label="Payload digest" value={version.payload_digest} />
          <ReadOnlyField label="创建时间" value={fmtTime(version.created_at)} />
          <ReadOnlyField label="激活时间" value={fmtTime(version.promoted_at)} />
        </div>
        {verification && (
          <div className="rounded-md border border-success/30 bg-success/10 px-3 py-2 text-success">
            已验证：{verification.status} · {verification.evidence} 条 metadata evidence
          </div>
        )}
        <p className="text-muted-foreground">
          页面只保存 Gateway 返回的版本元数据；payload 不会进入页面状态、任务上下文或公开请求结果。
        </p>
        <div className="flex justify-end gap-2">
          <Button
            onClick={onClose}
            size="sm"
          >
            关闭
          </Button>
          {version.state === "staged" && (
            <Button
              onClick={onPromote}
              disabled={busy}
              variant="primary" size="sm"
            >
              激活版本
            </Button>
          )}
        </div>
      </div>
    </DialogFrame>
  )
}

function VerifyDialog({
  session,
  credential,
  version,
  binding,
  onClose,
  onDone,
  onError,
}: {
  session: Session
  credential: CredentialResource
  version: CredentialVersion
  binding: CredentialBinding
  onClose: () => void
  onDone: (result: CredentialVerificationResponse) => void
  onError: (message: string) => void
}) {
  const [busy, setBusy] = useState(false)
  const [sessionID] = useState(() => randomSession("credential-verify"))
  const scope = binding.scope

  async function submit() {
    setBusy(true)
    onError("")
    try {
      const result = await verifyCredentialVersion(session, credential.id, {
        version_id: version.id,
        session_id: sessionID,
        scope,
      })
      onDone(result)
    } catch (err) {
      onError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <DialogFrame title={`验证暂存版本 · ${credential.name}`} onClose={onClose}>
      <div className="space-y-3 p-4">
        <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <ReadOnlyField label="Version ID" value={version.id} />
          <ReadOnlyField label="Binding ID" value={binding.id} />
          <ReadOnlyField label="Cluster / instance" value={`${scope.cluster}/${scope.instance}`} />
          <ReadOnlyField label="用途" value={scope.use || "按凭据类型推断"} />
        </div>
        <p className="text-xs text-muted-foreground">
          将按该 target 验证 staged version；namespace 等运行参数只用于物化，不参与授权。
        </p>
        <div className="flex justify-end gap-2">
          <Button
            onClick={onClose}
            size="sm"
          >
            取消
          </Button>
          <Button
            onClick={submit}
            disabled={busy || version.state !== "staged" || binding.state !== "active"}
            variant="primary" size="sm"
          >
            {busy ? "验证中" : "验证 target"}
          </Button>
        </div>
      </div>
    </DialogFrame>
  )
}

function BindingSummary({
  resource,
  version,
  disabled,
  busy = false,
  onVerify,
  onRevoke,
}: {
  resource: CredentialResource
  version?: CredentialVersion
  disabled: boolean
  busy?: boolean
  onVerify: (binding: CredentialBinding) => void
  onRevoke: (binding: CredentialBinding) => void
}) {
  if (!resource.bindings?.length) return <span>-</span>
  return (
    <div className="flex flex-col gap-1">
      {resource.bindings.map((binding) => (
        <div key={binding.id} className="flex flex-col gap-1.5">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="font-mono text-[11px] text-muted-foreground">{binding.id}</span>
            <span
              className={`rounded px-1.5 py-0.5 ${
                binding.state === "active"
                  ? "bg-success/15 text-success"
                  : "bg-muted text-muted-foreground"
              }`}
            >
              {binding.state}
            </span>
            {binding.state === "active" && !disabled && version?.state === "staged" && (
              <button
                onClick={() => onVerify(binding)}
                disabled={busy}
                className="rounded border px-1.5 py-0.5 text-[11px] text-foreground hover:bg-muted disabled:opacity-50"
              >
                验证暂存
              </button>
            )}
            {binding.state === "active" && !disabled && (
              <button
                onClick={() => onRevoke(binding)}
                disabled={busy}
                title="解除绑定"
                className="flex h-6 w-6 items-center justify-center rounded text-muted-foreground hover:bg-destructive/15 hover:text-destructive disabled:opacity-50"
              >
                <BanIcon width={12} height={12} />
              </button>
            )}
          </div>
          <div>
            {binding.scope.cluster}/{binding.scope.instance} · 用途 {binding.scope.use || "自动推断"}
          </div>
        </div>
      ))}
    </div>
  )
}

function VersionSummary({
  updatedAt,
  versions,
  selectedVersion,
  verification,
  onSelect,
}: {
  updatedAt?: string
  versions: CredentialVersion[]
  selectedVersion?: CredentialVersion
  verification?: VerificationStatus
  onSelect: (versionID: string) => void
}) {
  return (
    <div className="flex flex-col gap-1">
      <span>更新时间：{fmtTime(updatedAt)}</span>
      {versions.length > 0 ? (
        <>
          <label className="flex flex-wrap items-center gap-1.5">
            <span>版本</span>
            <Select
              className="min-w-48 py-1 text-[11px]"
              value={selectedVersion?.id ?? ""}
              onChange={(event) => onSelect(event.target.value)}
            >
              {versions.map((version) => (
                <option key={version.id} value={version.id}>
                  {version.state} · {version.id}
                </option>
              ))}
            </Select>
          </label>
          {selectedVersion ? (
            <>
              <span className="flex flex-wrap items-center gap-1.5">
                <span className="rounded bg-muted px-1.5 py-0.5 text-foreground">
                  {selectedVersion.state}
                </span>
                {verification && (
                  <span className="rounded bg-success/15 px-1.5 py-0.5 text-success">
                    {verification.status} · {verification.evidence} 条证据
                  </span>
                )}
              </span>
              <span className="font-mono text-[11px] text-muted-foreground">
                {selectedVersion.id}
              </span>
              <span className="font-mono text-[11px] text-muted-foreground">
                digest {selectedVersion.payload_digest}
              </span>
            </>
          ) : (
            <span className="text-muted-foreground">请选择一个版本</span>
          )}
        </>
      ) : (
        <span className="text-muted-foreground">当前资源没有可见版本</span>
      )}
    </div>
  )
}

function CreateCredentialForm({
  session,
  onCancel,
  onCreated,
  onError,
}: {
  session: Session
  onCancel: () => void
  onCreated: (resource: CredentialResource) => void
  onError: (message: string) => void
}) {
  const [name, setName] = useState("")
  const [scope, setScope] = useState<CredentialScope>("personal")
  const [type, setType] = useState<CredentialType>("registry")
  const [busy, setBusy] = useState(false)

  async function submit() {
    if (!name.trim()) return
    setBusy(true)
    onError("")
    try {
      onCreated(await createCredential(session, { name: name.trim(), scope, type }))
    } catch (err) {
      onError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex flex-wrap items-end gap-3 border-b bg-muted/30 px-4 py-3">
      <label className="flex min-w-48 flex-1 flex-col gap-1 text-xs text-muted-foreground">
        资源名称
        <Input
          value={name}
          onChange={(event) => setName(event.target.value)}
          placeholder="registry-oilan"
        />
      </label>
      <label className="flex flex-col gap-1 text-xs text-muted-foreground">
        归属
        <Select
          className="min-w-32"
          value={scope}
          onChange={(event) => setScope(event.target.value as CredentialScope)}
        >
          <option value="personal">个人</option>
          <option value="platform">平台</option>
        </Select>
      </label>
      <label className="flex flex-col gap-1 text-xs text-muted-foreground">
        类型
        <Select
          className="min-w-44"
          value={type}
          onChange={(event) => setType(event.target.value as CredentialType)}
        >
          {Object.entries(TYPE_LABELS).map(([value, label]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </Select>
      </label>
      <Button
        onClick={onCancel}
        size="sm"
      >
        取消
      </Button>
      <Button
        onClick={submit}
        disabled={busy || !name.trim()}
        variant="primary" size="sm"
      >
        {busy ? "创建中" : "创建"}
      </Button>
    </div>
  )
}

function DialogFrame({
  title,
  children,
  onClose,
}: {
  title: string
  children: React.ReactNode
  onClose: () => void
}) {
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      role="dialog"
      aria-modal="true"
    >
      <div className="max-h-[90dvh] w-full max-w-xl overflow-auto rounded-lg border bg-card shadow-xl">
        <div className="flex items-center justify-between border-b px-4 py-3">
          <h3 className="text-sm font-semibold text-foreground">{title}</h3>
          <button
            onClick={onClose}
            className="rounded px-2 py-1 text-xs text-muted-foreground hover:bg-muted"
          >
            关闭
          </button>
        </div>
        {children}
      </div>
    </div>
  )
}

function PayloadDialog({
  session,
  credential,
  onClose,
  onDone,
  onError,
}: {
  session: Session
  credential: CredentialResource
  onClose: () => void
  onDone: (version: CredentialVersion) => void
  onError: (message: string) => void
}) {
  const [payload, setPayload] = useState("")
  const [busy, setBusy] = useState(false)

  async function submit() {
    try {
      JSON.parse(payload)
    } catch {
      onError("凭据内容必须是合法 JSON")
      return
    }
    setBusy(true)
    onError("")
    try {
      const version = await putCredentialPayload(session, credential.id, payload)
      setPayload("")
      onDone(version)
    } catch (err) {
      onError((err as Error).message)
    } finally {
      setPayload("")
      setBusy(false)
    }
  }

  return (
    <DialogFrame
      title={`写入新版本 · ${credential.name}`}
      onClose={() => {
        setPayload("")
        onClose()
      }}
    >
      <div className="space-y-3 p-4">
        <p className="text-xs text-muted-foreground">
          内容只发送一次并加密持久化，提交后不会在页面或 API 中回显。
        </p>
        <Textarea
          value={payload}
          onChange={(event) => setPayload(event.target.value)}
          rows={9}
          spellCheck={false}
          autoComplete="off"
          className="w-full resize-y font-mono text-xs"
          placeholder='{"server":"registry.example.com","username":"deploy","password":"..."}'
        />
        <div className="flex justify-end">
          <Button
            onClick={submit}
            disabled={busy || !payload.trim()}
            variant="primary" size="sm"
          >
            <SaveIcon width={14} height={14} /> {busy ? "写入中" : "写入暂存版本"}
          </Button>
        </div>
      </div>
    </DialogFrame>
  )
}

function BindingDialog({
  session,
  target,
  credential,
  onClose,
  onDone,
  onError,
}: {
  session: Session
  target: Target
  credential: CredentialResource
  onClose: () => void
  onDone: (message: string) => void
  onError: (message: string) => void
}) {
  const [selectedUse, setSelectedUse] = useState<CredentialUse>(TYPE_USES[credential.type])
  const use = selectedUse
  const [namespace, setNamespace] = useState("")
  const [requiredKeys, setRequiredKeys] = useState("")
  const [busy, setBusy] = useState(false)

  async function submit() {
    let scope: CredentialBindingScope
    try {
      scope = buildBindingScope(target, { use, namespace, requiredKeys })
    } catch (err) {
      onError((err as Error).message)
      return
    }
    setBusy(true)
    onError("")
    try {
      await bindCredentialToTarget(session, credential.id, scope)
      onDone(`已绑定 ${credential.name} 到 ${target.cluster}/${target.instance}`)
    } catch (err) {
      onError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <DialogFrame title={`绑定当前目标 · ${credential.name}`} onClose={onClose}>
      <div className="grid grid-cols-1 gap-3 p-4 sm:grid-cols-2">
        <ReadOnlyField label="目标" value={`${target.cluster}/${target.instance}`} />
        {credential.type === "opaque" ? (
          <label className="flex flex-col gap-1 text-xs text-muted-foreground">
            用途
            <Select value={use} onChange={(event) => setSelectedUse(event.target.value as CredentialUse)}>
              <option value="opaqueSecret">Opaque Secret</option>
              <option value="database">数据库连接</option>
            </Select>
          </label>
        ) : <ReadOnlyField label="用途" value={use} />}
        <TextField
          label="Secret namespace"
          value={namespace}
          onChange={setNamespace}
          placeholder="例如 agents-test"
        />
        <p className="text-xs text-muted-foreground sm:col-span-2">
          请填写目标 Agent 已获 Kubernetes RBAC 授权、可创建临时 Secret 的 namespace。Gateway 当前不会提供可写 namespace 列表；不要假设 default 可用。
        </p>
        {use === "opaqueSecret" && (
          <div className="flex flex-col gap-1 sm:col-span-2">
            <TextField
              label="required keys（可选，逗号分隔）"
              value={requiredKeys}
              onChange={setRequiredKeys}
              placeholder="例如 DB_URL,DB_PASSWORD"
            />
            <p className="text-xs text-muted-foreground">
              填写后，验证要求版本内容的键集合与之完全相等；不填则按版本内容推断。只填键名，不要填值。
            </p>
          </div>
        )}
        <div className="flex justify-end sm:col-span-2">
          <Button
            onClick={submit}
            disabled={busy || !namespace.trim()}
            variant="primary" size="sm"
          >
            <ShieldIcon width={14} height={14} /> {busy ? "绑定中" : "确认绑定"}
          </Button>
        </div>
      </div>
    </DialogFrame>
  )
}

function TextField({
  label,
  value,
  onChange,
  placeholder,
}: {
  label: string
  value: string
  onChange: (value: string) => void
  placeholder?: string
}) {
  return (
    <label className="flex flex-col gap-1 text-xs text-muted-foreground">
      {label}
      <Input
        value={value}
        onChange={(event) => onChange(event.target.value)}
        placeholder={placeholder}
      />
    </label>
  )
}

function ReadOnlyField({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col gap-1 text-xs text-muted-foreground">
      {label}
      <div className="input font-mono text-xs text-foreground">{value}</div>
    </div>
  )
}
