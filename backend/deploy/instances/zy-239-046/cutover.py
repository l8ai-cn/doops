"""One-time edu-coder cutover to the verified 0.2.46 pair.

Run only inside the pinned independent Job. The old Agent submits the Job and
does not mutate its own Helm release. Failure before old-release removal
uninstalls only the new release.
"""
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import time

import yaml


NAMESPACE = "default"
OLD_RELEASE = "doops-agent-239"
NEXT_RELEASE = "doops-agent-239-next"
FORMAL_RELEASE = "doops-agent-239"
RECEIPT = "doops-zy-046-cutover"
IMAGE = "docker.cnb.cool/l8ai/ai/doops.sh@sha256:9b7435cb01c71bd7290b9f7c3b604455e0b28dac2f6a28d5433ddf0aaad5bd11"
COMMIT = "e1d2ba7c24b4283efe7a1d2c0c1fa19bba960358"
RELEASE = "v0.3.12"
DO_AGENT = "do-agent 0.2.46"
NODE = "cluster-node0014.172.16.50.239"
GATEWAY = {"cluster": "doops-zheyin", "instance": "node-239", "url": "https://doops.aiedulab.cn"}
OLD_DIGEST = "sha256:352fa6eff36cc19602612bd7f00d2bfa84c47c7694bed482033fafa2c9cd2a6a"
OLD_HOME = "/var/lib/doops-agent-home-239"
NEXT_HOME = "/var/lib/doops-agent-home-239-next"
MARKER = "RELEASE_SMOKE_ZY_046"
KUBECTL = ["kubectl", "--request-timeout=20s", "-n", NAMESPACE]


def run(*args, data=None, timeout=60):
    result = subprocess.run(args, input=data, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        detail = (result.stderr or result.stdout or "").strip().splitlines()
        raise RuntimeError(f"{Path(args[0]).name} {args[1] if len(args) > 1 else ''} failed: {detail[-1][:300] if detail else 'no output'}")
    return result.stdout


def get(kind, name):
    return json.loads(run(*KUBECTL, "get", kind, name, "-o", "json"))


def receipt(**fields):
    run(*KUBECTL, "patch", "configmap", RECEIPT, "--type=merge", "-p", json.dumps({"data": {k: str(v) for k, v in fields.items()}}))
    print(json.dumps({k: v for k, v in fields.items() if k not in ("oldValues", "oldManifest", "oldDeployment")}), flush=True)


def deployment_image(deployment):
    container = next(item for item in deployment["spec"]["template"]["spec"]["containers"] if item["name"] == "doops-agent")
    return container["image"]


def live_gateway(deployment):
    container = next(item for item in deployment["spec"]["template"]["spec"]["containers"] if item["name"] == "doops-agent")
    env = {item["name"]: item.get("value", "") for item in container.get("env", [])}
    return {"cluster": env.get("DOOPS_GATEWAY_CLUSTER"), "instance": env.get("DOOPS_GATEWAY_INSTANCE"), "url": env.get("DOOPS_GATEWAY_URL")}


def ready(release_name, selector):
    deployment = get("deployment", release_name)
    assert deployment["status"].get("observedGeneration") == deployment["metadata"]["generation"]
    assert deployment["status"].get("readyReplicas") == 1
    pods = json.loads(run(*KUBECTL, "get", "pods", "-l", selector, "-o", "json"))
    assert len(pods["items"]) == 1, "candidate Pod is not unique"
    pod = pods["items"][0]
    assert not pod["metadata"].get("deletionTimestamp")
    status = pod["status"]["containerStatuses"]
    assert len(status) == 1 and status[0]["ready"] and status[0]["imageID"] == IMAGE
    assert pod["spec"]["containers"][0]["image"] == IMAGE
    assert pod["spec"]["nodeName"] == NODE
    return pod["metadata"]["name"], pod["metadata"]["uid"]


def candidate_values(live):
    values = json.loads(json.dumps(live))
    values["image"] = {"repository": "docker.cnb.cool/l8ai/ai/doops.sh", "digest": IMAGE.split("@", 1)[1], "pullPolicy": "IfNotPresent"}
    values["gateway"] = dict(GATEWAY)
    values["workload"] = {"name": NEXT_RELEASE, "selectorLabels": {"app": NEXT_RELEASE}}
    values["nodeSelector"] = {"kubernetes.io/hostname": NODE}
    values["kubeconfig"] = {"source": "mounted"}
    values["hostAccess"] = {"enabled": True}
    homes = [item for item in values.get("volumes", []) if item.get("name") == "agent-home"]
    assert len(homes) == 1 and homes[0].get("hostPath", {}).get("path") == OLD_HOME
    homes[0]["hostPath"]["path"] = NEXT_HOME
    seen = set()
    unique = []
    for item in values.get("volumes", []):
        if item.get("name") in seen:
            continue
        seen.add(item.get("name"))
        unique.append(item)
    values["volumes"] = unique
    return values


def formal_values(values):
    renamed = json.loads(json.dumps(values))
    renamed["workload"] = {"name": FORMAL_RELEASE, "selectorLabels": {"app": FORMAL_RELEASE}}
    homes = [item for item in renamed.get("volumes", []) if item.get("name") == "agent-home"]
    assert len(homes) == 1
    homes[0]["hostPath"]["path"] = OLD_HOME
    return renamed


def install(release_name, values):
    path = Path(f"/tmp/{release_name}-values.yaml")
    path.write_text(yaml.safe_dump(values, sort_keys=False), encoding="utf-8")
    command = ["helm", "upgrade", "--install", release_name, "/app/deploy/helm/doops-agent",
               "-n", NAMESPACE, "--reset-values", "-f", str(path)]
    run(*command, "--dry-run=server", timeout=120)
    run(*command, "--atomic", "--wait", "--timeout", "240s", timeout=280)


def wait_for(pod_uid, formal=False):
    deadline = time.monotonic() + 600
    uid_key = "verifiedFormalPodUID" if formal else "verifiedPodUID"
    while time.monotonic() < deadline:
        data = get("configmap", RECEIPT)["data"]
        if data.get(uid_key) == pod_uid and data.get("acceptanceSession") and data.get("functionMarker") == MARKER:
            return
        time.sleep(5)
    raise RuntimeError("Gateway acceptance was not received within 600 seconds")


def protected_test_releases():
    data = json.loads(run("kubectl", "--request-timeout=20s", "-n", "agents-test", "get", "deployments", "-o", "json"))
    return {item["metadata"]["name"]: item["metadata"]["uid"] for item in data["items"]}


def stop(signum, frame):
    raise RuntimeError(f"controller interrupted by signal {signum}")


def main():
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    source = os.environ.get("KUBECONFIG")
    assert source == "/root/.kube/config"
    # The mounted admin.conf names apiserver.cluster.local, which is not in
    # cluster DNS. Rewrite only the server address to the in-cluster Service,
    # keeping the original client certificate.
    run("python3", "/app/configure_kubeconfig.py", source, "/tmp/kubeconfig",
        os.environ["KUBERNETES_SERVICE_HOST"], os.environ["KUBERNETES_SERVICE_PORT"])
    os.environ["KUBECONFIG"] = "/tmp/kubeconfig"
    assert run("/app/doops-agent", "-version").strip() == f"release={RELEASE} commit={COMMIT}"
    assert run("/usr/local/bin/do-agent", "--version").strip() == DO_AGENT
    releases = json.loads(run("helm", "list", "-a", "-n", NAMESPACE, "-o", "json"))
    assert [item["name"] for item in releases if str(item["chart"]).startswith("doops-agent-")] == [OLD_RELEASE]
    history = run("helm", "history", OLD_RELEASE, "-n", NAMESPACE, "-o", "json")
    assert [item["revision"] for item in json.loads(history) if item["status"] == "deployed"] == [12]
    live = json.loads(run("helm", "get", "values", OLD_RELEASE, "-n", NAMESPACE, "-o", "json"))
    assert {"cluster": live["gateway"]["cluster"], "instance": live["gateway"]["instance"]} == {"cluster": GATEWAY["cluster"], "instance": GATEWAY["instance"]}
    manifest = run("helm", "get", "manifest", OLD_RELEASE, "-n", NAMESPACE)
    objects = [item for item in yaml.safe_load_all(manifest) if item]
    assert [(item["kind"], item["metadata"]["name"]) for item in objects] == [("Deployment", OLD_RELEASE)]
    old = get("deployment", OLD_RELEASE)
    assert old["status"].get("readyReplicas") == 1
    assert deployment_image(old) == "docker.cnb.cool/l8ai/ai/doops.sh:zheyin-running"
    assert live_gateway(old) == GATEWAY
    assert old["spec"]["template"]["spec"]["nodeName"] if False else old["spec"]["template"]["spec"].get("nodeSelector", {}).get("kubernetes.io/hostname") == NODE
    old_uid = old["metadata"]["uid"]
    chart = yaml.safe_load(run("helm", "show", "chart", "/app/deploy/helm/doops-agent"))
    assert (chart["name"], chart["version"]) == ("doops-agent", "0.3.0")
    nxt = candidate_values(live)
    backup = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": RECEIPT, "namespace": NAMESPACE},
              "data": {"phase": "preflight", "oldUID": old_uid, "oldHistory": history, "oldValues": json.dumps(live),
                       "oldManifest": manifest, "oldDeployment": json.dumps(old), "image": IMAGE, "commit": COMMIT,
                       "controllerSHA256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}}
    run(*KUBECTL, "create", "-f", "-", data=json.dumps(backup))
    retiring_old = False
    formal_started = False
    retiring_next = False
    try:
        receipt(phase="installing-next")
        install(NEXT_RELEASE, nxt)
        pod_name, pod_uid = ready(NEXT_RELEASE, "app=doops-agent-239-next")
        assert get("deployment", OLD_RELEASE)["metadata"]["uid"] == old_uid
        receipt(phase="candidate-ready", podName=pod_name, podUID=pod_uid)
        wait_for(pod_uid)
        assert ready(NEXT_RELEASE, "app=doops-agent-239-next") == (pod_name, pod_uid)
        assert get("deployment", OLD_RELEASE)["metadata"]["uid"] == old_uid
        receipt(phase="retiring-old")
        retiring_old = True
        run("helm", "uninstall", OLD_RELEASE, "-n", NAMESPACE, "--wait", "--timeout", "180s", timeout=220)
        remaining = json.loads(run(*KUBECTL, "get", "deployments", "-o", "json"))
        assert OLD_RELEASE not in [item["metadata"]["name"] for item in remaining["items"]]
        receipt(phase="installing-formal")
        formal_started = True
        install(FORMAL_RELEASE, formal_values(nxt))
        formal_name, formal_uid = ready(FORMAL_RELEASE, "app=doops-agent-239")
        receipt(phase="formal-ready", formalPodName=formal_name, formalPodUID=formal_uid)
        wait_for(formal_uid, formal=True)
        assert ready(FORMAL_RELEASE, "app=doops-agent-239") == (formal_name, formal_uid)
        receipt(phase="retiring-next")
        retiring_next = True
        run("helm", "uninstall", NEXT_RELEASE, "-n", NAMESPACE, "--wait", "--timeout", "180s", timeout=220)
        assert ready(FORMAL_RELEASE, "app=doops-agent-239") == (formal_name, formal_uid)
        receipt(phase="complete", podName=formal_name, podUID=formal_uid)
    except BaseException as error:
        print(json.dumps({"phase": "failed", "cause": str(error)[:300]}), flush=True)
        try:
            releases = json.loads(run("helm", "list", "-a", "-n", NAMESPACE, "-o", "json"))
            names = [item["name"] for item in releases]
            if not retiring_old:
                if NEXT_RELEASE in names:
                    run("helm", "uninstall", NEXT_RELEASE, "-n", NAMESPACE, "--wait", "--timeout", "180s", timeout=220)
                run(*KUBECTL, "rollout", "status", f"deployment/{OLD_RELEASE}", "--timeout=180s", timeout=220)
                assert get("deployment", OLD_RELEASE)["metadata"]["uid"] == old_uid
                receipt(phase="old-preserved", error=str(error)[:300])
            elif not retiring_next:
                if formal_started and FORMAL_RELEASE in names:
                    run("helm", "uninstall", FORMAL_RELEASE, "-n", NAMESPACE, "--wait", "--timeout", "180s", timeout=220)
                assert ready(NEXT_RELEASE, "app=doops-agent-239-next")[1] == pod_uid
                receipt(phase="next-preserved", error=str(error)[:300])
            else:
                receipt(phase="cleanup-incomplete", error=str(error)[:300])
        except BaseException as recovery_error:
            print(json.dumps({"phase": "recovery-failed", "cause": str(error)[:200], "recovery": str(recovery_error)[:200]}), flush=True)
            raise RuntimeError("cutover failed and recovery failed") from recovery_error
        raise


if __name__ == "__main__":
    main()
