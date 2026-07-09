#!/usr/bin/env python3
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
CONFIG = ROOT / "gateway/nginx/doops-proxy.conf"
DEPLOY = ROOT / "scripts/deploy-gateway-tls-proxy.sh"


def test_gateway_tls_proxy_is_versioned_and_routes_all_gateway_protocols() -> None:
    assert CONFIG.is_file()
    text = CONFIG.read_text(encoding="utf-8")
    assert "server_name doops.l8ai.cn;" in text
    assert "listen 443 ssl;" in text
    assert "ssl_certificate /etc/nginx/certs/l8ai-wildcard.crt;" in text
    assert "proxy_pass http://127.0.0.1:42222;" in text
    assert "proxy_set_header Upgrade $http_upgrade;" in text
    assert "proxy_request_buffering off;" in text
    assert "client_max_body_size 2g;" in text
    assert "proxy_pass http://127.0.0.1:3000;" in text


def test_gateway_tls_deploy_validates_and_rolls_back_nginx_config() -> None:
    assert DEPLOY.is_file()
    text = DEPLOY.read_text(encoding="utf-8")
    assert "set -euo pipefail" in text
    assert 'backup="${config}.bak-' in text
    assert "nginx -t" in text
    assert "nginx -s reload" in text
    assert "restore_proxy_config" in text
    assert "https://doops.l8ai.cn/v1/targets" in text
    assert 'test "$code" = "401"' in text
    assert "--dry-run" in text
