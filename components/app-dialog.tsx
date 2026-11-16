"use client"

import { useCallback, useEffect, useId, useRef, useState } from "react"
import { createPortal } from "react-dom"
import { Button, Input } from "./ui"

export type PromptOptions = {
  title: string
  message?: string
  label?: string
  placeholder?: string
  defaultValue?: string
  confirmLabel?: string
  cancelLabel?: string
  inputType?: "text" | "password"
}

type PromptRequest = {
  id: number
  options: PromptOptions
  resolve: (value: string | null) => void
  settled: boolean
}

function settle(request: PromptRequest, value: string | null) {
  if (request.settled) return
  request.settled = true
  request.resolve(value)
}

export function useAppDialog() {
  const seq = useRef(0)
  const requestRef = useRef<PromptRequest | null>(null)
  const [request, setRequest] = useState<PromptRequest | null>(null)

  const replace = useCallback((next: PromptRequest) => {
    const previous = requestRef.current
    if (previous) settle(previous, null)
    requestRef.current = next
    setRequest(next)
  }, [])

  const prompt = useCallback(
    (options: PromptOptions) =>
      new Promise<string | null>((resolve) => {
        replace({ id: ++seq.current, options, resolve, settled: false })
      }),
    [replace],
  )

  const close = useCallback((value: string | null) => {
    const current = requestRef.current
    if (!current) return
    requestRef.current = null
    setRequest(null)
    settle(current, value)
  }, [])

  useEffect(() => {
    return () => {
      const current = requestRef.current
      if (!current) return
      requestRef.current = null
      settle(current, null)
    }
  }, [])

  const dialog = request ? <AppDialog key={request.id} request={request} onClose={close} /> : null
  return { prompt, dialog }
}

function AppDialog({
  request,
  onClose,
}: {
  request: PromptRequest
  onClose: (value: string | null) => void
}) {
  const titleId = useId()
  const messageId = useId()
  const inputRef = useRef<HTMLInputElement>(null)
  const [value, setValue] = useState(request.options.defaultValue ?? "")
  const [error, setError] = useState("")
  const message = request.options.message

  useEffect(() => {
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null
    const focusTarget = inputRef.current
    focusTarget?.focus()
    if (focusTarget instanceof HTMLInputElement) focusTarget.select()
    return () => previous?.focus()
  }, [])

  useEffect(() => {
    function onKey(event: KeyboardEvent) {
      if (event.key !== "Escape") return
      event.preventDefault()
      event.stopPropagation()
      onClose(null)
    }
    document.addEventListener("keydown", onKey, true)
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = "hidden"
    return () => {
      document.removeEventListener("keydown", onKey, true)
      document.body.style.overflow = previousOverflow
    }
  }, [onClose])

  function submitPrompt() {
    const next = request.options.inputType === "password" ? value : value.trim()
    if (!next) {
      setError(request.options.inputType === "password" ? "请输入密码" : "请填写内容")
      inputRef.current?.focus()
      return
    }
    onClose(next)
  }

  function cancel() {
    onClose(null)
  }

  if (typeof document === "undefined") return null

  return createPortal(
    <div
      className="fixed inset-0 z-[80] flex items-center justify-center bg-black/60 p-4"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) cancel()
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={message ? messageId : undefined}
        className="w-full max-w-md rounded-xl border bg-card shadow-xl"
        onMouseDown={(event) => event.stopPropagation()}
      >
        <div className="border-b px-4 py-3">
          <h3 id={titleId} className="text-sm font-semibold text-foreground">
            {request.options.title}
          </h3>
        </div>
        <form
          className="space-y-3 p-4"
          onSubmit={(event) => {
            event.preventDefault()
            submitPrompt()
          }}
        >
          {message ? (
            <p id={messageId} className="text-sm leading-relaxed text-foreground">
              {message}
            </p>
          ) : null}
          <label className="flex flex-col gap-1.5 text-xs text-muted-foreground">
            {request.options.label || "内容"}
            <Input
              ref={inputRef}
              value={value}
              onChange={(event) => {
                setValue(event.target.value)
                if (error) setError("")
              }}
              placeholder={request.options.placeholder}
              type={request.options.inputType === "password" ? "password" : "text"}
              autoComplete={request.options.inputType === "password" ? "new-password" : "off"}
              spellCheck={false}
              aria-invalid={error ? true : undefined}
              className="w-full"
            />
          </label>
          {error ? <p className="text-xs text-destructive">{error}</p> : null}
          <div className="flex justify-end gap-2 pt-1">
            <Button
              type="button"
              onClick={cancel}
              size="sm"
            >
              {request.options.cancelLabel || "取消"}
            </Button>
            <Button
              type="submit"
              size="sm"
              variant="primary"
            >
              {request.options.confirmLabel || "确认"}
            </Button>
          </div>
        </form>
      </div>
    </div>,
    document.body,
  )
}
