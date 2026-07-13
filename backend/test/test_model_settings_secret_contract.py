import subprocess
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[1]
CHART = ROOT / "deploy" / "helm" / "doops-agent"
VALUES = ROOT / "deploy" / "environments" / "oilan-values.yaml"
REGISTRY = ROOT / "deploy" / "environments.yaml"


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
            "image.tag=0123456789abcdef0123456789abcdef01234567",
        ],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    return [item for item in yaml.safe_load_all(output) if item][0]


def test_oilan_agent_model_settings_are_secret_backed():
    deployment = render_oilan_chart()
    volumes = deployment["spec"]["template"]["spec"]["volumes"]
    settings = next(item for item in volumes if item["name"] == "doagent-config")

    assert "configMap" not in settings
    assert settings["secret"] == {
        "secretName": "doagent-model-settings",
        "items": [{"key": "settings.json", "path": "settings.json"}],
    }

    registry = yaml.safe_load(REGISTRY.read_text(encoding="utf-8"))
    model_settings = registry["environments"]["oilan"]["modelSettings"]
    assert model_settings == {
        "provider": "minimax",
        "model": "minimax/MiniMax-M3",
        "secretRef": {"name": "doagent-model-settings", "key": "settings.json"},
    }


def test_public_agent_template_does_not_require_registry_auth():
    deployment = yaml.safe_load((ROOT / "agent" / "agent-default.yaml").read_text(encoding="utf-8"))
    pod_spec = deployment["spec"]["template"]["spec"]

    assert pod_spec.get("imagePullSecrets") in (None, [])
    assert all(item["name"] != "registry-auth" for item in pod_spec["volumes"])
