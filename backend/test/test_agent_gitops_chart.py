import subprocess
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "deploy" / "helm" / "doops-agent"
OILAN_VALUES = ROOT / "deploy" / "environments" / "oilan-values.yaml"
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
            "doops-agent-live",
            str(CHART),
            "--namespace",
            "kz-ops",
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
    assert deployment["metadata"]["name"] == "doops-agent-live"
    assert deployment["metadata"]["namespace"] == "kz-ops"
    assert "meta.helm.sh/release-name" not in deployment["metadata"].get("annotations", {})
    assert "meta.helm.sh/release-namespace" not in deployment["metadata"].get("annotations", {})
    assert deployment["spec"]["strategy"]["type"] == "Recreate"
    assert deployment["spec"]["template"]["spec"]["nodeSelector"]["kubernetes.io/hostname"] == "192.168.0.24"
    assert deployment["spec"]["template"]["spec"]["imagePullSecrets"] == []
    assert container["image"].endswith(f":{RELEASE_ID}")
    assert "DOOPS_ALLOW_INSECURE_GATEWAY" not in {item["name"] for item in container["env"]}
    assert env_value(container, "DOOPS_GATEWAY_URL")["value"] == "https://doops.l8ai.cn"
    assert env_value(container, "DOOPS_GATEWAY_CLUSTER")["value"] == "doops-edu"
    assert env_value(container, "DOOPS_GATEWAY_INSTANCE")["value"] == "edu-coder"
    assert env_value(container, "DO_AGENT_MODEL_ROUTING_POLICY")["value"] == "single-model"
    assert env_value(container, "DOOPS_GATEWAY_AGENT_TOKEN")["valueFrom"]["secretKeyRef"] == {
        "name": "doops-agent-runtime",
        "key": "agent-token",
    }
    assert '-listen "0.0.0.0"' in container["command"][-1]
    assert '-agent-token "$DOOPS_GATEWAY_AGENT_TOKEN"' in container["command"][-1]
    doagent_config = next(
        item
        for item in deployment["spec"]["template"]["spec"]["volumes"]
        if item["name"] == "doagent-config"
    )
    assert doagent_config["secret"]["secretName"] == "doagent-model-settings"
    assert doagent_config["secret"]["items"] == [
        {"key": "settings.json", "path": "settings.json"}
    ]
    volumes = {
        item["name"]: item["hostPath"]["path"]
        for item in deployment["spec"]["template"]["spec"]["volumes"]
        if "hostPath" in item
    }
    assert volumes["kube-admin"] == "/etc/kubernetes/admin.conf"
    assert volumes["agent-home"] == "/var/lib/doops-agent-home"
    assert volumes["containerd-socket"] == "/run/containerd/containerd.sock"
    assert volumes["nerdctl-bin"] == "/usr/bin/nerdctl"
    assert volumes["crictl-bin"] == "/usr/bin/crictl"


def test_legacy_bootstrap_job_is_removed():
    assert not (ROOT / "deploy" / "bootstrap" / "oilan-doops-agent-bootstrap.yaml").exists()


def test_deployment_doc_excludes_manual_bootstrap_commands():
    deployment_doc = (
        ROOT / "deploy" / "docs" / "oilan-doops-agent.md"
    ).read_text(encoding="utf-8")

    assert "doops cicd run" in deployment_doc
    for forbidden in (
        "kubectl create -f -",
        "kubectl apply -f -",
        "cicd submit",
        "DOOPS_CICD_PLAN_SIGNING_KEY",
        "Ed25519",
    ):
        assert forbidden not in deployment_doc


def test_bootstrap_workflow_is_a_v2_deployment_template_without_commands():
    workflow = load_yaml(WORKFLOW)
    registry = load_yaml(REGISTRY)

    assert workflow["apiVersion"] == "doops.sh/v2"
    assert workflow["kind"] == "DeploymentTemplate"
    assert workflow["spec"]["plan"]["target"]["environment"] == "oilan"
    assert workflow["spec"]["plan"]["desiredState"]["delivery"] == "declarative-agent-reconciliation"
    assert workflow["spec"]["plan"]["desiredState"]["configurationSource"] == "backend/deploy/environments.yaml"
    assert workflow["spec"]["plan"]["policy"]["maxAttempts"] == 3
    assert workflow["spec"]["plan"]["policy"]["maxNoProgress"] == 1
    assert registry["environments"]["oilan"]["target"] == "gw-edu-coder"
    assert registry["environments"]["oilan"]["cluster"] == "doops-edu"
    assert registry["environments"]["oilan"]["instance"] == "edu-coder"
    assert registry["environments"]["oilan"]["namespace"] == "kz-ops"
    assert registry["environments"]["oilan"]["release"] == "doops-agent-live"
    assert registry["environments"]["oilan"]["workload"] == "deployment/doops-agent-live"
    assert registry["environments"]["oilan"]["modelRouting"]["policy"] == "single-model"
    assert registry["environments"]["oilan"]["chart"] == "backend/deploy/helm/doops-agent"
    assert registry["environments"]["oilan"]["values"] == "backend/deploy/environments/oilan-values.yaml"

    workflow_text = WORKFLOW.read_text(encoding="utf-8")
    for forbidden in ("stages:", "uses:", "task:", "requiredCommand:", "verificationCommand:", "deploy.sh"):
        assert forbidden not in workflow_text

    assert not (ROOT / "ops" / "cicd" / "oilan-doops-agent-bootstrap.yaml").exists()


def test_environment_profile_owns_release_manifest_repository():
    registry = load_yaml(REGISTRY)

    assert "manifestRepository" not in registry["artifactContract"]
    assert (
        registry["environments"]["oilan"]["releaseManifestRepository"]
        == "https://github.com/l8ai-cn/doops.git"
    )


def test_agent_images_bundle_the_versioned_helm_chart():
    for path in ("Dockerfile", "agent/Dockerfile", "agent/Dockerfile.sandbox"):
        dockerfile = (ROOT / path).read_text(encoding="utf-8")
        assert "COPY --from=builder /app/deploy /app/deploy" in dockerfile
