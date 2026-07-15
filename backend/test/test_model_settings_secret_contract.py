import importlib.util
import json
import subprocess
from pathlib import Path

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "deploy" / "helm" / "doops-agent"
VALUES = ROOT / "deploy" / "environments" / "oilan-values.yaml"
REGISTRY = ROOT / "deploy" / "environments.yaml"
SETTINGS_MODULE = ROOT / "agent" / "configure_doagent_settings.py"
IMAGE_DIGEST = "sha256:" + ("b" * 64)


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
            str(VALUES),
            "--set-string",
            f"image.digest={IMAGE_DIGEST}",
        ],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    return [item for item in yaml.safe_load_all(output) if item][0]


def render_oilan_chart_with(*overrides):
    command = [
        "helm",
        "template",
        "doops-agent-live",
        str(CHART),
        "--namespace",
        "kz-ops",
        "--values",
        str(VALUES),
    ]
    for override in overrides:
        command.extend(["--set-string", override])
    return subprocess.run(command, capture_output=True, text=True)


def env_value(container, name: str):
    return next(item for item in container["env"] if item["name"] == name)


def load_settings_module():
    spec = importlib.util.spec_from_file_location(
        "configure_doagent_settings",
        SETTINGS_MODULE,
    )
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


def test_oilan_agent_model_settings_are_secret_backed():
    deployment = render_oilan_chart()
    container = deployment["spec"]["template"]["spec"]["containers"][0]
    volumes = deployment["spec"]["template"]["spec"]["volumes"]
    settings = next(item for item in volumes if item["name"] == "doagent-config")

    assert deployment["kind"] == "Deployment"
    assert deployment["metadata"]["name"] == "doops-agent-live"
    assert deployment["metadata"]["namespace"] == "kz-ops"
    assert deployment["spec"]["strategy"] == {
        "type": "RollingUpdate",
        "rollingUpdate": {"maxUnavailable": 0, "maxSurge": 1},
    }
    assert "hostNetwork" not in deployment["spec"]["template"]["spec"]
    assert all("hostPort" not in port for port in container["ports"])
    assert (
        deployment["spec"]["template"]["spec"]["nodeSelector"]["kubernetes.io/hostname"]
        == "192.168.0.24"
    )
    assert deployment["spec"]["template"]["spec"]["imagePullSecrets"] == []
    assert container["image"] == f"docker.cnb.cool/l8ai/ai/doops.sh@{IMAGE_DIGEST}"
    assert "DOOPS_ALLOW_INSECURE_GATEWAY" not in {
        item["name"] for item in container["env"]
    }
    assert env_value(container, "DOOPS_GATEWAY_URL")["value"] == "https://doops.l8ai.cn"
    assert env_value(container, "DOOPS_GATEWAY_CLUSTER")["value"] == "doops-edu"
    assert env_value(container, "DOOPS_GATEWAY_INSTANCE")["value"] == "edu-coder"
    assert (
        env_value(container, "DOOPS_GATEWAY_AGENT_TOKEN")["valueFrom"]["secretKeyRef"]
        == {"name": "doops-agent-runtime", "key": "agent-token"}
    )
    assert '-listen "0.0.0.0"' in container["command"][-1]
    assert '-agent-token "$DOOPS_GATEWAY_AGENT_TOKEN"' in container["command"][-1]
    probes = {
        name: container[name]["exec"]["command"][-1]
        for name in ("startupProbe", "readinessProbe", "livenessProbe")
    }
    assert all("pgrep -fc '^/app/doops-agent( |$)'" in probe for probe in probes.values())
    assert "/health/live" in probes["startupProbe"]
    assert "/health/ready" in probes["readinessProbe"]
    assert "/health/live" in probes["livenessProbe"]
    assert "configMap" not in settings
    assert settings["secret"] == {
        "secretName": "doagent-model-settings",
        "items": [{"key": "settings.json", "path": "settings.json"}],
    }

    values = yaml.safe_load(VALUES.read_text(encoding="utf-8"))
    assert values["modelRouting"] == {"policy": "single-model"}
    assert values["doagentSettings"] == {
        "secretName": "doagent-model-settings",
        "key": "settings.json",
    }

    registry = yaml.safe_load(REGISTRY.read_text(encoding="utf-8"))
    executor = registry["environments"]["oilan"]["executor"]["config"]
    assert "modelRouting" not in executor
    assert "modelSettings" not in executor
    assert "extensions" not in executor
    host_paths = {
        item["name"]: item["hostPath"]["path"]
        for item in volumes
        if "hostPath" in item
    }
    assert host_paths["kube-admin"] == "/etc/kubernetes/admin.conf"
    assert host_paths["agent-home"] == "/var/lib/doops-agent-home"
    assert host_paths["containerd-socket"] == "/run/containerd/containerd.sock"
    assert host_paths["nerdctl-bin"] == "/usr/bin/nerdctl"
    assert host_paths["crictl-bin"] == "/usr/bin/crictl"


def test_oilan_agent_chart_rejects_invalid_image_references():
    invalid_overrides = (
        ("image.digest=latest",),
        (f"image.digest={IMAGE_DIGEST}", "image.repository="),
        ("image.tag=" + ("a" * 40),),
    )

    for overrides in invalid_overrides:
        result = render_oilan_chart_with(*overrides)
        assert result.returncode != 0, result.stdout


def test_public_agent_template_does_not_require_registry_auth():
    deployment = yaml.safe_load((ROOT / "agent" / "agent-default.yaml").read_text(encoding="utf-8"))
    pod_spec = deployment["spec"]["template"]["spec"]

    assert pod_spec.get("imagePullSecrets") in (None, [])
    assert all(item["name"] != "registry-auth" for item in pod_spec["volumes"])


def test_single_model_policy_preserves_mounted_settings_and_omits_duplicate_tiers(
    tmp_path,
):
    module = load_settings_module()
    source = tmp_path / "mounted-settings.json"
    destination = tmp_path / "runtime-settings.json"
    source.write_text(
        json.dumps(
            {
                "model": "quan2go/gpt-5-4",
                "provider": {
                    "quan2go": {
                        "options": {"apiKey": "secret-must-be-preserved"},
                        "models": {"gpt-5-4": {"name": "GPT-5.4"}},
                    }
                },
                "model_tiers": {
                    "high": "quan2go/gpt-5-4",
                    "default": "quan2go/gpt-5-4",
                    "low": "quan2go/gpt-5-4",
                },
            }
        ),
        encoding="utf-8",
    )

    module.configure_settings(source, destination, "single-model")

    rendered = json.loads(destination.read_text(encoding="utf-8"))
    original = json.loads(source.read_text(encoding="utf-8"))
    assert rendered["model"] == "quan2go/gpt-5-4"
    assert (
        rendered["provider"]["quan2go"]["options"]["apiKey"]
        == "secret-must-be-preserved"
    )
    assert "model_tiers" not in rendered
    assert original["model_tiers"]["high"] == "quan2go/gpt-5-4"


def test_tiered_policy_rejects_duplicate_model_identifiers(tmp_path):
    module = load_settings_module()
    source = tmp_path / "mounted-settings.json"
    destination = tmp_path / "runtime-settings.json"
    source.write_text(
        json.dumps(
            {
                "model": "quan2go/gpt-5-4",
                "model_tiers": {
                    "high": "quan2go/gpt-5-4",
                    "default": "quan2go/gpt-5-4",
                    "low": "quan2go/gpt-5-4-mini",
                },
            }
        ),
        encoding="utf-8",
    )

    with pytest.raises(module.SettingsConfigurationError, match="distinct"):
        module.configure_settings(source, destination, "tiered")
