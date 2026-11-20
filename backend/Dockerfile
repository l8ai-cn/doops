ARG DOOPS_AGENT_BASE_IMAGE=invalid.invalid/doops-agent-base-image-must-be-specified
ARG DO_AGENT_IMAGE=invalid.invalid/doagent-image-must-be-specified
ARG DO_AGENT_VERSION
ARG DO_AGENT_REVISION
ARG DO_AGENT_INDEX_DIGEST
ARG DO_AGENT_PLATFORM
ARG DO_AGENT_PLATFORM_DIGEST
ARG DOOPS_RELEASE_TAG=dev
ARG DOOPS_SOURCE_COMMIT=missing-source-commit

# Stage 1: Build the Agent. The Gateway runs on its own host and ships as a
# release asset, so it is not part of the image every target pulls.
FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS builder
ARG DOOPS_RELEASE_TAG
ARG DOOPS_SOURCE_COMMIT
USER root
WORKDIR /app
COPY . .
WORKDIR /app/agent
RUN CGO_ENABLED=0 go build \
    -ldflags "-X github.com/user/doops/agent/pkg/release.Version=${DOOPS_RELEASE_TAG} -X github.com/user/doops/agent/pkg/release.Commit=${DOOPS_SOURCE_COMMIT}" \
    -o /app/doops-agent ./cmd/agent \
    && CGO_ENABLED=0 go build -o /app/doops-shell ./cmd/doops-shell

FROM ${DO_AGENT_IMAGE} AS doagent-runtime

# Stage 2: Lightweight app image.
# The base image owns sandbox/doagent/buildkit/system packages. This layer only
# changes when doops code, skills, docs, or entrypoints change.
FROM ${DOOPS_AGENT_BASE_IMAGE}
ARG DOOPS_AGENT_BASE_IMAGE
ARG DO_AGENT_IMAGE
ARG DO_AGENT_VERSION
ARG DO_AGENT_REVISION
ARG DO_AGENT_INDEX_DIGEST
ARG DO_AGENT_PLATFORM
ARG DO_AGENT_PLATFORM_DIGEST
ARG DOOPS_RELEASE_TAG
ARG DOOPS_SOURCE_COMMIT
LABEL org.opencontainers.image.base.name="${DOOPS_AGENT_BASE_IMAGE}"
LABEL org.l8ai.doagent.image="${DO_AGENT_IMAGE}"
LABEL org.l8ai.doagent.version="${DO_AGENT_VERSION}"
LABEL org.l8ai.doagent.revision="${DO_AGENT_REVISION}"
LABEL org.l8ai.doagent.index.digest="${DO_AGENT_INDEX_DIGEST}"
LABEL org.l8ai.doagent.platform="${DO_AGENT_PLATFORM}"
LABEL org.l8ai.doagent.platform.digest="${DO_AGENT_PLATFORM_DIGEST}"

COPY --from=builder /app/doops-agent /app/doops-agent
COPY --from=builder /app/doops-shell /app/doops-shell
COPY --from=doagent-runtime /usr/local/bin/do-agent /usr/local/bin/do-agent
COPY --from=builder /app/agent/skills /app/skills
COPY --from=builder /app/agent/agent-entrypoint.sh /app/agent-entrypoint.sh
COPY --from=builder /app/agent/sandbox-entrypoint.sh /app/sandbox-entrypoint.sh
COPY --from=builder /app/agent/configure_kubeconfig.py /app/configure_kubeconfig.py
COPY --from=builder /app/agent/adopt_agent.py /app/adopt_agent.py
COPY --from=builder /app/deploy /app/deploy
COPY --from=builder /app/README.md /app/self-docs/README.md
COPY --from=builder /app/docs /app/self-docs/docs

RUN chmod +x /app/agent-entrypoint.sh /app/sandbox-entrypoint.sh \
    && /app/doops-shell -c 'test -n "$BASH_VERSION"' \
    && /app/doops-shell --argv kubectl version --client=true \
    && /app/skills/k8s/scripts/workload-status --help >/dev/null \
    && /app/skills/k8s/scripts/release-status --help >/dev/null \
    && /app/doops-agent -help >/dev/null \
    && test "$(/app/doops-agent -version)" = "release=${DOOPS_RELEASE_TAG} commit=${DOOPS_SOURCE_COMMIT}" \
    && ! test -e /app/doops-gateway \
    && test "$(/usr/local/bin/do-agent --version)" = "do-agent ${DO_AGENT_VERSION}" \
    && /usr/local/bin/do-agent --help >/dev/null \
    && buildctl --version \
    && python3 -c 'import yaml' \
    && node --check /app/skills/doweb/scripts/doweb.mjs \
    && helm version --short \
    && ssh -V \
    && ! command -v sshd >/dev/null 2>&1

ENTRYPOINT ["/app/sandbox-entrypoint.sh"]
