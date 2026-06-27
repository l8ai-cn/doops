# Contributing

Thanks for improving doops. Keep changes focused, tested, and free of environment-specific secrets.

## Development

Run the relevant package tests before opening a pull request:

```bash
(cd backend/agent && go test ./...)
(cd backend/gateway && go test ./...)
(cd backend/skills/doops-cli && go test ./...)
pnpm exec tsc --noEmit
pnpm build
```

## Security and Configuration

- Do not commit passwords, tokens, kubeconfigs, private keys, production IPs, or internal registry credentials.
- Use placeholders such as `gateway.example.com`, `registry.example.com`, and `<GATEWAY_USER_TOKEN>` in docs and tests.
- Put local runtime config under ignored paths such as `~/.agent/skills/doops/` or environment variables.
- If a secret was committed, rotate it first, then remove it from both the working tree and git history before publishing.
