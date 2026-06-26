# Security Policy

## Reporting

Please report security issues privately to the maintainers. Do not open a public issue with credentials, exploit details, or production target information.

## Secrets

The repository must not contain real credentials or private infrastructure data. Use environment variables, Kubernetes Secrets, or local ignored config files for:

- Gateway user tokens
- Agent tokens
- OpenAI-compatible API keys
- Registry credentials
- SSH credentials
- Kubeconfig files

Before publishing a branch, run a secret scan and verify that examples use placeholder domains and tokens.
