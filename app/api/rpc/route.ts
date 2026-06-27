import WebSocket from "ws"
import { readAuth, toWsBase } from "@/lib/gateway"

export const runtime = "nodejs"
export const dynamic = "force-dynamic"

interface RpcBody {
  cluster: string
  instance: string
  tool: string
  arguments: Record<string, unknown>
}

const AGENT_PROMPT_SILENCE_MS = 45_000

// 通过服务端 WS 桥接 gateway /v1/rpc。
// 浏览器 POST 工具调用，服务端注入 Bearer token，
// 并把 notifications/message 与最终结果以 NDJSON 行流式回传。
export async function POST(req: Request) {
  const { gateway, token } = readAuth(req)
  if (!gateway || !token) {
    return Response.json({ error: "缺少 gateway 地址或 token" }, { status: 400 })
  }

  let body: RpcBody
  try {
    body = (await req.json()) as RpcBody
  } catch {
    return Response.json({ error: "请求体不是合法 JSON" }, { status: 400 })
  }

  const { cluster, instance, tool, arguments: args } = body
  if (!cluster || !instance || !tool) {
    return Response.json({ error: "缺少 cluster / instance / tool" }, { status: 400 })
  }

  const wsUrl = `${toWsBase(gateway)}/v1/rpc?cluster=${encodeURIComponent(
    cluster,
  )}&instance=${encodeURIComponent(instance)}`

  const encoder = new TextEncoder()

  const stream = new ReadableStream({
    start(controller) {
      let closed = false
      const send = (obj: unknown) => {
        if (closed) return
        controller.enqueue(encoder.encode(JSON.stringify(obj) + "\n"))
      }
      const finish = () => {
        if (closed) return
        closed = true
        try {
          controller.close()
        } catch {
          /* noop */
        }
      }

      let ws: WebSocket
      let sawToolEvent = false
      let silenceTimer: ReturnType<typeof setTimeout> | null = null
      const clearSilenceTimer = () => {
        if (silenceTimer) {
          clearTimeout(silenceTimer)
          silenceTimer = null
        }
      }
      const armSilenceTimer = () => {
        clearSilenceTimer()
        if (tool !== "doops_agent_prompt") return
        silenceTimer = setTimeout(() => {
          if (closed || sawToolEvent) return
          send({
            type: "error",
            error:
              "智能体长时间没有返回任何事件。远端 doagent 可能模型凭据过期、模型服务不可用，或 ACP 事件流没有结束。请检查目标节点 /var/log/do-agent-acp.log。",
          })
          try {
            ws.close()
          } catch {
            /* noop */
          }
          finish()
        }, AGENT_PROMPT_SILENCE_MS)
      }
      try {
        ws = new WebSocket(wsUrl, {
          headers: { Authorization: `Bearer ${token}` },
        })
      } catch (err) {
        send({ type: "error", error: `无法建立连接: ${(err as Error).message}` })
        finish()
        return
      }

      const callId = 2

      ws.on("open", () => {
        send({ type: "open" })
        ws.send(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: {} }))
      })

      ws.on("message", (raw: WebSocket.RawData) => {
        let msg: any
        try {
          msg = JSON.parse(raw.toString())
        } catch {
          return
        }

        // initialize 应答后发起工具调用
        if (msg.id === 1 && (msg.result !== undefined || msg.error !== undefined)) {
          if (msg.error) {
            send({ type: "error", error: msg.error?.message || "initialize 失败" })
            ws.close()
            return
          }
          ws.send(
            JSON.stringify({
              jsonrpc: "2.0",
              id: callId,
              method: "tools/call",
              params: { name: tool, arguments: args || {} },
            }),
          )
          armSilenceTimer()
          return
        }

        // 流式输出通知
        if (msg.method === "notifications/message") {
          sawToolEvent = true
          clearSilenceTimer()
          const data = msg.params?.data ?? ""
          send({ type: "output", data, session: msg.params?.sessionID })
          return
        }

        // 最终结果
        if (msg.id === callId) {
          sawToolEvent = true
          clearSilenceTimer()
          if (msg.error) {
            send({ type: "error", error: msg.error?.message || "执行失败", code: msg.error?.code })
          } else {
            send({ type: "result", result: msg.result })
          }
          ws.close()
        }
      })

      ws.on("error", (err: Error) => {
        clearSilenceTimer()
        send({ type: "error", error: `连接错误: ${err.message}` })
        finish()
      })

      ws.on("close", () => {
        clearSilenceTimer()
        send({ type: "done" })
        finish()
      })

      // 客户端取消时关闭上游 WS
      req.signal.addEventListener("abort", () => {
        clearSilenceTimer()
        try {
          ws.close()
        } catch {
          /* noop */
        }
        finish()
      })
    },
  })

  return new Response(stream, {
    headers: {
      "Content-Type": "application/x-ndjson; charset=utf-8",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
    },
  })
}
