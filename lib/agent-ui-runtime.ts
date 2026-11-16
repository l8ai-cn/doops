"use client"

import {
  createAgentWorkbenchRuntime,
  type AgentSessionRuntimeV2,
} from "@agent-cloud/agent-ui/runtime"
import type { AgentAttachmentReference } from "@agent-cloud/agent-ui"
import type { Session, Target } from "./client"
import { consoleFetch } from "./network-error.ts"
import type { AgentSessionSnapshot } from "@agent-cloud/agent-ui"
import { createDoopsAgentSessionTransport } from "./agent-session-transport"
import { agentErrorMessage } from "./agent-error"
import { createDoopsCodeWorkspace, loadDoopsWorkspaceArtifact } from "./agent-code-workspace"
import type { CodeWorkspaceSource } from "@agent-cloud/agent-ui"

export function isDoopsAgentSessionBusy(snapshot: AgentSessionSnapshot): boolean {
  return snapshot.status === "running" ||
    snapshot.items.some((item) => item.kind === "message" && item.role === "user" && item.status === "pending") ||
    snapshot.interactions.some((interaction) => interaction.status === "pending" || interaction.status === "running")
}

export function createDoopsAgentUiRuntime(options: {
  session: Session
  target: Target
  sessionId: string
  codeWorkspace?: CodeWorkspaceSource
  readOnly?: boolean
}): AgentSessionRuntimeV2 {
  const codeWorkspace = options.codeWorkspace || createDoopsCodeWorkspace(options)
  const transport = createDoopsAgentSessionTransport(options)
  if (options.readOnly) {
    transport.execute = async () => { throw new Error("原发布回执只读，请返回当前发布会话提交") }
  }
  return createAgentWorkbenchRuntime({
    sessionId: options.sessionId,
    title: "DoAgent",
    agentLabel: "DoAgent",
    interactionMode: "acp",
    transport,
    createArtifactLoader: () => async ({ artifactId, sessionId }) =>
      loadDoopsWorkspaceArtifact(codeWorkspace, sessionId, artifactId),
    uploadAttachment: options.readOnly ? undefined : (sessionId, file) => uploadDoopsAttachment(options, sessionId, file),
    connectionOptions: { maxConsecutiveFailures: 2 },
    projectSnapshot: (snapshot) => {
      const model = snapshot.configuration?.find((control) => control.id === "model")?.value
      return {
        ...snapshot,
        error: agentErrorMessage(snapshot.error),
        metadata: [
          { id: "target", label: "目标", value: `${options.target.cluster}/${options.target.instance}` },
          ...(model ? [{ id: "model", label: "模型", value: model }] : []),
        ],
      }
    },
  })
}

async function uploadDoopsAttachment(
  options: { session: Session; target: Target },
  sessionId: string,
  file: File,
): Promise<AgentAttachmentReference> {
  if (options.session.demo) {
    return { id: `demo-${crypto.randomUUID()}`, name: file.name, mediaType: file.type || "application/octet-stream", bytes: file.size }
  }
  const form = new FormData()
  form.set("cluster", options.target.cluster)
  form.set("instance", options.target.instance)
  form.set("session_id", sessionId)
  form.set("file", file, file.name)
  const response = await consoleFetch("/api/agent-attachments", {
    method: "POST",
    body: form,
  })
  let detail: { error?: string } = {}
  try { detail = await response.json() } catch { /* response body is not a contract on transport failure */ }
  if (!response.ok) throw new Error(detail.error || `附件上传失败 (${response.status})`)
  if (!detail || typeof detail !== "object" || typeof (detail as AgentAttachmentReference).id !== "string") {
    throw new Error("附件上传返回了无效协议")
  }
  return detail as AgentAttachmentReference
}
