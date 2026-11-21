const UNSAFE_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"])

// rejectCrossOriginMutation refuses cookie-authenticated changes whose Origin
// is not this console. Sibling sites on the same registrable domain are a
// different origin and are rejected.
//
// Set DOOPS_CONSOLE_ORIGIN (for example https://doops.example.com) to pin the
// console origin explicitly when the proxy or public URL differs from Next.js.
// Without this setting, only the URL and Host seen by Next.js are used.
export function rejectCrossOriginMutation(req: Request): Response | null {
  if (!UNSAFE_METHODS.has(req.method.toUpperCase())) return null
  const origin = (req.headers.get("origin") || "").trim()
  if (!origin || !consoleOrigins(req).has(origin)) {
    return Response.json({ error: "跨站请求被拒绝" }, { status: 403 })
  }
  const site = (req.headers.get("sec-fetch-site") || "").trim().toLowerCase()
  if (site && site !== "same-origin" && site !== "none") {
    return Response.json({ error: "跨站请求被拒绝" }, { status: 403 })
  }
  return null
}

function consoleOrigins(req: Request): Set<string> {
  const allowed = new Set<string>()
  const configured = process.env.DOOPS_CONSOLE_ORIGIN?.trim()
  if (configured) {
    for (const item of configured.split(",")) {
      try {
        allowed.add(new URL(item.trim()).origin)
      } catch {
        // An invalid entry allows nothing.
      }
    }
    return allowed
  }
  let url: URL | null = null
  try {
    url = new URL(req.url)
  } catch {
    url = null
  }
  const schemes = new Set<string>()
  if (url) schemes.add(url.protocol.replace(/:$/, ""))
  const hosts = new Set<string>()
  for (const host of [(req.headers.get("host") || "").trim(), url?.host || ""]) {
    if (host) hosts.add(host.toLowerCase())
  }
  for (const scheme of schemes) {
    for (const host of hosts) {
      try {
        allowed.add(new URL(`${scheme}://${host}`).origin)
      } catch {
        // Ignore a malformed host header.
      }
    }
  }
  return allowed
}
