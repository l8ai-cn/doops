"use client"

import { create, fromJson, toJson, type DescMessage, type JsonValue, type MessageShape } from "@bufbuild/protobuf"
import { Code, ConnectError } from "@connectrpc/connect"
import type { AgentWorkbenchSessionTransport } from "@agent-cloud/agent-ui/runtime"
import {
  CommandEnvelopeSchema,
  CommandReceiptSchema,
  SessionCursorSchema,
  SessionDeltaBatchSchema,
  SessionHistoryPageSchema,
  SessionSnapshotSchema,
} from "@agent-cloud/agent-ui/protocol"
import type { Session, Target } from "./client"
import { consoleFetch } from "./network-error.ts"
import { createDemoAgentSessionTransport } from "./demo-agent-session"

export function createDoopsAgentSessionTransport({ session, target, sessionId }: {
  session: Session
  target: Target
  sessionId: string
}): AgentWorkbenchSessionTransport {
  if (session.demo) return createDemoAgentSessionTransport(sessionId)
  const scope = { cluster: target.cluster, instance: target.instance, session_id: sessionId }
  async function request(url: string, init: RequestInit): Promise<Response> {
    try {
      const response = await consoleFetch(url, {
        ...init,
        cache: "no-store",
        headers: {
          ...init.headers,
        },
      })
      if (!response.ok) {
        const code = httpFailureCode(response.status)
        let detail: unknown
        try { detail = await response.json() } catch (cause) {
          throw new ConnectError(`智能体请求失败 (${response.status})：错误响应无效`, code, response.headers, undefined, cause)
        }
        const message = detail && typeof detail === "object" && "error" in detail && typeof detail.error === "string"
          ? detail.error : `智能体请求失败 (${response.status})`
        throw new ConnectError(message, code, response.headers)
      }
      return response
    } catch (cause) {
      throw ConnectError.from(cause, init.signal?.aborted ? Code.Canceled : Code.Unavailable)
    }
  }

  async function readMessage<Schema extends DescMessage>(response: Response, schema: Schema, signal?: AbortSignal) {
    try { return parseMessage(schema, await response.text()) } catch (cause) {
      throw ConnectError.from(cause, signal?.aborted ? Code.Canceled : Code.Unavailable)
    }
  }

  return {
    async execute(command, signal) {
      const response = await request("/api/agent-sessions", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...scope, command: toJson(CommandEnvelopeSchema, command) }),
        signal,
      })
      return readMessage(response, CommandReceiptSchema, signal)
    },
    async getSnapshot(signal) {
      const query = new URLSearchParams({ ...scope, view: "snapshot" })
      const response = await request(`/api/agent-sessions?${query}`, { signal })
      return readMessage(response, SessionSnapshotSchema, signal)
    },
    async getHistoryPage(_signal, beforeSequence = BigInt(0)) {
      // DoOps 快照从不报告更早的历史（hasOlderItems 恒为 false），runtime 因此不会请求
      // 分页；返回空页保持传输接口完整，而不是编造或丢弃真实历史。
      return create(SessionHistoryPageSchema, { nextBeforeSequence: beforeSequence })
    },
    async *streamDeltas(cursor, signal) {
      const query = new URLSearchParams({
        ...scope,
        view: "events",
        cursor: JSON.stringify(toJson(SessionCursorSchema, cursor)),
      })
      const controller = new AbortController()
      const abort = () => controller.abort(signal.reason)
      signal.addEventListener("abort", abort, { once: true })
      if (signal.aborted) abort()
      let reader: ReadableStreamDefaultReader<Uint8Array> | undefined
      try {
        const response = await request(`/api/agent-sessions?${query}`, {
          headers: { Accept: "application/x-ndjson" },
          signal: controller.signal,
        })
        if (!response.body) throw new ConnectError("智能体增量流缺少响应正文", Code.DataLoss)
        reader = response.body.getReader()
        const decoder = new TextDecoder("utf-8", { fatal: true })
        let buffer = ""
        while (true) {
          controller.signal.throwIfAborted()
          const { done, value } = await reader.read()
          controller.signal.throwIfAborted()
          try { buffer += decoder.decode(value, { stream: !done }) } catch (cause) {
            throw new ConnectError("智能体增量流包含无效 UTF-8", Code.DataLoss, undefined, undefined, cause)
          }
          let end = buffer.indexOf("\n")
          while (end !== -1) {
            const line = buffer.slice(0, end)
            buffer = buffer.slice(end + 1)
            yield parseMessage(SessionDeltaBatchSchema, line)
            controller.signal.throwIfAborted()
            end = buffer.indexOf("\n")
          }
          if (done) {
            if (buffer.length) throw new ConnectError("智能体增量流记录被截断", Code.DataLoss)
            return
          }
        }
      } catch (cause) {
        throw ConnectError.from(cause, controller.signal.aborted ? Code.Canceled : Code.Unavailable)
      } finally {
        signal.removeEventListener("abort", abort)
        controller.abort()
        reader?.releaseLock()
      }
    },
  }
}

function parseMessage<Schema extends DescMessage>(schema: Schema, text: string): MessageShape<Schema> {
  try {
    const value: unknown = JSON.parse(text)
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("protobuf JSON 必须为对象")
    return fromJson(schema, value as JsonValue)
  } catch (cause) {
    throw new ConnectError("智能体返回了无效协议数据", Code.DataLoss, undefined, undefined, cause)
  }
}

function httpFailureCode(status: number): Code {
  switch (status) {
    case 400: case 422: return Code.InvalidArgument
    case 401: return Code.Unauthenticated
    case 403: return Code.PermissionDenied
    case 404: case 410: return Code.NotFound
    case 405: case 501: return Code.Unimplemented
    case 408: case 504: return Code.DeadlineExceeded
    case 409: return Code.Aborted
    case 412: return Code.FailedPrecondition
    case 413: case 429: return Code.ResourceExhausted
    case 416: return Code.OutOfRange
    case 499: return Code.Canceled
    case 500: return Code.Internal
    default: return status >= 500 ? Code.Unavailable : Code.InvalidArgument
  }
}
