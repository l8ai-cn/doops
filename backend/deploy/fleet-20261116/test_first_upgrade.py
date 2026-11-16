"""Transaction tests at the subprocess boundary; no cluster or registry is contacted."""
import base64
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
from types import SimpleNamespace

import pytest
import yaml

HERE = Path(__file__).parent
CANARY = "SYNTHETIC_SECRET_NEVER_LOG"
OLD_MANIFEST = json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:" + "a" * 64, "size": 2},
    "layers": []}, indent=2).replace("\n", "\r\n").encode()
OLD_DIGEST = "sha256:" + hashlib.sha256(OLD_MANIFEST).hexdigest()
OLD = "registry.example/agent@" + OLD_DIGEST
NEW = "registry.example/agent@sha256:" + "b" * 64
OLD_COMMIT = "76abae08a36d920c0058486d2d8ab86880c4c121"
NEW_COMMIT = "c" * 40


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@pytest.fixture
def transaction(monkeypatch):
    m = load("fleet_first_upgrade", HERE / "first-upgrade.py")
    m.A = load("fleet_adopt_helpers", HERE.parents[1] / "agent/adopt_agent.py")
    m.A.run = m.run
    a = SimpleNamespace(namespace="ai", release="doops-agent", workload="doops-agent", cluster="hdu",
        instance="235", base_revision=5, image=NEW, source_commit=NEW_COMMIT, release_tag="v0.3.4",
        gateway_url="https://doops.aiedulab.cn", job_name="fleet-hdu-r5", old_commit=OLD_COMMIT)
    values = {"gateway": {"url": "https://doops.l8ai.cn", "cluster": a.cluster, "instance": a.instance},
              "image": {"repository": "registry.example/agent", "digest": OLD.split("@")[1], "tag": "old"},
              "privateSetting": CANARY, "rbac": {"create": False}, "kubeconfig": {"source": "mounted"}}
    spec = {"replicas": 1, "selector": {"matchLabels": {"app": a.workload}}, "template": {
        "metadata": {"labels": {"app": a.workload}}, "spec": {"nodeSelector": {"role": "admin"},
        "volumes": [{"name": "admin-kube", "hostPath": {"path": "/etc/admin.conf", "type": "File"}}],
        "imagePullSecrets": [{"name": "registry-auth"}], "containers": [{"name": "doops-agent", "image": OLD,
            "command": ["old-entrypoint"], "readinessProbe": {"exec": {"command": ["old-health"]}},
            "volumeMounts": [{"name": "admin-kube", "mountPath": "/root/.kube/config"}],
            "env": [{"name": "DOOPS_GATEWAY_URL", "value": a.gateway_url},
                    {"name": "DOOPS_GATEWAY_CLUSTER", "value": a.cluster},
                    {"name": "DOOPS_GATEWAY_INSTANCE", "value": a.instance},
                    {"name": "PRIVATE_SETTING", "value": CANARY},
                    {"name": "TOKEN", "valueFrom": {"secretKeyRef": {"name": "enroll", "key": "token"}}}]}]}}}
    original = {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": a.workload, "namespace": a.namespace,
        "uid": "deployment-uid", "resourceVersion": "12", "generation": 1,
        "annotations": {"meta.helm.sh/release-name": a.release, "meta.helm.sh/release-namespace": a.namespace}}, "spec": spec}
    old_doc = copy.deepcopy(original)
    m.container(old_doc)["env"][0]["value"] = values["gateway"]["url"]
    state = {"live": copy.deepcopy(original), "rows": [{"revision": 5, "status": "deployed"}],
             "calls": [], "receipt": {}, "backups": [], "mode": "success", "clock": 0,
             "host": "controller", "version": NEW_COMMIT, "accepting": False, "accepted": False}

    def pod(controller=False):
        image = NEW if controller else m.container(state["live"])["image"]
        name = "controller" if controller else "doops-agent"
        return {"metadata": {"name": "controller" if controller else "agent-pod", "uid": "job-pod" if controller else
                    ("old-pod" if image == OLD else "new-pod"), "ownerReferences": [{"kind": "ReplicaSet", "name": "rs", "uid": "rs-uid"}]},
                "spec": {"containers": [dict(copy.deepcopy(m.container(state["live"])), name=name, image=image)],
                         "nodeName": "hdu-node", "runtimeClassName": "runc", "volumes": spec["template"]["spec"]["volumes"],
                         "imagePullSecrets": [{"name": "registry-auth"}]},
                "status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}],
                           "containerStatuses": [{"name": name, "ready": True, "imageID": "containerd://" + image}]}}

    def resource(kind, name):
        if kind == "deployment":
            obj = copy.deepcopy(state["live"])
            obj["status"] = {"observedGeneration": obj["metadata"]["generation"], "updatedReplicas": 1,
                             "readyReplicas": 1, "availableReplicas": 1}
            return obj
        if kind == "replicaset":
            return {"metadata": {"uid": "rs-uid", "ownerReferences": [{"kind": "Deployment", "uid": "deployment-uid"}]}}
        if kind == "pod":
            obj = pod(name == "controller")
            if state["mode"] == "accept-wrong-pod" and state["accepting"]:
                obj["metadata"]["uid"] = "another-pod"
            return obj
        if kind == "pods":
            return {"items": [pod()]}
        if kind == "secret":
            if name.startswith("sh.helm.release."):
                return {"metadata": {"labels": {"doops.sh/first-upgrade": a.job_name}}}
            return {"data": {".dockerconfigjson": base64.b64encode(json.dumps({"auths": {"registry.example": {
                "auth": CANARY}}}).encode()).decode()}}
        if kind == "configmap":
            if state["receipt"].get("phase") == "awaiting-gateway" and not state["accepted"] and not state["accepting"]:
                if state["mode"] == "accept-timeout":
                    return {"data": copy.deepcopy(state["receipt"]), "metadata": {"resourceVersion": "4"}}
                state.update(accepting=True, host="agent-pod")
                try:
                    m.accept(a)
                    state["accepted"] = True
                finally:
                    state.update(accepting=False, host="controller")
            return {"data": copy.deepcopy(state["receipt"]), "metadata": {"resourceVersion": "4"}}
        raise AssertionError((kind, name))

    def fake(command, input=None, **kwargs):
        state["calls"].append((command, input))
        assert CANARY not in " ".join(command)
        output = ""
        if command[0] == "python3":
            pass
        elif command[0] == "/app/doops-agent":
            output = f"release={a.release_tag} commit={state['version']}"
        elif command[0] == "skopeo":
            auth = json.loads(Path(command[command.index("--authfile") + 1]).read_text())
            assert auth["auths"]["registry.example"]["auth"] == CANARY
            assert Path(command[command.index("--authfile") + 1]).stat().st_mode & 0o777 == 0o600
            assert kwargs["text"] is False
            output = OLD_MANIFEST if command[-1] == "docker://" + OLD else b"{}"
            if state["mode"] == "registry-digest-mismatch":
                output = OLD_MANIFEST + b" "
        elif command[0] == "kubectl":
            ix = command.index("-n")
            verb, rest = command[ix + 2], command[ix + 3:]
            if verb == "get":
                output = json.dumps(resource(rest[0], rest[1]))
            elif verb == "exec":
                if "-version" in command:
                    commit = OLD_COMMIT if m.container(state["live"])["image"] == OLD else NEW_COMMIT
                    output = f"release={a.release_tag} commit={commit}"
            elif verb == "create":
                obj = json.loads(input)
                if obj["kind"] == "Secret":
                    assert obj["immutable"] is True
                    state["backups"].append(obj)
                else:
                    assert CANARY not in input
                    state["receipt"] = obj["data"]
            elif verb == "replace":
                assert "--dry-run=server" in command
                output = input
            elif verb == "patch":
                patch = json.loads(input if input else command[command.index("-p") + 1])
                if rest[0] == "deployment":
                    assert patch[:2] == [{"op": "test", "path": "/metadata/uid", "value": "deployment-uid"},
                                        {"op": "test", "path": "/metadata/resourceVersion", "value": "14"}]
                    assert state["live"]["spec"] == old_doc["spec"]
                    if state["mode"] == "restore-cas-conflict":
                        state["live"]["spec"]["replicas"] = 2
                        return subprocess.CompletedProcess(command, 1, "", CANARY)
                    state["live"]["spec"] = patch[-1]["value"]
                elif isinstance(patch, list):
                    assert patch[0]["path"] == "/metadata/resourceVersion" and patch[1]["value"] == "awaiting-gateway"
                    state["receipt"]["gatewayAcceptance"] = patch[-1]["value"]
                else:
                    assert CANARY not in json.dumps(patch)
                    state["receipt"].update(patch["data"])
            else:
                raise AssertionError(command)
        elif command[0] == "helm":
            verb = command[1]
            if verb == "history":
                output = json.dumps(state["rows"])
            elif verb == "get":
                output = json.dumps(values) if command[2] == "values" else yaml.safe_dump(old_doc)
            elif verb == "template":
                path = Path(command[command.index("-f") + 1])
                assert path.stat().st_mode & 0o777 == 0o600
                candidate = json.loads(path.read_text())
                expected = copy.deepcopy(values)
                expected["gateway"]["url"] = a.gateway_url
                expected["image"]["digest"] = NEW.split("@")[1]
                assert candidate == expected
                new = copy.deepcopy(original)
                m.container(new).update(image=NEW, command=["new-entrypoint"],
                                        readinessProbe={"exec": {"command": ["new-health"]}})
                if state["mode"] == "drop-kubeconfig":
                    m.container(new)["volumeMounts"] = []
                if state["mode"] == "change-sa":
                    new["spec"]["template"]["spec"]["serviceAccountName"] = "new-admin"
                output = yaml.safe_dump(new)
            elif verb == "upgrade":
                assert len(state["backups"]) == 1
                if "--dry-run=server" in command:
                    if state["mode"] == "dry-run-failure":
                        return subprocess.CompletedProcess(command, 1, "", CANARY)
                    if state["mode"] == "concurrent-preflight":
                        state["live"]["spec"]["replicas"] = 2
                else:
                    if state["mode"] == "unknown-before-revision":
                        raise subprocess.TimeoutExpired(command, 210, stderr=CANARY)
                    m.container(state["live"]).update(image=NEW, command=["new-entrypoint"],
                                                      readinessProbe={"exec": {"command": ["new-health"]}})
                    state["live"]["metadata"]["resourceVersion"] = "13"
                    state["rows"][0]["status"] = "superseded"
                    state["rows"].append({"revision": 6, "status": "deployed", "description": command[command.index("--description") + 1]})
                    if state["mode"] in ("unknown-after-upgrade", "concurrent-before-rollback", "concurrent-during-rollback", "restore-cas-conflict", "rollback-response-lost"):
                        if state["mode"] == "concurrent-before-rollback":
                            state["live"]["spec"]["replicas"] = 2
                        raise subprocess.TimeoutExpired(command, 210, stderr=CANARY)
            elif verb == "rollback":
                assert command[3] == "5"
                state["live"]["spec"] = copy.deepcopy(old_doc["spec"])
                state["live"]["metadata"]["resourceVersion"] = "14"
                state["rows"][-1]["status"] = "superseded"
                state["rows"].append({"revision": 7, "status": "deployed", "description": "Rollback to 5"})
                if state["mode"] == "concurrent-during-rollback":
                    state["live"]["spec"]["replicas"] = 2
                if state["mode"] == "rollback-response-lost":
                    raise subprocess.TimeoutExpired(command, 330, stderr=CANARY)
            else:
                raise AssertionError(command)
        else:
            raise AssertionError(command)
        return subprocess.CompletedProcess(command, 0, output, "")

    monkeypatch.setattr(m.subprocess, "run", fake)
    monkeypatch.setattr(m.socket, "gethostname", lambda: state["host"])
    monkeypatch.setattr(m.signal, "signal", lambda *_: None)
    monkeypatch.setattr(m.signal, "alarm", lambda *_: None)
    monkeypatch.setattr(m.time, "monotonic", lambda: state["clock"])
    monkeypatch.setattr(m.time, "sleep", lambda seconds: state.update(clock=state["clock"] + seconds))
    monkeypatch.setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
    for key, value in {"DOOPS_GATEWAY_CLUSTER": a.cluster, "DOOPS_GATEWAY_INSTANCE": a.instance,
                       "DOOPS_GATEWAY_URL": a.gateway_url}.items():
        monkeypatch.setenv(key, value)
    return m, a, state, original


def test_render_on_old_hdu_agent_preserves_only_kubeconfig(transaction, capsys):
    m, a, s, original = transaction
    s.update(host="old-agent", version=OLD_COMMIT)
    m.render(a)
    docs = json.loads(capsys.readouterr().out)["items"]
    assert [d["kind"] for d in docs] == ["ConfigMap", "Job"]
    assert docs[0]["immutable"] and CANARY not in json.dumps(docs)
    job = docs[1]["spec"]
    assert (job["backoffLimit"], job["activeDeadlineSeconds"]) == (0, 3600)
    pod = job["template"]["spec"]
    assert pod["nodeName"] == "hdu-node" and pod["runtimeClassName"] == "runc"
    assert pod["terminationGracePeriodSeconds"] == 900 and not pod["automountServiceAccountToken"]
    assert "serviceAccountName" not in pod
    assert pod["volumes"][0] == original["spec"]["template"]["spec"]["volumes"][0]
    assert pod["containers"][0]["image"] == NEW
    assert pod["containers"][0]["env"] == [{"name": "KUBECONFIG", "value": "/root/.kube/config"}]
    assert not any(c[0][0] == "kubectl" and "create" in c[0] for c in s["calls"])


def test_complete_requires_real_gateway_accept_and_stability(transaction, capsys):
    m, a, s, _ = transaction
    m.upgrade(a)
    assert s["receipt"]["phase"] == "complete" and s["accepted"] and s["clock"] == 15
    assert m.container(s["live"])["env"][0]["value"] == a.gateway_url
    assert m.container(s["live"])["command"] == ["new-entrypoint"]
    assert m.container(s["live"])["readinessProbe"] == {"exec": {"command": ["new-health"]}}
    backup = s["backups"][0]["stringData"]
    assert set(backup) == {"history.json", "values.json", "all-values.json", "manifest.yaml", "deployment.json", "baseline.json"}
    assert CANARY in backup["values.json"] and CANARY in backup["deployment.json"]
    assert "serviceAccountName" not in s["live"]["spec"]["template"]["spec"]
    assert json.loads(s["receipt"]["gatewayAcceptance"]) == {
        "podUID": "new-pod", "runtimeDigest": NEW.split("@")[1], "release_commit": NEW_COMMIT}
    assert CANARY not in capsys.readouterr().out


@pytest.mark.parametrize("mode", ["drop-kubeconfig", "change-sa", "dry-run-failure", "concurrent-preflight", "registry-digest-mismatch"])
def test_preflight_fails_before_upgrade_mutation(transaction, mode, capsys):
    m, a, s, _ = transaction
    s["mode"] = mode
    with pytest.raises(m.UpgradeError):
        m.upgrade(a)
    assert s["receipt"]["phase"] == "preflight-failed"
    if mode == "dry-run-failure":
        assert s["receipt"]["failureStep"] == "helm-upgrade-dry-run"
        assert s["receipt"]["failureCategory"] == "command_exit_nonzero"
    if mode == "registry-digest-mismatch":
        assert s["receipt"]["failureCategory"] == "old_reference_digest_mismatch"
        assert not s["backups"]
    assert not any(c[:2] == ["helm", "rollback"] or c[:2] == ["helm", "upgrade"] and
                   "--dry-run=server" not in c for c, _ in s["calls"])
    assert CANARY not in capsys.readouterr().out


@pytest.mark.parametrize("mode,phase", [
    ("unknown-after-upgrade", "rolled-back"), ("rollback-response-lost", "rolled-back"),
    ("unknown-before-revision", "failed-unchanged"), ("accept-timeout", "rolled-back"),
    ("concurrent-before-rollback", "rollback-failed"), ("concurrent-during-rollback", "rollback-failed"),
    ("restore-cas-conflict", "rollback-failed")])
def test_failed_transactions_keep_nonzero_and_guard_concurrent_changes(transaction, mode, phase, capsys):
    m, a, s, original = transaction
    s["mode"] = mode
    with pytest.raises(m.UpgradeError):
        m.upgrade(a)
    assert s["receipt"]["phase"] == phase
    if mode.startswith("unknown-"):
        assert s["receipt"]["failureStep"] == "helm-upgrade"
        assert s["receipt"]["failureCategory"] == "command_timeout"
    if mode == "restore-cas-conflict":
        assert s["receipt"]["recoveryStep"] == "kubectl-patch-deployment"
        assert s["receipt"]["recoveryCategory"] == "command_exit_nonzero"
    calls = [c for c, _ in s["calls"]]
    rollbacks = [c for c in calls if c[:2] == ["helm", "rollback"]]
    assert len(rollbacks) == int(mode not in ("unknown-before-revision", "concurrent-before-rollback"))
    if phase in ("rolled-back", "failed-unchanged"):
        assert s["live"]["spec"] == original["spec"]
        assert s["receipt"]["releaseCommit"] == OLD_COMMIT
        assert s["receipt"]["gatewayAcceptance"] == "" and s["receipt"]["podUID"] == "old-pod"
    else:
        assert s["live"]["spec"]["replicas"] == 2
    assert CANARY not in capsys.readouterr().out


def test_external_accept_cannot_accept_another_gateway_pod(transaction, capsys):
    m, a, s, original = transaction
    s["mode"] = "accept-wrong-pod"
    with pytest.raises(m.UpgradeError):
        m.upgrade(a)
    assert not s["accepted"] and s["receipt"]["phase"] == "rolled-back"
    assert s["live"]["spec"] == original["spec"]
    assert CANARY not in capsys.readouterr().out


@pytest.mark.parametrize("layout,issue", [
    ("single", None), ("oci-index", None), ("docker-list", None),
    ("single", "moved-tag"), ("oci-index", "missing-runtime"),
    ("oci-index", "wrong-child"), ("oci-index", "wrong-size")])
def test_rollback_registry_identity_at_subprocess_boundary(transaction, monkeypatch, layout, issue):
    m, a, s, _ = transaction
    reference = "registry.example:5000/agent:zheyin-running"
    child_ref = "docker://registry.example:5000/agent@" + OLD_DIGEST
    descriptor = {"digest": OLD_DIGEST, "size": len(OLD_MANIFEST),
                  "mediaType": "application/vnd.oci.image.manifest.v1+json", "platform": {"os": "linux", "architecture": "amd64"}}
    if issue == "missing-runtime":
        descriptor["digest"] = "sha256:" + "e" * 64
    if issue == "wrong-size":
        descriptor["size"] += 1
    index = {"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json" if layout == "oci-index" else
             "application/vnd.docker.distribution.manifest.list.v2+json", "manifests": [
                 {"digest": "sha256:" + "d" * 64, "size": 99, "mediaType": descriptor["mediaType"],
                  "platform": {"os": "linux", "architecture": "arm64"}}, descriptor]}
    raw = OLD_MANIFEST if layout == "single" else json.dumps(index).encode()
    if issue == "moved-tag":
        raw += b" "
    child = OLD_MANIFEST.replace(b"aaaa", b"eeee") if issue == "wrong-child" else OLD_MANIFEST
    calls = []
    original_run = m.subprocess.run

    def registry_subprocess(command, **kwargs):
        if command[0] != "skopeo":
            return original_run(command, **kwargs)
        calls.append(command[-1])
        assert kwargs["text"] is False
        assert Path(command[command.index("--authfile") + 1]).stat().st_mode & 0o777 == 0o600
        output = {"docker://" + reference: raw, child_ref: child, "docker://" + NEW: b"{}"}[command[-1]]
        return subprocess.CompletedProcess(command, 0, output, b"")

    monkeypatch.setattr(m.subprocess, "run", registry_subprocess)
    if issue:
        category = {"moved-tag": "old_runtime_manifest_mismatch", "missing-runtime": "old_runtime_descriptor_mismatch",
                    "wrong-child": "old_runtime_manifest_mismatch", "wrong-size": "old_runtime_descriptor_size_mismatch"}[issue]
        with pytest.raises(m.UpgradeError, match=category):
            m.registry(a, reference, {"imagePullSecrets": [{"name": "registry-auth"}]}, OLD_DIGEST)
        assert "docker://" + NEW not in calls
    else:
        m.registry(a, reference, {"imagePullSecrets": [{"name": "registry-auth"}]}, OLD_DIGEST)
        assert calls == ["docker://" + reference] + ([child_ref] if layout != "single" else []) + ["docker://" + NEW]
