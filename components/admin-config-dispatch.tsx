"use client"

import { useCallback, useEffect, useRef, useState } from "react"
import type { Session } from "@/lib/client"
import type { Target } from "@/lib/gateway"
import { randomSession } from "@/lib/gateway"
import { listInstances, type AdminInstance } from "@/lib/admin"
import {
  loadConfigs,
  upsertConfig,
  removeConfig,
  newConfigId,
  markPublished,
  applyConfigToNode,
  configKind,
  configTargetPath,
  hasUnpublishedChanges,
  type ModelConfig,
  type ConfigKind,
} from "@/lib/model-configs"
import {
  LayersIcon,
  PlusIcon,
  TrashIcon,
  SaveIcon,
  RefreshIcon,
  EyeIcon,
  EyeOffIcon,
  ServerIcon,
  CheckIcon,
  KeyIcon,
  RocketIcon,
  FileTextIcon,
} from "./icons"

type DispatchState = "idle" | "running" | "ok" | "err"

interface DispatchRow {
  key: string
  state: DispatchState
  message?: string
}

const EMPTY_MODEL: ModelConfig = {
  id: "",
  name: "",
  kind: "model",
  provider: "openai-compatible",
  model: "",
  base_url: "",
  api_key: "",
  temperature: 0.2,
  max_tokens: 8192,
  models: [],
  updated_at: "",
}

const EMPTY_FILE: ModelConfig = {
  id: "",
  name: "",
  kind: "file",
  path: "",
  format: "text",
  content: "",
  updated_at: "",
}

export function AdminConfigDispatch({ session }: { session: Session }) {
  const [configs, setConfigs] = useState<ModelConfig[]>([])
  const [editing, setEditing] = useState<ModelConfig | null>(null)
  const [showKey, setShowKey] = useState(false)

  const [instances, setInstances] = useState<AdminInstance[]>([])
  const [loadingInst, setLoadingInst] = useState(false)
  const [selected, setSelected] = useState<Record<string, boolean>>({})
  const [dispatchCfg, setDispatchCfg] = useState("")
  const [rows, setRows] = useState<Record<string, DispatchRow>>({})
  const [dispatching, setDispatching] = useState(false)
  const sessionRef = useRef(randomSession("admin-cfg"))

  useEffect(() => {
    setConfigs(loadConfigs())
  }, [])

  const loadInst = useCallback(async () => {
    setLoadingInst(true)
    try {
      const list = await listInstances(session)
      setInstances(list)
    } finally {
      setLoadingInst(false)
    }
  }, [session])

  useEffect(() => {
    loadInst()
  }, [loadInst])

  function startNew(kind: ConfigKind) {
    const base = kind === "file" ? EMPTY_FILE : EMPTY_MODEL
    setEditing({ ...base, id: newConfigId() })
    setShowKey(false)
  }

  function startEdit(cfg: ModelConfig) {
    setEditing({ ...cfg, models: cfg.models ? [...cfg.models] : [] })
    setShowKey(false)
  }

  function saveEditing() {
    if (!editing) return
    if (!editing.name.trim()) {
      alert("请填写配置名称")
      return
    }
    if (configKind(editing) === "file" && !editing.path?.trim()) {
      alert("配置文件需要填写目标路径")
      return
    }
    const list = upsertConfig({ ...editing, name: editing.name.trim() })
    setConfigs(list)
    setEditing(null)
  }

  function deleteConfig(id: string) {
    if (!window.confirm("确定删除该配置？")) return
    const list = removeConfig(id)
    setConfigs(list)
    if (dispatchCfg === id) setDispatchCfg("")
  }

  const onlineKeys = instances
    .filter((i) => i.status === "online")
    .map((i) => `${i.cluster}/${i.instance}`)

  function toggle(key: string) {
    setSelected((p) => ({ ...p, [key]: !p[key] }))
  }
  function selectAllOnline() {
    const next: Record<string, boolean> = {}
    onlineKeys.forEach((k) => (next[k] = true))
    setSelected(next)
  }
  function clearSelection() {
    setSelected({})
  }

  // 应用 / 发布上线
  async function publish() {
    const cfg = configs.find((c) => c.id === dispatchCfg)
    if (!cfg) {
      alert("请先选择要发布的配置")
      return
    }
    const targets = instances.filter((i) => selected[`${i.cluster}/${i.instance}`])
    if (targets.length === 0) {
      alert("请至少勾选一个目标实例")
      return
    }
    setDispatching(true)
    const init: Record<string, DispatchRow> = {}
    targets.forEach((t) => {
      init[`${t.cluster}/${t.instance}`] = { key: `${t.cluster}/${t.instance}`, state: "running" }
    })
    setRows(init)

    let anyOk = false
    for (const inst of targets) {
      const key = `${inst.cluster}/${inst.instance}`
      const target: Target = { cluster: inst.cluster, instance: inst.instance, key }
      try {
        await applyConfigToNode(session, target, sessionRef.current, cfg)
        anyOk = true
        setRows((p) => ({ ...p, [key]: { key, state: "ok", message: `已写入 ${configTargetPath(cfg)}` } }))
      } catch (e) {
        setRows((p) => ({
          ...p,
          [key]: { key, state: "err", message: e instanceof Error ? e.message : String(e) },
        }))
      }
    }
    if (anyOk) {
      setConfigs(markPublished(cfg.id))
    }
    setDispatching(false)
  }

  const selectedCount = Object.values(selected).filter(Boolean).length

  return (
    <div className="flex h-full min-h-0 flex-col lg:flex-row">
      {/* 左：配置库 */}
      <section className="flex min-h-0 flex-1 flex-col border-b lg:border-b-0 lg:border-r">
        <div className="flex flex-wrap items-center justify-between gap-2 border-b bg-card/50 px-4 py-3">
          <div className="min-w-0">
            <h2 className="flex items-center gap-1.5 text-sm font-semibold text-foreground">
              <LayersIcon width={16} height={16} className="text-primary" /> 配置中心
            </h2>
            <p className="text-xs text-muted-foreground">统一管理大模型与配置文件，发布后才会下发到线上</p>
          </div>
          <div className="flex items-center gap-1.5">
            <button
              onClick={() => startNew("model")}
              className="flex shrink-0 items-center gap-1.5 whitespace-nowrap rounded-lg bg-primary px-2.5 py-1.5 text-xs font-medium text-primary-foreground transition-opacity hover:opacity-90"
            >
              <PlusIcon width={14} height={14} /> 大模型
            </button>
            <button
              onClick={() => startNew("file")}
              className="flex shrink-0 items-center gap-1.5 whitespace-nowrap rounded-lg border px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-muted"
            >
              <PlusIcon width={14} height={14} /> 配置文件
            </button>
          </div>
        </div>

        <div className="min-h-0 flex-1 overflow-auto p-3">
          {editing ? (
            <ConfigEditor
              cfg={editing}
              showKey={showKey}
              onToggleKey={() => setShowKey((v) => !v)}
              onChange={setEditing}
              onSave={saveEditing}
              onCancel={() => setEditing(null)}
            />
          ) : configs.length === 0 ? (
            <p className="px-1 py-8 text-center text-sm text-muted-foreground">
              暂无配置，点击右上角「大模型」或「配置文件」新建。
            </p>
          ) : (
            <ul className="flex flex-col gap-2">
              {configs.map((c) => {
                const isFile = configKind(c) === "file"
                const unpublished = hasUnpublishedChanges(c)
                return (
                  <li
                    key={c.id}
                    className="flex items-center justify-between gap-3 rounded-lg border bg-card px-3 py-2.5"
                  >
                    <div className="flex min-w-0 items-center gap-2.5">
                      <span
                        className={`flex h-8 w-8 shrink-0 items-center justify-center rounded-md ${
                          isFile ? "bg-muted text-muted-foreground" : "bg-primary/15 text-primary"
                        }`}
                      >
                        {isFile ? <FileTextIcon width={16} height={16} /> : <LayersIcon width={16} height={16} />}
                      </span>
                      <div className="min-w-0">
                        <div className="flex items-center gap-1.5">
                          <p className="truncate text-sm font-medium text-foreground">{c.name}</p>
                          <span className="shrink-0 rounded border px-1 py-0.5 text-[10px] text-muted-foreground">
                            {isFile ? "配置文件" : "大模型"}
                          </span>
                        </div>
                        <p className="truncate font-mono text-xs text-muted-foreground">
                          {isFile ? configTargetPath(c) || "未设置路径" : `${c.model || "—"}${c.provider ? ` · ${c.provider}` : ""}`}
                        </p>
                      </div>
                    </div>
                    <div className="flex shrink-0 items-center gap-1.5">
                      <span
                        className={`rounded px-1.5 py-0.5 text-[10px] font-medium ${
                          unpublished
                            ? "bg-warning/15 text-warning"
                            : "bg-success/15 text-success"
                        }`}
                        title={c.published_at ? `最近发布：${new Date(c.published_at).toLocaleString()}` : "尚未发布"}
                      >
                        {unpublished ? (c.published_at ? "有改动" : "未发布") : "已发布"}
                      </span>
                      <button
                        onClick={() => startEdit(c)}
                        className="rounded-md border px-2 py-1 text-xs text-foreground transition-colors hover:bg-muted"
                      >
                        编辑
                      </button>
                      <button
                        onClick={() => deleteConfig(c.id)}
                        className="rounded-md border border-destructive/40 px-2 py-1 text-destructive transition-colors hover:bg-destructive/10"
                        aria-label="删除配置"
                      >
                        <TrashIcon width={14} height={14} />
                      </button>
                    </div>
                  </li>
                )
              })}
            </ul>
          )}
        </div>
      </section>

      {/* 右：发布上线 */}
      <section className="flex min-h-0 flex-1 flex-col">
        <div className="flex flex-wrap items-center justify-between gap-2 border-b bg-card/50 px-4 py-3">
          <div>
            <h2 className="flex items-center gap-1.5 text-sm font-semibold text-foreground">
              <RocketIcon width={16} height={16} className="text-primary" /> 应用 / 发布上线
            </h2>
            <p className="text-xs text-muted-foreground">选择配置与目标实例，应用后写入节点并标记为已发布</p>
          </div>
          <button
            onClick={loadInst}
            disabled={loadingInst}
            className="flex items-center gap-1.5 rounded-lg border px-2.5 py-1.5 text-xs text-foreground transition-colors hover:bg-muted disabled:opacity-50"
          >
            <RefreshIcon width={14} height={14} className={loadingInst ? "animate-spin" : ""} /> 刷新实例
          </button>
        </div>

        <div className="flex flex-wrap items-end gap-2 border-b bg-muted/30 px-4 py-3">
          <label className="flex min-w-48 flex-1 flex-col gap-1.5">
            <span className="text-xs font-medium text-muted-foreground">要发布的配置</span>
            <select value={dispatchCfg} onChange={(e) => setDispatchCfg(e.target.value)} className="input">
              <option value="">选择一个配置…</option>
              {configs.map((c) => (
                <option key={c.id} value={c.id}>
                  {configKind(c) === "file" ? "[文件] " : "[模型] "}
                  {c.name}
                </option>
              ))}
            </select>
          </label>
          <button
            onClick={publish}
            disabled={dispatching || !dispatchCfg || selectedCount === 0}
            className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
          >
            <RocketIcon width={15} height={15} />
            {dispatching ? "发布中…" : `发布上线 (${selectedCount})`}
          </button>
        </div>

        {dispatchCfg && (
          <p className="border-b bg-background px-4 py-2 font-mono text-xs text-muted-foreground">
            目标路径：{configTargetPath(configs.find((c) => c.id === dispatchCfg)!) || "—"}
          </p>
        )}

        <div className="flex items-center gap-2 px-4 py-2 text-xs">
          <button onClick={selectAllOnline} className="text-primary hover:underline">
            全选在线
          </button>
          <span className="text-border">|</span>
          <button onClick={clearSelection} className="text-muted-foreground hover:text-foreground">
            清空
          </button>
          <span className="ml-auto text-muted-foreground">
            已选 {selectedCount} / 在线 {onlineKeys.length}
          </span>
        </div>

        <div className="min-h-0 flex-1 overflow-auto px-3 pb-3">
          {instances.length === 0 ? (
            <p className="px-1 py-8 text-center text-sm text-muted-foreground">
              {loadingInst ? "加载中…" : "暂无已注册实例"}
            </p>
          ) : (
            <ul className="flex flex-col gap-2">
              {instances.map((i) => {
                const key = `${i.cluster}/${i.instance}`
                const online = i.status === "online"
                const row = rows[key]
                return (
                  <li
                    key={key}
                    className={`flex items-center gap-3 rounded-lg border px-3 py-2.5 ${
                      online ? "bg-card" : "bg-card/40 opacity-70"
                    }`}
                  >
                    <input
                      type="checkbox"
                      checked={!!selected[key]}
                      disabled={!online}
                      onChange={() => toggle(key)}
                      className="h-4 w-4 accent-[var(--primary)] disabled:opacity-40"
                      aria-label={`选择 ${key}`}
                    />
                    <span
                      className={`h-2 w-2 shrink-0 rounded-full ${online ? "bg-success" : "bg-muted-foreground"}`}
                    />
                    <div className="min-w-0 flex-1">
                      <p className="truncate text-sm font-medium text-foreground">
                        {i.cluster}
                        <span className="text-muted-foreground"> / </span>
                        {i.instance}
                      </p>
                      <p className="truncate font-mono text-xs text-muted-foreground">{i.remote || "—"}</p>
                    </div>
                    {row && (
                      <span
                        className={`flex items-center gap-1 text-xs ${
                          row.state === "ok"
                            ? "text-success"
                            : row.state === "err"
                              ? "text-destructive"
                              : "text-muted-foreground"
                        }`}
                        title={row.message}
                      >
                        {row.state === "ok" && <CheckIcon width={13} height={13} />}
                        {row.state === "running" && <RefreshIcon width={13} height={13} className="animate-spin" />}
                        {row.state === "ok"
                          ? "已发布"
                          : row.state === "err"
                            ? "失败"
                            : row.state === "running"
                              ? "写入中"
                              : ""}
                      </span>
                    )}
                  </li>
                )
              })}
            </ul>
          )}
        </div>
      </section>
    </div>
  )
}

function ConfigEditor({
  cfg,
  showKey,
  onToggleKey,
  onChange,
  onSave,
  onCancel,
}: {
  cfg: ModelConfig
  showKey: boolean
  onToggleKey: () => void
  onChange: (c: ModelConfig) => void
  onSave: () => void
  onCancel: () => void
}) {
  const set = (patch: Partial<ModelConfig>) => onChange({ ...cfg, ...patch })
  const isFile = configKind(cfg) === "file"
  return (
    <div className="flex flex-col gap-3 rounded-lg border bg-card p-4">
      <div className="flex items-center gap-2">
        <span
          className={`flex h-7 items-center gap-1.5 rounded-md px-2 text-xs font-medium ${
            isFile ? "bg-muted text-muted-foreground" : "bg-primary/15 text-primary"
          }`}
        >
          {isFile ? <FileTextIcon width={13} height={13} /> : <LayersIcon width={13} height={13} />}
          {isFile ? "配置文件" : "大模型配置"}
        </span>
      </div>

      <Row label="配置名称">
        <input
          className="input w-full"
          value={cfg.name}
          onChange={(e) => set({ name: e.target.value })}
          placeholder={isFile ? "如：Nginx 反代配置" : "如：生产 GPT 配置"}
        />
      </Row>

      {isFile ? (
        <>
          <Row label="目标路径 (节点上的绝对路径)">
            <input
              className="input w-full font-mono"
              value={cfg.path ?? ""}
              onChange={(e) => set({ path: e.target.value })}
              placeholder="/etc/nginx/conf.d/doops.conf"
            />
          </Row>
          <Row label="格式">
            <select
              className="input w-full"
              value={cfg.format ?? "text"}
              onChange={(e) => set({ format: e.target.value })}
            >
              <option value="text">纯文本</option>
              <option value="json">JSON</option>
              <option value="yaml">YAML</option>
              <option value="env">ENV</option>
              <option value="ini">INI / Conf</option>
            </select>
          </Row>
          <Row label="文件内容">
            <textarea
              className="input min-h-[14rem] w-full resize-y font-mono text-xs leading-relaxed"
              value={cfg.content ?? ""}
              onChange={(e) => set({ content: e.target.value })}
              spellCheck={false}
              placeholder="# 在此粘贴配置文件内容，发布时原样写入目标路径"
            />
          </Row>
        </>
      ) : (
        <>
          <Row label="Provider">
            <input
              className="input w-full"
              value={cfg.provider ?? ""}
              onChange={(e) => set({ provider: e.target.value })}
              placeholder="openai-compatible / anthropic"
            />
          </Row>
          <Row label="模型 ID">
            <input
              className="input w-full font-mono"
              value={cfg.model ?? ""}
              onChange={(e) => set({ model: e.target.value })}
              placeholder="openai/gpt-5.4"
            />
          </Row>
          <Row label="Base URL">
            <input
              className="input w-full font-mono"
              value={cfg.base_url ?? ""}
              onChange={(e) => set({ base_url: e.target.value })}
              placeholder="https://api.openai.com/v1"
            />
          </Row>
          <Row label="API 密钥">
            <div className="flex items-center gap-1.5">
              <input
                className="input w-full font-mono"
                type={showKey ? "text" : "password"}
                value={cfg.api_key ?? ""}
                onChange={(e) => set({ api_key: e.target.value })}
                placeholder="sk-..."
              />
              <button
                onClick={onToggleKey}
                className="shrink-0 rounded-md border p-2 text-muted-foreground transition-colors hover:bg-muted"
                aria-label={showKey ? "隐藏密钥" : "显示密钥"}
              >
                {showKey ? <EyeOffIcon width={15} height={15} /> : <EyeIcon width={15} height={15} />}
              </button>
            </div>
          </Row>
          <div className="grid grid-cols-2 gap-3">
            <Row label="Temperature">
              <input
                className="input w-full"
                type="number"
                step="0.1"
                value={cfg.temperature ?? ""}
                onChange={(e) => set({ temperature: e.target.value === "" ? undefined : Number(e.target.value) })}
              />
            </Row>
            <Row label="Max Tokens">
              <input
                className="input w-full"
                type="number"
                value={cfg.max_tokens ?? ""}
                onChange={(e) => set({ max_tokens: e.target.value === "" ? undefined : Number(e.target.value) })}
              />
            </Row>
          </div>
          <Row label="候选模型 (逗号分隔)">
            <input
              className="input w-full font-mono"
              value={(cfg.models ?? []).join(", ")}
              onChange={(e) => set({ models: e.target.value.split(",").map((s) => s.trim()).filter(Boolean) })}
              placeholder="openai/gpt-5.4, openai/gpt-5-mini"
            />
          </Row>
        </>
      )}

      <div className="mt-1 flex items-center gap-2">
        <button
          onClick={onSave}
          className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90"
        >
          <SaveIcon width={15} height={15} /> 保存配置
        </button>
        <button
          onClick={onCancel}
          className="rounded-lg border px-3 py-2 text-sm text-foreground transition-colors hover:bg-muted"
        >
          取消
        </button>
        <span className="ml-auto flex items-center gap-1 text-xs text-muted-foreground">
          <KeyIcon width={12} height={12} /> 仅保存在浏览器本地，发布后才写入节点
        </span>
      </div>
    </div>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-1.5">
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      {children}
    </label>
  )
}
