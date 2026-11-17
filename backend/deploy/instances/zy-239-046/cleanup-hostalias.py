"""Remove the temporary gateway hostAlias from the Zheyin DoOps Agent.

The hostAlias existed only to bypass a stale campus DNS record. The DNS record
is now correct, so the pin is redundant and would keep shadowing any future
gateway address change. The Agent cannot rebuild its own Pod, so this runs in an
independent Job.
"""
import json
import os
import subprocess

import yaml

NAMESPACE = "default"
RELEASE = "doops-agent-239"
SOURCE_KUBECONFIG = "/root/.kube/config"


def run(*args, **kwargs):
    result = subprocess.run(args, capture_output=True, text=True, **kwargs)
    if result.returncode:
        detail = (result.stderr or result.stdout).strip().splitlines()
        raise RuntimeError(f"{args[0]} failed: {detail[-1][:300] if detail else 'no output'}")
    return result.stdout


run(
    "python3",
    "/app/configure_kubeconfig.py",
    SOURCE_KUBECONFIG,
    "/tmp/kubeconfig",
    os.environ["KUBERNETES_SERVICE_HOST"],
    os.environ["KUBERNETES_SERVICE_PORT"],
)
os.environ["KUBECONFIG"] = "/tmp/kubeconfig"

values = json.loads(run("helm", "-n", NAMESPACE, "get", "values", RELEASE, "-o", "json"))
print("current hostAliases:", json.dumps(values.get("hostAliases")), flush=True)
if "hostAliases" not in values:
    print("nothing to clean", flush=True)
    raise SystemExit(0)

deployment = json.loads(run("kubectl", "-n", NAMESPACE, "get", "deploy", RELEASE, "-o", "json"))
before_pod = run("kubectl", "-n", NAMESPACE, "get", "pods", "-l", f"app={RELEASE}",
                 "-o", "jsonpath={.items[*].metadata.name}").strip()
values.pop("hostAliases", None)

path = "/tmp/values.yaml"
with open(path, "w", encoding="utf-8") as handle:
    handle.write(yaml.safe_dump(values, sort_keys=False))

# --atomic restores the previous revision automatically if the new Pod never
# becomes Ready, so a failed cleanup cannot leave the cluster without an Agent.
run("helm", "upgrade", RELEASE, "/app/deploy/helm/doops-agent", "-n", NAMESPACE,
    "--reset-values", "-f", path, "--atomic", "--wait", "--timeout", "300s")

after = json.loads(run("kubectl", "-n", NAMESPACE, "get", "deploy", RELEASE, "-o", "json"))
assert not after["spec"]["template"]["spec"].get("hostAliases"), "hostAliases still present after upgrade"
pods = json.loads(run("kubectl", "-n", NAMESPACE, "get", "pods", "-l", f"app={RELEASE}", "-o", "json"))["items"]
# During a rolling update the terminating Pod can still be listed here, so count
# only Pods that are ready and not already being deleted.
ready = [
    item for item in pods
    if not item["metadata"].get("deletionTimestamp")
    and (item["status"].get("containerStatuses") or [{}])[0].get("ready")
]
assert len(ready) == 1, f"expected 1 ready pod, got {len(ready)}"
pod = ready[0]
hosts = run("kubectl", "-n", NAMESPACE, "exec", pod["metadata"]["name"], "--", "cat", "/etc/hosts")
assert "aiedulab" not in hosts, "hosts entry still present"

print(json.dumps({
    "removedFor": deployment["spec"]["template"]["spec"].get("hostAliases"),
    "oldPod": before_pod,
    "newPod": pod["metadata"]["name"],
    "revision": after["metadata"]["annotations"].get("deployment.kubernetes.io/revision"),
}), flush=True)
print("cleanup-complete", flush=True)
