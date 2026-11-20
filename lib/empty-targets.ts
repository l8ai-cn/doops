// 没有可用目标时的空状态文案。
// /v1/targets 对普通用户已按授权过滤，Web 分不清「没有授权」和「已授权但 Agent 都不在线」，
// 所以普通用户的文案同时说明两种可能；部署 Agent 的引导只对管理员有意义。
export interface EmptyTargetsCopy {
  title: string
  description: string
  showDeployAgent: boolean
  contactTitle: string
  contactHint: string
}

const ADMIN_COPY: EmptyTargetsCopy = {
  title: "暂无在线 Agent 目标",
  description: "首次接入时，请按部署指南将 Agent 连接到网关。已有 Agent 的话，请确认连接状态后刷新。",
  showDeployAgent: true,
  contactTitle: "联系管理员",
  contactHint: "需要协助接入？请联系管理员确认网关地址、路由分组和 Agent 实例名。",
}

const USER_COPY: EmptyTargetsCopy = {
  title: "暂无可用 Agent 目标",
  description: "可能尚未获得目标授权，也可能已授权的 Agent 目前都不在线。",
  showDeployAgent: false,
  contactTitle: "联系平台管理员",
  contactHint: "请将用户名和目标实例名告诉平台管理员。授权生效且 Agent 上线后，点击「刷新」即可选择。",
}

export function emptyTargetsCopy(isAdmin: boolean): EmptyTargetsCopy {
  return isAdmin ? ADMIN_COPY : USER_COPY
}

export interface TargetSwitcherEmptyCopy {
  label: string
  hint: string
}

// isAdmin 未传入时（调用方没有会话信息）用对两种角色都成立的说法，不给部署引导。
export function targetSwitcherEmptyCopy(isAdmin?: boolean): TargetSwitcherEmptyCopy {
  if (isAdmin === true) return { label: "暂无在线 Agent 目标", hint: "还没有在线的 Agent，部署或接入后点刷新" }
  if (isAdmin === false) return { label: "暂无可用 Agent 目标", hint: `${USER_COPY.description}请联系平台管理员确认授权。` }
  return { label: "暂无可用 Agent 目标", hint: "没有在线且你有权使用的 Agent 目标，点刷新重新查询" }
}
