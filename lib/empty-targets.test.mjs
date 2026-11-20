import assert from "node:assert/strict"
import test from "node:test"
import { emptyTargetsCopy, targetSwitcherEmptyCopy } from "./empty-targets.ts"

test("overview empty state keeps the admin copy and deploy guide only for admins", () => {
  const cases = [
    {
      name: "admin",
      isAdmin: true,
      want: {
        title: "暂无在线 Agent 目标",
        description: "首次接入时，请按部署指南将 Agent 连接到网关。已有 Agent 的话，请确认连接状态后刷新。",
        showDeployAgent: true,
        contactTitle: "联系管理员",
        contactHint: "需要协助接入？请联系管理员确认网关地址、路由分组和 Agent 实例名。",
      },
    },
    {
      name: "ordinary user",
      isAdmin: false,
      want: {
        title: "暂无可用 Agent 目标",
        description: "可能尚未获得目标授权，也可能已授权的 Agent 目前都不在线。",
        showDeployAgent: false,
        contactTitle: "联系平台管理员",
        contactHint: "请将用户名和目标实例名告诉平台管理员。授权生效且 Agent 上线后，点击「刷新」即可选择。",
      },
    },
  ]
  for (const { name, isAdmin, want } of cases) {
    assert.deepEqual(emptyTargetsCopy(isAdmin), want, name)
  }
})

test("ordinary users are told both possible causes and never pointed at deploying an Agent", () => {
  const copy = emptyTargetsCopy(false)
  const text = Object.values(copy).filter((value) => typeof value === "string").join("\n")
  assert.match(text, /授权/)
  assert.match(text, /不在线/)
  assert.match(text, /平台管理员/)
  for (const word of ["部署", "首次接入", "机器", "gateway 地址"]) {
    assert.ok(!text.includes(word), `ordinary user copy must not mention ${JSON.stringify(word)}`)
  }
})

test("target switcher empty label depends on whether the role is known", () => {
  const cases = [
    { name: "admin", isAdmin: true, label: "暂无在线 Agent 目标", hintHas: ["部署"], hintLacks: ["授权"] },
    { name: "ordinary user", isAdmin: false, label: "暂无可用 Agent 目标", hintHas: ["授权", "不在线", "平台管理员"], hintLacks: ["部署", "机器"] },
    { name: "role not passed", isAdmin: undefined, label: "暂无可用 Agent 目标", hintHas: ["刷新"], hintLacks: ["部署", "机器"] },
  ]
  for (const { name, isAdmin, label, hintHas, hintLacks } of cases) {
    const copy = targetSwitcherEmptyCopy(isAdmin)
    assert.equal(copy.label, label, name)
    for (const word of hintHas) assert.ok(copy.hint.includes(word), `${name}: hint should mention ${word}`)
    for (const word of hintLacks) assert.ok(!copy.hint.includes(word), `${name}: hint must not mention ${word}`)
  }
})
