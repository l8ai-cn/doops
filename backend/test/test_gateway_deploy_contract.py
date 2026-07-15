from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
DEPLOY = ROOT / "scripts" / "deploy-gateway.sh"


def test_gateway_deploy_detects_systemd_unit_without_pipefail_pipeline():
    text = DEPLOY.read_text()

    assert "systemctl list-unit-files" not in text
    assert "sudo -n systemctl cat doops-gateway.service >/dev/null" in text
    assert "REMOTE_LOCK=\"/run/lock/doops-gateway-deploy.lock\"" in text
    assert "REMOTE_TMP=\"/tmp/doops-gateway-deploy-${DEPLOY_ID}\"" in text
    assert "ControlMaster=auto" in text
    assert "ControlPersist=30" in text
    assert "ControlPath=${SSH_CONTROL_PATH}" in text


def test_gateway_deploy_bootstraps_external_key_only_without_existing_ciphertext():
    text = DEPLOY.read_text()

    assert "password_ciphertext" in text
    assert "payload_ciphertext" in text
    assert "secrets.token_hex(32)" in text
    assert "gateway encrypted data exists but no legacy secret key is available" in text
