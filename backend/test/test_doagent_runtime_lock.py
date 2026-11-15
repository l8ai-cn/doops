import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
WORKFLOWS = ROOT.parent / ".github" / "workflows"
RUNTIME = ROOT / "runtime-versions.env"
IMAGE_BUILD = ROOT / "scripts" / "build-agent-image.sh"
BASE_IMAGE_BUILD = ROOT / "scripts" / "build-agent-base-image.sh"
LIGHT_BASE = ROOT / "Dockerfile.base.light"
DOCKERFILES = (ROOT / "Dockerfile", LIGHT_BASE)

FAIL_CLOSED_INPUTS = (
    ': "${DOOPS_IMAGE_REGISTRY:?DOOPS_IMAGE_REGISTRY is required}"',
    ': "${DOOPS_IMAGE_REPOSITORY:?DOOPS_IMAGE_REPOSITORY is required}"',
    ': "${DOOPS_SOURCE_COMMIT:?DOOPS_SOURCE_COMMIT is required}"',
)


def test_doagent_runtime_lock_includes_exact_source_revision():
    runtime = dict(
        line.split("=", 1)
        for line in RUNTIME.read_text().splitlines()
        if line and not line.startswith("#")
    )
    revision = runtime["DO_AGENT_REVISION"]

    assert len(revision) == 40
    assert runtime["DO_AGENT_VERSION"] == "0.2.46"
    assert re.fullmatch(r"sha256:[0-9a-f]{64}", runtime["DO_AGENT_INDEX_DIGEST"])
    # Registry sources are explicit operator inputs, never pinned in the lock.
    assert "DO_AGENT_IMAGE" not in runtime
    assert "DOOPS_AGENT_BASE_IMAGE" not in runtime

    image_build = IMAGE_BUILD.read_text()
    for required in FAIL_CLOSED_INPUTS:
        assert required in image_build
    assert ': "${DO_AGENT_IMAGE:?DO_AGENT_IMAGE is required' in image_build
    assert '${DOOPS_AGENT_BASE_IMAGE:?DOOPS_AGENT_BASE_IMAGE is required' in image_build
    assert '--build-arg DO_AGENT_REVISION="${DO_AGENT_REVISION}"' in image_build
    assert '--build-arg DOOPS_RELEASE_TAG="${DOOPS_IMAGE_TAG}"' in image_build
    assert 'release=${DOOPS_IMAGE_TAG} commit=${DOOPS_SOURCE_COMMIT}' in image_build
    assert '[[ "${DOOPS_PUBLISH_LATEST:-}" == "1" && "${DOOPS_IMAGE_TAG}" != "latest" ]]' in image_build
    assert 'APP_IMAGE="${DOOPS_IMAGE_REGISTRY}/${DOOPS_IMAGE_REPOSITORY}:${DOOPS_IMAGE_TAG}"' in image_build
    assert 'latest_image="${DOOPS_IMAGE_REGISTRY}/${DOOPS_IMAGE_REPOSITORY}:latest"' in image_build
    assert 'docker push "${latest_image}"' in image_build
    assert 'test "${ACTUAL_DOAGENT_REVISION}" = "${DO_AGENT_REVISION}"' in image_build

    for dockerfile in DOCKERFILES:
        text = dockerfile.read_text()
        assert "ARG DO_AGENT_REVISION" in text
        assert 'LABEL org.l8ai.doagent.revision="${DO_AGENT_REVISION}"' in text
    server_dockerfile = (ROOT / "Dockerfile").read_text()
    assert "ARG DOOPS_RELEASE_TAG" in server_dockerfile
    assert "release.Version=${DOOPS_RELEASE_TAG}" in server_dockerfile


def test_agent_runtime_keeps_ssh_client_and_rejects_ssh_server():
    base = LIGHT_BASE.read_text()

    assert "openssh-client" in base
    assert "openssh-server openssh-client" not in base
    assert "ssh -V" in base
    assert "! command -v sshd" in base

    for dockerfile in DOCKERFILES:
        text = dockerfile.read_text()
        assert "ssh -V" in text
        assert "! command -v sshd" in text

    image_build = IMAGE_BUILD.read_text()
    assert "--entrypoint /usr/bin/ssh" in image_build
    assert "ssh client" in image_build
    assert "command -v sshd" in image_build
    assert "DOOPS_AGENT_BASE_IMAGE_OVERRIDE" in image_build

    base_image_build = BASE_IMAGE_BUILD.read_text()
    assert "--entrypoint /usr/bin/ssh" in base_image_build
    assert "command -v sshd" in base_image_build
    assert "DOOPS_BASE_IMAGE_OUTPUT" in base_image_build

    # Production images are built by the operator with explicit inputs; CI only
    # tests the repository and publishes the CLI/Gateway/Skill release assets.
    for workflow in sorted(WORKFLOWS.glob("*.yml")):
        text = workflow.read_text(encoding="utf-8")
        assert "resolve-agent-base-image.sh" not in text, workflow
        assert "build-agent-image.sh" not in text, workflow
        assert "build-agent-base-image.sh" not in text, workflow

    # The base is rebuilt only when its inputs change, so unchanged base layers
    # stay cached on every target node across releases.
    resolve = (ROOT / "scripts" / "resolve-agent-base-image.sh").read_text()
    assert "Dockerfile.base.light" in resolve
    assert "runtime-versions.env" in resolve
    assert "grep -v '^DOOPS_AGENT_BASE_IMAGE='" in resolve
    assert 'BASE_TAG="inputs-${INPUT_HASH}"' in resolve
    assert "scripts/build-agent-base-image.sh" in resolve
    assert "@sha256:[0-9a-f]{64}$" in resolve


def test_only_init_reaps_orphan_processes():
    for name in ("agent-entrypoint.sh", "sandbox-entrypoint.sh"):
        entrypoint = (ROOT / "agent" / name).read_text()
        assert 'tini -s -- /app/doops-agent "$@"' in entrypoint
        assert "tini is required for process cleanup" in entrypoint
        assert "falling back to direct" not in entrypoint
