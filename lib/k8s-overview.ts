import { callTool, type Session, type Target } from "./client.ts"

export const K8S_OVERVIEW_QUERIES = [
  { key: "pods", label: "Pod", kind: "pods", allNamespaces: true },
  { key: "deployments", label: "Deployment", kind: "deployments", allNamespaces: true },
  { key: "namespaces", label: "命名空间", kind: "namespaces", allNamespaces: false },
  { key: "nodes", label: "节点", kind: "nodes", allNamespaces: false },
] as const

export type K8sOverviewKey = (typeof K8S_OVERVIEW_QUERIES)[number]["key"]

export type K8sResource = {
  name: string
  namespace?: string
  status?: string
  ready?: string
}

export type K8sResourceGroup = {
  items: K8sResource[]
  error?: string
}

export type K8sOverview = Record<K8sOverviewKey, K8sResourceGroup> & {
  fetchedAt: string
}

type ResourceList = {
  kind?: string
  items?: Array<Record<string, unknown>>
  metadata?: Record<string, unknown>
}

function metadataOf(item: Record<string, unknown>) {
  return (item.metadata && typeof item.metadata === "object" ? item.metadata : {}) as Record<string, unknown>
}

function stringValue(value: unknown): string | undefined {
  return typeof value === "string" && value ? value : undefined
}

function numberValue(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined
}

function itemFor(key: K8sOverviewKey, item: Record<string, unknown>): K8sResource {
  const metadata = metadataOf(item)
  const spec = (item.spec && typeof item.spec === "object" ? item.spec : {}) as Record<string, unknown>
  const status = (item.status && typeof item.status === "object" ? item.status : {}) as Record<string, unknown>
  const name = stringValue(metadata.name)
  if (!name) throw new Error(`${key} 资源缺少名称`)
  const resource: K8sResource = {
    name,
    namespace: stringValue(metadata.namespace),
  }

  if (key === "nodes") {
    const conditions = Array.isArray(status.conditions) ? status.conditions : []
    const ready = conditions.find((condition) => {
      if (!condition || typeof condition !== "object") return false
      const value = condition as Record<string, unknown>
      return value.type === "Ready"
    }) as Record<string, unknown> | undefined
    resource.status = ready?.status === "True" ? "Ready" : ready?.status === "False" ? "NotReady" : "Unknown"
  } else if (key === "namespaces") {
    resource.status = stringValue(status.phase) || "Unknown"
  } else if (key === "deployments") {
    const desired = numberValue(spec.replicas) ?? 0
    const ready = numberValue(status.readyReplicas) ?? 0
    resource.ready = `${ready}/${desired}`
    resource.status = ready >= desired ? "Ready" : ready > 0 ? "Progressing" : "Unavailable"
  } else {
    resource.status = stringValue(status.phase) || "Unknown"
    const containerStatuses = Array.isArray(status.containerStatuses) ? status.containerStatuses : []
    const readyCount = containerStatuses.filter((container) => {
      return Boolean(container && typeof container === "object" && (container as Record<string, unknown>).ready === true)
    }).length
    if (containerStatuses.length > 0) resource.ready = `${readyCount}/${containerStatuses.length}`
  }

  return resource
}

export function parseK8sResourceList(output: string, key: K8sOverviewKey): K8sResource[] {
  let value: ResourceList
  try {
    value = JSON.parse(output) as ResourceList
  } catch {
    throw new Error(`${K8S_OVERVIEW_QUERIES.find((query) => query.key === key)?.label || key} 返回了无法解析的 JSON`)
  }
  const items = value?.items
  if (!Array.isArray(items) || items.some((item) => !item || typeof item !== "object")) {
    throw new Error(`${K8S_OVERVIEW_QUERIES.find((query) => query.key === key)?.label || key} 返回格式无效`)
  }
  return items.map((item) => itemFor(key, item as Record<string, unknown>))
}

async function fetchResourceGroup(
  session: Session,
  target: Pick<Target, "cluster" | "instance">,
  sessionId: string,
  query: (typeof K8S_OVERVIEW_QUERIES)[number],
  signal: AbortSignal,
): Promise<K8sResourceGroup> {
  let output = ""
  try {
    await callTool(
      session,
      {
        cluster: target.cluster,
        instance: target.instance,
        tool: "doops_k8s",
        signal,
        arguments: {
          session_id: sessionId,
          operation: "get",
          kind: query.kind,
          output: "json",
          ...(query.allNamespaces ? { all_namespaces: true } : {}),
        },
      },
      (event) => {
        if (event.type === "output") output += event.data
      },
    )
    return { items: parseK8sResourceList(output, query.key) }
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    const detail = output.split("\n").find((line) => /^Error from server|^error:/i.test(line))
    return { items: [], error: detail ? `${message} · ${detail}` : message }
  }
}

export async function fetchK8sOverview(
  session: Session,
  target: Pick<Target, "cluster" | "instance">,
  sessionId: string,
  signal: AbortSignal,
): Promise<K8sOverview> {
  const groups = await Promise.all(
    K8S_OVERVIEW_QUERIES.map(async (query) => [query.key, await fetchResourceGroup(session, target, sessionId, query, signal)] as const),
  )
  const overview = Object.fromEntries(groups) as Record<K8sOverviewKey, K8sResourceGroup>
  return { ...overview, fetchedAt: new Date().toISOString() }
}
