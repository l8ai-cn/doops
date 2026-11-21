#!/usr/bin/env node
import { execFileSync, execSync, spawnSync } from "node:child_process"
import {
  cpSync,
  existsSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  realpathSync,
  renameSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs"
import { isIP } from "node:net"
import { dirname, join, resolve } from "node:path"

const DEFAULTS = {
  baseDir: "/var/lib/doweb",
  nginxAvailableDir: "/etc/nginx/sites-available",
  nginxEnabledDir: "/etc/nginx/sites-enabled",
  nginxSnippetDir: "/etc/nginx/doweb-snippets",
  nginxBin: "nginx",
  reloadCmd: "nginx -s reload",
  dockerImage: "nginx:1.27-alpine",
  dockerContainer: "doweb-nginx",
}

const MULTI_FLAGS = new Set(["proxy", "upstream", "health-url"])

// Layout belongs to the target operator, never to model-generated tool arguments.
const runtimeLayout = loadRuntimeLayout()

function loadRuntimeLayout() {
  if (!process.env.DOWEB_RUNTIME_CONFIG) return {}
  const config = JSON.parse(readFileSync(process.env.DOWEB_RUNTIME_CONFIG, "utf8"))
  const fields = new Set(["base-dir", "nginx-available-dir", "nginx-enabled-dir", "nginx-snippet-dir", "nginx-bin", "reload-argv", "listen-port", "listen-address"])
  if (!config || Array.isArray(config) || typeof config !== "object") throw new Error("DoWeb runtime config must be an object")
  for (const [key, value] of Object.entries(config)) {
    if (!fields.has(key)) throw new Error(`unsupported DoWeb runtime config field: ${key}`)
    if (key === "reload-argv") {
      if (!Array.isArray(value) || !value.length || value.some((part) => typeof part !== "string" || !part)) throw new Error("reload-argv must be a nonempty argument array")
    } else if (key === "listen-address") {
      if (typeof value !== "string" || !isIP(value)) throw new Error("listen-address must be an IP address")
    } else if (key === "listen-port") validatePort(value, key)
    else if (typeof value !== "string" || !value.trim()) throw new Error(`${key} must be text`)
  }
  return config
}

function parseArgs(argv) {
  const [command, ...rest] = argv
  if (!command || command.startsWith("--")) {
    throw new Error("usage: doweb <render|deploy|check|rollback|config-get|config-set|snippet-set|install-nginx|domain-set|domain-list|site-list> [flags]")
  }
  const options = {}
  for (let i = 0; i < rest.length; i += 1) {
    const raw = rest[i]
    if (!raw.startsWith("--")) throw new Error(`unexpected argument: ${raw}`)
    const key = raw.slice(2)
    if (["ssh-host", "ssh-user", "ssh-port", "password-env", "password-file", "token"].includes(key)) throw new Error("DoWeb does not accept SSH credentials or a separate login")
    const next = rest[i + 1]
    if (!next || next.startsWith("--")) throw new Error(`missing value for flag: --${key}`)
    const value = next
    i += 1
    if (MULTI_FLAGS.has(key)) {
      options[key] ||= []
      options[key].push(value)
    } else {
      if (Object.hasOwn(options, key)) throw new Error(`duplicate flag: --${key}`)
      options[key] = value
    }
  }
  return { command, options }
}

function opt(options, key, fallback = undefined) {
  return options[key] ?? fallback
}

function required(options, key) {
  const value = options[key]
  if (!value || value === "true") throw new Error(`missing required flag: --${key}`)
  return value
}

function normalizeSiteID(value) {
  const siteID = String(value || "").trim().toLowerCase()
  if (!/^[a-z0-9][a-z0-9-]{0,62}$/.test(siteID)) {
    throw new Error("site-id must match ^[a-z0-9][a-z0-9-]{0,62}$")
  }
  return siteID
}

function validateDomain(value) {
  const domain = String(value || "").trim().toLowerCase()
  if (domain === "localhost") return domain
  if (domain.length > 253 || domain.includes("..")) throw new Error("domain is invalid")
  const labels = domain.split(".")
  if (labels.length < 2) throw new Error("domain must contain at least two labels")
  for (const label of labels) {
    if (!/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label)) {
      throw new Error(`domain label is invalid: ${label}`)
    }
  }
  return domain
}

function validateIP(value) {
  const ip = String(value || "").trim()
  if (/^(?:\d{1,3}\.){3}\d{1,3}$/.test(ip)) {
    const parts = ip.split(".").map((part) => Number.parseInt(part, 10))
    if (parts.every((part) => part >= 0 && part <= 255)) return ip
  }
  if (/^[0-9a-fA-F:]+$/.test(ip) && ip.includes(":")) return ip
  throw new Error(`host-ip is invalid: ${ip}`)
}

function normalizeLocationPath(value) {
  let path = String(value || "").trim()
  if (!path.startsWith("/")) throw new Error(`location path must start with /: ${path}`)
  if (path.includes("..") || /[\s\\;{}$"']/.test(path)) throw new Error(`location path is unsafe: ${path}`)
  if (!path.endsWith("/")) path += "/"
  return path
}

function validateUpstreamName(value) {
  const name = String(value || "").trim()
  if (!/^[a-zA-Z][a-zA-Z0-9_-]{0,62}$/.test(name)) {
    throw new Error(`upstream name is invalid: ${name}`)
  }
  return name
}

function validateContainerName(value) {
  const name = String(value || "").trim()
  if (!/^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(name)) {
    throw new Error(`container name is invalid: ${name}`)
  }
  return name
}

function validateDockerImage(value) {
  const image = String(value || "").trim()
  if (!/^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,255}$/.test(image)) {
    throw new Error(`docker image is invalid: ${image}`)
  }
  return image
}

function domainFile(domainPaths, domain) {
  return join(domainPaths.domainsDir, `${domain}.json`)
}

function readDomainRegistry(domainPaths, domain) {
  const file = domainFile(domainPaths, domain)
  if (!existsSync(file)) return null
  return JSON.parse(readFileSync(file, "utf8"))
}

function listDomainRegistry(domainPaths) {
  if (!existsSync(domainPaths.domainsDir)) return []
  return readdirSync(domainPaths.domainsDir)
    .filter((name) => name.endsWith(".json"))
    .map((name) => JSON.parse(readFileSync(join(domainPaths.domainsDir, name), "utf8")))
    .sort((a, b) => a.domain.localeCompare(b.domain))
}

function resolveDomain(options, siteID) {
  const explicit = opt(options, "domain", "")
  const base = opt(options, "domain-base", "")
  if (explicit && base) throw new Error("use either --domain or --domain-base, not both")
  if (explicit) return validateDomain(explicit)
  if (!base) throw new Error("missing required flag: --domain or --domain-base")
  const baseDomain = validateDomain(base)
  const domainPaths = paths(options)
  const registry = readDomainRegistry(domainPaths, baseDomain)
  if (!registry) throw new Error(`domain-base is not registered: ${baseDomain}`)
  return `${siteID}.${baseDomain}`
}

function assertDomainAvailable(options, siteID, domain) {
  const sitePaths = paths(options, siteID)
  const stateDir = join(sitePaths.baseDir, "state")
  if (!existsSync(stateDir)) return
  for (const name of readdirSync(stateDir)) {
    if (!name.endsWith(".json")) continue
    const statePath = join(stateDir, name)
    const state = JSON.parse(readFileSync(statePath, "utf8"))
    if (state.domain === domain && state.site_id !== siteID) {
      throw new Error(`domain conflict: ${domain} is already used by ${state.site_id}`)
    }
  }
}

function validatePort(value, name) {
  const port = Number.parseInt(String(value), 10)
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error(`${name} must be a TCP port from 1 to 65535`)
  }
  return String(port)
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", "'\\''")}'`
}

function parseURLTarget(value) {
  let url
  try {
    url = new URL(value)
  } catch {
    throw new Error(`target URL is invalid: ${value}`)
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error(`target URL must use http or https: ${value}`)
  }
  if (url.username || url.password || url.hash || /[\s;{}$"'\\]/.test(value)) throw new Error("target URL contains unsupported credentials or Nginx syntax")
  return url
}

function parseProxy(value) {
  const idx = value.indexOf("=")
  if (idx <= 0) throw new Error(`proxy must use <path>=<url|upstream:name>: ${value}`)
  const path = normalizeLocationPath(value.slice(0, idx))
  const target = value.slice(idx + 1).trim()
  if (!target) throw new Error(`proxy target is empty for ${path}`)
  if (target.startsWith("upstream:")) {
    return { path, type: "upstream", upstream: validateUpstreamName(target.slice("upstream:".length)) }
  }
  parseURLTarget(target)
  return { path, type: "url", url: target }
}

function hostPort(url) {
  return url.host
}

function parseUpstream(value) {
  const idx = value.indexOf("=")
  if (idx <= 0) throw new Error(`upstream must use <name>=<url>,<url>: ${value}`)
  const name = validateUpstreamName(value.slice(0, idx))
  const endpoints = value
    .slice(idx + 1)
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean)
    .map((item) => {
      const url = parseURLTarget(item)
      if ((url.pathname && url.pathname !== "/") || url.search || url.hash) {
        throw new Error(`upstream endpoint must not include path, query or fragment: ${item}`)
      }
      return hostPort(url)
    })
  if (endpoints.length === 0) throw new Error(`upstream has no endpoints: ${name}`)
  return { name, endpoints }
}

function paths(options, siteID = "") {
  const settings = { ...runtimeLayout, ...options }
  const baseDir = resolve(opt(settings, "base-dir", DEFAULTS.baseDir))
  const siteDir = siteID ? join(baseDir, "sites", siteID) : ""
  return {
    baseDir,
    domainsDir: join(baseDir, "domains"),
    nginxAvailableDir: resolve(opt(settings, "nginx-available-dir", DEFAULTS.nginxAvailableDir)),
    nginxEnabledDir: resolve(opt(settings, "nginx-enabled-dir", DEFAULTS.nginxEnabledDir)),
    nginxSnippetDir: resolve(opt(settings, "nginx-snippet-dir", DEFAULTS.nginxSnippetDir)),
    nginxBin: opt(settings, "nginx-bin", DEFAULTS.nginxBin),
    reloadCmd: opt(settings, "reload-cmd", DEFAULTS.reloadCmd),
    reloadArgv: runtimeLayout["reload-argv"],
    listenPort: runtimeLayout["listen-port"],
    listenAddress: runtimeLayout["listen-address"],
    siteDir,
    releasesDir: siteID ? join(siteDir, "releases") : "",
    currentLink: siteID ? join(siteDir, "current") : "",
    statePath: siteID ? join(baseDir, "state", `${siteID}.json`) : "",
    configPath: siteID ? join(resolve(opt(settings, "nginx-available-dir", DEFAULTS.nginxAvailableDir)), `doweb-${siteID}.conf`) : "",
    enabledPath: siteID ? join(resolve(opt(settings, "nginx-enabled-dir", DEFAULTS.nginxEnabledDir)), `doweb-${siteID}.conf`) : "",
    snippetSiteDir: siteID ? join(resolve(opt(settings, "nginx-snippet-dir", DEFAULTS.nginxSnippetDir)), siteID) : "",
  }
}

function validateDist(dist) {
  const resolved = resolve(dist)
  if (!existsSync(resolved) || !lstatSync(resolved).isDirectory()) {
    throw new Error(`dist directory does not exist: ${resolved}`)
  }
  if (!existsSync(join(resolved, "index.html"))) {
    throw new Error(`dist directory must contain index.html: ${resolved}`)
  }
  function check(directory) {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const path = join(directory, entry.name)
      if (entry.isSymbolicLink()) throw new Error(`dist contains a symbolic link: ${entry.name}`)
      if (entry.isDirectory()) check(path)
      else if (!entry.isFile()) throw new Error(`dist contains a non-regular file: ${entry.name}`)
    }
  }
  check(resolved)
  return resolved
}

function releaseID() {
  const stamp = new Date().toISOString().replace(/[-:.TZ]/g, "").slice(0, 17)
  const suffix = Math.random().toString(36).slice(2, 8)
  return `${stamp}-${suffix}`
}

function atomicWrite(file, content) {
  mkdirSync(dirname(file), { recursive: true })
  const tmp = `${file}.${process.pid}.${Date.now()}.tmp`
  writeFileSync(tmp, content)
  renameSync(tmp, file)
}

function replaceSymlink(linkPath, targetPath) {
  mkdirSync(dirname(linkPath), { recursive: true })
  const tmp = `${linkPath}.${process.pid}.${Date.now()}.tmp`
  rmSync(tmp, { force: true, recursive: true })
  symlinkSync(targetPath, tmp)
  if (existsSync(linkPath) && !lstatSync(linkPath).isSymbolicLink()) throw new Error(`expected managed symlink: ${linkPath}`)
  renameSync(tmp, linkPath)
}

function readCurrentSymlink(linkPath) {
  if (!existsSync(linkPath)) return null
  if (!lstatSync(linkPath).isSymbolicLink()) throw new Error(`expected symlink: ${linkPath}`)
  return realpathSync(linkPath)
}

function readState(statePath) {
  if (!existsSync(statePath)) return { releases: [] }
  return JSON.parse(readFileSync(statePath, "utf8"))
}

function writeState(statePath, state) {
  atomicWrite(statePath, `${JSON.stringify(state, null, 2)}\n`)
}

function buildSpec(options, release = "") {
  const siteID = normalizeSiteID(required(options, "site-id"))
  const sitePaths = paths(options, siteID)
  const dist = opt(options, "dist") ? validateDist(opt(options, "dist")) : ""
  const domain = resolveDomain(options, siteID)
  const proxies = (options.proxy || []).map(parseProxy)
  const upstreams = (options.upstream || []).map(parseUpstream)
  const upstreamNames = new Set(upstreams.map((upstream) => upstream.name))
  for (const proxy of proxies) {
    if (proxy.type === "upstream" && !upstreamNames.has(proxy.upstream)) {
      throw new Error(`proxy references missing upstream: ${proxy.upstream}`)
    }
  }
  const staticRoot = opt(options, "static-root", sitePaths.currentLink)
  if (!!opt(options, "tls-cert", "") !== !!opt(options, "tls-key", "")) throw new Error("tls-cert and tls-key must be supplied together")
  for (const path of [staticRoot, sitePaths.nginxSnippetDir, opt(options, "tls-cert", ""), opt(options, "tls-key", "")]) {
    if (/[\s;{}$"'\\]/.test(path)) throw new Error("Nginx paths contain unsupported syntax")
  }
  if (new Set(proxies.map((proxy) => proxy.path)).size !== proxies.length) throw new Error("duplicate proxy location")
  return {
    site_id: siteID,
    domain,
    dist,
    release,
    static_root: staticRoot,
    spa_fallback: opt(options, "spa-fallback", "true") !== "false",
    tls_cert: opt(options, "tls-cert", ""),
    tls_key: opt(options, "tls-key", ""),
    proxies,
    upstreams,
  }
}

function renderConfig(spec, sitePaths) {
  const lines = [
    "# Managed by DoWeb. Edit through doweb config-set or snippet-set.",
    `# SiteSpec: ${spec.site_id} ${spec.domain}`,
  ]
  for (const upstream of spec.upstreams) {
    lines.push(`upstream ${spec.site_id}_${upstream.name} {`)
    for (const endpoint of upstream.endpoints) lines.push(`  server ${endpoint};`)
    lines.push("}", "")
  }
  const listen = spec.tls_cert && spec.tls_key ? "443 ssl http2" : String(sitePaths.listenPort || 80)
  const address = sitePaths.listenAddress
  const binding = address && isIP(address) === 6 ? `[${address}]` : address
  lines.push("server {")
  lines.push(`  listen ${binding ? `${binding}:` : ""}${listen};`)
  lines.push(`  server_name ${spec.domain};`)
  if (spec.tls_cert && spec.tls_key) {
    lines.push(`  ssl_certificate ${spec.tls_cert};`)
    lines.push(`  ssl_certificate_key ${spec.tls_key};`)
  }
  lines.push(`  root ${spec.static_root};`)
  lines.push("  index index.html;")
  lines.push(`  include ${join(sitePaths.nginxSnippetDir, spec.site_id, "*.conf")};`)
  const hasRootProxy = spec.proxies.some((proxy) => proxy.path === "/")
  if (!hasRootProxy) {
    lines.push("")
    lines.push("  location / {")
    lines.push(spec.spa_fallback ? "    try_files $uri $uri/ /index.html;" : "    try_files $uri $uri/ =404;")
    lines.push("  }")
  }
  for (const proxy of spec.proxies) {
    const target = proxy.type === "upstream" ? `http://${spec.site_id}_${proxy.upstream}` : proxy.url
    lines.push("")
    lines.push(`  location ${proxy.path} {`)
    lines.push("    proxy_http_version 1.1;")
    lines.push("    proxy_set_header Host $host;")
    lines.push("    proxy_set_header X-Real-IP $remote_addr;")
    lines.push("    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;")
    lines.push("    proxy_set_header X-Forwarded-Proto $scheme;")
    lines.push(`    proxy_pass ${target};`)
    lines.push("  }")
  }
  lines.push("}")
  return `${lines.join("\n")}\n`
}

function validateNginx(sitePaths) {
  return runNginx(sitePaths.nginxBin, ["-t"])
}

function reloadNginx(sitePaths) {
  if (sitePaths.reloadArgv) return runNginx(sitePaths.reloadArgv[0], sitePaths.reloadArgv.slice(1))
  return execSync(sitePaths.reloadCmd, {
    encoding: "utf8",
    env: process.env,
    shell: "/bin/sh",
    stdio: ["ignore", "pipe", "pipe"],
  })
}

function runNginx(command, args) {
  const result = spawnSync(command, args, { encoding: "utf8", timeout: 30000 })
  const output = `${result.stdout || ""}${result.stderr || ""}`.trim()
  if (result.error || result.status !== 0) throw new Error(`Nginx command failed: ${output || result.error?.message || result.status}`)
  return output
}

function restoreAfterFailure(sitePaths, previous, error, operation) {
  try {
    restoreActivation(sitePaths, previous)
    validateNginx(sitePaths)
    reloadNginx(sitePaths)
  } catch (recoveryError) {
    throw Object.assign(new Error(`${operation} failed: ${error.message}; rollback failed: ${recoveryError.message}; outcome requires inspection`), { executionOutcome: "unknown" })
  }
  throw new Error(`${operation} failed: ${error.message}; previous activation validated and reloaded`)
}

async function checkHealth(urls) {
  const results = []
  for (const url of urls) {
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), 10000)
    try {
      const response = await fetch(url, { redirect: "manual", signal: controller.signal })
      results.push({ url, status: response.status, ok: response.status >= 200 && response.status < 400 })
    } finally {
      clearTimeout(timer)
    }
  }
  const failed = results.find((result) => !result.ok)
  if (failed) throw new Error(`health check failed for ${failed.url}: HTTP ${failed.status}`)
  return results
}

function restoreActivation(sitePaths, previous) {
  if (previous.config === null) {
    rmSync(sitePaths.configPath, { force: true })
    rmSync(sitePaths.enabledPath, { force: true })
  } else {
    atomicWrite(sitePaths.configPath, previous.config)
    replaceSymlink(sitePaths.enabledPath, sitePaths.configPath)
  }
  if (previous.current === null) {
    rmSync(sitePaths.currentLink, { force: true, recursive: true })
  } else {
    replaceSymlink(sitePaths.currentLink, previous.current)
  }
  if (previous.state === null) rmSync(sitePaths.statePath, { force: true })
  else atomicWrite(sitePaths.statePath, previous.state)
}

function activationSnapshot(sitePaths) {
  return {
    config: existsSync(sitePaths.configPath) ? readFileSync(sitePaths.configPath, "utf8") : null,
    current: readCurrentSymlink(sitePaths.currentLink),
    state: existsSync(sitePaths.statePath) ? readFileSync(sitePaths.statePath, "utf8") : null,
  }
}

function installSpec(options) {
  const mode = required(options, "mode")
  if (mode !== "docker" && mode !== "host") throw new Error("install-nginx supports --mode docker or --mode host")
  if (opt(options, "local", "true") !== "true") throw new Error("DoWeb install-nginx only executes on the selected target")
  for (const key of ["ssh-host", "ssh-user", "ssh-port", "password-env", "password-file"]) {
    if (options[key]) throw new Error("DoWeb does not accept SSH or password arguments")
  }
  return {
    mode,
    dryRun: opt(options, "dry-run", "false") === "true",
    containerName: validateContainerName(opt(options, "container-name", DEFAULTS.dockerContainer)),
    image: validateDockerImage(opt(options, "image", DEFAULTS.dockerImage)),
    httpPort: validatePort(opt(options, "http-port", "80"), "http-port"),
    httpsPort: validatePort(opt(options, "https-port", "443"), "https-port"),
    baseDir: resolve(opt(options, "base-dir", DEFAULTS.baseDir)),
    dockerNginxDir: resolve(opt(options, "docker-nginx-dir", "/etc/doweb/nginx")),
  }
}

function hostDeployFlags(spec) {
  return [
    `--base-dir ${shellQuote(spec.baseDir)}`,
    "--nginx-available-dir /etc/doweb/nginx/sites-available",
    "--nginx-enabled-dir /etc/nginx/conf.d",
    "--nginx-snippet-dir /etc/doweb/nginx/snippets",
    "--nginx-bin nginx",
    "--reload-cmd 'nginx -s reload'",
  ]
}

function dockerDeployFlags(spec) {
  const available = join(spec.dockerNginxDir, "sites-available")
  const enabled = join(spec.dockerNginxDir, "conf.d")
  const snippets = join(spec.dockerNginxDir, "snippets")
  return [
    `--base-dir ${shellQuote(spec.baseDir)}`,
    `--nginx-available-dir ${shellQuote(available)}`,
    `--nginx-enabled-dir ${shellQuote(enabled)}`,
    `--nginx-snippet-dir ${shellQuote(snippets)}`,
    "--nginx-bin /usr/local/bin/doweb-nginx",
    `--reload-cmd ${shellQuote(`docker exec ${spec.containerName} nginx -s reload`)}`,
  ]
}

function hostInstallScript(spec) {
  const baseDir = shellQuote(spec.baseDir)
  return `#!/bin/sh
set -eu

SUDO=""
if [ "$(id -u)" != "0" ]; then
  SUDO="sudo"
fi

if ! command -v nginx >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    $SUDO apt-get update
    $SUDO apt-get install -y --no-install-recommends nginx
  elif command -v dnf >/dev/null 2>&1; then
    $SUDO dnf install -y nginx
  elif command -v yum >/dev/null 2>&1; then
    $SUDO yum install -y nginx
  else
    echo "No supported package manager found for Nginx installation" >&2
    exit 1
  fi
fi

$SUDO mkdir -p ${baseDir}/sites ${baseDir}/state ${baseDir}/domains /etc/doweb/nginx/sites-available /etc/doweb/nginx/snippets
$SUDO chmod 755 ${baseDir} ${baseDir}/sites ${baseDir}/state ${baseDir}/domains /etc/doweb /etc/doweb/nginx /etc/doweb/nginx/sites-available /etc/doweb/nginx/snippets
nginx -t
`
}

function dockerInstallScript(spec) {
  const container = shellQuote(spec.containerName)
  const image = shellQuote(spec.image)
  const baseDir = shellQuote(spec.baseDir)
  const nginxDir = shellQuote(spec.dockerNginxDir)
  const httpPort = shellQuote(spec.httpPort)
  const httpsPort = shellQuote(spec.httpsPort)
  return `#!/bin/sh
set -eu

SUDO=""
if [ "$(id -u)" != "0" ]; then
  SUDO="sudo"
fi

need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

install_docker() {
  if need_cmd docker; then
    return 0
  fi
  if need_cmd apt-get; then
    export DEBIAN_FRONTEND=noninteractive
    $SUDO apt-get update
    $SUDO apt-get install -y --no-install-recommends docker.io ca-certificates curl
  elif need_cmd dnf; then
    $SUDO dnf install -y docker
  elif need_cmd yum; then
    $SUDO yum install -y docker
  else
    echo "No supported package manager found for Docker installation" >&2
    exit 1
  fi
}

start_docker() {
  if need_cmd systemctl; then
    $SUDO systemctl enable --now docker
  elif need_cmd service; then
    $SUDO service docker start
  fi
  docker info >/dev/null
}

install_docker
start_docker

$SUDO mkdir -p ${baseDir}/sites ${baseDir}/state ${nginxDir}/sites-available ${nginxDir}/conf.d ${nginxDir}/snippets
$SUDO chmod 755 ${baseDir} ${baseDir}/sites ${baseDir}/state ${nginxDir} ${nginxDir}/sites-available ${nginxDir}/conf.d ${nginxDir}/snippets

$SUDO tee /usr/local/bin/doweb-nginx >/dev/null <<'DOWEB_NGINX_WRAPPER'
#!/bin/sh
exec docker exec ${spec.containerName} nginx "$@"
DOWEB_NGINX_WRAPPER
$SUDO chmod 0755 /usr/local/bin/doweb-nginx

docker pull ${image}
if docker ps -a --format '{{.Names}}' | grep -Fx ${container} >/dev/null 2>&1; then
  echo "Existing DoWeb Nginx container must be managed explicitly, not replaced by bootstrap" >&2
  exit 1
fi
docker run -d \\
  --name ${container} \\
  --restart unless-stopped \\
  -p ${httpPort}:80 \\
  -p ${httpsPort}:443 \\
  -v ${shellQuote(`${spec.baseDir}:${spec.baseDir}:ro`)} \\
  -v ${shellQuote(`${spec.dockerNginxDir}:${spec.dockerNginxDir}:ro`)} \\
  -v ${shellQuote(`${spec.dockerNginxDir}/conf.d:/etc/nginx/conf.d:ro`)} \\
  ${image}

/usr/local/bin/doweb-nginx -t
docker ps --filter name=${container}
`
}

async function commandInstallNginx(options) {
  const spec = installSpec(options)
  const script = spec.mode === "host" ? hostInstallScript(spec) : dockerInstallScript(spec)
  const deployFlags = spec.mode === "host" ? hostDeployFlags(spec) : dockerDeployFlags(spec)
  if (spec.dryRun) {
    process.stdout.write(JSON.stringify({ status: "dry_run", mode: spec.mode, execution: "target_local", script, deploy_flags: deployFlags.join(" ") }, null, 2) + "\n")
    return
  }
  const output = execFileSync("sh", ["-s"], {
    input: script,
    encoding: "utf8",
    env: process.env,
    timeout: 180000,
    stdio: ["pipe", "pipe", "pipe"],
  })
  process.stdout.write(JSON.stringify({ status: "installed", mode: spec.mode, execution: "target_local", deploy_flags: deployFlags.join(" "), output }, null, 2) + "\n")
}

async function commandDomainSet(options) {
  const domainPaths = paths(options)
  const domain = validateDomain(required(options, "domain"))
  const hostIP = validateIP(required(options, "host-ip"))
  const provider = opt(options, "provider", "manual")
  if (!/^[a-zA-Z0-9_-]{1,64}$/.test(provider)) throw new Error(`provider is invalid: ${provider}`)
  const record = {
    domain,
    host_ip: hostIP,
    provider,
    wildcard: opt(options, "wildcard", "true") !== "false",
    updated_at: new Date().toISOString(),
  }
  atomicWrite(domainFile(domainPaths, domain), `${JSON.stringify(record, null, 2)}\n`)
  process.stdout.write(JSON.stringify({ status: "domain_configured", ...record }, null, 2))
  process.stdout.write("\n")
}

function commandSiteList(options) {
  const dir = join(paths(options).baseDir, "state")
  let entries
  try {
    entries = readdirSync(dir, { withFileTypes: true })
  } catch (error) {
    if (error.code !== "ENOENT") throw error
    entries = []
  }
  const sites = []
  for (const entry of entries) {
    if (!entry.name.endsWith(".json")) continue
    const siteID = normalizeSiteID(entry.name.slice(0, -5))
    if (!entry.isFile() || entry.name !== `${siteID}.json`) throw new Error(`invalid DoWeb state entry: ${entry.name}`)
    const raw = readFileSync(join(dir, entry.name), "utf8")
    let state
    try {
      state = JSON.parse(raw)
    } catch {
      throw new Error(`invalid DoWeb state JSON for ${siteID}`)
    }
    if (state?.site_id !== siteID || typeof state.current_release !== "string" || !state.current_release ||
        typeof state.updated_at !== "string" || !Number.isFinite(Date.parse(state.updated_at)) ||
        (state.previous_release != null && typeof state.previous_release !== "string")) {
      throw new Error(`invalid DoWeb release record for ${siteID}`)
    }
    sites.push({ site_id: siteID, domain: validateDomain(state.domain), current_release: state.current_release,
      previous_release: state.previous_release ?? null, updated_at: state.updated_at })
  }
  sites.sort((a, b) => a.site_id.localeCompare(b.site_id))
  process.stdout.write(`${JSON.stringify({ sites })}\n`)
}

async function commandDomainList(options) {
  const domainPaths = paths(options)
  process.stdout.write(JSON.stringify({ status: "ok", domains: listDomainRegistry(domainPaths) }, null, 2))
  process.stdout.write("\n")
}

async function commandRender(options) {
  const spec = buildSpec(options)
  process.stdout.write(renderConfig(spec, paths(options, spec.site_id)))
}

async function commandDeploy(options) {
  const spec = buildSpec(options, releaseID())
  const sitePaths = paths(options, spec.site_id)
  assertDomainAvailable(options, spec.site_id, spec.domain)
  const state = readState(sitePaths.statePath)
  const previous = activationSnapshot(sitePaths)
  const releaseDir = join(sitePaths.releasesDir, spec.release)
  mkdirSync(sitePaths.releasesDir, { recursive: true })
  mkdirSync(sitePaths.snippetSiteDir, { recursive: true })
  cpSync(spec.dist, releaseDir, { recursive: true, dereference: true })
  const releaseSpec = { ...spec, static_root: sitePaths.currentLink }
  atomicWrite(join(releaseDir, ".doweb-site.json"), `${JSON.stringify(releaseSpec, null, 2)}\n`)
  try {
    replaceSymlink(sitePaths.currentLink, releaseDir)
    atomicWrite(sitePaths.configPath, renderConfig(releaseSpec, sitePaths))
    replaceSymlink(sitePaths.enabledPath, sitePaths.configPath)
    const validation = validateNginx(sitePaths)
    const reload = reloadNginx(sitePaths)
    const health = options["health-url"] ? await checkHealth(options["health-url"]) : []
    const releases = [...(state.releases || []).filter((item) => item !== spec.release), spec.release].slice(-20)
    writeState(sitePaths.statePath, {
      site_id: spec.site_id,
      current_release: spec.release,
      previous_release: state.current_release || null,
      releases,
      domain: spec.domain,
      updated_at: new Date().toISOString(),
    })
    const evidence = { status: "deployed", site_id: spec.site_id, domain: spec.domain, release: spec.release, validation, reload, health }
    atomicWrite(join(sitePaths.baseDir, "evidence", spec.site_id, `${spec.release}.json`), `${JSON.stringify(evidence, null, 2)}\n`)
    process.stdout.write(JSON.stringify(evidence, null, 2))
    process.stdout.write("\n")
  } catch (error) {
    restoreAfterFailure(sitePaths, previous, error, "deploy")
  }
}

async function commandCheck(options) {
  const siteID = normalizeSiteID(required(options, "site-id"))
  const sitePaths = paths(options, siteID)
  const validation = validateNginx(sitePaths)
  const health = options["health-url"] ? await checkHealth(options["health-url"]) : []
  process.stdout.write(JSON.stringify({ status: "ok", site_id: siteID, validation, health }, null, 2))
  process.stdout.write("\n")
}

async function commandRollback(options) {
  const siteID = normalizeSiteID(required(options, "site-id"))
  const sitePaths = paths(options, siteID)
  const state = readState(sitePaths.statePath)
  const releases = state.releases || []
  const targetRelease = state.previous_release
  if (!targetRelease) throw new Error(`site has no previous release: ${siteID}`)
  const releaseDir = join(sitePaths.releasesDir, targetRelease)
  const releaseSpecPath = join(releaseDir, ".doweb-site.json")
  if (!existsSync(releaseDir) || !existsSync(releaseSpecPath)) {
    throw new Error(`previous release is missing: ${targetRelease}`)
  }
  const previous = activationSnapshot(sitePaths)
  const spec = JSON.parse(readFileSync(releaseSpecPath, "utf8"))
  try {
    replaceSymlink(sitePaths.currentLink, releaseDir)
    atomicWrite(sitePaths.configPath, renderConfig(spec, sitePaths))
    replaceSymlink(sitePaths.enabledPath, sitePaths.configPath)
    const validation = validateNginx(sitePaths)
    const reload = reloadNginx(sitePaths)
    const health = options["health-url"] ? await checkHealth(options["health-url"]) : []
    const targetIndex = releases.lastIndexOf(targetRelease)
    writeState(sitePaths.statePath, {
      ...state,
      domain: spec.domain,
      current_release: targetRelease,
      previous_release: targetIndex > 0 ? releases[targetIndex - 1] : null,
      updated_at: new Date().toISOString(),
    })
    process.stdout.write(JSON.stringify({ status: "rolled_back", site_id: siteID, release: targetRelease, validation, reload, health }, null, 2))
    process.stdout.write("\n")
  } catch (error) {
    restoreAfterFailure(sitePaths, previous, error, "rollback")
  }
}

async function commandConfigGet(options) {
  const siteID = normalizeSiteID(required(options, "site-id"))
  const sitePaths = paths(options, siteID)
  if (!existsSync(sitePaths.configPath)) throw new Error(`config does not exist: ${sitePaths.configPath}`)
  process.stdout.write(readFileSync(sitePaths.configPath, "utf8"))
}

async function commandConfigSet(options) {
  const siteID = normalizeSiteID(required(options, "site-id"))
  const sitePaths = paths(options, siteID)
  const config = candidateText(options, "config")
  if (!/\bserver\s*\{/.test(config)) throw new Error("config-file must contain an nginx server block")
  const previous = activationSnapshot(sitePaths)
  try {
    atomicWrite(sitePaths.configPath, config)
    replaceSymlink(sitePaths.enabledPath, sitePaths.configPath)
    const validation = validateNginx(sitePaths)
    const reload = reloadNginx(sitePaths)
    const health = options["health-url"] ? await checkHealth(options["health-url"]) : []
    process.stdout.write(JSON.stringify({ status: "configured", site_id: siteID, config: sitePaths.configPath, validation, reload, health }, null, 2))
    process.stdout.write("\n")
  } catch (error) {
    restoreAfterFailure(sitePaths, previous, error, "config-set")
  }
}

async function commandSnippetSet(options) {
  const siteID = normalizeSiteID(required(options, "site-id"))
  const sitePaths = paths(options, siteID)
  const name = validateUpstreamName(required(options, "name")).toLowerCase()
  const snippet = candidateText(options, "snippet")
  if (/\b(events|http|stream)\s*\{/.test(snippet)) {
    throw new Error("snippet-set only accepts directives valid inside a server block")
  }
  const target = join(sitePaths.snippetSiteDir, `${name}.conf`)
  const previous = existsSync(target) ? readFileSync(target, "utf8") : null
  try {
    atomicWrite(target, snippet)
    const validation = validateNginx(sitePaths)
    const effectiveConfig = runNginx(sitePaths.nginxBin, ["-T"])
    if (!effectiveConfig.split("\n").includes(`# configuration file ${target}:`)) {
      throw new Error(`site configuration does not include the managed snippet: ${target}`)
    }
    const reload = reloadNginx(sitePaths)
    const health = options["health-url"] ? await checkHealth(options["health-url"]) : []
    process.stdout.write(JSON.stringify({ status: "snippet_configured", site_id: siteID, snippet: target, validation, reload, health }, null, 2))
    process.stdout.write("\n")
  } catch (error) {
    if (previous === null) rmSync(target, { force: true })
    else atomicWrite(target, previous)
    try {
      validateNginx(sitePaths)
      reloadNginx(sitePaths)
    } catch (recoveryError) {
      throw Object.assign(new Error(`snippet-set failed: ${error.message}; rollback failed: ${recoveryError.message}; outcome requires inspection`), { executionOutcome: "unknown" })
    }
    throw new Error(`snippet-set failed: ${error.message}; previous snippet validated and reloaded`)
  }
}

function candidateText(options, kind) {
  const file = options[`${kind}-file`]
  const content = options[`${kind}-content`]
  if (!!file === !!content) throw new Error(`supply exactly one of ${kind}-file or ${kind}-content`)
  return file ? readFileSync(resolve(file), "utf8") : content
}

async function main() {
  const { command, options } = parseArgs(process.argv.slice(2))
  switch (command) {
    case "render":
      return commandRender(options)
    case "deploy":
      return commandDeploy(options)
    case "check":
      return commandCheck(options)
    case "rollback":
      return commandRollback(options)
    case "config-get":
      return commandConfigGet(options)
    case "config-set":
      return commandConfigSet(options)
    case "snippet-set":
      return commandSnippetSet(options)
    case "install-nginx":
      return commandInstallNginx(options)
    case "domain-set":
      return commandDomainSet(options)
    case "site-list":
      return commandSiteList(options)
    case "domain-list":
      return commandDomainList(options)
    default:
      throw new Error(`unknown command: ${command}`)
  }
}

main().catch((error) => {
  process.stderr.write(`${JSON.stringify({ status: "failed", executionOutcome: error.executionOutcome || "failed", retryable: false, error: error.message }, null, 2)}\n`)
  process.exit(1)
})
