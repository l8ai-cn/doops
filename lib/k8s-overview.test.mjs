import assert from "node:assert/strict"
import test from "node:test"
import { parseK8sResourceList } from "./k8s-overview.ts"

test("parses nodes and exposes readiness", () => {
  const resources = parseK8sResourceList(JSON.stringify({ items: [
    { metadata: { name: "node-a" }, status: { conditions: [{ type: "Ready", status: "True" }] } },
    { metadata: { name: "node-b" }, status: { conditions: [{ type: "Ready", status: "False" }] } },
  ] }), "nodes")
  assert.deepEqual(resources.map(({ name, status }) => ({ name, status })), [
    { name: "node-a", status: "Ready" },
    { name: "node-b", status: "NotReady" },
  ])
})

test("parses deployments and pods into compact operator fields", () => {
  const deployments = parseK8sResourceList(JSON.stringify({ items: [
    { metadata: { name: "api", namespace: "apps" }, spec: { replicas: 2 }, status: { readyReplicas: 1, availableReplicas: 1 } },
  ] }), "deployments")
  const pods = parseK8sResourceList(JSON.stringify({ items: [
    { metadata: { name: "api-0", namespace: "apps" }, status: { phase: "Running", containerStatuses: [{ ready: true }, { ready: false }] } },
  ] }), "pods")
  assert.equal(deployments[0].ready, "1/2")
  assert.equal(deployments[0].status, "Progressing")
  assert.equal(pods[0].status, "Running")
  assert.equal(pods[0].ready, "1/2")
})

test("surfaces malformed resource output instead of hiding it", () => {
  assert.throws(() => parseK8sResourceList("not-json", "pods"), /Pod 返回了无法解析的 JSON/)
})
