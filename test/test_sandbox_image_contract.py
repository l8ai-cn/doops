from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def read(path: str) -> str:
    return (ROOT / path).read_text(encoding="utf-8")


def test_sandbox_dockerfile_uses_agent_infra_sandbox_and_composes_doops_doagent():
    dockerfile = read("agent/Dockerfile.sandbox")

    assert "ghcr.io/agent-infra/sandbox:latest" in dockerfile
    assert "registry.example.com/lab/doops-agent:sandbox-ghcr-20260509" in dockerfile
    assert "COPY --from=builder /app/doops-agent /app/doops-agent" in dockerfile
    assert "COPY --from=doagent /usr/local/bin/do-agent /usr/local/bin/do-agent" in dockerfile
    assert "RUN /usr/local/bin/do-agent --help >/dev/null" in dockerfile
    assert 'ENTRYPOINT ["/app/sandbox-entrypoint.sh"]' in dockerfile


def test_sandbox_runtime_contract_has_no_legacy_agent_surface():
    combined = "\n".join(
        [
            read("agent/Dockerfile.sandbox"),
            read("agent/sandbox-entrypoint.sh"),
        ]
    )

    forbidden = [
        "repo.zjcm.edu.cn",
        "repo.jm.aiedulab.cn",
        "api.example.com:8443",
        "/sse",
        "-password",
        "--password",
        "opencode",
        "OpenCode",
        "lab/webide",
    ]
    for needle in forbidden:
        assert needle not in combined


def test_sandbox_entrypoint_starts_doagent_buildkit_and_gateway():
    entrypoint = read("agent/sandbox-entrypoint.sh")

    assert "DO_AGENT_PORT=\"${DO_AGENT_PORT:-9000}\"" in entrypoint
    assert "/usr/local/bin/do-agent acp-http --port" in entrypoint
    assert "buildkitd --containerd-worker=false" in entrypoint
    assert "tini -s -- /app/doops-agent" in entrypoint
    assert "https://api.example.com/v1" in entrypoint


def test_lightweight_base_contract_keeps_runtime_tools_without_webide_surface():
    dockerfile = read("Dockerfile.base.light")

    assert "docker.cnb.cool/l8ai/ai/doops.sh:v1.0.4" in dockerfile
    assert "kubectl version --client=true" in dockerfile
    assert "buildctl --version" in dockerfile
    assert "apt-get purge -y --auto-remove openssh-server openssh-client rsync sudo" in dockerfile
    assert "rm -f /usr/local/bin/start-webide.sh /entrypoint.sh /usr/bin/entrypoint.sh /init.sh" in dockerfile
