# doops

`doops` is a gateway-first operations console and agent runtime. The public
repository layout is shared by GitHub and CNB so the same checkout can build the
web console, gateway, agent, and CLI without path rewrites.

## Repository Layout

```text
app/                         Next.js routes and API proxy endpoints
components/                  Console UI components
lib/                         Browser and API client helpers
backend/agent/               doops-agent, gateway hub, ACP bridge, scheduler
backend/gateway/             doops-gateway compatibility entrypoint
backend/skills/doops-cli/    doops CLI source and prebuilt CLI artifacts
backend/docs/                Runtime, deployment, and protocol documentation
Dockerfile.web               Production web console image
.cnb.yml                     CNB CI/release pipeline
.github/workflows/ci.yml     GitHub CI checks
```

The current standard source layout keeps frontend code at the repository root
and all Go services under `backend/`. Older CNB branches may still have Go
sources at the repository root; new work should use the layout above.

## Development Checks

Run the same checks before publishing to GitHub or CNB:

```bash
(cd backend/agent && go test ./...)
(cd backend/gateway && go test ./...)
(cd backend/skills/doops-cli && go test ./...)
pnpm exec tsc --noEmit
pnpm build
```

## Publishing Model

- GitHub and CNB should receive the same commit SHA for a release branch.
- GitHub uses `main` as the normalized public branch.
- CNB may still retain a legacy `master` branch; publish the normalized tree to
  `main` or to the same feature branch name used on GitHub.
- Do not publish local runtime directories such as `.next/`, `node_modules/`,
  `.pytest_cache/`, top-level `examples/`, or top-level `test/`.

## Runtime Boundary

Daily operations go through `doops-gateway` and registered `doops-agent`
instances. Do not rely on SSH credentials for normal agent operations; remote
actions should travel through doops RPC, gateway routing, and agent-side tools.
