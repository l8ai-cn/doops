#!/usr/bin/env python3
"""Recover Gateway values and upgrade revision 20 of agents-test/doops-agent-test."""
import argparse
import base64
import copy
import json
import os
from pathlib import Path
import re
import signal
import socket
import sys
import tempfile
import time

import yaml


NS = "agents-test"
RELEASE = "doops-agent-test"
NAME = "zyj-test-first-upgrade-r20"
BASE_REVISION = 20
ORIGINAL_BACKUP = "zyj-test-first-upgrade-r18-backup"
GATEWAY_URL = "https://doops.aiedulab.cn"
BASE_COMMIT = "76abae08a36d920c0058486d2d8ab86880c4c121"
BASE_DIGEST = "sha256:778312b10cf925c74bd943d54b6f9509948d0a33ad414cebf87622edee76b8af"
DOAGENT_COMMIT = "64f075e37ccf06122ee4f88646b0ebd1548351da"
DOAGENT_DIGEST = "sha256:14e753702568bc2ed8bcc213c7a23a2c600788b3a0b989d84676c7a172ccef06"
VERSION = "v0.3.3"
K = ["kubectl", "--request-timeout=20s", "-n", NS]
H = ["helm"]
A = None


class UpgradeError(Exception):
    pass


def require(ok, category):
    if not ok:
        raise UpgradeError(category)


def run(args, **kwargs):
    return A.run(args, timeout=kwargs.pop("timeout", 30), **kwargs)


def get(kind, name):
    return json.loads(run(K + ["get", kind, name, "-o", "json"]))


def history():
    return json.loads(run(H + ["history", RELEASE, "-n", NS, "-o", "json"]))


def baseline_revision():
    rows = history()
    require(max(int(r["revision"]) for r in rows) == BASE_REVISION
            and [int(r["revision"]) for r in rows if r["status"] == "deployed"] == [BASE_REVISION],
            "baseline_revision_changed")
    return rows


def receipt(**fields):
    # Only explicit metadata goes here; values/manifests/diagnostics never do.
    fields = {k: str(v) for k, v in fields.items()}
    run(K + ["patch", "configmap", NAME + "-receipt", "--type=merge",
             "-p", json.dumps({"data": fields})])
    print(json.dumps(fields, sort_keys=True), flush=True)


def version(image, commit, expected_version):
    result = A.verify(NS, RELEASE, image, runtime_digest=image.rsplit("@", 1)[1],
                      expected_commit=commit, deadline_seconds=300, clock=time.monotonic)
    pods = get("pods", "-l=app=" + RELEASE)["items"]
    pod = next(p for p in pods if p["metadata"]["uid"] == result["podUID"])
    actual = run(K + ["exec", pod["metadata"]["name"], "-c", "doops-agent", "--",
                     "/usr/local/bin/do-agent", "--version"])
    require(actual.strip() == "do-agent " + expected_version, "doagent_version_mismatch")
    release_version = run(K + ["exec", pod["metadata"]["name"], "-c", "doops-agent", "--",
                              "/app/doops-agent", "-version"]).strip()
    return dict(result, podName=pod["metadata"]["name"], doopsVersion=release_version,
                doagentVersion=actual.strip())


def record(image, evidence):
    A.write_verified_receipt(NS, RELEASE, RELEASE, dict(
        evidence, image=image, outcome="succeeded", namespace=NS, release=RELEASE,
        workload=RELEASE, verification_stage="zyj-test-first-upgrade"))


def registry_preflight(old_image, image, pull_secrets):
    # Reuse self_upgrade's pull-secret authfile contract, without emitting payloads.
    auths = {}
    for ref in pull_secrets:
        data = get("secret", ref["name"])["data"]
        require(".dockerconfigjson" in data, "unsupported_pull_secret")
        auths.update(json.loads(base64.b64decode(data[".dockerconfigjson"]))["auths"])
    with tempfile.NamedTemporaryFile(mode="w", prefix="zyj-registry-") as auth:
        json.dump({"auths": auths}, auth)
        auth.flush()
        auth_args = ["--authfile", auth.name] if auths else ["--no-creds"]
        for ref in (old_image, image):
            run(["skopeo", "inspect", "--raw", *auth_args, "docker://" + ref], timeout=120)
        config = json.loads(run(["skopeo", "inspect", "--config", *auth_args,
                                 "docker://" + image], timeout=120))
    labels = config["config"].get("Labels") or {}
    require(all(labels.get(k) == v for k, v in {
        "org.l8ai.doagent.version": "0.2.45",
        "org.l8ai.doagent.revision": DOAGENT_COMMIT,
        "org.l8ai.doagent.index.digest": DOAGENT_DIGEST,
        "org.l8ai.doagent.platform.digest": DOAGENT_DIGEST,
        "org.l8ai.doagent.platform": "linux/amd64",
    }.items()), "candidate_doagent_provenance_mismatch")


def documents(manifest):
    return [d for d in yaml.safe_load_all(manifest) if d]


def check_manifest(manifest, account):
    docs = documents(manifest)
    require({(d["kind"], d["metadata"]["name"]) for d in docs} == {
        ("ServiceAccount", RELEASE),
        ("ClusterRoleBinding", NS + "-" + RELEASE + "-cluster-admin"),
        ("Deployment", RELEASE),
    } and len(docs) == 3, "unexpected_chart_resources")
    require(account != RELEASE and not any(
        s.get("name") == account for d in docs for s in d.get("subjects", [])
    ), "controller_identity_in_helm")
    require(all(d["kind"] == "ClusterRoleBinding" or d["metadata"].get("namespace", NS) == NS
                for d in docs), "chart_namespace_changed")
    return next(d for d in docs if d["kind"] == "Deployment")


def wait_acceptance(image, commit, evidence):
    receipt(phase="awaiting-gateway", image=image, sourceCommit=commit,
            podUID=evidence["podUID"], podName=evidence["podName"],
            runtimeDigest=evidence["runtimeDigest"])
    deadline = time.monotonic() + 300
    expected = {k: evidence[k] for k in ("podUID", "runtimeDigest", "release_commit")}
    while time.monotonic() < deadline:
        data = get("configmap", NAME + "-receipt")["data"]
        if data.get("gatewayAcceptance"):
            require(json.loads(data["gatewayAcceptance"]) == expected,
                    "gateway_acceptance_mismatch")
            return
        time.sleep(5)
    raise UpgradeError("gateway_acceptance_timeout")


def interrupted(_signum, _frame):
    raise UpgradeError("controller_interrupted")


def original_deployment(live):
    backup = get("secret", ORIGINAL_BACKUP)
    require(backup.get("immutable") is True, "original_backup_not_immutable")
    original = json.loads(base64.b64decode(backup["data"]["deployment.json"], validate=True))
    require(original["metadata"]["uid"] == live["metadata"]["uid"], "original_deployment_changed")
    A.validate_deployment(original, "doops-zyj", "zyj-test")
    container = A.agent_container(original)
    require(container["image"].endswith("@" + BASE_DIGEST), "original_image_changed")
    require([e.get("value") for e in container.get("env", [])
             if e["name"] == "DOOPS_GATEWAY_URL"] == [GATEWAY_URL],
            "original_gateway_route_changed")
    return original


def restore_original_deployment(original, baseline):
    live = get("deployment", RELEASE)
    require(live["metadata"]["uid"] == original["metadata"]["uid"], "rollback_deployment_changed")
    require(live["spec"] == baseline["spec"], "rollback_deployment_spec_changed")
    run(K + ["patch", "deployment", RELEASE, "--type=json", "--patch-file=/dev/stdin"],
        stdin=json.dumps([
        {"op": "test", "path": "/metadata/resourceVersion",
         "value": live["metadata"]["resourceVersion"]},
        {"op": "replace", "path": "/spec", "value": original["spec"]},
    ]))


def upgrade(args):
    os.umask(0o077)
    os.environ["HOSTNAME"] = socket.gethostname()
    os.environ["KUBECONFIG"] = "/tmp/zyj-first-upgrade-kubeconfig"
    run(["python3", "/app/configure_kubeconfig.py", "--service-account",
         os.environ["KUBECONFIG"], os.environ["KUBERNETES_SERVICE_HOST"],
         os.environ.get("KUBERNETES_SERVICE_PORT_HTTPS", "443")])
    for sig in (signal.SIGTERM, signal.SIGINT, signal.SIGALRM):
        signal.signal(sig, interrupted)
    signal.alarm(1800)  # Leave the Job deadline and grace period room for rollback.
    mutated = False
    receipt_created = False
    try:
        run(K + ["create", "-f", "-"], stdin=json.dumps({
            "apiVersion": "v1", "kind": "ConfigMap",
            "metadata": {"name": NAME + "-receipt", "namespace": NS},
            "data": {"phase": "preflight", "image": args.image, "sourceCommit": args.source_commit},
        }))
        receipt_created = True
        require(run(["/app/doops-agent", "-version"]).strip()
                == f"release={VERSION} commit={args.source_commit}", "candidate_release_mismatch")
        require(run(["/usr/local/bin/do-agent", "--version"]).strip() == "do-agent 0.2.45",
                "candidate_doagent_version_mismatch")
        controller = get("pod", socket.gethostname())
        require(controller["spec"]["serviceAccountName"] == NAME, "controller_account_mismatch")
        require(A.controller_runtime_digest(NS, args.image) == args.image.split("@")[1],
                "candidate_requires_platform_digest")
        identity = json.loads(run(["kubectl", "auth", "whoami", "-o", "json"]))
        require(identity["status"]["userInfo"]["username"] == f"system:serviceaccount:{NS}:{NAME}",
                "controller_identity_mismatch")
        for kind, name in (("serviceaccount", NAME), ("clusterrolebinding", NS + "-" + NAME)):
            meta = get(kind, name)["metadata"]
            require(not meta.get("ownerReferences") and
                    not meta.get("annotations", {}).get("meta.helm.sh/release-name"),
                    "controller_identity_owned")
        active = get("jobs", "-l=app.kubernetes.io/name=doops-self-upgrade")["items"]
        require(not any(j["metadata"]["name"] != NAME and not any(
            c["type"] in ("Complete", "Failed") and c["status"] == "True"
            for c in j.get("status", {}).get("conditions", [])
        ) for j in active), "another_upgrade_active")
        rows = baseline_revision()
        live = get("deployment", RELEASE)
        A.validate_deployment(live, "doops-zyj", "zyj-test")
        original = original_deployment(live)
        old_image = A.agent_container(live)["image"]
        require(old_image.endswith("@" + BASE_DIGEST), "baseline_image_changed")
        baseline = version(old_image, BASE_COMMIT, "0.2.42")
        values_raw = run(H + ["get", "values", RELEASE, "-n", NS, "--revision", str(BASE_REVISION), "-o", "json"])
        manifest = run(H + ["get", "manifest", RELEASE, "-n", NS, "--revision", str(BASE_REVISION)])
        values = json.loads(values_raw)
        require((values["gateway"]["cluster"], values["gateway"]["instance"]) ==
                ("doops-zyj", "zyj-test"), "target_identity_mismatch")
        old = check_manifest(manifest, NAME)
        require(A.agent_container(old)["image"] == old_image, "helm_live_image_mismatch")
        registry_preflight(old_image, args.image, live["spec"]["template"]["spec"]["imagePullSecrets"])
        backup = {
            "history.json": json.dumps(rows), "values.json": values_raw, "manifest.yaml": manifest,
            "all-values.json": run(H + ["get", "values", RELEASE, "-n", NS, "--revision", str(BASE_REVISION),
                                       "--all", "-o", "json"]),
            "deployment.json": json.dumps(live), "baseline.json": json.dumps(baseline),
            "versions.json": json.dumps({"oldCommit": BASE_COMMIT, "oldDoAgent": "0.2.42",
                                        "image": args.image, "sourceCommit": args.source_commit,
                                        "release": VERSION, "doAgent": "0.2.45"}),
        }
        run(K + ["create", "-f", "-"], stdin=json.dumps({
            "apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": True,
            "metadata": {"name": NAME + "-backup", "namespace": NS}, "stringData": backup,
        }))
        receipt(backupSecret=NAME + "-backup", baselineRevision=BASE_REVISION, baselineImage=old_image,
                originalBackup=ORIGINAL_BACKUP, gatewayURL=GATEWAY_URL)
        record(old_image, baseline)
        candidate = copy.deepcopy(values)
        candidate["gateway"]["url"] = GATEWAY_URL
        candidate.setdefault("image", {})
        repository, digest = args.image.split("@")
        candidate["image"].update(repository=repository, digest=digest, pullPolicy="IfNotPresent")
        candidate["image"].pop("tag", None)
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json") as vf:
            json.dump(candidate, vf)
            vf.flush()
            rendered = run(H + ["template", RELEASE, A.CHART, "-n", NS, "-f", vf.name])
            new = check_manifest(rendered, NAME)
            require(A.agent_container(new)["image"] == args.image, "rendered_image_mismatch")
            require(new["spec"]["selector"] == live["spec"]["selector"], "selector_changed")
            old_volumes = {v["name"] for v in old["spec"]["template"]["spec"].get("volumes") or []}
            new_volumes = {v["name"] for v in new["spec"]["template"]["spec"].get("volumes") or []}
            require(not any(m["name"] in old_volumes - new_volumes
                            for c in live["spec"]["template"]["spec"]["containers"]
                            for m in c.get("volumeMounts") or []), "stale_mounts_require_review")
            command = H + ["upgrade", RELEASE, A.CHART, "-n", NS, "-f", vf.name,
                           "--timeout", "180s", "--wait=false"]
            run(command + ["--dry-run=server"], timeout=210)
            baseline_revision()
            now = get("deployment", RELEASE)
            require(now["metadata"]["uid"] == live["metadata"]["uid"] and now["spec"] == live["spec"],
                    "baseline_deployment_changed")
            receipt(phase="upgrading")
            mutated = True  # An error/timeout can occur after Helm has changed the release.
            run(command, timeout=210)
        evidence = version(args.image, args.source_commit, "0.2.45")
        wait_acceptance(args.image, args.source_commit, evidence)
        time.sleep(15)
        final = version(args.image, args.source_commit, "0.2.45")
        require(final == evidence and [int(r["revision"]) for r in history()
                if r["status"] == "deployed"] == [BASE_REVISION + 1], "candidate_changed_during_acceptance")
        record(args.image, final)
        receipt(phase="complete", releaseCommit=final["release_commit"], revision=BASE_REVISION + 1)
    except BaseException as error:
        if mutated:
            # Do not let a second termination interrupt recovery halfway through.
            signal.signal(signal.SIGTERM, signal.SIG_IGN)
            signal.signal(signal.SIGINT, signal.SIG_IGN)
            signal.alarm(800)
            try:
                before_rollback = max(int(r["revision"]) for r in history())
                require(before_rollback in (BASE_REVISION, BASE_REVISION + 1),
                        "concurrent_revision_blocks_rollback")
                try:
                    run(H + ["rollback", RELEASE, str(BASE_REVISION), "-n", NS, "--timeout", "300s"],
                        timeout=330)
                except Exception:
                    # A lost response does not establish whether Helm applied the rollback.
                    receipt(rollbackCommandOutcome="unconfirmed")
                rolled_back = max(history(), key=lambda row: int(row["revision"]))
                require(int(rolled_back["revision"]) == before_rollback + 1
                        and rolled_back["description"] == f"Rollback to {BASE_REVISION}"
                        and rolled_back["status"] == "deployed", "rollback_result_unconfirmed")
                restore_original_deployment(original, live)
                restored = version(old_image, BASE_COMMIT, "0.2.42")
                record(old_image, restored)
                receipt(phase="rolled-back", image=old_image, sourceCommit=BASE_COMMIT,
                        releaseCommit=restored["release_commit"], podUID=restored["podUID"],
                        podName=restored["podName"], runtimeDigest=restored["runtimeDigest"],
                        gatewayAcceptance="")
            except BaseException:
                receipt(phase="rollback-failed")
                raise UpgradeError("automatic_rollback_failed") from None
        elif receipt_created:
            category = str(error) if isinstance(error, UpgradeError) else "preflight_failed"
            receipt(phase="preflight-failed", category=category)
        raise
    finally:
        signal.alarm(0)


def accept(args):
    """Run via Gateway exec on the candidate Agent, never inside the Job."""
    stored = get("configmap", NAME + "-receipt")
    data = stored["data"]
    require(data["phase"] == "awaiting-gateway" and data["image"] == args.image
            and data["sourceCommit"] == args.source_commit, "acceptance_scope_mismatch")
    require((os.environ.get("DOOPS_GATEWAY_CLUSTER"), os.environ.get("DOOPS_GATEWAY_INSTANCE"))
            == ("doops-zyj", "zyj-test"), "acceptance_wrong_target")
    pod = get("pod", socket.gethostname())
    require(pod["metadata"]["uid"] == data["podUID"], "gateway_routed_to_wrong_pod")
    container = next(c for c in pod["spec"]["containers"] if c["name"] == "doops-agent")
    require(container["image"] == args.image, "acceptance_image_mismatch")
    require(any(c["type"] == "Ready" and c["status"] == "True"
                for c in pod["status"].get("conditions", [])), "acceptance_pod_not_ready")
    require(run(["/app/doops-agent", "-version"]).strip()
            == f"release={VERSION} commit={args.source_commit}", "acceptance_commit_mismatch")
    require(run(["/usr/local/bin/do-agent", "--version"]).strip() == "do-agent 0.2.45",
            "acceptance_doagent_mismatch")
    run(["curl", "-fsS", "--max-time", "5", "http://127.0.0.1:42222/health/ready"])
    evidence = {"podUID": pod["metadata"]["uid"], "runtimeDigest": A.runtime_digest_of(pod),
                "release_commit": args.source_commit}
    require(evidence["runtimeDigest"] == args.image.split("@")[1], "acceptance_digest_mismatch")
    run(K + ["patch", "configmap", NAME + "-receipt", "--type=json", "-p", json.dumps([
        {"op": "test", "path": "/metadata/resourceVersion",
         "value": stored["metadata"]["resourceVersion"]},
        {"op": "test", "path": "/data/phase", "value": "awaiting-gateway"},
        {"op": "add", "path": "/data/gatewayAcceptance", "value": json.dumps(evidence, sort_keys=True)},
    ])])
    print(json.dumps({"gatewayAcceptedPodUID": evidence["podUID"]}), flush=True)


def render(args):
    docs = documents(Path(__file__).with_name("first-upgrade-job.yaml").read_text())
    docs[2]["data"]["first-upgrade.py"] = Path(__file__).read_text()
    container = docs[3]["spec"]["template"]["spec"]["containers"][0]
    container["image"] = args.image
    container["args"] = ["run", "--image", args.image, "--source-commit", args.source_commit]
    print(json.dumps({"apiVersion": "v1", "kind": "List", "items": docs}, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("render", "run", "accept"))
    parser.add_argument("--image", required=True)
    parser.add_argument("--source-commit", required=True)
    args = parser.parse_args()
    require(re.fullmatch(r"[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]{1,5})?"
                         r"(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+@sha256:[0-9a-f]{64}", args.image),
            "pinned_image_required")
    require(re.fullmatch(r"[0-9a-f]{40}", args.source_commit), "full_source_commit_required")
    if args.action == "render":
        render(args)
        return
    global A
    sys.path.insert(0, "/app")
    import adopt_agent
    A = adopt_agent
    (upgrade if args.action == "run" else accept)(args)


if __name__ == "__main__":
    try:
        main()
    except BaseException as error:
        if isinstance(error, SystemExit):
            raise
        # Do not reflect API/Helm/subprocess payloads or tracebacks into logs.
        category = str(error) if isinstance(error, UpgradeError) else "controller_failed"
        print(json.dumps({"outcome": "failed", "category": category}), file=sys.stderr)
        sys.exit(1)
