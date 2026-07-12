ARG DOOPS_AGENT_BASE_IMAGE=docker.cnb.cool/l8ai/ai/doops.sh/base-light:latest

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
ARG TARGETARCH
ARG KUBECTL_VERSION=v1.33.1
LABEL org.opencontainers.image.base.name="${DOOPS_AGENT_BASE_IMAGE}"

COPY --from=builder /app/doops-agent /app/doops-agent
COPY --from=builder /app/agent/skills /app/skills
COPY --from=builder /app/agent/agent-entrypoint.sh /app/agent-entrypoint.sh
COPY --from=builder /app/agent/sandbox-entrypoint.sh /app/sandbox-entrypoint.sh
COPY --from=builder /app/agent/configure_doagent_settings.py /app/configure_doagent_settings.py
COPY --from=builder /app/deploy /app/deploy
COPY --from=builder /app/README.md /app/self-docs/README.md
COPY --from=builder /app/docs /app/self-docs/docs

RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) kubectl_sha256="5de4e9f2266738fd112b721265a0c1cd7f4e5208b670f811861f699474a100a3" ;; \
        arm64) kubectl_sha256="d595d1a26b7444e0beb122e25750ee4524e74414bbde070b672b423139295ce6" ;; \
        *) echo "unsupported kubectl architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl --fail --location --silent --show-error \
        "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
        --output /usr/local/bin/kubectl; \
    echo "${kubectl_sha256}  /usr/local/bin/kubectl" | sha256sum --check --status; \
    chmod 0755 /usr/local/bin/kubectl; \
    chmod +x /app/agent-entrypoint.sh /app/sandbox-entrypoint.sh; \
    /usr/local/bin/kubectl version --client >/dev/null \
    && /usr/local/bin/do-agent --help >/dev/null \
    && buildctl --version

ENTRYPOINT ["/app/sandbox-entrypoint.sh"]
