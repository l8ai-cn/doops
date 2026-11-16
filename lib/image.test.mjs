import assert from "node:assert/strict"
import { execFileSync, spawnSync } from "node:child_process"
import { createHash } from "node:crypto"
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import test from "node:test"
import {
  OUTPUT_MAX_LINES,
  appendImageOutput,
  cleanPath,
  createImageOutput,
  durationWarning,
  explainImageFailure,
  finishImageOutput,
  imageBuildRequest,
  imageCompletion,
  imageCopyRequest,
  imageOutputText,
  imageResults,
  registrySyncRequest,
} from "./image.ts"

const build = { context: "/root/ws/image", dockerfile: "", platform: "linux/amd64,linux/arm64", output: "oci", outputPath: "/tmp/image.oci", image: "", push: false }
const copy = { source: "docker://registry.example/source/app:release", destination: "oci-archive:/tmp/app.oci", sourceAuthfile: "/run/source-auth.json", destinationAuthfile: "", all: true, verify: true, timeout: 600 }
const sync = { sourceRegistry: "registry.example.com/source/team", targetRegistry: "registry.example.com/mirror/team", tag: "release-20261004", images: "api, worker", sourceAuthfile: "/run/doops/source-auth.json", targetAuthfile: "/run/doops/target-auth.json", timeout: 600 }

const PAYLOAD_KEYS = ["payload", "password", "token", "secret", "credential_payload", "username", "data"]

function assertNoPayload(request) {
  assert.deepEqual(Object.keys(request).sort(), ["arguments", "tool"])
  assert.deepEqual(Object.keys(request.arguments).sort(), ["command", "session_id"])
  for (const key of PAYLOAD_KEYS) assert.equal(key in request.arguments, false)
}

test("image build maps form fields to the CLI BuildKit command", () => {
  const cases = [
    {
      name: "oci archive with default Dockerfile",
      input: build,
      want: ["command -v buildctl", "test -S /run/buildkit/buildkitd.sock", "buildctl --addr 'unix:///run/buildkit/buildkitd.sock' build --progress=plain --frontend dockerfile.v0 --local context='/root/ws/image' --local dockerfile='/root/ws/image' --opt filename='Dockerfile'", "--opt 'platform=linux/amd64,linux/arm64'", "--output 'type=oci,dest=/tmp/image.oci'"],
    },
    {
      name: "image output pushed with explicit Dockerfile",
      input: { ...build, dockerfile: "/root/ws/image/docker/Dockerfile.prod", output: "image", outputPath: "", image: "registry.example/team/app:v1", push: true },
      want: ["--local dockerfile='/root/ws/image/docker' --opt filename='Dockerfile.prod'", "--output 'type=image,name=registry.example/team/app:v1,push=true'"],
    },
    {
      name: "no platform omits the platform opt",
      input: { ...build, platform: "" },
      unwanted: ["platform="],
    },
    {
      name: "spaces around platform commas are dropped",
      input: { ...build, platform: "linux/amd64, linux/arm64" },
      want: ["--opt 'platform=linux/amd64,linux/arm64'"],
    },
  ]
  for (const c of cases) {
    const request = imageBuildRequest("img-test", c.input)
    assert.equal(request.tool, "doops_shell", c.name)
    assert.equal(request.arguments.session_id, "img-test", c.name)
    for (const want of c.want || []) assert.ok(request.arguments.command.includes(want), `${c.name}: missing ${want}`)
    for (const unwanted of c.unwanted || []) assert.ok(!request.arguments.command.includes(unwanted), `${c.name}: unexpected ${unwanted}`)
    assertNoPayload(request)
  }
})

test("image build rejects inputs the CLI rejects", () => {
  for (const [name, invalid] of [
    ["missing context", { context: " " }],
    ["oci without path", { outputPath: "" }],
    ["image without reference", { output: "image", image: "" }],
    ["push with oci", { push: true }],
    ["unknown output", { output: "tar" }],
    ["dockerfile names a directory", { dockerfile: "/" }],
    ["newline injection", { context: "/root/ws\nrm -rf /tmp/x" }],
  ]) {
    assert.throws(() => imageBuildRequest("img-test", { ...build, ...invalid }), undefined, name)
  }
})

// Values the shell would quote correctly can still reach BuildKit or Skopeo as
// options: a leading "--" becomes a skopeo flag, a comma becomes a buildctl
// output key. Those are stopped before anything is sent.
test("inputs that would reach the shell or a tool as options are rejected before sending", () => {
  const builders = { build: [imageBuildRequest, build], copy: [imageCopyRequest, copy], sync: [registrySyncRequest, sync] }
  const cases = [
    ["build", "context with newline", { context: "/root/ws\nrm -rf /tmp/x" }, /换行或控制字符/],
    ["build", "relative context", { context: "root/ws" }, /绝对路径/],
    ["build", "relative dockerfile", { dockerfile: "Dockerfile" }, /绝对路径/],
    ["build", "oci path with a comma adds a buildctl output option", { outputPath: "/tmp/a.oci,type=local" }, /归档路径/],
    ["build", "oci path with a double quote breaks the buildctl CSV", { outputPath: '/tmp/a".oci' }, /归档路径/],
    ["build", "relative oci path", { outputPath: "out.oci" }, /归档路径/],
    ["build", "image reference with a comma turns on push", { output: "image", outputPath: "", image: "registry.example/app:v1,push=true" }, /镜像引用/],
    ["build", "image reference with a space", { output: "image", outputPath: "", image: "registry.example/app v1" }, /镜像引用/],
    ["build", "platform with shell metacharacters", { platform: "linux/amd64;id" }, /平台/],
    ["copy", "source passes inline credentials as a skopeo flag", { source: "--src-creds=user:synthetic-canary" }, /传输方式/],
    ["copy", "destination passes an option", { destination: "--dest-authfile=/tmp/x" }, /传输方式/],
    ["copy", "source without a transport", { source: "registry.example/app:v1" }, /传输方式/],
    ["copy", "source with whitespace adds a second reference", { source: "docker://registry.example/app:v1 docker://other.example/x" }, /传输方式/],
    ["copy", "relative source authfile", { sourceAuthfile: "auth.json" }, /authfile/],
    ["copy", "authfile with a newline", { destinationAuthfile: "/run/a\n.json" }, /换行或控制字符/],
    ["copy", "timeout above one day", { timeout: 86401 }, /超时/],
    ["copy", "timeout that is not a number", { timeout: Number.NaN }, /超时/],
    ["sync", "tag with a semicolon", { tag: "v1;rm" }, /tag/],
    ["sync", "image with command substitution", { images: "api $(id)" }, /镜像名/],
    ["sync", "image with a quote", { images: "api'" }, /镜像名/],
    ["sync", "registry with a dollar sign", { sourceRegistry: "reg.example/$HOME" }, /源 Registry/],
    ["sync", "relative target authfile", { targetAuthfile: "target.json" }, /authfile/],
    ["sync", "timeout written in exponent form", { timeout: 1e21 }, /超时/],
  ]
  for (const [mode, name, invalid, message] of cases) {
    const [builder, base] = builders[mode]
    assert.throws(() => builder("img-test", { ...base, ...invalid }), message, `${mode}: ${name}`)
  }
})

test("image copy maps flags, authfiles and verification like the CLI", () => {
  const cases = [
    {
      name: "verified copy with source authfile",
      input: copy,
      want: ["command -v skopeo", "command -v sha256sum", "timeout 600s 'skopeo' 'copy' '--all' '--retry-times' '3' '--src-authfile' '/run/source-auth.json' 'docker://registry.example/source/app:release' 'oci-archive:/tmp/app.oci'", "'skopeo' 'inspect' '--raw' '--authfile' '/run/source-auth.json'", "doops_image_error=digest_mismatch", "manifest_digest=sha256:"],
      unwanted: ["--dest-authfile"],
    },
    {
      name: "single manifest without verification",
      input: { ...copy, all: false, verify: false, sourceAuthfile: "", destinationAuthfile: "/run/dst.json", timeout: 30 },
      want: ["timeout 30s 'skopeo' 'copy' '--retry-times' '3' '--dest-authfile' '/run/dst.json'"],
      unwanted: ["'--all'", "sha256sum", "inspect", "--src-authfile"],
    },
  ]
  for (const c of cases) {
    const request = imageCopyRequest("img-test", c.input)
    for (const want of c.want) assert.ok(request.arguments.command.includes(want), `${c.name}: missing ${want}`)
    for (const unwanted of c.unwanted) assert.ok(!request.arguments.command.includes(unwanted), `${c.name}: unexpected ${unwanted}`)
    assertNoPayload(request)
  }
  for (const invalid of [{ source: "" }, { destination: "" }, { timeout: 0 }, { timeout: 1.5 }]) {
    assert.throws(() => imageCopyRequest("img-test", { ...copy, ...invalid }))
  }
})

test("registry sync copies every image and checks each digest", () => {
  const request = registrySyncRequest("img-test", sync)
  const command = request.arguments.command
  for (const want of [
    "skopeo copy --all --retry-times 3 --src-authfile '/run/doops/source-auth.json' --dest-authfile '/run/doops/target-auth.json'",
    "'docker://registry.example.com/source/team/api:release-20261004'",
    "'docker://registry.example.com/mirror/team/worker:release-20261004'",
    "skopeo inspect --raw --authfile '/run/doops/source-auth.json'",
    "doops_image_error=digest_mismatch",
    "ref=worker:release-20261004",
  ]) assert.ok(command.includes(want), `missing ${want}`)
  assertNoPayload(request)

  const bare = registrySyncRequest("img-test", { ...sync, images: "api", sourceAuthfile: "", targetAuthfile: "" }).arguments.command
  for (const unwanted of ["--src-authfile", "--dest-authfile", "--authfile"]) assert.ok(!bare.includes(unwanted))

  for (const invalid of [{ images: "" }, { tag: "" }, { tag: "v1;rm" }, { sourceRegistry: "reg example" }, { images: "api $(id)" }, { timeout: -1 }]) {
    assert.throws(() => registrySyncRequest("img-test", { ...sync, ...invalid }))
  }
})

test("image mutations build requests without confirmation and still require a session", () => {
  for (const [request, input] of [[imageBuildRequest, build], [imageCopyRequest, copy], [registrySyncRequest, sync]]) {
    const result = request("img-test", input)
    assert.equal(result.tool, "doops_shell")
    assert.deepEqual(Object.keys(result.arguments).sort(), ["command", "session_id"])
    assert.throws(() => request("", input), /缺少操作 session/)
  }
})

test("shell quoting keeps single quotes inside one argument", () => {
  const command = imageCopyRequest("img-test", { ...copy, source: "docker://r/a:it's" }).arguments.command
  assert.ok(command.includes(`'docker://r/a:it'"'"'s'`))
})

test("path cleaning matches Go path.Clean", () => {
  assert.equal(cleanPath("/root/ws//image/./Dockerfile"), "/root/ws/image/Dockerfile")
  assert.equal(cleanPath("ctx/../Dockerfile"), "Dockerfile")
})

// The generated commands run here under the same bash the Agent uses, with
// stub buildctl/skopeo/timeout on an otherwise empty PATH. Each stub records
// its argv, so the tests see exactly what a real tool would receive.

function which(name) {
  try { return execFileSync("bash", ["-c", `command -v ${name}`], { encoding: "utf8" }).trim() } catch { return "" }
}
const BASH = which("bash")
const REAL_TOOLS = { sha256sum: which("sha256sum"), cut: which("cut") }
const SHELL_SKIP = BASH && REAL_TOOLS.sha256sum && REAL_TOOLS.cut ? false : "执行生成的命令需要 bash、sha256sum 和 cut"
const SOCKET_PRESENT = existsSync("/run/buildkit/buildkitd.sock")

const LOG = 'log() { { printf "%s\\n" "${0##*/}"; for a in "$@"; do printf "%s\\n" "$a"; done; printf -- "---\\n"; } >> "$STUB_LOG"; }'
const MANIFEST = '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json"}'
const OTHER_MANIFEST = '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}'
const sha = (text) => "sha256:" + createHash("sha256").update(text).digest("hex")
const BLOB = "1a".repeat(32)

const TIMEOUT_PASS = 'shift\nexec "$@"'
const TIMEOUT_COPY_EXPIRES = 'shift\n[ "$1 $2" = "skopeo copy" ] && exit 124\nexec "$@"'
const SKOPEO_OK = `case "$1" in copy) echo "Copying blob sha256:${BLOB} done"; echo "Writing manifest to image destination";; inspect) printf '%s' '${MANIFEST}';; esac`
const SKOPEO_401 = `[ "$1" = copy ] && { echo 'FATA[0000] initializing source docker://registry.example/source/app:release: reading manifest release in registry.example/source/app: unauthorized: authentication required' >&2; exit 1; }`
const SKOPEO_404 = `[ "$1" = copy ] && { echo 'FATA[0000] initializing source docker://registry.example/source/app:release: reading manifest release in registry.example/source/app: manifest unknown' >&2; exit 1; }`
const SKOPEO_MISMATCH = `for last; do :; done\ncase "$1" in inspect) if [ "$last" = oci-archive:/tmp/app.oci ]; then printf '%s' '${OTHER_MANIFEST}'; else printf '%s' '${MANIFEST}'; fi;; esac`
const SKOPEO_INSPECT_FAILS = '[ "$1" = inspect ] && exit 1\nexit 0'
const SKOPEO_WORKER_FAILS = `for last; do :; done\ncase "$1" in copy) case "$last" in */worker:*) echo 'FATA[0000] writing manifest: unauthorized: authentication required' >&2; exit 1;; esac;; inspect) printf '%s' '${MANIFEST}';; esac`

function runScript(command, stubs, setup) {
  const dir = mkdtempSync(join(tmpdir(), "doops-image-"))
  try {
    const bin = join(dir, "bin")
    mkdirSync(bin)
    for (const [name, real] of Object.entries(REAL_TOOLS)) symlinkSync(real, join(bin, name))
    for (const [name, body] of Object.entries(stubs)) {
      const file = join(bin, name)
      writeFileSync(file, `#!${BASH}\n${LOG}\nlog "$@"\n${body}\n`)
      chmodSync(file, 0o755)
    }
    const logFile = join(dir, "calls.log")
    writeFileSync(logFile, "")
    setup?.(dir)
    const result = spawnSync(BASH, ["-c", command(dir)], { cwd: dir, env: { PATH: bin, STUB_LOG: logFile, HOME: dir }, encoding: "utf8" })
    const calls = readFileSync(logFile, "utf8").split("---\n").filter(Boolean).map((block) => block.replace(/\n$/, "").split("\n")).filter((call) => call[0] !== "timeout")
    const created = ["p1", "p2", "p3", "p4"].filter((name) => existsSync(join(dir, name)))
    return { dir, status: result.status, stdout: result.stdout, output: result.stdout + result.stderr, calls, created }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

const copyDefault = { ...copy, sourceAuthfile: "", destinationAuthfile: "" }
const syncDefault = { ...sync, sourceAuthfile: "", targetAuthfile: "" }
// Every shell-active character: quote break, command substitution, backticks,
// command separator. Bash builtins write the marker files if any of them runs.
const HOSTILE_REF = "docker://registry.example/app'$(echo>p1)'`echo>p2`;echo>p3:v1"
const HOSTILE_DIR = "ctx it's;$(echo>p1)`echo>p2`"

test("generated commands report each failure mode with a marker and exit code", { skip: SHELL_SKIP }, () => {
  const cases = [
    {
      name: "copy on a target without skopeo reports the missing tool",
      command: () => imageCopyRequest("img-test", copyDefault).arguments.command,
      stubs: { timeout: TIMEOUT_PASS },
      status: 127, marker: "doops_image_error=missing_tool tool=skopeo", noCalls: ["skopeo"],
    },
    {
      name: "build on a target without buildctl reports the missing tool",
      command: (dir) => imageBuildRequest("img-test", { ...build, context: dir, outputPath: join(dir, "out.oci") }).arguments.command,
      stubs: {},
      status: 127, marker: "doops_image_error=missing_tool tool=buildctl",
    },
    {
      name: "registry sync on a target without timeout reports the missing tool",
      command: () => registrySyncRequest("img-test", syncDefault).arguments.command,
      stubs: { skopeo: SKOPEO_OK },
      status: 127, marker: "doops_image_error=missing_tool tool=timeout", noCalls: ["skopeo"],
    },
    {
      name: "a named authfile that does not exist stops before skopeo runs",
      command: (dir) => imageCopyRequest("img-test", { ...copyDefault, sourceAuthfile: join(dir, "missing auth.json") }).arguments.command,
      stubs: { timeout: TIMEOUT_PASS, skopeo: SKOPEO_OK },
      status: 1, marker: "doops_image_error=authfile_missing role=source path=", noCalls: ["skopeo"],
    },
    {
      name: "registry sync with a missing target authfile stops before skopeo runs",
      command: (dir) => registrySyncRequest("img-test", { ...syncDefault, targetAuthfile: join(dir, "target.json") }).arguments.command,
      stubs: { timeout: TIMEOUT_PASS, skopeo: SKOPEO_OK },
      status: 1, marker: "doops_image_error=authfile_missing role=target path=", noCalls: ["skopeo"],
    },
    {
      name: "a registry 401 on copy names the failed step",
      command: () => imageCopyRequest("img-test", copyDefault).arguments.command,
      stubs: { timeout: TIMEOUT_PASS, skopeo: SKOPEO_401 },
      status: 1, marker: "doops_image_error=step_failed rc=1 step=copy", output: /unauthorized: authentication required/,
    },
    {
      name: "a registry 404 on copy names the failed step",
      command: () => imageCopyRequest("img-test", copyDefault).arguments.command,
      stubs: { timeout: TIMEOUT_PASS, skopeo: SKOPEO_404 },
      status: 1, marker: "doops_image_error=step_failed rc=1 step=copy", output: /manifest unknown/,
    },
    {
      name: "a copy that runs out of time keeps the timeout exit code",
      command: () => imageCopyRequest("img-test", copyDefault).arguments.command,
      stubs: { timeout: TIMEOUT_COPY_EXPIRES, skopeo: SKOPEO_OK },
      status: 124, marker: "doops_image_error=step_failed rc=124 step=copy",
    },
    {
      name: "different manifests fail verification and print both digests",
      command: () => imageCopyRequest("img-test", copyDefault).arguments.command,
      stubs: { timeout: TIMEOUT_PASS, skopeo: SKOPEO_MISMATCH },
      status: 1, marker: `doops_image_error=digest_mismatch source=${sha(MANIFEST)} destination=${sha(OTHER_MANIFEST)}`, notOutput: /manifest_digest=/,
    },
    {
      // Without pipefail both failed inspects hash the empty string, compare
      // equal and the copy is reported as verified.
      name: "failed inspects on both sides are not reported as a verified copy",
      command: () => imageCopyRequest("img-test", copyDefault).arguments.command,
      stubs: { timeout: TIMEOUT_PASS, skopeo: SKOPEO_INSPECT_FAILS },
      status: 1, marker: "doops_image_error=step_failed rc=1 step=inspect_source", notOutput: /manifest_digest=/,
    },
    {
      name: "a verified copy prints the destination manifest digest",
      command: () => imageCopyRequest("img-test", copyDefault).arguments.command,
      stubs: { timeout: TIMEOUT_PASS, skopeo: SKOPEO_OK },
      status: 0, stdout: `manifest_digest=${sha(MANIFEST)}`,
    },
    {
      name: "registry sync stops at the first failing image and keeps the earlier digest",
      command: () => registrySyncRequest("img-test", syncDefault).arguments.command,
      stubs: { timeout: TIMEOUT_PASS, skopeo: SKOPEO_WORKER_FAILS },
      status: 1, stdout: `api:release-20261004 manifest_digest=${sha(MANIFEST)}`, marker: "doops_image_error=step_failed rc=1 step=copy ref=worker:release-20261004",
    },
    {
      name: "a missing build context is reported before BuildKit runs",
      command: (dir) => imageBuildRequest("img-test", { ...build, context: join(dir, "absent"), outputPath: join(dir, "out.oci") }).arguments.command,
      stubs: { buildctl: "" },
      status: 1, marker: "doops_image_error=path_missing role=context path=", noCalls: ["buildctl"],
    },
    {
      name: "a missing Dockerfile is reported before BuildKit runs",
      command: (dir) => imageBuildRequest("img-test", { ...build, context: dir, outputPath: join(dir, "out.oci") }).arguments.command,
      stubs: { buildctl: "" },
      status: 1, marker: "doops_image_error=path_missing role=dockerfile path=", noCalls: ["buildctl"],
    },
    {
      name: "shell metacharacters in a source reference reach skopeo as one literal argument",
      command: () => imageCopyRequest("img-test", { ...copyDefault, source: HOSTILE_REF }).arguments.command,
      stubs: { timeout: TIMEOUT_PASS, skopeo: SKOPEO_OK },
      status: 0, call: ["skopeo", "copy", "--all", "--retry-times", "3", HOSTILE_REF, "oci-archive:/tmp/app.oci"],
    },
    {
      name: "shell metacharacters in a build context path stay literal",
      setup: (dir) => { mkdirSync(join(dir, HOSTILE_DIR)); writeFileSync(join(dir, HOSTILE_DIR, "Dockerfile"), "FROM scratch\n") },
      command: (dir) => imageBuildRequest("img-test", { ...build, context: join(dir, HOSTILE_DIR), outputPath: join(dir, "out.oci") }).arguments.command,
      stubs: { buildctl: "" },
      ...(SOCKET_PRESENT ? {} : { status: 1, marker: "doops_image_error=buildkit_socket_missing path=/run/buildkit/buildkitd.sock" }),
    },
  ]
  for (const c of cases) {
    const run = runScript(c.command, c.stubs, c.setup)
    if (c.status !== undefined) assert.equal(run.status, c.status, `${c.name}: exit code\n${run.output}`)
    if (c.marker) assert.ok(run.output.includes(c.marker), `${c.name}: missing ${c.marker}\n${run.output}`)
    if (c.output) assert.match(run.output, c.output, c.name)
    if (c.notOutput) assert.doesNotMatch(run.output, c.notOutput, c.name)
    if (c.stdout) assert.ok(run.stdout.includes(c.stdout), `${c.name}: stdout missing ${c.stdout}\n${run.output}`)
    for (const tool of c.noCalls || []) assert.ok(!run.calls.some((call) => call[0] === tool), `${c.name}: ${tool} must not run`)
    if (c.call) assert.deepEqual(run.calls.find((call) => call[0] === c.call[0] && call[1] === c.call[1]), c.call, c.name)
    assert.deepEqual(run.created, [], `${c.name}: injected command ran`)
  }
})

test("the BuildKit invocation passes every form field as one argument", { skip: SHELL_SKIP }, () => {
  let expected
  const run = runScript((dir) => {
    const context = join(dir, HOSTILE_DIR)
    const outputPath = join(dir, "out it's.oci")
    expected = ["buildctl", "--addr", "unix:///run/buildkit/buildkitd.sock", "build", "--progress=plain", "--frontend", "dockerfile.v0", "--local", `context=${context}`, "--local", `dockerfile=${context}`, "--opt", "filename=Dockerfile", "--opt", "platform=linux/amd64", "--output", `type=oci,dest=${outputPath}`]
    const command = imageBuildRequest("img-test", { ...build, context, platform: "linux/amd64", outputPath }).arguments.command
    return command.split("\n").find((line) => line.startsWith("buildctl "))
  }, { buildctl: "" })
  assert.equal(run.status, 0, run.output)
  assert.deepEqual(run.calls, [expected])
  assert.deepEqual(run.created, [])
})

// Panel-facing helpers: what the operator sees while a command streams and
// after it ends. These run on synthetic output only.

const CLUSTER_TARGET = { cluster: "test", instance: "node-1" }
const DIGEST_A = "sha256:" + "a1".repeat(32)
const DIGEST_B = "sha256:" + "b2".repeat(32)
const DIGEST_L = "sha256:" + "4c".repeat(32)
const BLOB_DIGEST = "sha256:" + "1a".repeat(32)
const HEARTBEAT = "\r\u001b[K[agent] ⏳ 命令后台执行中... (耗时 5s)"

function outputOf(...chunks) {
  const state = createImageOutput()
  for (const chunk of chunks) appendImageOutput(state, chunk)
  return finishImageOutput(state)
}

test("streamed output keeps heartbeats out of the log and stays bounded", () => {
  const beat = outputOf(HEARTBEAT)
  assert.deepEqual(beat.lines, [])
  assert.equal(beat.progress, "[agent] ⏳ 命令后台执行中... (耗时 5s)")
  assert.doesNotMatch(imageOutputText(beat), /\u001b|耗时/)

  const colored = outputOf("\u001b[31mFATA\u001b[0m[0000] boom\n")
  assert.deepEqual(colored.lines, ["FATA[0000] boom"])

  const split = outputOf("Copying bl", "ob done\nWriting manifest\n", "tail without newline")
  assert.deepEqual(split.lines, ["Copying blob done", "Writing manifest", "tail without newline"])

  const overwritten = outputOf("progress 10%\rprogress 100%\n")
  assert.deepEqual(overwritten.lines, ["progress 100%"])

  const total = OUTPUT_MAX_LINES + 500
  const chunks = [`manifest_digest=${DIGEST_A}\n`]
  for (let i = 1; i < total; i++) chunks.push(`line ${i}\n`)
  const long = outputOf(...chunks)
  assert.ok(long.lines.length <= OUTPUT_MAX_LINES, "visible lines are capped")
  const text = imageOutputText(long)
  assert.match(text, /^…已省略前 \d+ 行…\n/)
  assert.ok(text.endsWith(`line ${total - 1}`), "the newest line stays visible")
  assert.deepEqual(imageResults("copy", long).digests, [{ ref: "", digest: DIGEST_A, kind: "manifest" }], "a digest printed before the cap is kept")

  const wide = outputOf("x".repeat(100_000) + "\n")
  assert.ok(wide.lines[0].length < 10_000, "one huge line is truncated")
  assert.match(wide.lines[0], /已截断/)
})

test("results list manifest digests only, never blob or config digests", () => {
  const cases = [
    {
      name: "verified copy",
      mode: "copy",
      output: [`Copying blob ${BLOB_DIGEST} done\n`, "Writing manifest to image destination\n", `manifest_digest=${DIGEST_A}\n`],
      digests: [{ ref: "", digest: DIGEST_A, kind: "manifest" }],
    },
    {
      name: "copy without verification has no manifest digest",
      mode: "copy",
      output: [`Copying blob ${BLOB_DIGEST} done\n`, "Writing manifest to image destination\n"],
      digests: [],
    },
    {
      name: "registry sync names each image",
      mode: "sync",
      output: [`api:release-1 manifest_digest=${DIGEST_A}\n`, `Copying blob ${BLOB_DIGEST} done\n`, `worker:release-1 manifest_digest=${DIGEST_B}\n`],
      digests: [{ ref: "api:release-1", digest: DIGEST_A, kind: "manifest" }, { ref: "worker:release-1", digest: DIGEST_B, kind: "manifest" }],
    },
    {
      name: "multi-platform build reports the manifest list",
      mode: "build",
      output: [`#9 exporting manifest ${DIGEST_A} 0.0s done\n`, `#9 exporting config ${BLOB_DIGEST} done\n`, `#9 exporting manifest ${DIGEST_B} done\n`, `#9 exporting manifest list ${DIGEST_L} 0.0s done\n`],
      digests: [{ ref: "", digest: DIGEST_L, kind: "manifest list" }],
    },
    {
      name: "single-platform build reports its manifest",
      mode: "build",
      output: [`#9 exporting layers done\n`, `#9 exporting manifest ${DIGEST_A} 0.0s done\n`, `#9 exporting config ${BLOB_DIGEST} done\n`],
      digests: [{ ref: "", digest: DIGEST_A, kind: "manifest" }],
    },
  ]
  for (const c of cases) assert.deepEqual(imageResults(c.mode, outputOf(...c.output)).digests, c.digests, c.name)

  const failure = imageResults("copy", outputOf("doops_image_error=authfile_missing role=source path=/run/missing auth.json\n")).failure
  assert.deepEqual(failure, { kind: "authfile_missing", fields: { role: "source", path: "/run/missing auth.json" } })
})

test("failures are explained in terms an operator can act on", () => {
  const exit1 = "Command failed with error: exit status 1"
  const cases = [
    { name: "skopeo missing", message: "Command failed with error: exit status 127", output: ["doops_image_error=missing_tool tool=skopeo\n"], kind: "missing_tool", text: /缺少 skopeo.*Agent 镜像/ },
    { name: "buildctl missing", message: "Command failed with error: exit status 127", output: ["doops_image_error=missing_tool tool=buildctl\n"], kind: "missing_tool", text: /缺少 buildctl/ },
    { name: "BuildKit not running", message: exit1, output: ["doops_image_error=buildkit_socket_missing path=/run/buildkit/buildkitd.sock\n"], kind: "buildkit", text: /BuildKit.*buildkitd\.sock/ },
    { name: "authfile missing", message: exit1, output: ["doops_image_error=authfile_missing role=destination path=/run/a b.json\n"], kind: "authfile", text: /目标 authfile 不存在或不可读：\/run\/a b\.json.*留空/ },
    { name: "context missing", message: exit1, output: ["doops_image_error=path_missing role=context path=/root/ws/app\n"], kind: "path", text: /构建上下文目录不存在：\/root\/ws\/app/ },
    { name: "registry 401", message: exit1, output: ["FATA[0000] reading manifest release: unauthorized: authentication required\n", "doops_image_error=step_failed rc=1 step=copy\n"], kind: "registry_auth", text: /401.*authfile/ },
    { name: "registry 403", message: exit1, output: ["FATA[0000] writing blob: denied: requested access to the resource is denied\n", "doops_image_error=step_failed rc=1 step=copy\n"], kind: "registry_denied", text: /403/ },
    { name: "registry 404", message: exit1, output: ["FATA[0000] reading manifest release: manifest unknown\n", "doops_image_error=step_failed rc=1 step=copy\n"], kind: "not_found", text: /404.*tag/ },
    { name: "step timeout", message: "Command failed with error: exit status 124", output: ["doops_image_error=step_failed rc=124 step=copy\n"], timeout: 600, kind: "timeout", text: /复制.*超时.*600 秒/ },
    { name: "sync names the failed image", message: exit1, output: ["doops_image_error=step_failed rc=1 step=copy ref=worker:release-1\n"], kind: "step", text: /worker:release-1.*退出码 1/ },
    { name: "digest mismatch", message: exit1, output: [`doops_image_error=digest_mismatch source=${DIGEST_A} destination=${DIGEST_B}\n`], kind: "digest_mismatch", text: new RegExp(`${DIGEST_A}.*${DIGEST_B}.*重试`) },
    { name: "empty manifest", message: exit1, output: ["doops_image_error=empty_manifest role=destination\n"], kind: "empty_manifest", text: /目标.*manifest 为空/ },
    { name: "no grant: Gateway refuses the upgrade", message: "连接错误: Unexpected server response: 403", kind: "forbidden", text: /无权.*test\/node-1.*授权/ },
    { name: "grant revoked during dispatch", message: "forbidden: exec on test/node-1", kind: "forbidden", text: /无权/ },
    { name: "login expired", message: "连接错误: Unexpected server response: 401", kind: "auth", text: /重新登录/ },
    { name: "console route: no grant at the upgrade", message: "无权访问该目标，请联系管理员授权（HTTP 403）", kind: "forbidden", text: /无权.*test\/node-1.*授权/ },
    { name: "console route: expired login at the upgrade", message: "登录已失效，请重新登录（HTTP 401）", kind: "auth", text: /重新登录/ },
    { name: "console route: unknown target at the upgrade", message: "目标不存在或已下线（HTTP 404）", kind: "offline", text: /离线.*test\/node-1/ },
    { name: "target offline", message: "target offline: test/node-1", kind: "offline", text: /离线.*test\/node-1/ },
    { name: "session busy", message: "session has an active operation; wait for terminal evidence: op-7", kind: "busy", text: /正忙/ },
    { name: "queue full", message: "target queue full: test/node-1", kind: "busy", text: /正忙/ },
    { name: "user stop", message: "operation canceled", stop: true, kind: "stopped", text: /已停止/ },
    { name: "stop before dispatch", message: "请求派发前已停止", stop: true, kind: "stopped", text: /已停止/ },
    { name: "Agent time limit", message: "operation canceled", kind: "canceled", text: /30 分钟/ },
    { name: "stop not confirmed", message: "execution_outcome_unknown：远端终态未确认，请核对实际状态，不要直接重放原任务", stop: true, kind: "unknown", text: /不要直接重新执行/ },
    { name: "command did not start", message: "Error starting command: fork/exec /bin/bash: resource temporarily unavailable", kind: "start", text: /没能在目标上启动/ },
    { name: "plain non-zero exit", message: "Command failed with error: exit status 2", kind: "other", text: /退出码 2/ },
  ]
  for (const c of cases) {
    const explained = explainImageFailure(c.message, outputOf(...(c.output || [])), { stopRequested: c.stop === true, target: CLUSTER_TARGET, timeout: c.timeout })
    assert.equal(explained.kind, c.kind, c.name)
    assert.match(explained.text, c.text, `${c.name}: ${explained.text}`)
  }
})

test("a finished result only counts as success when the Agent reports completion", () => {
  assert.deepEqual(imageCompletion("Operation complete."), { ok: true, text: "" })
  assert.deepEqual(imageCompletion("Error starting command: fork failed"), { ok: false, text: "Error starting command: fork failed" })
})

test("a worst-case duration above the Agent time limit is called out before running", () => {
  const cases = [
    { name: "default verified copy fits", mode: "copy", input: copy, want: "" },
    { name: "long verified copy", mode: "copy", input: { ...copy, timeout: 900 }, want: /45 分钟.*30 分钟/ },
    { name: "unverified copy is one step", mode: "copy", input: { ...copy, verify: false, timeout: 1800 }, want: "" },
    { name: "one image sync fits", mode: "sync", input: { ...sync, images: "api" }, want: "" },
    { name: "two image sync", mode: "sync", input: sync, want: /60 分钟.*分批/ },
    { name: "duplicate image names count once", mode: "sync", input: { ...sync, images: "api api" }, want: "" },
    { name: "invalid timeout gives no estimate", mode: "copy", input: { ...copy, timeout: Number.NaN }, want: "" },
    { name: "build has no per-step bound", mode: "build", input: build, want: "" },
  ]
  for (const c of cases) {
    const warning = durationWarning(c.mode, c.input)
    if (c.want === "") assert.equal(warning, "", c.name)
    else assert.match(warning, c.want, c.name)
  }
})

test("registry sync copies a repeated image name once and rejects a tag inside the name", () => {
  const command = registrySyncRequest("img-test", { ...syncDefault, images: "api, api" }).arguments.command
  assert.equal(command.split("\n").filter((line) => line.startsWith("timeout 600s skopeo copy")).length, 1)
  assert.throws(() => registrySyncRequest("img-test", { ...sync, images: "api:v2" }), /镜像名.*tag/)
  assert.throws(() => registrySyncRequest("img-test", { ...sync, images: "api@sha256:abc" }), /镜像名.*tag/)
})
