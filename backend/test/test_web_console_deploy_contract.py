import os
from pathlib import Path
import subprocess


ROOT = Path(__file__).resolve().parents[1]
DEPLOY = ROOT / "scripts" / "deploy-web-console.sh"
PROXY_DEPLOY = ROOT / "scripts" / "deploy-gateway-tls-proxy.sh"
NGINX = ROOT / "gateway" / "nginx" / "doops-proxy.conf"


def test_web_console_host_deploy_is_immutable_and_rollback_capable():
    deploy = DEPLOY.read_text()
    proxy_deploy = PROXY_DEPLOY.read_text()
    nginx = NGINX.read_text()

    assert '--image must use an immutable sha256 registry digest' in deploy
    assert 'DOOPS_CONSOLE_ALLOWED_GATEWAYS=$gateway' in deploy
    assert "127.0.0.1:3001:3000" in deploy
    assert "127.0.0.1:3000:3000" in deploy
    assert "doops-web-rollback-" in deploy
    assert 'docker rename "$backup" doops-web' in deploy
    assert 'docker inspect --format \'{{.Config.Image}}\' doops-web' in deploy

    assert "location ^~ /v1/" in nginx
    assert "location = /health" in nginx
    assert "location / {" in nginx
    assert "server_name *.l8ai.cn;" in nginx
    assert nginx.count("proxy_pass http://127.0.0.1:42222;") == 4
    assert "proxy_pass http://127.0.0.1:3000;" in nginx

    assert "https://doops.l8ai.cn/v1/targets" in proxy_deploy
    assert "https://doops.l8ai.cn/" in proxy_deploy
    assert 'test "$web_code" = "200"' in proxy_deploy
    assert 'SSH=(ssh -p "$SSH_PORT" "${SSH_USER}@${HOST}")' in proxy_deploy
    assert 'SCP=(scp -P "$SSH_PORT")' in proxy_deploy
    assert "SSH_PREFIX" not in proxy_deploy


def test_deploy_forwards_console_origin_and_checks_both_containers(tmp_path):
    ssh = tmp_path / "ssh"
    ssh.write_text(r'''#!/bin/bash
printf "%s\n" "$@" >> "$SSH_TRACE"
if [[ "$*" == *"bash -s"* ]]; then cat >> "$SSH_TRACE"; fi
''')
    ssh.chmod(0o755)
    trace = tmp_path / "trace"
    env = {**os.environ, "PATH": f"{tmp_path}:{os.environ['PATH']}", "SSH_TRACE": str(trace)}
    args = ["bash", str(DEPLOY), "--host", "console.example.test", "--image", "registry.example.test/web@sha256:" + "a" * 64]
    subprocess.run([*args, "--console-origin", "https://console.example.test"], env=env, check=True, capture_output=True)
    log = trace.read_text()
    assert "https://console.example.test\n" in log
    assert log.count('DOOPS_CONSOLE_ORIGIN=$console_origin') == 2
    for port in (3000, 3001):
        assert f"check_console_origin {port}" in log

    trace.unlink()
    rejected = subprocess.run([*args, "--console-origin", "https://console.example.test/login"], env=env, capture_output=True)
    assert rejected.returncode == 2
    assert not trace.exists()
