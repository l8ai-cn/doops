ARG DOOPS_AGENT_BASE_IMAGE=invalid.invalid/doops-agent-base-image-must-be-specified
ARG DO_AGENT_VERSION

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
ARG DOOPS_AGENT_BASE_IMAGE
ARG DO_AGENT_VERSION
LABEL org.opencontainers.image.base.name="${DOOPS_AGENT_BASE_IMAGE}"
LABEL org.l8ai.doagent.version="${DO_AGENT_VERSION}"

COPY --from=builder /app/doops-agent /app/doops-agent
COPY --from=builder /app/agent/skills /app/skills
COPY --from=builder /app/agent/agent-entrypoint.sh /app/agent-entrypoint.sh
COPY --from=builder /app/agent/sandbox-entrypoint.sh /app/sandbox-entrypoint.sh
COPY --from=builder /app/agent/configure_doagent_settings.py /app/configure_doagent_settings.py
COPY --from=builder /app/deploy /app/deploy
COPY --from=builder /app/README.md /app/self-docs/README.md
COPY --from=builder /app/docs /app/self-docs/docs

RUN chmod +x /app/agent-entrypoint.sh /app/sandbox-entrypoint.sh \
    && /app/doops-agent -help >/dev/null \
    && test "$(/usr/local/bin/do-agent --version)" = "do-agent ${DO_AGENT_VERSION}" \
    && /usr/local/bin/do-agent --help >/dev/null \
    && buildctl --version \
    && python3 -c 'import yaml' \
    && helm version --short

ENTRYPOINT ["/app/sandbox-entrypoint.sh"]
