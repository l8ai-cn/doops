import subprocess
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "deploy" / "helm" / "doops-agent"
OILAN_VALUES = ROOT / "deploy" / "environments" / "oilan-values.yaml"
BOOTSTRAP_JOB = ROOT / "deploy" / "bootstrap" / "oilan-doops-agent-bootstrap.yaml"
WORKFLOW = ROOT / "deploy" / "workflows" / "oilan-agent-bootstrap.yaml"
REGISTRY = ROOT / "deploy" / "environments.yaml"
RELEASE_ID = "a" * 40


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
            "--set-string",
            f"image.tag={RELEASE_ID}",
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
    assert deployment["spec"]["template"]["spec"]["imagePullSecrets"] == [
        {"name": "doops-registry-pull"}
    ]
    assert container["image"].endswith(f":{RELEASE_ID}")
    assert "DOOPS_ALLOW_INSECURE_GATEWAY" not in {item["name"] for item in container["env"]}
    assert env_value(container, "DOOPS_GATEWAY_URL")["value"] == "https://doops.l8ai.cn"
    assert env_value(container, "DOOPS_GATEWAY_AGENT_TOKEN")["valueFrom"]["secretKeyRef"] == {
        "name": "doops-agent-runtime",
        "key": "agent-token",
    }
    assert "agent-token" not in container["command"][-1]
    assert "106.54.197.139" not in str(deployment)


def test_bootstrap_job_uses_candidate_image_and_runs_helm_after_adoption():
    job = load_yaml(BOOTSTRAP_JOB)

    assert job["kind"] == "Job"
    assert job["metadata"]["namespace"] == "doops-system"
    assert job["metadata"]["generateName"] == "doops-agent-helm-bootstrap-"
    assert "name" not in job["metadata"]
    assert job["metadata"]["labels"]["doops.sh/release-id"] == "__DOOPS_AGENT_IMAGE_TAG__"
    assert job["spec"]["template"]["spec"]["restartPolicy"] == "Never"
    assert job["spec"]["template"]["spec"]["imagePullSecrets"] == [
        {"name": "doops-registry-pull"}
    ]
    init_containers = job["spec"]["template"]["spec"]["initContainers"]
    assert {item["name"] for item in init_containers} == {
        "adopt-helm-label",
        "adopt-helm-release-name",
        "adopt-helm-release-namespace",
        "normalize-gateway-environment",
    }
    normalize = next(
        item
        for item in init_containers
        if item["name"] == "normalize-gateway-environment"
    )
    assert normalize["command"] == ["/usr/local/bin/kubectl"]
    assert normalize["args"] == [
        "-n",
        "doops-system",
        "set",
        "env",
        "deployment/doops-agent",
        "DOOPS_ALLOW_INSECURE_GATEWAY-",
        "DOOPS_GATEWAY_URL=https://doops.l8ai.cn",
        "DOOPS_GATEWAY_CLUSTER=doops-oilan",
        "DOOPS_GATEWAY_INSTANCE=oilan-node",
    ]
    assert {item["image"] for item in init_containers} == {"__DOOPS_AGENT_IMAGE__"}
    helm = job["spec"]["template"]["spec"]["containers"][0]
    assert helm["image"] == "__DOOPS_AGENT_IMAGE__"
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
        "--set-string",
        "image.tag=__DOOPS_AGENT_IMAGE_TAG__",
        "--wait",
        "--timeout",
        "10m",
    ]


def test_bootstrap_job_with_generate_name_is_created_not_applied():
    deployment_doc = (
        ROOT / "deploy" / "docs" / "oilan-doops-agent.md"
    ).read_text(encoding="utf-8")

    assert "kubectl create -f -" in deployment_doc
    assert "kubectl apply -f -" not in deployment_doc


def test_bootstrap_workflow_is_a_v2_deployment_template_without_commands():
    workflow = load_yaml(WORKFLOW)
    registry = load_yaml(REGISTRY)

    assert workflow["apiVersion"] == "doops.sh/v2"
    assert workflow["kind"] == "DeploymentTemplate"
    assert workflow["spec"]["plan"]["target"]["environment"] == "oilan"
    assert workflow["spec"]["plan"]["desiredState"]["delivery"] == "bootstrap-helm-agent"
    assert workflow["spec"]["plan"]["desiredState"]["configurationSource"] == "backend/deploy/environments.yaml"
    assert registry["environments"]["oilan"]["target"] == "gw-oilan-node"
    assert registry["environments"]["oilan"]["chart"] == "backend/deploy/helm/doops-agent"
    assert registry["environments"]["oilan"]["values"] == "backend/deploy/environments/oilan-values.yaml"

    workflow_text = WORKFLOW.read_text(encoding="utf-8")
    for forbidden in ("stages:", "uses:", "task:", "requiredCommand:", "verificationCommand:", "deploy.sh"):
        assert forbidden not in workflow_text

    assert not (ROOT / "ops" / "cicd" / "oilan-doops-agent-bootstrap.yaml").exists()


def test_agent_images_bundle_the_versioned_helm_chart():
    for path in ("Dockerfile", "agent/Dockerfile", "agent/Dockerfile.sandbox"):
        dockerfile = (ROOT / path).read_text(encoding="utf-8")
        assert "COPY --from=builder /app/deploy /app/deploy" in dockerfile
