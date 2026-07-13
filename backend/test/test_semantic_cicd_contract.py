import platform
import re
import subprocess
from pathlib import Path

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[1]
REPO_ROOT = ROOT.parent
WORKFLOW = ROOT / "deploy" / "workflows" / "oilan-agent-bootstrap.yaml"
REGISTRY = ROOT / "deploy" / "environments.yaml"
SKILL = ROOT / "agent" / "skills" / "semantic-deployment" / "SKILL.md"
ARCHITECTURE = ROOT / "docs" / "AGENT_NATIVE_CICD.md"
AGENT_DESIGN = ROOT / "agent" / "DESIGN.md"
SYSTEM_DESIGN = ROOT / "docs" / "design" / "AI_Ops_System_Design.md"
SKILL_GUIDE = ROOT / "docs" / "SKILL_INTEGRATION_GUIDE.md"
SEMANTIC_CICD = ROOT / "docs" / "SEMANTIC_CICD.md"
CLI_BIN = ROOT / "skills" / "doops-cli" / "bin"
GATEWAY_PROXY = ROOT / "gateway" / "nginx" / "doops-proxy.conf"
GATEWAY_DEPLOY = ROOT / "scripts" / "deploy-gateway-tls-proxy.sh"


def read_backend(path: str) -> str:
    return (ROOT / path).read_text(encoding="utf-8")


def read_repo(path: str) -> str:
    return (REPO_ROOT / path).read_text(encoding="utf-8")


def read_env(path: str) -> dict[str, str]:
    values = {}
    for line in read_backend(path).splitlines():
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


def test_oilan_workflow_declares_only_deployment_intent():
    workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))

    assert workflow["apiVersion"] == "doops.sh/v2"
    assert workflow["kind"] == "DeploymentTemplate"
    assert set(workflow["spec"]) == {
        "parameters",
        "application",
        "release",
        "environment",
        "configurationSource",
    }
    assert set(workflow["spec"]["parameters"]) == {"releaseId"}
    assert workflow["spec"]["application"] == "doops-agent"
    assert workflow["spec"]["environment"] == "oilan"


def test_oilan_registry_separates_target_executor_and_verification():
    registry = yaml.safe_load(REGISTRY.read_text(encoding="utf-8"))
    artifact = registry["artifactContract"]
    environment = registry["environments"]["oilan"]
    executor = environment["executor"]

    assert artifact["type"] == "image-set"
    assert artifact["imageReferenceFormat"] == "repository@digest"
    assert set(environment) == {"target", "executor", "verificationProfile"}
    assert set(environment["target"]) == {"name", "cluster", "instance"}
    assert executor["type"] == "helm"
    assert executor["config"]["imageBindings"] == {"doops-agent": "image"}
    assert "modelRouting" not in executor["config"]
    assert "modelSettings" not in executor["config"]
    assert environment["verificationProfile"] in registry["verificationProfiles"]


def test_semantic_deployment_contract_has_one_version_and_real_failure_evidence():
    text = SKILL.read_text(encoding="utf-8")

    assert "doops.sh/v2" in text
    assert "doops.sh/v3" not in text
    assert "requiredFailureEvidence" not in text
    assert "failureEvidence" in text
    assert "实际发生 mutation" in text
    assert "多 Agent" in text
    assert "Skill" in text
    assert "executionEvidence" in text
    assert "toolCallId" in text
    assert "toolDigest" in text
    assert "traceDigest" in text
    assert "buildctl" not in text
    assert "kubectl" not in text
    assert "last known good Helm" not in text


def test_agent_native_cicd_document_owns_framework_boundary():
    text = ARCHITECTURE.read_text(encoding="utf-8")

    assert "doops.sh/v2" in text
    assert "doops.sh/v3" not in text
    assert "doagent" in text
    assert "多 Agent" in text
    assert "Skill" in text
    assert "DoOps 不实现 Agent 框架" in text
    assert "permission.updated" in text
    assert "executionEvidence" in text
    assert "toolCallId" in text
    assert "toolDigest" in text
    assert "traceDigest" in text


def test_related_design_docs_use_the_same_agent_native_boundary():
    agent_design = AGENT_DESIGN.read_text(encoding="utf-8")
    system_design = SYSTEM_DESIGN.read_text(encoding="utf-8")
    skill_guide = SKILL_GUIDE.read_text(encoding="utf-8")
    semantic_cicd = SEMANTIC_CICD.read_text(encoding="utf-8")

    for text in (agent_design, system_design, skill_guide, semantic_cicd):
        assert "AGENT_NATIVE_CICD.md" in text
        assert "动态 Skill 组装引擎" not in text
        assert "AI 修复尝试" not in text

    assert "多 Agent" in agent_design
    assert "多 Agent" in system_design
    assert "固定命令序列" in skill_guide
    assert "buildctl --addr" not in skill_guide
    assert "kubectl rollout" not in skill_guide
    assert "executionEvidence" in semantic_cicd
    assert "toolCallId" in semantic_cicd
    assert "toolDigest" in semantic_cicd


def test_prebuilt_cli_accepts_current_deployment_template():
    system = platform.system().lower()
    machine = platform.machine().lower()
    architecture = {
        "x86_64": "amd64",
        "amd64": "amd64",
        "aarch64": "arm64",
        "arm64": "arm64",
    }.get(machine)
    if system not in {"darwin", "linux"} or architecture is None:
        pytest.skip(f"unsupported prebuilt CLI platform: {system}/{machine}")

    binary = CLI_BIN / f"doops-{system}-{architecture}"
    result = subprocess.run(
        [str(binary), "cicd", "lint", "-f", str(WORKFLOW)],
        check=False,
        capture_output=True,
        text=True,
    )

    assert result.returncode == 0, result.stdout + result.stderr
    assert "deployment template ok" in result.stdout


def test_legacy_bootstrap_and_manual_release_paths_are_removed():
    assert not (
        ROOT / "deploy" / "bootstrap" / "oilan-doops-agent-bootstrap.yaml"
    ).exists()
    assert not (ROOT / "deploy.sh").exists()

    deployment_doc = read_backend("deploy/docs/oilan-doops-agent.md")
    assert "doops cicd run" in deployment_doc
    for forbidden in (
        "kubectl create -f -",
        "kubectl apply -f -",
        "cicd submit",
        "DOOPS_CICD_PLAN_SIGNING_KEY",
        "Ed25519",
    ):
        assert forbidden not in deployment_doc


def test_environment_profile_owns_release_manifest_repository():
    registry = yaml.safe_load(REGISTRY.read_text(encoding="utf-8"))

    assert "manifestRepository" not in registry["artifactContract"]
    assert (
        registry["environments"]["oilan"]["executor"]["config"][
            "releaseManifestRepository"
        ]
        == "https://github.com/l8ai-cn/doops.git"
    )


def test_agent_images_bundle_the_versioned_helm_chart():
    for path in ("Dockerfile", "agent/Dockerfile", "agent/Dockerfile.sandbox"):
        assert "COPY --from=builder /app/deploy /app/deploy" in read_backend(path)


def test_gateway_tls_proxy_is_versioned_and_routes_all_gateway_protocols():
    text = GATEWAY_PROXY.read_text(encoding="utf-8")

    assert "server_name doops.l8ai.cn;" in text
    assert "listen 443 ssl;" in text
    assert "ssl_certificate /etc/nginx/certs/l8ai-wildcard.crt;" in text
    assert "proxy_pass http://127.0.0.1:42222;" in text
    assert "proxy_set_header Upgrade $http_upgrade;" in text
    assert "proxy_request_buffering off;" in text
    assert "client_max_body_size 2g;" in text
    assert "proxy_pass http://127.0.0.1:3000;" in text


def test_gateway_tls_deploy_validates_and_rolls_back_nginx_config():
    text = GATEWAY_DEPLOY.read_text(encoding="utf-8")

    assert "set -euo pipefail" in text
    assert 'backup="${config}.bak-' in text
    assert "nginx -t" in text
    assert "nginx -s reload" in text
    assert "restore_proxy_config" in text
    assert "https://doops.l8ai.cn/v1/targets" in text
    assert 'test "$code" = "401"' in text
    assert "--dry-run" in text


def test_sandbox_dockerfile_is_light_update_layer_with_runtime_gates():
    dockerfile = read_backend("agent/Dockerfile.sandbox")

    assert "COPY --from=builder /app/doops-agent /app/doops-agent" in dockerfile
    assert "/usr/local/bin/do-agent --help >/dev/null" in dockerfile
    assert "buildctl --version" in dockerfile
    assert 'ENTRYPOINT ["/app/sandbox-entrypoint.sh"]' in dockerfile


def test_sandbox_runtime_contract_has_no_legacy_agent_surface():
    combined = "\n".join(
        [
            read_backend("agent/Dockerfile.sandbox"),
            read_backend("agent/sandbox-entrypoint.sh"),
        ]
    )

    for forbidden in (
        "repo.zjcm.edu.cn",
        "repo.jm.aiedulab.cn",
        "api.example.com:8443",
        "/sse",
        "-password",
        "--password",
        "opencode",
        "OpenCode",
        "lab/webide",
    ):
        assert forbidden not in combined


def test_sandbox_entrypoint_starts_doagent_buildkit_and_gateway():
    entrypoint = read_backend("agent/sandbox-entrypoint.sh")

    assert 'DO_AGENT_PORT="${DO_AGENT_PORT:-9000}"' in entrypoint
    assert "DO_AGENT_MODEL_ROUTING_POLICY" in entrypoint
    assert "/app/configure_doagent_settings.py" in entrypoint
    assert "/usr/local/bin/do-agent acp-http --port" in entrypoint
    assert "buildkitd --containerd-worker=false" in entrypoint
    assert "tini -s -- /app/doops-agent" in entrypoint
    assert "https://api.example.com/v1" not in entrypoint


def test_agent_entrypoints_do_not_require_registry_auth_for_public_images():
    for path in ("agent/agent-entrypoint.sh", "agent/sandbox-entrypoint.sh"):
        entrypoint = read_backend(path)
        assert "DOOPS_REGISTRY_AUTH_FILE" not in entrypoint
        assert "registry auth config is required" not in entrypoint


def test_lightweight_base_contract_keeps_runtime_tools_without_webide_surface():
    dockerfile = read_backend("Dockerfile.base.light")
    baseline = read_env("runtime-versions.env")

    assert dockerfile.startswith(
        "ARG DO_AGENT_VERSION\n"
        "ARG DO_AGENT_IMAGE=invalid.invalid/doagent-image-must-be-specified\n"
    )
    assert re.fullmatch(r"\d+\.\d+\.\d+", baseline["DO_AGENT_VERSION"])
    assert baseline["DO_AGENT_IMAGE"].startswith(
        f"docker.cnb.cool/l8ai/ai/doagent:v{baseline['DO_AGENT_VERSION']}@sha256:"
    )
    assert re.fullmatch(
        r"sha256:[0-9a-f]{64}",
        baseline["DO_AGENT_IMAGE"].split("@", 1)[1],
    )
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
    assert "dl.k8s.io" not in dockerfile
    assert "get.helm.sh" not in dockerfile
    assert "helm.tar.gz" not in dockerfile
    assert "helm version --short" in dockerfile
    assert (
        "apt-get purge -y --auto-remove openssh-server openssh-client rsync sudo"
        in dockerfile
    )
    assert "apk del openssh-server openssh-client rsync sudo" in dockerfile
    assert (
        "rm -f /usr/local/bin/start-webide.sh /entrypoint.sh "
        "/usr/bin/entrypoint.sh /init.sh"
        in dockerfile
    )


def test_agent_update_dockerfiles_default_to_base_light_runtime():
    required_arg = (
        "ARG DOOPS_AGENT_BASE_IMAGE="
        "invalid.invalid/doops-agent-base-image-must-be-specified\n"
    )

    for path in ("Dockerfile", "agent/Dockerfile", "agent/Dockerfile.sandbox"):
        dockerfile = read_backend(path)
        assert dockerfile.startswith(required_arg)
        assert "base-light:latest" not in dockerfile
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
    assert "ACTUAL_BASE_IMAGE=" in push_block
    assert 'test "${ACTUAL_BASE_IMAGE}" = "${BASE_IMAGE}"' in push_block
    assert 'docker run --rm --entrypoint /app/doops-agent "${APP_IMAGE}" -help' in push_block
    assert '--entrypoint /usr/local/bin/buildctl "${APP_IMAGE}" --version' in push_block
    assert '--entrypoint /usr/bin/python3 "${APP_IMAGE}" -c \'import yaml\'' in push_block
    assert '--entrypoint /usr/local/bin/helm "${APP_IMAGE}" version --short' in push_block
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


def test_agent_bundles_and_syncs_semantic_deployment_skill():
    skill = read_backend("agent/skills/semantic-deployment/SKILL.md")
    system_prompt = read_backend("agent/skills/system_prompt.md")

    assert "name: semantic-deployment" in skill
    assert "DeploymentPlan" in skill
    assert "ReconciliationResult" in skill
    assert "requires: []" in skill
    assert "conflicts: []" in skill
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

    for path in ("Dockerfile", "agent/Dockerfile", "agent/Dockerfile.sandbox"):
        dockerfile = read_backend(path)
        assert "COPY --from=builder /app/agent/skills /app/skills" in dockerfile
        assert (
            "COPY --from=builder /app/agent/configure_doagent_settings.py "
            "/app/configure_doagent_settings.py"
            in dockerfile
        )

    for path in ("agent/agent-entrypoint.sh", "agent/sandbox-entrypoint.sh"):
        entrypoint = read_backend(path)
        assert "/app/configure_doagent_settings.py" in entrypoint
        assert "DO_AGENT_SETTINGS" in entrypoint
        assert "runtime-settings.json" in entrypoint
        assert "for d in /app/skills/*/; do" in entrypoint
        assert 'mkdir -p "/root/.agent/skills/$name"' in entrypoint
        assert 'cp -rf "$d"* "/root/.agent/skills/$name/"' in entrypoint
