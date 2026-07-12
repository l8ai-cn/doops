import subprocess
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "deploy" / "helm" / "doops-agent"
OILAN_VALUES = ROOT / "deploy" / "environments" / "oilan-values.yaml"
BOOTSTRAP_JOB = ROOT / "deploy" / "bootstrap" / "oilan-doops-agent-bootstrap.yaml"
WORKFLOW = ROOT / "ops" / "cicd" / "oilan-doops-agent-bootstrap.yaml"


def load_yaml(path: Path):
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def env_value(container, name: str):
    for item in container["env"]:
        if item["name"] == name:
            return item
    raise AssertionError(f"missing environment variable {name}")


def render_oilan_chart():
    output = subprocess.run(
        [
            "helm",
            "template",
            "doops-agent",
            str(CHART),
            "--namespace",
            "doops-system",
            "--values",
            str(OILAN_VALUES),
        ],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    return [item for item in yaml.safe_load_all(output) if item]


def test_oilan_agent_chart_is_helm_owned_and_uses_secret_refs():
    rendered = render_oilan_chart()
    assert len(rendered) == 1
    deployment = rendered[0]
    container = deployment["spec"]["template"]["spec"]["containers"][0]

    assert deployment["kind"] == "Deployment"
    assert deployment["metadata"]["namespace"] == "doops-system"
    assert deployment["metadata"]["annotations"]["meta.helm.sh/release-name"] == "doops-agent"
    assert deployment["metadata"]["annotations"]["meta.helm.sh/release-namespace"] == "doops-system"
    assert deployment["spec"]["strategy"]["type"] == "Recreate"
    assert deployment["spec"]["template"]["spec"]["nodeSelector"]["kubernetes.io/hostname"] == "gpu-ampere01"
    assert container["image"].endswith(":agent-runtime-20260712-01")
    assert "DOOPS_ALLOW_INSECURE_GATEWAY" not in {item["name"] for item in container["env"]}
    assert env_value(container, "DOOPS_GATEWAY_URL")["value"] == "https://doops.l8ai.cn"
    assert env_value(container, "DOOPS_GATEWAY_AGENT_TOKEN")["valueFrom"]["secretKeyRef"] == {
        "name": "doops-agent-runtime",
        "key": "agent-token",
    }
    assert "agent-token" not in container["command"][-1]
    assert "106.54.197.139" not in str(deployment)


def test_bootstrap_job_uses_candidate_image_and_runs_helm_after_adoption():
    values = load_yaml(OILAN_VALUES)
    job = load_yaml(BOOTSTRAP_JOB)
    candidate = f'{values["image"]["repository"]}:{values["image"]["tag"]}'

    assert job["kind"] == "Job"
    assert job["metadata"]["namespace"] == "doops-system"
    assert job["spec"]["template"]["spec"]["restartPolicy"] == "Never"
    assert job["spec"]["template"]["spec"]["imagePullSecrets"] == [{"name": "harbor-pull"}]
    assert {item["name"] for item in job["spec"]["template"]["spec"]["initContainers"]} == {
        "adopt-helm-label",
        "adopt-helm-release-name",
        "adopt-helm-release-namespace",
    }
    helm = job["spec"]["template"]["spec"]["containers"][0]
    assert helm["image"] == candidate
    assert helm["command"] == ["/usr/local/bin/helm"]
    assert helm["args"] == [
        "upgrade",
        "--install",
        "doops-agent",
        "/app/deploy/helm/doops-agent",
        "--namespace",
        "doops-system",
        "--values",
        "/app/deploy/environments/oilan-values.yaml",
        "--wait",
        "--timeout",
        "10m",
    ]


def test_bootstrap_workflow_is_agent_native_and_has_no_shell_deploy_stage():
    workflow = load_yaml(WORKFLOW)
    stages = workflow["spec"]["stages"]

    assert workflow["spec"]["policy"]["agentNative"] is True
    assert workflow["spec"]["environments"][0]["target"] == "gw-oilan-node"
    assert workflow["spec"]["environments"][0]["deploymentDoc"] == "backend/deploy/docs/oilan-doops-agent.md"
    assert all(stage["uses"] != "shell" for stage in stages)
    assert any(
        stage["uses"] == "agent.task"
        and stage["with"]["task"] == "run-versioned-build-tool"
        and stage["mutates"] is True
        and stage["confirm"] is True
        for stage in stages
    )
    assert any(
        stage["uses"] == "doops.k8s"
        and stage["with"]["operation"] == "apply-manifest"
        and stage["mutates"] is True
        and stage["confirm"] is True
        for stage in stages
    )


def test_agent_images_bundle_the_versioned_helm_chart():
    for path in ("Dockerfile", "agent/Dockerfile", "agent/Dockerfile.sandbox"):
        dockerfile = (ROOT / path).read_text(encoding="utf-8")
        assert "COPY --from=builder /app/deploy /app/deploy" in dockerfile
