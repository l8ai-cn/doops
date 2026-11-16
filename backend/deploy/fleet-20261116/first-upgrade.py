#!/usr/bin/env python3
"""One-shot fleet bootstrap. Run render on the old Agent and accept on the new
Agent through Gateway exec; apply the rendered List through that same channel.
run belongs only to the detached Job. All three commands take the same flags.
No permissions are provisioned: render requires an independent mounted kubeconfig.
"""
import argparse
import base64
import copy
import hashlib
import importlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import tempfile
import time

import yaml

A = None


class UpgradeError(Exception):
    def __init__(self, category, step="validation"):
        self.category, self.step = category, step
        super().__init__(category)


def require(ok, category):
    if not ok:
        raise UpgradeError(category)


def run(command, stdin=None, timeout=30, raw=False):
    executable = Path(command[0]).name
    step = {"python3": "configure-kubeconfig", "skopeo": "registry-pull", "doops-agent": "local-version"}.get(executable, "helper-command")
    if executable == "helm":
        step = "helm-" + command[1] + ("-" + command[2] if command[1] == "get" else "")
        if "--dry-run=server" in command:
            step += "-dry-run"
    elif executable == "kubectl":
        args = command[command.index("-n") + 2:]
        step = "kubectl-" + args[0]
        if args[0] in ("get", "patch"):
            step += "-" + args[1]
        elif args[0] == "create":
            step += "-" + json.loads(stdin)["kind"].lower()
    try:
        result = subprocess.run(command, input=stdin, text=not raw, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise UpgradeError("command_timeout", step) from None
    except OSError:
        raise UpgradeError("command_os_error", step) from None
    if result.returncode != 0:
        raise UpgradeError("command_exit_nonzero", step)
    return result.stdout


def failure(error):
    return {"failureCategory": error.category if isinstance(error, UpgradeError) else "controller_exception",
            "failureStep": error.step if isinstance(error, UpgradeError) else "controller",
            "failureType": type(error).__name__}


def k(a, *args, **kwargs):
    return run(["kubectl", "--request-timeout=20s", "-n", a.namespace, *args], **kwargs)


def get(a, kind, name):
    return json.loads(k(a, "get", kind, name, "-o", "json"))


def helm(a, *args, **kwargs):
    return run(["helm", *args, "-n", a.namespace], **kwargs)


def history(a, revision=None):
    rows = json.loads(helm(a, "history", a.release, "--max", "2147483647", "-o", "json"))
    if revision is not None:
        require(max(int(r["revision"]) for r in rows) == revision and
                [int(r["revision"]) for r in rows if r["status"] == "deployed"] == [revision],
                "revision_changed")
    return rows


def container(deployment):
    return next(c for c in deployment["spec"]["template"]["spec"]["containers"] if c["name"] == "doops-agent")


def target(a, env):
    require((env.get("DOOPS_GATEWAY_CLUSTER"), env.get("DOOPS_GATEWAY_INSTANCE"),
             env.get("DOOPS_GATEWAY_URL")) == (a.cluster, a.instance, a.gateway_url), "live_target_mismatch")


def local_version(commit, release=None):
    version = run(["/app/doops-agent", "-version"]).strip()
    require(re.fullmatch(r"release=\S+ commit=" + commit, version) and
            (release is None or version == f"release={release} commit={commit}"), "local_version_mismatch")


def receipt(a, **fields):
    # Call sites supply explicit metadata only; never reflect exceptions or command output.
    data = {key: str(value) for key, value in fields.items()}
    k(a, "patch", "configmap", a.job_name + "-receipt", "--type=merge", "-p", json.dumps({"data": data}))
    print(json.dumps(data, sort_keys=True), flush=True)


def render(a):
    target(a, os.environ)
    local_version(a.old_commit)
    live = get(a, "deployment", a.workload)
    pod = get(a, "pod", socket.gethostname())
    owner = pod["metadata"]["ownerReferences"]
    require(len(owner) == 1 and owner[0]["kind"] == "ReplicaSet", "bootstrap_pod_owner_mismatch")
    rs = get(a, "replicaset", owner[0]["name"])
    require(rs["metadata"]["uid"] == owner[0]["uid"] and any(
        r["uid"] == live["metadata"]["uid"] and r["kind"] == "Deployment"
        for r in rs["metadata"]["ownerReferences"]), "bootstrap_workload_mismatch")
    source = pod["spec"]
    agent = next(c for c in source["containers"] if c["name"] == "doops-agent")
    mounts = [m for m in agent.get("volumeMounts", []) if
              m["mountPath"].rstrip("/") in ("/root/.kube", "/root/.kube/config")]
    require(len(mounts) == 1 and source.get("nodeName"), "independent_kubeconfig_required")
    volume = next(v for v in source["volumes"] if v["name"] == mounts[0]["name"])
    require(any(key in volume for key in ("hostPath", "secret", "persistentVolumeClaim")) and
            volume["name"] != "fleet-code", "independent_kubeconfig_required")
    for key, kind, field in (("secret", "secret", "secretName"),
                             ("persistentVolumeClaim", "persistentvolumeclaim", "claimName")):
        if key in volume:
            metadata = get(a, kind, volume[key][field])["metadata"]
            require(not metadata.get("ownerReferences") and not metadata.get("annotations", {}).get(
                "meta.helm.sh/release-name"), "kubeconfig_source_owned")
    params = {key: value for key, value in vars(a).items() if key != "action"}
    command = ["run"]
    for key, value in params.items():
        command.extend(["--" + key.replace("_", "-"), str(value)])
    spec = {key: copy.deepcopy(source[key]) for key in ("nodeName", "runtimeClassName", "imagePullSecrets",
            "tolerations", "hostAliases", "dnsPolicy", "dnsConfig") if key in source}
    spec.update(restartPolicy="Never", automountServiceAccountToken=False, terminationGracePeriodSeconds=900,
                volumes=[volume, {"name": "fleet-code", "configMap": {"name": a.job_name + "-code"}}],
                containers=[{"name": "controller", "image": a.image, "imagePullPolicy": "IfNotPresent",
                    "command": ["python3", "/release/first-upgrade.py"], "args": command,
                    "env": [{"name": "KUBECONFIG", "value": "/root/.kube/config"}],
                    "volumeMounts": [dict(mounts[0], readOnly=True),
                                     {"name": "fleet-code", "mountPath": "/release", "readOnly": True}]}])
    docs = [{"apiVersion": "v1", "kind": "ConfigMap", "immutable": True,
             "metadata": {"name": a.job_name + "-code", "namespace": a.namespace},
             "data": {"first-upgrade.py": Path(__file__).read_text(), "parameters.json": json.dumps(params)}},
            {"apiVersion": "batch/v1", "kind": "Job",
             "metadata": {"name": a.job_name, "namespace": a.namespace,
                          "labels": {"app.kubernetes.io/name": "doops-self-upgrade"}},
             "spec": {"backoffLimit": 0, "activeDeadlineSeconds": 3600,
                      "template": {"spec": spec}}}]
    print(json.dumps({"apiVersion": "v1", "kind": "List", "items": docs}))


def normalized(a, live, document):
    obj = copy.deepcopy(live)
    obj.pop("status", None)
    obj["spec"] = copy.deepcopy(document["spec"])
    return json.loads(k(a, "replace", "--dry-run=server", "-f", "-", "-o", "json", stdin=json.dumps(obj)))["spec"]


def documents(a, manifest):
    docs = [d for d in yaml.safe_load_all(manifest) if d]
    keys = [(d["apiVersion"], d["kind"], d["metadata"].get("namespace", ""), d["metadata"]["name"]) for d in docs]
    require(len(set(keys)) == len(keys) and all(not d["metadata"].get("annotations", {}).get("helm.sh/hook")
            for d in docs), "unsupported_chart_resources")
    require(sum(d["kind"] == "Deployment" and d["metadata"]["name"] == a.workload for d in docs) == 1,
            "chart_workload_mismatch")
    return dict(zip(keys, docs))


def authority(spec):
    spec = copy.deepcopy(spec)
    for key in ("strategy", "minReadySeconds", "progressDeadlineSeconds", "revisionHistoryLimit"):
        spec.pop(key, None)
    agent = container({"spec": spec})
    for key in ("image", "imagePullPolicy", "command", "args", "startupProbe", "readinessProbe", "livenessProbe"):
        agent.pop(key, None)
    return spec


def registry(a, old_image, pod, old_digest):
    auths = {}
    for ref in pod.get("imagePullSecrets", []):
        data = get(a, "secret", ref["name"])["data"]
        auths.update(json.loads(base64.b64decode(data[".dockerconfigjson"], validate=True))["auths"])
    with tempfile.NamedTemporaryFile("w") as auth:
        json.dump({"auths": auths}, auth)
        auth.flush()
        command = ["skopeo", "inspect", "--raw", "--authfile", auth.name]
        raw = run(command + ["docker://" + old_image], timeout=120, raw=True)
        manifest = json.loads(raw)
        media_types = ("application/vnd.oci.image.manifest.v1+json",
                       "application/vnd.docker.distribution.manifest.v2+json")
        if "@" in old_image:
            require("sha256:" + hashlib.sha256(raw).hexdigest() == old_image.rsplit("@", 1)[1],
                    "old_reference_digest_mismatch")
        if "manifests" in manifest:
            require(manifest.get("schemaVersion") == 2 and manifest.get("mediaType") in (
                "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"),
                "old_index_invalid")
            matches = [d for d in manifest["manifests"] if d.get("digest") == old_digest]
            require(len(matches) == 1 and matches[0].get("mediaType") in media_types, "old_runtime_descriptor_mismatch")
            repository = old_image.split("@", 1)[0]
            if ":" in repository.rsplit("/", 1)[-1]:
                repository = repository.rsplit(":", 1)[0]
            raw = run(command + ["docker://" + repository + "@" + old_digest], timeout=120, raw=True)
            require(len(raw) == matches[0].get("size"), "old_runtime_descriptor_size_mismatch")
            manifest = json.loads(raw)
        require(manifest.get("schemaVersion") == 2 and manifest.get("mediaType") in media_types and
                "manifests" not in manifest and "config" in manifest and "layers" in manifest and
                "sha256:" + hashlib.sha256(raw).hexdigest() == old_digest, "old_runtime_manifest_mismatch")
        run(command + ["docker://" + a.image], timeout=120, raw=True)


def verify(a, image, commit, digest):
    return A.verify(a.namespace, a.workload, image, runtime_digest=digest, expected_commit=commit,
                    deadline_seconds=240, clock=time.monotonic, sleep=time.sleep)


def interrupted(_signum, _frame):
    raise UpgradeError("interrupted")


def upgrade(a):
    os.umask(0o077)
    os.environ["HOSTNAME"] = socket.gethostname()
    os.environ["KUBECONFIG"] = "/tmp/fleet-kubeconfig"
    attempted = created = False
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGALRM):
        signal.signal(sig, interrupted)
    signal.alarm(1800)
    try:
        run(["python3", "/app/configure_kubeconfig.py", "/root/.kube/config", os.environ["KUBECONFIG"],
             os.environ["KUBERNETES_SERVICE_HOST"], os.environ.get("KUBERNETES_SERVICE_PORT_HTTPS", "443")])
        k(a, "create", "-f", "-", stdin=json.dumps({"apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": a.job_name + "-receipt", "namespace": a.namespace},
            "data": {"phase": "preflight", "image": a.image, "sourceCommit": a.source_commit,
                     "namespace": a.namespace, "release": a.release, "workload": a.workload,
                     "cluster": a.cluster, "instance": a.instance, "gatewayURL": a.gateway_url}}))
        created = True
        require(os.environ.get("HELM_DRIVER", "secret") == "secret", "helm_secret_driver_required")
        local_version(a.source_commit, a.release_tag)
        digest = A.controller_runtime_digest(a.namespace, a.image)
        require(digest == a.image.split("@")[1], "platform_digest_required")
        rows = history(a, a.base_revision)
        live = get(a, "deployment", a.workload)
        meta = live["metadata"]
        require(meta.get("annotations", {}).get("meta.helm.sh/release-name") == a.release and
                meta["annotations"].get("meta.helm.sh/release-namespace") == a.namespace and
                live["spec"].get("replicas", 1) == 1, "live_release_mismatch")
        target(a, A.env_map(container(live)))
        old_image = container(live)["image"]
        selector = ",".join(f"{key}={value}" for key, value in sorted(live["spec"]["selector"]["matchLabels"].items()))
        pods = get(a, "pods", "-l=" + selector)["items"]
        pods = [p for p in pods if not p["metadata"].get("deletionTimestamp")]
        require(len(pods) == 1 and A.runtime_digest_of(pods[0]), "baseline_runtime_unknown")
        old_digest = A.runtime_digest_of(pods[0])
        baseline = verify(a, old_image, a.old_commit, old_digest)
        values_raw = helm(a, "get", "values", a.release, "--revision", str(a.base_revision), "-o", "json")
        manifest = helm(a, "get", "manifest", a.release, "--revision", str(a.base_revision))
        values = json.loads(values_raw)
        require((values["gateway"]["cluster"], values["gateway"]["instance"]) == (a.cluster, a.instance),
                "helm_target_mismatch")
        registry(a, old_image, live["spec"]["template"]["spec"], old_digest)
        backup = {"history.json": json.dumps(rows), "values.json": values_raw, "manifest.yaml": manifest,
                  "all-values.json": helm(a, "get", "values", a.release, "--revision", str(a.base_revision), "--all", "-o", "json"),
                  "deployment.json": json.dumps(live), "baseline.json": json.dumps(baseline)}
        k(a, "create", "-f", "-", stdin=json.dumps({"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
            "immutable": True, "metadata": {"name": a.job_name + "-backup", "namespace": a.namespace}, "stringData": backup}))
        candidate = copy.deepcopy(values)
        candidate["gateway"]["url"] = a.gateway_url
        repository, digest = a.image.split("@")
        candidate.setdefault("image", {}).update(repository=repository, digest=digest)
        old_docs = documents(a, manifest)
        old = next(d for d in old_docs.values() if d["kind"] == "Deployment")
        old_spec = normalized(a, live, old)
        corrected = copy.deepcopy(old_spec)
        old_env = container({"spec": corrected})["env"]
        route = [e for e in old_env if e["name"] == "DOOPS_GATEWAY_URL"]
        require(len(route) == 1 and "value" in route[0], "helm_route_unknown")
        route[0]["value"] = a.gateway_url
        require(corrected == live["spec"] and container(old)["image"] == old_image, "unsupported_live_drift")
        with tempfile.NamedTemporaryFile("w", suffix=".json") as vf:
            json.dump(candidate, vf)
            vf.flush()
            new_docs = documents(a, helm(a, "template", a.release, A.CHART, "-f", vf.name))
            require(new_docs.keys() == old_docs.keys(), "chart_resource_set_changed")
            for key in old_docs:
                if old_docs[key]["kind"] != "Deployment":
                    require(new_docs[key] == old_docs[key], "chart_authority_changed")
            new = next(d for d in new_docs.values() if d["kind"] == "Deployment")
            new_spec = normalized(a, live, new)
            require(container(new)["image"] == a.image and authority(new_spec) == authority(live["spec"]),
                    "live_authority_changed")
            marker = "fleet:" + a.job_name + ":" + meta["uid"]
            command = ["upgrade", a.release, A.CHART, "-f", vf.name, "--reset-values", "--history-max", "0",
                       "--description", marker, "--labels", "doops.sh/first-upgrade=" + a.job_name,
                       "--no-hooks", "--timeout", "180s", "--wait=false"]
            helm(a, *command, "--dry-run=server", timeout=210)
            history(a, a.base_revision)
            now = get(a, "deployment", a.workload)
            require(now["metadata"]["uid"] == meta["uid"] and now["spec"] == live["spec"], "baseline_changed")
            receipt(a, phase="upgrading", backupSecret=a.job_name + "-backup", baselineRevision=a.base_revision)
            attempted = True
            helm(a, *command, timeout=210)
        history(a, a.base_revision + 1)
        evidence = verify(a, a.image, a.source_commit, digest)
        receipt(a, phase="awaiting-gateway", podUID=evidence["podUID"], runtimeDigest=evidence["runtimeDigest"])
        deadline = time.monotonic() + 300
        expected = {key: evidence[key] for key in ("podUID", "runtimeDigest", "release_commit")}
        while True:
            data = get(a, "configmap", a.job_name + "-receipt")["data"]
            if data.get("gatewayAcceptance"):
                require(json.loads(data["gatewayAcceptance"]) == expected, "gateway_acceptance_mismatch")
                break
            require(time.monotonic() < deadline, "gateway_acceptance_timeout")
            time.sleep(5)
        time.sleep(15)
        require(verify(a, a.image, a.source_commit, digest) == evidence, "candidate_changed")
        history(a, a.base_revision + 1)
        require(get(a, "deployment", a.workload)["spec"] == new_spec, "candidate_spec_changed")
        receipt(a, phase="complete", releaseCommit=evidence["release_commit"], revision=a.base_revision + 1)
    except BaseException as error:
        diagnosis = failure(error)
        phase = "preflight-failed"
        if attempted:
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            signal.alarm(800)
            try:
                # A command error is an unknown result: inspect history before any recovery mutation.
                latest = max(history(a), key=lambda r: int(r["revision"]))
                now = get(a, "deployment", a.workload)
                require(now["metadata"]["uid"] == meta["uid"], "rollback_uid_changed")
                if int(latest["revision"]) == a.base_revision:
                    history(a, a.base_revision)
                    require(now["spec"] == live["spec"], "rollback_spec_changed")
                    phase = "failed-unchanged"
                else:
                    require(int(latest["revision"]) == a.base_revision + 1 and
                            get(a, "secret", f"sh.helm.release.v1.{a.release}.v{a.base_revision + 1}")["metadata"].get(
                                "labels", {}).get("doops.sh/first-upgrade") == a.job_name and
                            now["spec"] in (live["spec"], new_spec), "concurrent_change_blocks_rollback")
                    try:
                        helm(a, "rollback", a.release, str(a.base_revision), "--no-hooks", "--history-max", "0",
                             "--timeout", "300s", timeout=330)
                    except Exception:
                        pass  # Read the result below; never replay a rollback with an unknown outcome.
                    rolled = max(history(a, a.base_revision + 2), key=lambda r: int(r["revision"]))
                    require(rolled.get("description") == f"Rollback to {a.base_revision}", "rollback_unconfirmed")
                    now = get(a, "deployment", a.workload)
                    require(now["metadata"]["uid"] == meta["uid"] and now["spec"] == old_spec, "rollback_spec_changed")
                    k(a, "patch", "deployment", a.workload, "--type=json", "--patch-file=/dev/stdin", stdin=json.dumps([
                        {"op": "test", "path": "/metadata/uid", "value": meta["uid"]},
                        {"op": "test", "path": "/metadata/resourceVersion", "value": now["metadata"]["resourceVersion"]},
                        {"op": "replace", "path": "/spec", "value": live["spec"]}]))
                    require(get(a, "deployment", a.workload)["spec"] == live["spec"], "restored_spec_changed")
                    phase = "rolled-back"
                restored = verify(a, old_image, a.old_commit, old_digest)
                receipt(a, **diagnosis, phase=phase, image=old_image, sourceCommit=a.old_commit, releaseCommit=restored["release_commit"],
                        podUID=restored["podUID"], runtimeDigest=restored["runtimeDigest"], gatewayAcceptance="")
            except BaseException as recovery_error:
                recovery = failure(recovery_error)
                receipt(a, **diagnosis, phase="rollback-failed", recoveryCategory=recovery["failureCategory"],
                        recoveryStep=recovery["failureStep"], recoveryType=recovery["failureType"])
                raise UpgradeError("automatic_rollback_failed") from None
        elif created:
            receipt(a, phase=phase, **diagnosis)
        raise UpgradeError(diagnosis["failureCategory"], diagnosis["failureStep"]) from None
    finally:
        signal.alarm(0)


def accept(a):
    target(a, os.environ)
    local_version(a.source_commit, a.release_tag)
    stored = get(a, "configmap", a.job_name + "-receipt")
    data = stored["data"]
    require(all(data.get(key) == str(value) for key, value in dict(phase="awaiting-gateway", image=a.image,
        sourceCommit=a.source_commit, namespace=a.namespace, release=a.release, workload=a.workload,
        cluster=a.cluster, instance=a.instance, gatewayURL=a.gateway_url).items()), "acceptance_scope_mismatch")
    pod = get(a, "pod", socket.gethostname())
    require(pod["metadata"]["uid"] == data["podUID"] and not pod["metadata"].get("deletionTimestamp"), "acceptance_wrong_pod")
    evidence = verify(a, a.image, a.source_commit, a.image.split("@")[1])
    require(evidence["podUID"] == data["podUID"] and evidence["runtimeDigest"] == data["runtimeDigest"], "acceptance_runtime_mismatch")
    accepted = {key: evidence[key] for key in ("podUID", "runtimeDigest", "release_commit")}
    k(a, "patch", "configmap", a.job_name + "-receipt", "--type=json", "-p", json.dumps([
        {"op": "test", "path": "/metadata/resourceVersion", "value": stored["metadata"]["resourceVersion"]},
        {"op": "test", "path": "/data/phase", "value": "awaiting-gateway"},
        {"op": "add", "path": "/data/gatewayAcceptance", "value": json.dumps(accepted, sort_keys=True)}]))
    print(json.dumps({"gatewayAcceptedPodUID": evidence["podUID"]}), flush=True)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("render", "run", "accept"))
    for name in ("namespace", "release", "workload", "cluster", "instance", "base-revision", "image",
                 "source-commit", "release-tag", "gateway-url", "job-name", "old-commit"):
        parser.add_argument("--" + name, required=True, type=int if name == "base-revision" else str)
    a = parser.parse_args(argv)
    try:
        require(re.fullmatch(r"[a-z0-9.:/_-]+@sha256:[0-9a-f]{64}", a.image) and
                all(re.fullmatch(r"[0-9a-f]{40}", c) for c in (a.source_commit, a.old_commit)) and
                re.fullmatch(r"https://[a-zA-Z0-9.-]+(?::[0-9]+)?", a.gateway_url) and a.base_revision > 0,
                "invalid_parameters")
        require(all(re.fullmatch(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?", value) for value in
                    (a.namespace, a.release, a.workload, a.job_name)) and len(a.job_name) <= 55 and
                all(re.fullmatch(r"[A-Za-z0-9_.-]+", value) for value in (a.cluster, a.instance, a.release_tag)),
                "invalid_target_parameters")
        if a.action != "render":
            global A
            sys.path.insert(0, "/app")
            A = importlib.import_module("adopt_agent")
            A.run = run
        {"render": render, "run": upgrade, "accept": accept}[a.action](a)
        return 0
    except BaseException as error:
        print(json.dumps(dict(outcome="failed", **failure(error))), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
