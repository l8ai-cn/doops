"""Local transaction checks; no cluster, registry or build is contacted."""
import copy
import base64
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
from types import SimpleNamespace

import pytest
import yaml


HERE = Path(__file__).parent
COMMIT = "a" * 40
IMAGE = "registry.example.com/doops@sha256:" + "b" * 64
CANARY = "SYNTHETIC_SECRET_DO_NOT_LOG"


@pytest.fixture
def controller(monkeypatch):
    spec = importlib.util.spec_from_file_location("first_upgrade", HERE / "first-upgrade.py")
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    old_image = "registry.example.com/doops@" + m.BASE_DIGEST
    state = {"image": old_image, "revision": m.BASE_REVISION, "receipt": {}, "calls": [], "backups": [],
             "mode": "success", "ticks": 0, "rolled_back": False}
    args = SimpleNamespace(image=IMAGE, source_commit=COMMIT)
    values = {"gateway": {"cluster": "doops-zyj", "instance": "zyj-test",
                          "url": "https://old-gateway.example"},
              "image": {"repository": "registry.example.com/doops", "digest": m.BASE_DIGEST},
              "privateSetting": CANARY}
    live = {"kind": "Deployment", "metadata": {"name": m.RELEASE, "uid": "deployment-uid",
                                             "resourceVersion": "12"},
            "spec": {"replicas": 1, "selector": {"matchLabels": {"app": m.RELEASE}},
                     "template": {"spec": {
                         "imagePullSecrets": [{"name": "test-pull"}],
                         "containers": [{"name": "doops-agent", "image": old_image,
                                         "env": [{"name": "PRIVATE_SETTING", "value": CANARY},
                                                 {"name": "DOOPS_GATEWAY_URL",
                                                  "value": m.GATEWAY_URL}]}]}}}}
    original = copy.deepcopy(live)
    live["spec"]["template"]["spec"]["containers"][0]["env"][1]["value"] = values["gateway"]["url"]

    def manifest(image):
        deployment = copy.deepcopy(live)
        deployment["spec"]["template"]["spec"]["containers"][0]["image"] = image
        return yaml.safe_dump_all([
            {"kind": "ServiceAccount", "metadata": {"name": m.RELEASE}},
            {"kind": "ClusterRoleBinding",
             "metadata": {"name": m.NS + "-" + m.RELEASE + "-cluster-admin"}},
            deployment,
        ])

    def evidence(image, commit, _version):
        assert image == state["image"]
        return {"podUID": "old-uid" if image == old_image else "new-uid",
                "podName": "old-pod" if image == old_image else "new-pod",
                "runtimeDigest": image.split("@")[1], "release_commit": commit,
                "runtimeClass": "", "node": "node"}

    def get(kind, name):
        if kind == "secret":
            assert name == m.ORIGINAL_BACKUP
            saved = copy.deepcopy(original)
            if state["mode"] == "original-route-changed":
                saved["spec"]["template"]["spec"]["containers"][0]["env"][1]["value"] = "https://wrong.example"
            if state["mode"] == "original-uid-changed":
                saved["metadata"]["uid"] = "another-deployment"
            return {"immutable": state["mode"] != "backup-mutable", "data": {"deployment.json":
                base64.b64encode(json.dumps(saved).encode()).decode()}}
        if kind == "deployment":
            return copy.deepcopy(live)
        if kind == "pod":
            return {"spec": {"serviceAccountName": m.NAME}}
        if kind == "jobs":
            return {"items": []}
        if kind in ("serviceaccount", "clusterrolebinding"):
            return {"metadata": {}}
        if kind == "configmap":
            data = dict(state["receipt"])
            if data.get("phase") == "awaiting-gateway" and state["mode"] != "timeout":
                data["gatewayAcceptance"] = json.dumps({
                    k: v for k, v in evidence(IMAGE, COMMIT, "0.2.45").items()
                    if k in ("podUID", "runtimeDigest", "release_commit")
                })
                state["receipt"]["gatewayAcceptance"] = data["gatewayAcceptance"]
            return {"data": data}
        raise AssertionError((kind, name))

    def run(command, stdin=None, **_kwargs):
        state["calls"].append(command)
        if command[0] == "python3":
            return ""
        if command[0] == "/app/doops-agent":
            return f"release=v0.3.3 commit={COMMIT}\n"
        if command[0] == "/usr/local/bin/do-agent":
            return "do-agent 0.2.45\n"
        if command[:3] == ["kubectl", "auth", "whoami"]:
            return json.dumps({"status": {"userInfo": {"username":
                              f"system:serviceaccount:{m.NS}:{m.NAME}"}}})
        if command[0] == "kubectl":
            if "create" in command:
                obj = json.loads(stdin)
                if obj["kind"] == "Secret":
                    assert obj["immutable"] is True and CANARY in obj["stringData"]["values.json"]
                    state["backups"].append(obj)
                else:
                    assert CANARY not in stdin
                    state["receipt"] = obj["data"]
            elif "patch" in command:
                if "deployment" in command:
                    assert "--patch-file=/dev/stdin" in command
                    patch = json.loads(stdin)
                    assert patch[0]["path"] == "/metadata/resourceVersion"
                    assert patch[1] == {"op": "replace", "path": "/spec", "value": original["spec"]}
                else:
                    patch = json.loads(command[command.index("-p") + 1])
                    assert CANARY not in json.dumps(patch)
                    state["receipt"].update(patch["data"])
            else:
                raise AssertionError(command)
            return ""
        if command[:2] == ["helm", "history"]:
            return json.dumps([{"revision": state["revision"], "status": "deployed",
                                "description": f"Rollback to {m.BASE_REVISION}"
                                if state["rolled_back"] else "Upgrade complete"}])
        if command[:3] == ["helm", "get", "values"]:
            return json.dumps(values)
        if command[:3] == ["helm", "get", "manifest"]:
            return manifest(old_image)
        if command[:2] == ["helm", "template"]:
            candidate = json.loads(Path(command[command.index("-f") + 1]).read_text())
            assert candidate["privateSetting"] == CANARY
            assert candidate["image"]["digest"] == IMAGE.split("@")[1]
            assert candidate["gateway"]["url"] == m.GATEWAY_URL
            return manifest(IMAGE)
        if command[:2] == ["helm", "upgrade"]:
            assert len(state["backups"]) == 1
            if "--dry-run=server" in command:
                if state["mode"] == "dry-run-failure":
                    raise m.UpgradeError("dry_run_failed")
                return ""
            if state["mode"] == "upgrade-before-revision":
                raise OSError(CANARY)
            state.update(image=IMAGE, revision=m.BASE_REVISION + 1)
            if state["mode"] == "unknown":
                raise TimeoutError(CANARY)
            return ""
        if command[:2] == ["helm", "rollback"]:
            assert command[3] == str(m.BASE_REVISION)
            assert "--wait" not in command
            if state["mode"] == "rollback-failure":
                raise OSError(CANARY)
            state.update(image=old_image, revision=state["revision"] + 1, rolled_back=True)
            if state["mode"] == "concurrent-spec-change":
                live["spec"]["replicas"] = 2
            if state["mode"] == "rollback-response-lost":
                raise TimeoutError(CANARY)
            return ""
        raise AssertionError(command)

    def clock():
        state["ticks"] += 100
        return state["ticks"]

    def probe(*_args):
        if state["mode"] == "registry-failure":
            raise OSError(CANARY)

    monkeypatch.setattr(m, "run", run)
    monkeypatch.setattr(m, "get", get)
    monkeypatch.setattr(m, "version", evidence)
    monkeypatch.setattr(m, "registry_preflight", probe)
    monkeypatch.setattr(m, "A", SimpleNamespace(
        CHART="/app/deploy/helm/doops-agent",
        validate_deployment=lambda *_: None,
        agent_container=lambda d: d["spec"]["template"]["spec"]["containers"][0],
        controller_runtime_digest=lambda *_: IMAGE.split("@")[1],
        write_verified_receipt=lambda *_: None,
    ))
    monkeypatch.setattr(m.os, "umask", lambda *_: 0)
    monkeypatch.setattr(m.signal, "signal", lambda *_: None)
    monkeypatch.setattr(m.signal, "alarm", lambda *_: None)
    monkeypatch.setattr(m.time, "sleep", lambda *_: None)
    monkeypatch.setattr(m.time, "monotonic", clock)
    monkeypatch.setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
    monkeypatch.setenv("KUBECONFIG", "test-only")
    monkeypatch.setenv("HOSTNAME", "test-only")
    return m, args, state


@pytest.mark.parametrize("mode", ["success", "timeout", "accepted-then-failed", "unknown", "dry-run-failure",
                                  "registry-failure", "rollback-failure", "revision-changed",
                                  "original-route-changed", "original-uid-changed", "backup-mutable",
                                  "rollback-response-lost", "concurrent-spec-change", "upgrade-before-revision"])
def test_transaction(controller, capsys, mode, monkeypatch):
    m, args, state = controller
    state["mode"] = mode
    if mode == "revision-changed":
        state["revision"] = m.BASE_REVISION + 1
    if mode in ("rollback-failure", "rollback-response-lost", "concurrent-spec-change"):
        monkeypatch.setattr(m, "wait_acceptance",
                            lambda *_: (_ for _ in ()).throw(m.UpgradeError("acceptance_failed")))
    if mode == "accepted-then-failed":
        original_wait = m.wait_acceptance

        def accepted_then_failed(*args):
            original_wait(*args)
            raise m.UpgradeError("candidate_changed_during_acceptance")

        monkeypatch.setattr(m, "wait_acceptance", accepted_then_failed)
    if mode == "success":
        m.upgrade(args)
        assert state["receipt"]["phase"] == "complete"
    else:
        with pytest.raises(BaseException):
            m.upgrade(args)
        expected = {"timeout": "rolled-back", "accepted-then-failed": "rolled-back",
                    "unknown": "rolled-back", "rollback-response-lost": "rolled-back",
                    "upgrade-before-revision": "rolled-back",
                    "rollback-failure": "rollback-failed",
                    "concurrent-spec-change": "rollback-failed"}.get(mode, "preflight-failed")
        assert state["receipt"]["phase"] == expected
        if expected == "rolled-back":
            assert state["receipt"]["image"] == "registry.example.com/doops@" + m.BASE_DIGEST
            assert state["receipt"]["sourceCommit"] == m.BASE_COMMIT
            assert state["receipt"]["releaseCommit"] == m.BASE_COMMIT
            assert state["receipt"]["podUID"] == "old-uid"
            assert state["receipt"]["podName"] == "old-pod"
            assert state["receipt"]["runtimeDigest"] == m.BASE_DIGEST
            assert state["receipt"]["gatewayAcceptance"] == ""
    rollbacks = [c for c in state["calls"] if c[:2] == ["helm", "rollback"]]
    assert len(rollbacks) == int(mode in ("timeout", "accepted-then-failed", "unknown", "rollback-failure",
                                        "rollback-response-lost", "concurrent-spec-change", "upgrade-before-revision"))
    restores = [c for c in state["calls"] if "patch" in c and "deployment" in c]
    assert len(restores) == int(mode in ("timeout", "accepted-then-failed", "unknown", "rollback-response-lost",
                                       "upgrade-before-revision"))
    assert all(CANARY not in " ".join(c) for c in state["calls"])
    assert CANARY not in capsys.readouterr().out


def test_render_and_reject_mutable_image():
    command = [sys.executable, "-B", str(HERE / "first-upgrade.py"), "render",
               "--source-commit", COMMIT, "--image"]
    result = subprocess.run(command + [IMAGE], capture_output=True, text=True, check=True)
    docs = json.loads(result.stdout)["items"]
    assert [d["kind"] for d in docs] == ["ServiceAccount", "ClusterRoleBinding", "ConfigMap", "Job"]
    assert docs[2]["immutable"] and "def upgrade(" in docs[2]["data"]["first-upgrade.py"]
    pod = docs[3]["spec"]["template"]["spec"]
    assert pod["serviceAccountName"] != "doops-agent-test"
    assert pod["containers"][0]["image"] == IMAGE
    assert pod["containers"][0]["args"][-1] == COMMIT
    result = subprocess.run(command + ["registry.example.com/doops:latest"],
                            capture_output=True, text=True)
    assert result.returncode != 0 and not result.stdout


def test_gateway_acceptance_checks_actual_pod_and_uses_cas(controller, monkeypatch):
    m, args, state = controller
    pod = {"metadata": {"uid": "new-uid"},
           "spec": {"containers": [{"name": "doops-agent", "image": IMAGE}]},
           "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
    stored = {"metadata": {"resourceVersion": "12"}, "data": {
        "phase": "awaiting-gateway", "image": IMAGE, "sourceCommit": COMMIT, "podUID": "new-uid",
    }}
    monkeypatch.setenv("DOOPS_GATEWAY_CLUSTER", "doops-zyj")
    monkeypatch.setenv("DOOPS_GATEWAY_INSTANCE", "zyj-test")
    monkeypatch.setattr(m, "get", lambda kind, _: stored if kind == "configmap" else pod)
    m.A.runtime_digest_of = lambda _: IMAGE.split("@")[1]
    calls = []

    def run(command, **_kwargs):
        calls.append(command)
        if command[0] == "/app/doops-agent":
            return f"release=v0.3.3 commit={COMMIT}"
        if command[0] == "/usr/local/bin/do-agent":
            return "do-agent 0.2.45"
        return ""

    monkeypatch.setattr(m, "run", run)
    m.accept(args)
    patch = json.loads(calls[-1][-1])
    assert patch[0] == {"op": "test", "path": "/metadata/resourceVersion", "value": "12"}
    assert patch[1]["value"] == "awaiting-gateway"
    pod["metadata"]["uid"] = "wrong-pod"
    calls.clear()
    with pytest.raises(m.UpgradeError, match="gateway_routed_to_wrong_pod"):
        m.accept(args)
    assert not calls


def test_existing_chart_preserves_release_identity(controller):
    m, _, _ = controller
    chart = HERE.parents[1] / "helm" / "doops-agent"
    rendered = subprocess.run(
        ["helm", "template", m.RELEASE, str(chart), "-n", m.NS, "-f", str(HERE / "values.yaml"),
         "--set", "image.repository=registry.example.com/doops", "--set", "image.digest=" + IMAGE.split("@")[1]],
        check=True, capture_output=True, text=True,
    ).stdout
    deployment = m.check_manifest(rendered, m.NAME)
    assert deployment["spec"]["template"]["spec"]["serviceAccountName"] == m.RELEASE
    assert m.A.agent_container(deployment)["image"] == IMAGE
