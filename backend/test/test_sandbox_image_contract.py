from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
REPO_ROOT = ROOT.parent


def read(path: str) -> str:
    return (ROOT / path).read_text(encoding="utf-8")


def read_repo(path: str) -> str:
    return (REPO_ROOT / path).read_text(encoding="utf-8")


def top_level_block(text: str, key: str) -> str:
    start = text.index(f"{key}:\n")
    end = len(text)
    for marker in ("\nmain:\n", "\nmaster:\n", "\n$:\n"):
        pos = text.find(marker, start + len(key) + 2)
        if pos != -1:
            end = min(end, pos + 1)
    return text[start:end]


def require_ordered(text: str, *needles: str) -> None:
    previous = -1
    for needle in needles:
        current = text.index(needle)
        assert current > previous
        previous = current


def test_sandbox_dockerfile_is_light_update_layer_with_runtime_gates():
    dockerfile = read("agent/Dockerfile.sandbox")

    assert "COPY --from=builder /app/doops-agent /app/doops-agent" in dockerfile
    assert "/usr/local/bin/do-agent --help >/dev/null" in dockerfile
    assert "buildctl --version" in dockerfile
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


def test_agent_registry_auth_uses_one_mounted_multi_registry_config():
    for path in ("agent/agent.yaml", "agent/agent-default.yaml"):
        manifest = read(path)

        assert "doops-registry-auth" in manifest
        assert "mountPath: /root/.docker" in manifest
        assert "key: config.json" in manifest
        assert "REGISTRY_URL" not in manifest
        assert "REGISTRY_USER" not in manifest
        assert "REGISTRY_PASS" not in manifest


def test_agent_entrypoints_require_mounted_registry_auth_config():
    for path in ("agent/agent-entrypoint.sh", "agent/sandbox-entrypoint.sh"):
        entrypoint = read(path)

        assert "DOOPS_REGISTRY_AUTH_FILE" in entrypoint
        assert "registry auth config is required" in entrypoint
        assert "REGISTRY_URL" not in entrypoint
        assert "REGISTRY_USER" not in entrypoint
        assert "REGISTRY_PASS" not in entrypoint


def test_lightweight_base_contract_keeps_runtime_tools_without_webide_surface():
    dockerfile = read("Dockerfile.base.light")

    assert "docker.cnb.cool/l8ai/ai/doops.sh:v1.1-metadata-20260704" in dockerfile
    assert "kubectl version --client=true" in dockerfile
    assert "buildctl --version" in dockerfile
    assert "python3 python3-yaml" in dockerfile
    assert "python3 py3-yaml" in dockerfile
    assert "python3 -c 'import yaml'" in dockerfile
    assert "ARG HELM_VERSION=v3.14.4" in dockerfile
    assert "FROM ${DO_AGENT_IMAGE}\n\nARG KUBECTL_VERSION\nARG HELM_VERSION\n\nUSER root" in dockerfile
    assert "helm-${HELM_VERSION}-linux-${ARCH}.tar.gz" in dockerfile
    assert "helm version --short" in dockerfile
    assert "&& { apt-get purge -y --auto-remove openssh-server openssh-client rsync sudo || true; }" in dockerfile
    assert "&& { apk del openssh-server openssh-client rsync sudo 2>/dev/null || true; }" in dockerfile
    assert "rm -f /usr/local/bin/start-webide.sh /entrypoint.sh /usr/bin/entrypoint.sh /init.sh 2>/dev/null || true" not in dockerfile
    assert "apt-get purge -y --auto-remove openssh-server openssh-client rsync sudo" in dockerfile
    assert "rm -f /usr/local/bin/start-webide.sh /entrypoint.sh /usr/bin/entrypoint.sh /init.sh" in dockerfile


def test_agent_update_dockerfiles_default_to_base_light_runtime():
    expected_arg = "ARG DOOPS_AGENT_BASE_IMAGE=docker.cnb.cool/l8ai/ai/doops.sh/base-light:latest"
    old_heavy_base = "ARG DOOPS_AGENT_BASE_IMAGE=docker.cnb.cool/l8ai/ai/doops.sh/base:v1"

    for path in ("Dockerfile", "agent/Dockerfile", "agent/Dockerfile.sandbox"):
        dockerfile = read(path)

        assert expected_arg in dockerfile
        assert old_heavy_base not in dockerfile
        assert 'LABEL org.opencontainers.image.base.name="${DOOPS_AGENT_BASE_IMAGE}"' in dockerfile
        assert "/usr/local/bin/do-agent --help >/dev/null" in dockerfile
        assert "buildctl --version" in dockerfile


def test_cnb_ci_runs_release_contract_tests_on_pr_and_push():
    cnb = read_repo(".cnb.yml")
    contract_cmd = "python3 -m pytest backend/test/test_sandbox_image_contract.py -q"

    for branch in ("main", "master"):
        block = top_level_block(cnb, branch)
        pr_block, push_block = block.split("  push:", 1)

        assert contract_cmd in pr_block
        assert contract_cmd in push_block
        assert "py3-pytest" in pr_block
        assert "py3-pytest" in push_block


def test_cnb_release_validates_app_image_base_and_runtime_before_push():
    cnb = read_repo(".cnb.yml")

    require_ordered(
        cnb,
        "name: build app image",
        "name: validate release app image",
        "name: push app image",
    )
    assert ': "${DOOPS_AGENT_BASE_IMAGE:?release base image was not exported}"' in cnb
    assert "org.opencontainers.image.base.name" in cnb
    assert 'test "${ACTUAL_BASE}" = "${DOOPS_AGENT_BASE_IMAGE}"' in cnb
    assert 'docker run --rm --entrypoint /app/doops-agent "${APP_IMAGE}" -help >/dev/null' in cnb
    assert 'docker run --rm --entrypoint /usr/local/bin/do-agent "${APP_IMAGE}" --help >/dev/null' in cnb
    assert 'docker run --rm --entrypoint /bin/sh "${APP_IMAGE}" -lc "buildctl --version"' in cnb


def test_deploy_script_rolls_out_the_image_it_builds():
    script = read("deploy.sh")

    require_ordered(
        script,
        "step 3 \"远端 BuildKit 构建并推送镜像\"",
        "kubectl -n ${NAMESPACE} set image ${DEPLOY_NAME} doops-agent=${IMAGE}",
        "kubectl rollout status ${DEPLOY_NAME} -n ${NAMESPACE}",
    )
