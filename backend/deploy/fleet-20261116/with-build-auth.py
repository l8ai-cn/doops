#!/usr/bin/env python3
"""Use an existing managed opaque Secret for one build without persisting auth."""
import argparse
import base64
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile


def main():
    def terminated(signum, _frame):
        raise SystemExit(128 + signum)

    signal.signal(signal.SIGTERM, terminated)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--secret", required=True)
    parser.add_argument("--credential-id", required=True)
    parser.add_argument("--grant-id", required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        raise ValueError("build command is required")
    result = subprocess.run(["kubectl", "get", "secret", args.secret, "-n", args.namespace,
                             "-o", "json", "--request-timeout=20s"], capture_output=True, timeout=30)
    if result.returncode:
        raise ValueError("managed build Secret is unavailable")
    secret = json.loads(result.stdout)
    labels = secret["metadata"].get("labels", {})
    if labels.get("doops.sh/credential-id") != args.credential_id or labels.get("doops.sh/credential-grant-id") != args.grant_id:
        raise ValueError("managed build Secret binding differs")
    data = secret["data"]
    user = base64.b64decode(data["CNB_USERNAME"], validate=True).decode()
    token = base64.b64decode(data["CNB_TOKEN"], validate=True).decode()
    if not user or not token:
        raise ValueError("managed build credential is empty")
    auth = base64.b64encode((user + ":" + token).encode()).decode()
    with tempfile.TemporaryDirectory(prefix="doops-build-auth-") as directory:
        path = Path(directory) / "config.json"
        with open(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as config:
            json.dump({"auths": {"docker.cnb.cool": {"auth": auth}}}, config)
        environment = dict(os.environ, DOCKER_CONFIG=directory, REGISTRY_AUTH_FILE=str(path))
        return subprocess.run(command, env=environment).returncode


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ValueError, KeyError, OSError, subprocess.TimeoutExpired):
        raise SystemExit("build_auth_setup_failed") from None
