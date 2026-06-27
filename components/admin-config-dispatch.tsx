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
  applyConfigToNode,
  type ModelConfig,
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
} from "./icons"

type DispatchState = "idle" | "running" | "ok" | "err"

interface DispatchRow {
  key: string
  state: DispatchState
  message?: string
}

const EMPTY: ModelConfig = {
  id: "",
  name: "",
  provider: "openai-compatible",
  model: "",
  base_url: "",
  api_key: "",
  temperature: 0.2,
  max_tokens: 8192,
  models: [],
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

  function startNew() {
    setEditing({ ...EMPTY, id: newConfigId() })
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
    const list = upsertConfig({ ...editing, name: editing.name.trim() })
    setConfigs(list)
    setEditing(null)
  }

  function deleteConfig(id: string) {
    if (!window.confirm("确定删除该模型配置？")) return
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

  async function dispatch() {
    const cfg = configs.find((c) => c.id === dispatchCfg)
    if (!cfg) {
      alert("请先选择要下发的模型配置")
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

    for (const inst of targets) {
      const key = `${inst.cluster}/${inst.instance}`
      const target: Target = { cluster: inst.cluster, instance: inst.instance, key }
      try {
        await applyConfigToNode(session, target, sessionRef.current, cfg)
        setRows((p) => ({ ...p, [key]: { key, state: "ok", message: "已写入 settings.json" } }))
      } catch (e) {
        setRows((p) => ({
          ...p,
          [key]: { key, state: "err", message: e instanceof Error ? e.message : String(e) },
        }))
      }
    }
    setDispatching(false)
  }

  const selectedCount = Object.values(selected).filter(Boolean).length

  return (
    <div className="flex h-full min-h-0 flex-col lg:flex-row">
      {/* 左：模型配置库 */}
      <section className="flex min-h-0 flex-1 flex-col border-b lg:border-b-0 lg:border-r">
        <div className="flex items-center justify-between gap-2 border-b bg-card/50 px-4 py-3">
          <div>
            <h2 className="flex items-center gap-1.5 text-sm font-semibold text-foreground">
              <LayersIcon width={16} height={16} className="text-primary" /> 模型配置库
            </h2>
            <p className="text-xs text-muted-foreground">维护可复用的模型配置，下发到任意节点</p>
          </div>
          <button
            onClick={startNew}
            className="flex items-center gap-1.5 rounded-lg bg-primary px-2.5 py-1.5 text-xs font-medium text-primary-foreground transition-opacity hover:opacity-90"
          >
            <PlusIcon width={14} height={14} /> 新建配置
          </button>
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
              暂无模型配置，点击「新建配置」添加。
            </p>
          ) : (
            <ul className="flex flex-col gap-2">
              {configs.map((c) => (
                <li
                  key={c.id}
                  className="flex items-center justify-between gap-3 rounded-lg border bg-card px-3 py-2.5"
                >
                  <div className="min-w-0">
                    <p className="truncate text-sm font-medium text-foreground">{c.name}</p>
                    <p className="truncate font-mono text-xs text-muted-foreground">
                      {c.model || "—"}
                      {c.provider ? ` · ${c.provider}` : ""}
                      {c.api_key ? " · 含密钥" : " · 无密钥"}
                    </p>
                  </div>
                  <div className="flex shrink-0 items-center gap-1">
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
              ))}
            </ul>
          )}
        </div>
      </section>

      {/* 右：下发到节点 */}
      <section className="flex min-h-0 flex-1 flex-col">
        <div className="flex flex-wrap items-center justify-between gap-2 border-b bg-card/50 px-4 py-3">
          <div>
            <h2 className="flex items-center gap-1.5 text-sm font-semibold text-foreground">
              <ServerIcon width={16} height={16} className="text-primary" /> 下发到 doagent 节点
            </h2>
            <p className="text-xs text-muted-foreground">选择配置与目标实例，写入节点 /root/.agent/settings.json</p>
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
            <span className="text-xs font-medium text-muted-foreground">下发的模型配置</span>
            <select
              value={dispatchCfg}
              onChange={(e) => setDispatchCfg(e.target.value)}
              className="input"
            >
              <option value="">选择一个配置…</option>
              {configs.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                  {c.model ? ` · ${c.model}` : ""}
                </option>
              ))}
            </select>
          </label>
          <button
            onClick={dispatch}
            disabled={dispatching || !dispatchCfg || selectedCount === 0}
            className="flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:opacity-40"
          >
            <SaveIcon width={15} height={15} />
            {dispatching ? "下发中…" : `一键下发 (${selectedCount})`}
          </button>
        </div>

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
                      className={`h-2 w-2 shrink-0 rounded-full ${
                        online ? "bg-success" : "bg-muted-foreground"
                      }`}
                    />
                    <div className="min-w-0 flex-1">
                      <p className="truncate text-sm font-medium text-foreground">
                        {i.cluster}
                        <span className="text-muted-foreground"> / </span>
                        {i.instance}
                      </p>
                      <p className="truncate font-mono text-xs text-muted-foreground">
                        {i.remote || "—"}
                      </p>
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
                        {row.state === "running" && (
                          <RefreshIcon width={13} height={13} className="animate-spin" />
                        )}
                        {row.state === "ok"
                          ? "已下发"
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
  return (
    <div className="flex flex-col gap-3 rounded-lg border bg-card p-4">
      <Row label="配置名称">
        <input
          className="input w-full"
          value={cfg.name}
          onChange={(e) => set({ name: e.target.value })}
          placeholder="如：生产 GPT 配置"
        />
      </Row>
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
          onChange={(e) =>
            set({ models: e.target.value.split(",").map((s) => s.trim()).filter(Boolean) })
          }
          placeholder="openai/gpt-5.4, openai/gpt-5-mini"
        />
      </Row>

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
          <KeyIcon width={12} height={12} /> 密钥仅保存在浏览器本地
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
