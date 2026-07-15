from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
DEPLOY = ROOT / "scripts" / "deploy-gateway.sh"


def test_gateway_deploy_detects_systemd_unit_without_pipefail_pipeline():
    text = DEPLOY.read_text()

    assert "systemctl list-unit-files" not in text
    assert "sudo systemctl cat doops-gateway.service >/dev/null 2>&1" in text
