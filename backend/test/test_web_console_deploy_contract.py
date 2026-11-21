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


def test_origin_check_controls_cutover_and_rollback(tmp_path):
    remote = DEPLOY.read_text().split("<<'REMOTE'\n", 1)[1].rsplit("\nREMOTE", 1)[0]
    fake_tools = {
        "sudo": r'''#!/bin/bash
set -eu
shift 2
printf '%s\n' "$*" >> "$TRACE"
case "$1 $2" in
  'inspect --format')
    if [[ $3 == *Config.Image* ]]; then
      echo "$IMAGE"
    else
      echo 'DOOPS_CONSOLE_ALLOWED_GATEWAYS=https://gateway.invalid'
    fi ;;
  'inspect backup') test -f "$STATE" ;;
  'rename doops-web') touch "$STATE" ;;
  'rename backup') test -f "$STATE" ;;
esac
''',
        "curl": r'''#!/bin/bash
for arg in "$@"; do
  case "$arg" in
    http://127.0.0.1:300*/api/login) url="$arg" ;;
    'Origin: http://cross-origin.invalid') cross=yes ;;
  esac
done
if [[ -z ${url:-} ]]; then echo 200
elif [[ ${cross:-} == yes ]]; then echo 403
elif [[ $url == *":$FAIL_PORT/"* ]]; then echo 403
else echo 400
fi
''',
    }
    for name, body in fake_tools.items():
        path = tmp_path / name
        path.write_text(body)
        path.chmod(0o755)

    image = "registry.invalid/web@sha256:" + "a" * 64
    for branch in ("success", "3001", "3000"):
        trace = tmp_path / f"trace-{branch}"
        env = {
            **os.environ,
            "PATH": f"{tmp_path}:{os.environ['PATH']}",
            "TRACE": str(trace),
            "IMAGE": image,
            "STATE": str(tmp_path / f"state-{branch}"),
            "FAIL_PORT": branch,
        }
        result = subprocess.run(
            ["bash", "-s", "--", image, "https://gateway.invalid", "candidate", "backup", "https://console.invalid"],
            input=remote, text=True, env=env, capture_output=True, timeout=10,
        )
        log = trace.read_text().splitlines()
        assert (result.returncode == 0) == (branch == "success"), result.stderr
        if branch == "success":
            assert "rename doops-web backup" in log
            assert "rename backup doops-web" not in log
        elif branch == "3001":
            assert "stop doops-web" not in log
        else:
            assert log[-3:] == ["rm -f doops-web", "rename backup doops-web", "start doops-web"]
