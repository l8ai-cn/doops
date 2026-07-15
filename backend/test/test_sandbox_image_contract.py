import re
import signal
import socket
import subprocess
import time
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
REPO_ROOT = ROOT.parent


def read(path: str) -> str:
    return (ROOT / path).read_text(encoding="utf-8")


def read_repo(path: str) -> str:
    return (REPO_ROOT / path).read_text(encoding="utf-8")


def read_env(path: str) -> dict[str, str]:
    values = {}
    for line in read(path).splitlines():
        key, value = line.split("=", 1)
        values[key] = value
    return values


def top_level_block(text: str, key: str) -> str:
    start = text.index(f"{key}:\n")
    end = len(text)
    for marker in ("\nmain:\n", "\nmaster:\n", "\n$:\n"):
        pos = text.find(marker, start + len(key) + 2)
        if pos != -1:
            end = min(end, pos + 1)
    return text[start:end]


def shell_function(text: str, name: str) -> str:
    match = re.search(
        rf"^{re.escape(name)}\(\) \{{\n.*?^\}}\n",
        text,
        flags=re.MULTILINE | re.DOTALL,
    )
    assert match is not None, f"missing shell function: {name}"
    return match.group(0)


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
    assert "DO_AGENT_MODEL_ROUTING_POLICY" in entrypoint
    assert "/app/configure_doagent_settings.py" in entrypoint
    assert "/usr/local/bin/do-agent acp-http --port" in entrypoint
    assert "buildkitd --containerd-worker=false" in entrypoint
    assert "tini -s -- /app/doops-agent" in entrypoint
    assert "start_background /usr/local/bin/entrypoint.sh" not in entrypoint
    assert "elif [ -x /usr/local/bin/entrypoint.sh ]" not in entrypoint
    assert "https://api.example.com/v1" not in entrypoint


def test_sandbox_entrypoint_waits_for_ports_and_fails_closed():
    entrypoint = read("agent/sandbox-entrypoint.sh")

    assert 'DO_AGENT_PORT="${DO_AGENT_PORT:-9000}"' in entrypoint
    assert 'DOOPS_LISTEN="$(flag_value -listen 0.0.0.0 "$@")"' in entrypoint
    assert 'DOOPS_PORT="$(flag_value -port 42222 "$@")"' in entrypoint
    wait_call = (
        'wait_for_tcp_ports_free 120 "0.0.0.0:${DO_AGENT_PORT}" '
        '"${DOOPS_LISTEN}:${DOOPS_PORT}"'
    )
    assert wait_call in entrypoint
    assert entrypoint.count("wait_for_tcp_ports_free ") == 1
    assert entrypoint.index(
        wait_call
    ) < entrypoint.index("\nstart_doagent\n")
    assert "doagent failed to start, doops_agent_prompt will not work" not in entrypoint
    assert "doagent exited before becoming healthy" in entrypoint


def test_port_waiter_rejects_an_occupied_port_and_accepts_a_free_port():
    entrypoint = read("agent/sandbox-entrypoint.sh")
    functions = "\n".join(
        [
            shell_function(entrypoint, "tcp_ports_available"),
            shell_function(entrypoint, "wait_for_tcp_ports_free"),
        ]
    )

    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    port = listener.getsockname()[1]

    occupied = subprocess.run(
        ["bash"],
        input=f"{functions}\nwait_for_tcp_ports_free 1 127.0.0.1:{port}\n",
        text=True,
        capture_output=True,
        check=False,
    )
    listener.close()

    assert occupied.returncode != 0
    assert "still in use" in occupied.stderr

    free = subprocess.run(
        ["bash"],
        input=f"{functions}\nwait_for_tcp_ports_free 1 127.0.0.1:{port}\n",
        text=True,
        capture_output=True,
        check=False,
    )

    assert free.returncode == 0, free.stderr


def test_port_waiter_is_interruptible_during_port_handoff():
    entrypoint = read("agent/sandbox-entrypoint.sh")
    preamble = entrypoint.split("start_sandbox_services() {", 1)[0]

    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    port = listener.getsockname()[1]
    process = subprocess.Popen(
        ["bash"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    assert process.stdin is not None
    process.stdin.write(
        f"{preamble}\n"
        f"wait_for_tcp_ports_free 120 127.0.0.1:{port}\n"
    )
    process.stdin.close()

    time.sleep(0.5)
    started = time.monotonic()
    process.send_signal(signal.SIGTERM)
    process.wait(timeout=3)
    elapsed = time.monotonic() - started
    listener.close()

    assert process.returncode == 143
    assert elapsed < 1.5


def test_port_waiter_rejects_an_invalid_endpoint_without_waiting():
    entrypoint = read("agent/sandbox-entrypoint.sh")
    functions = "\n".join(
        [
            shell_function(entrypoint, "tcp_ports_available"),
            shell_function(entrypoint, "wait_for_tcp_ports_free"),
        ]
    )

    invalid = subprocess.run(
        ["bash"],
        input=f"{functions}\nwait_for_tcp_ports_free 120 127.0.0.1:not-a-port\n",
        text=True,
        capture_output=True,
        check=False,
        timeout=3,
    )

    assert invalid.returncode == 2
    assert "invalid TCP endpoint" in invalid.stderr


def test_agent_deployment_has_a_startup_probe_for_host_port_handoff():
    deployment = read("deploy/helm/doops-agent/templates/deployment.yaml")

    assert "startupProbe:" in deployment
    startup_probe = deployment.split("startupProbe:", 1)[1].split(
        "readinessProbe:", 1
    )[0]
    assert "exec:" in startup_probe
    assert "pgrep -fc" in startup_probe
    assert "curl -fsS --max-time 1 http://127.0.0.1:42222/health" in startup_probe
    assert "failureThreshold: 40" in startup_probe
    assert "periodSeconds: 5" in startup_probe
    assert "tcpSocket:" not in deployment
    assert deployment.count("pgrep -fc") == 3
    assert deployment.count(
        "curl -fsS --max-time 1 http://127.0.0.1:42222/health"
    ) == 3


def test_agent_entrypoints_do_not_require_registry_auth_for_public_images():
    for path in ("agent/agent-entrypoint.sh", "agent/sandbox-entrypoint.sh"):
        entrypoint = read(path)

        assert "DOOPS_REGISTRY_AUTH_FILE" not in entrypoint
        assert "registry auth config is required" not in entrypoint


def test_lightweight_base_contract_keeps_runtime_tools_without_webide_surface():
    dockerfile = read("Dockerfile.base.light")
    baseline = read_env("runtime-versions.env")

    assert dockerfile.startswith(
        "ARG DO_AGENT_VERSION\n"
        "ARG DO_AGENT_IMAGE=invalid.invalid/doagent-image-must-be-specified\n"
    )
    assert re.fullmatch(r"\d+\.\d+\.\d+", baseline["DO_AGENT_VERSION"])
    image_name, image_digest = baseline["DO_AGENT_IMAGE"].rsplit("@", 1)
    assert image_name
    assert re.fullmatch(r"sha256:[0-9a-f]{64}", image_digest)
    assert (
        'test "$(/usr/local/bin/do-agent --version)" = "do-agent ${DO_AGENT_VERSION}"'
        in dockerfile
    )
    assert "kubectl version --client=true" in dockerfile
    assert "buildctl --version" in dockerfile
    assert re.search(
        r"ARG BUILDKIT_IMAGE=moby/buildkit:v0\.21\.1@sha256:[0-9a-f]{64}",
        dockerfile,
    )
    assert "FROM ${BUILDKIT_IMAGE} AS buildkit" in dockerfile
    assert "COPY --from=buildkit /usr/bin/buildctl /usr/local/bin/buildctl" in dockerfile
    assert "COPY --from=buildkit /usr/bin/buildkit* /usr/local/bin/" in dockerfile
    assert re.search(
        r"ARG KUBECTL_IMAGE=registry\.k8s\.io/kubectl:v1\.33\.1@sha256:[0-9a-f]{64}",
        dockerfile,
    )
    assert "FROM ${KUBECTL_IMAGE} AS kubectl" in dockerfile
    assert "COPY --from=kubectl /bin/kubectl /usr/local/bin/kubectl" in dockerfile
    assert 'LABEL org.l8ai.kubectl.image="${KUBECTL_IMAGE}"' in dockerfile
    assert "ghproxy.net" not in dockerfile
    assert "| tar -xzC /usr/local" not in dockerfile
    assert "python3 python3-yaml" in dockerfile
    assert "python3 py3-yaml" in dockerfile
    assert "python3 -c 'import yaml'" in dockerfile
    assert re.search(
        r"ARG HELM_IMAGE=alpine/helm:3\.14\.4@sha256:[0-9a-f]{64}",
        dockerfile,
    )
    assert "FROM ${HELM_IMAGE} AS helm" in dockerfile
    assert "COPY --from=helm /usr/bin/helm /usr/local/bin/helm" in dockerfile
    assert 'LABEL org.l8ai.helm.image="${HELM_IMAGE}"' in dockerfile
    assert (
        "FROM ${DO_AGENT_IMAGE}\n\n"
        "ARG DO_AGENT_VERSION\n"
        "ARG BUILDKIT_IMAGE\n"
        "ARG HELM_IMAGE\n"
        "ARG KUBECTL_IMAGE\n"
        "\n"
        "USER root"
    ) in dockerfile
    assert "dl.k8s.io" not in dockerfile
    assert "get.helm.sh" not in dockerfile
    assert "helm.tar.gz" not in dockerfile
    assert "helm version --short" in dockerfile
    assert "&& { apt-get purge -y --auto-remove openssh-server openssh-client rsync sudo || true; }" in dockerfile
    assert "&& { apk del openssh-server openssh-client rsync sudo 2>/dev/null || true; }" in dockerfile
    assert "rm -f /usr/local/bin/start-webide.sh /entrypoint.sh /usr/bin/entrypoint.sh /init.sh 2>/dev/null || true" not in dockerfile
    assert "apt-get purge -y --auto-remove openssh-server openssh-client rsync sudo" in dockerfile
    assert "rm -f /usr/local/bin/start-webide.sh /entrypoint.sh /usr/bin/entrypoint.sh /init.sh" in dockerfile


def test_agent_update_dockerfiles_default_to_base_light_runtime():
    required_arg = (
        "ARG DOOPS_AGENT_BASE_IMAGE="
        "invalid.invalid/doops-agent-base-image-must-be-specified\n"
    )
    old_heavy_base = "ARG DOOPS_AGENT_BASE_IMAGE=docker.cnb.cool/l8ai/ai/doops.sh/base:v1"

    for path in ("Dockerfile", "agent/Dockerfile", "agent/Dockerfile.sandbox"):
        dockerfile = read(path)

        assert dockerfile.startswith(required_arg)
        assert "base-light:latest" not in dockerfile
        assert old_heavy_base not in dockerfile
        assert "ARG DO_AGENT_VERSION\n" in dockerfile
        assert 'LABEL org.opencontainers.image.base.name="${DOOPS_AGENT_BASE_IMAGE}"' in dockerfile
        assert (
            'test "$(/usr/local/bin/do-agent --version)" = "do-agent ${DO_AGENT_VERSION}"'
            in dockerfile
        )
        assert "/app/doops-agent -help >/dev/null" in dockerfile
        assert "buildctl --version" in dockerfile
        assert "python3 -c 'import yaml'" in dockerfile
        assert "helm version --short" in dockerfile


def test_cnb_ci_runs_release_contract_tests_on_pr_and_push():
    cnb = read_repo(".cnb.yml")
    contract_cmd = "python3 -m pytest backend/test -q"

    for branch in ("main", "master"):
        block = top_level_block(cnb, branch)
        pr_block, push_block = block.split("  push:", 1)

        assert contract_cmd in pr_block
        assert contract_cmd in push_block
        assert "py3-pytest" in pr_block
        assert "py3-pytest" in push_block
        assert "py3-yaml" in pr_block
        assert "py3-yaml" in push_block
        assert " helm" in pr_block
        assert " helm" in push_block


def test_cnb_main_builds_full_revision_tagged_agent_images_after_checks():
    cnb = read_repo(".cnb.yml")
    main = top_level_block(cnb, "main")
    _, push_block = main.split("  push:", 1)

    assert "services:\n        - docker" in push_block
    assert "docker-cli" in push_block
    assert "set -euo pipefail" in push_block
    assert 'RELEASE_TAG="${CNB_COMMIT}"' in push_block
    assert "CNB_COMMIT_SHORT" not in push_block
    assert 'date -u +%Y%m%d' not in push_block
    assert "source backend/runtime-versions.env" in push_block
    assert "backend/Dockerfile.base.light" in push_block
    assert "backend/Dockerfile" in push_block
    assert push_block.count("--platform linux/amd64") == 2
    assert '--build-arg DO_AGENT_IMAGE="${DO_AGENT_IMAGE}"' in push_block
    assert '--build-arg DO_AGENT_VERSION="${DO_AGENT_VERSION}"' in push_block
    assert '--build-arg DOOPS_AGENT_BASE_IMAGE="${BASE_IMAGE}"' in push_block
    assert push_block.count("ACTUAL_DOAGENT_VERSION=") == 2
    assert (
        push_block.count('test "${ACTUAL_DOAGENT_VERSION}" = "${DO_AGENT_VERSION}"')
        == 2
    )
    base_build, app_build = push_block.split('docker push "${BASE_IMAGE}"', 1)
    assert "ACTUAL_DOAGENT_VERSION=" in base_build
    assert 'test "${ACTUAL_DOAGENT_VERSION}" = "${DO_AGENT_VERSION}"' in base_build
    assert "ACTUAL_BASE_IMAGE=" in app_build
    assert 'test "${ACTUAL_BASE_IMAGE}" = "${BASE_IMAGE}"' in app_build
    assert 'docker run --rm --entrypoint /app/doops-agent "${APP_IMAGE}" -help' in app_build
    assert "ACTUAL_DOAGENT_VERSION=" in app_build
    assert 'test "${ACTUAL_DOAGENT_VERSION}" = "${DO_AGENT_VERSION}"' in app_build
    assert '--entrypoint /usr/local/bin/buildctl "${APP_IMAGE}" --version' in app_build
    assert '--entrypoint /usr/bin/python3 "${APP_IMAGE}" -c \'import yaml\'' in app_build
    assert '--entrypoint /usr/local/bin/helm "${APP_IMAGE}" version --short' in app_build
    assert "docker push" in push_block
    for forbidden in ("kubectl ", "helm upgrade", "doops -session", "cicd submit"):
        assert forbidden not in push_block


def test_root_readme_matches_cnb_build_responsibility():
    readme = read_repo("README.md")

    assert "no CI/CD" not in readme
    assert "CNB is a source mirror only" not in readme
    assert "CNB CI builds, verifies, and publishes immutable agent images" in readme


def test_doops_cicd_is_the_only_agent_release_definition():
    cnb = read_repo(".cnb.yml")
    workflow = read_repo("backend/deploy/workflows/oilan-agent-bootstrap.yaml")

    assert "tag_push:" not in cnb
    assert "release-image" not in cnb
    assert "apiVersion: doops.sh/v2" in workflow
    assert "kind: DeploymentTemplate" in workflow
    assert "stages:" not in workflow
    assert "requiredCommand:" not in workflow
    assert "cicd submit" not in workflow


def test_agent_bundles_a_semantic_deployment_skill():
    skill = read("agent/skills/semantic-deployment/SKILL.md")
    system_prompt = read("agent/skills/system_prompt.md")

    assert "name: semantic-deployment" in skill
    assert "DeploymentPlan" in skill
    assert "ReconciliationResult" in skill
    assert "requires: []" in skill
    assert "conflicts: [pipeline, image-build, k8s, docker, shell]" in skill
    assert "semantic-deployment" in system_prompt
    for forbidden in (
        "cicd submit",
        "CNB",
        "stages:",
        "uses: shell",
        "requiredCommand",
        "verificationCommand",
        "buildctl ",
        "helm ",
        "kubectl ",
    ):
        assert forbidden not in skill


def test_agent_images_sync_semantic_skills_into_doagent_discovery_path():
    for path in ("Dockerfile", "agent/Dockerfile", "agent/Dockerfile.sandbox"):
        dockerfile = read(path)
        assert "COPY --from=builder /app/agent/skills /app/skills" in dockerfile
        assert "COPY --from=builder /app/agent/configure_doagent_settings.py /app/configure_doagent_settings.py" in dockerfile

    for path in ("agent/agent-entrypoint.sh", "agent/sandbox-entrypoint.sh"):
        entrypoint = read(path)
        assert "/app/configure_doagent_settings.py" in entrypoint
        assert "DO_AGENT_SETTINGS" in entrypoint
        assert "runtime-settings.json" in entrypoint
        assert "for d in /app/skills/*/; do" in entrypoint
        assert entrypoint.count('destination="/root/.agent/skills/$name"') == 2
        assert entrypoint.count('rm -rf "$destination"') == 2
        assert entrypoint.count('mkdir -p "$destination"') == 2
        assert entrypoint.count('cp -a "$d." "$destination/"') == 2
        assert 'cp -rf "$d"* "/root/.agent/skills/$name/"' not in entrypoint


def test_legacy_manual_agent_deploy_script_is_removed():
    assert not (ROOT / "deploy.sh").exists()
