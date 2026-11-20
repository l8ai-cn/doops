import json
import os
from pathlib import Path
import signal
import subprocess
import time

import pytest


BACKEND = Path(__file__).resolve().parents[1]
RULES = json.dumps([{"name": "marker", "match": "DENIED_MARKER", "hint": "marker rejected"}])


@pytest.fixture(scope="module")
def wrapper(tmp_path_factory):
    binary = tmp_path_factory.mktemp("native-bash") / "doops-shell"
    subprocess.run(["go", "build", "-o", binary, "./cmd/doops-shell"], cwd=BACKEND / "agent", check=True)
    return binary


def test_wrapper_checks_original_command_before_any_side_effect(wrapper, tmp_path):
    env = {"PATH": os.defpath, "HOME": str(tmp_path), "DOOPS_COMMAND_DENY_RULES": RULES}
    (tmp_path / ".bash_profile").write_text("echo 'login profile diagnostic' >&2\n")

    def run(*args):
        return subprocess.run(args, cwd=tmp_path, capture_output=True, text=True, env=env)

    command = "shopt -q login_shell || exit 9\nvalues=('two words'); printf '%s' \"${values[0]}\"; exit 27"
    expected = run("/bin/bash", "-lc", command)
    assert expected.returncode == 27 and expected.stdout.endswith("two words")
    allowed = run(wrapper, "-c", command)
    assert (allowed.returncode, allowed.stdout, allowed.stderr) == (expected.returncode, expected.stdout, expected.stderr)
    denied = run(wrapper, "-c", "touch denied; printf '%s' DENIED_MARKER")
    assert (denied.returncode, denied.stdout, denied.stderr) == (1, "", "blocked_command(marker): next: marker rejected\n")
    assert not (tmp_path / "denied").exists()
    for args in [(), ("-lc", "touch invalid"), ("-c", "touch invalid", "extra")]:
        assert run(wrapper, *args).returncode == 2
    assert not (tmp_path / "invalid").exists()


def test_bash_inherits_policy_proxy_and_ca_but_not_mutation_authority(wrapper, tmp_path):
    env = {
        "HOME": str(tmp_path), "PATH": os.defpath, "DOOPS_COMMAND_DENY_RULES": RULES,
        "DO_AGENT_MUTATION_AUTHORITY_SECRET": "test-only", "DO_AGENT_MUTATION_AUTHORITY_TOKEN": "test-only",
        "HTTPS_PROXY": "http://proxy.invalid", "NO_PROXY": "localhost", "SSL_CERT_FILE": "/test-ca",
    }
    command = '''test -z "${DO_AGENT_MUTATION_AUTHORITY_SECRET+x}${DO_AGENT_MUTATION_AUTHORITY_TOKEN+x}" &&
        test -n "$DOOPS_COMMAND_DENY_RULES" && test "$HTTPS_PROXY" = http://proxy.invalid &&
        test "$NO_PROXY" = localhost && test "$SSL_CERT_FILE" = /test-ca'''
    subprocess.run([wrapper, "-c", command], cwd=tmp_path, env=env, check=True)


def test_plugin_argv_and_ask_report_the_same_prestart_denial(wrapper, tmp_path):
    executable = tmp_path / "kubectl"
    executable.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > started\nexit 17\n')
    executable.chmod(0o755)
    env = {"PATH": str(tmp_path), "HOME": str(tmp_path), "DOOPS_COMMAND_DENY_RULES": RULES}
    for args in [("-c", "kubectl get DENIED_MARKER"), ("--argv", "kubectl", "get", "DENIED_MARKER")]:
        result = subprocess.run([wrapper, *args], cwd=tmp_path, env=env, text=True, capture_output=True)
        assert (result.returncode, result.stderr) == (1, "blocked_command(marker): next: marker rejected\n")
        assert not (tmp_path / "started").exists()
    result = subprocess.run([wrapper, "--argv", "kubectl", "get", "two words"], cwd=tmp_path, env=env)
    assert result.returncode == 17
    assert (tmp_path / "started").read_text() == "get\ntwo words\n"


def test_cancel_targets_original_bash_pid(wrapper, tmp_path):
    ready = tmp_path / "ready"
    child = subprocess.Popen([wrapper, "-c", "trap 'exit 23' TERM; echo $$ > ready; while :; do sleep .05; done"],
                             cwd=tmp_path, env={"HOME": str(tmp_path), "PATH": os.defpath})
    try:
        for _ in range(200):
            if ready.exists() and ready.stat().st_size:
                break
            time.sleep(.01)
        assert int(ready.read_text()) == child.pid
        child.send_signal(signal.SIGTERM)
        assert child.wait(timeout=3) == 23
    finally:
        child.kill()
        child.wait()
