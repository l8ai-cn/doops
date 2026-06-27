ARG DOOPS_AGENT_BASE_IMAGE=docker.cnb.cool/l8ai/ai/doops.sh/base:v1

# Stage 1: Build doops-agent (Go gateway)
FROM golang:1.24-alpine AS builder
USER root
WORKDIR /app
COPY . .
WORKDIR /app/agent
RUN CGO_ENABLED=0 go build -o /app/doops-agent ./cmd/agent

# Stage 2: Lightweight app image.
# The base image owns sandbox/doagent/buildkit/system packages. This layer only
# changes when doops code, skills, docs, or entrypoints change.
FROM ${DOOPS_AGENT_BASE_IMAGE}

COPY --from=builder /app/doops-agent /app/doops-agent
COPY --from=builder /app/agent/skills /app/skills
COPY --from=builder /app/agent/agent-entrypoint.sh /app/agent-entrypoint.sh
COPY --from=builder /app/agent/sandbox-entrypoint.sh /app/sandbox-entrypoint.sh
COPY --from=builder /app/README.md /app/self-docs/README.md
COPY --from=builder /app/docs /app/self-docs/docs

RUN chmod +x /app/agent-entrypoint.sh /app/sandbox-entrypoint.sh \
    && /usr/local/bin/do-agent --help >/dev/null \
    && buildctl --version

ENTRYPOINT ["/app/sandbox-entrypoint.sh"]
