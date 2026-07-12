import importlib.util
import json
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "agent" / "configure_doagent_settings.py"


def load_module():
    spec = importlib.util.spec_from_file_location("configure_doagent_settings", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


def test_single_model_policy_preserves_mounted_settings_and_omits_duplicate_tiers(tmp_path):
    module = load_module()
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
    assert rendered["provider"]["quan2go"]["options"]["apiKey"] == "secret-must-be-preserved"
    assert "model_tiers" not in rendered
    assert original["model_tiers"]["high"] == "quan2go/gpt-5-4"


def test_tiered_policy_rejects_duplicate_model_identifiers(tmp_path):
    module = load_module()
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
