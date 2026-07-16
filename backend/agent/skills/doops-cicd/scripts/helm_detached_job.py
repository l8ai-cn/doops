#!/usr/bin/env python3

import argparse
import hashlib
import json
import os
import re
import shlex
import subprocess
import sys
from datetime import datetime, timezone


DNS_LABEL = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
SHA256_DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")


def job_name(run_id):
    digest = hashlib.sha256(run_id.encode("utf-8")).hexdigest()[:16]
    return f"doops-cicd-helm-{digest}"


def require_dns_label(name, value):
    if not value or len(value) > 63 or not DNS_LABEL.fullmatch(value):
        raise ValueError(f"{name} must be a Kubernetes DNS label")


def require_workspace_path(name, value):
    if not os.path.isabs(value) or not os.path.normpath(value).startswith("/root/ws/"):
        raise ValueError(f"{name} must be an absolute path under /root/ws")


def shell_join(parts):
    return " ".join(shlex.quote(part) for part in parts)


def build_job_manifest(
    *,
    run_id,
    namespace,
    release,
    chart,
    values,
    image_repository,
    image_digest,
    executor_image_repository,
    executor_image_digest,
    node_selector_key,
    node_selector_value,
    kubeconfig_host_path,
    agent_home_host_path,
    timeout,
):
    require_dns_label("namespace", namespace)
    require_dns_label("release", release)
    require_workspace_path("chart", chart)
    require_workspace_path("values", values)
    if not SHA256_DIGEST.fullmatch(image_digest):
        raise ValueError("image_digest must be an immutable sha256 digest")
    if not image_repository or "@" in image_repository:
        raise ValueError("image_repository must not contain a digest")
    if not SHA256_DIGEST.fullmatch(executor_image_digest):
        raise ValueError("executor_image_digest must be an immutable sha256 digest")
    if not executor_image_repository or "@" in executor_image_repository:
        raise ValueError("executor_image_repository must not contain a digest")
    if not node_selector_key or not node_selector_value:
        raise ValueError("node selector is required")
    if not os.path.isabs(kubeconfig_host_path) or not os.path.isabs(agent_home_host_path):
        raise ValueError("host paths must be absolute")
    if not re.fullmatch(r"[1-9][0-9]*[smh]", timeout):
        raise ValueError("timeout must be a positive Helm duration")

    executor_image = f"{executor_image_repository}@{executor_image_digest}"
    helm_command = shell_join(
        [
            "helm",
            "upgrade",
            "--install",
            release,
            chart,
            "--namespace",
            namespace,
            "--values",
            values,
            "--set-string",
            f"image.repository={image_repository}",
            "--set-string",
            f"image.digest={image_digest}",
            "--atomic",
            "--wait",
            "--timeout",
            timeout,
        ]
    )
    command = (
        'python3 /app/configure_kubeconfig.py /root/.kube/config /tmp/doops-kubeconfig '
        '"$KUBERNETES_SERVICE_HOST" "${KUBERNETES_SERVICE_PORT_HTTPS:-443}"'
        " && export KUBECONFIG=/tmp/doops-kubeconfig"
        f" && {helm_command}"
    )
    name = job_name(run_id)
    manifest = {
        "apiVersion": "batch/v1",
        "kind": "Job",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": {
                "app.kubernetes.io/name": "doops-cicd-helm",
                "app.kubernetes.io/part-of": "doops-cicd",
            },
        },
        "spec": {
            "backoffLimit": 0,
            "activeDeadlineSeconds": helm_timeout_seconds(timeout) + 600,
            "ttlSecondsAfterFinished": 86400,
            "template": {
                "metadata": {
                    "labels": {
                        "app.kubernetes.io/name": "doops-cicd-helm",
                        "doops.sh/run": name,
                    }
                },
                "spec": {
                    "restartPolicy": "Never",
                    "nodeSelector": {node_selector_key: node_selector_value},
                    "containers": [
                        {
                            "name": "helm",
                            "image": executor_image,
                            "imagePullPolicy": "IfNotPresent",
                            "command": ["/bin/sh", "-lc"],
                            "args": [command],
                            "volumeMounts": [
                                {
                                    "name": "kubeconfig",
                                    "mountPath": "/root/.kube/config",
                                    "readOnly": True,
                                },
                                {
                                    "name": "agent-home",
                                    "mountPath": "/root",
                                },
                            ],
                        }
                    ],
                    "volumes": [
                        {
                            "name": "kubeconfig",
                            "hostPath": {
                                "path": kubeconfig_host_path,
                                "type": "File",
                            },
                        },
                        {
                            "name": "agent-home",
                            "hostPath": {
                                "path": agent_home_host_path,
                                "type": "Directory",
                            },
                        },
                    ],
                },
            },
        },
    }
    spec_digest = hashlib.sha256(
        json.dumps(manifest["spec"], sort_keys=True, separators=(",", ":")).encode("utf-8")
    ).hexdigest()
    manifest["metadata"]["annotations"] = {"doops.sh/spec-digest": f"sha256:{spec_digest}"}
    return manifest


def helm_timeout_seconds(value):
    match = re.fullmatch(r"([1-9][0-9]*)([smh])", value)
    if match is None:
        raise ValueError("timeout must be a positive Helm duration")
    amount = int(match.group(1))
    multiplier = {"s": 1, "m": 60, "h": 3600}[match.group(2)]
    return amount * multiplier


def kubectl(args, *, input_text=None, check=True):
    return subprocess.run(
        ["kubectl", *args],
        input=input_text,
        text=True,
        capture_output=True,
        check=check,
    )


def summarize(job):
    status = job.get("status", {})
    conditions = {
        item.get("type"): item.get("status")
        for item in status.get("conditions", [])
        if item.get("type")
    }
    return {
        "job": job["metadata"]["name"],
        "namespace": job["metadata"]["namespace"],
        "specDigest": job["metadata"].get("annotations", {}).get("doops.sh/spec-digest", ""),
        "active": status.get("active", 0),
        "succeeded": status.get("succeeded", 0),
        "failed": status.get("failed", 0),
        "complete": conditions.get("Complete") == "True",
        "failure": conditions.get("Failed") == "True",
    }


def emit(subject, data):
    print(
        json.dumps(
            {
                "subject": subject,
                "observedAt": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
                "data": data,
            },
            sort_keys=True,
        )
    )


def get_job(namespace, name):
    result = kubectl(["-n", namespace, "get", "job", name, "-o", "json"], check=False)
    if result.returncode == 0:
        return json.loads(result.stdout)
    if "NotFound" in result.stderr:
        return None
    raise RuntimeError(result.stderr.strip() or "kubectl get job failed")


def submit(args):
    values = vars(args).copy()
    values.pop("command")
    values.pop("handler")
    manifest = build_job_manifest(**values)
    existing = get_job(args.namespace, manifest["metadata"]["name"])
    if existing is not None:
        expected = manifest["metadata"]["annotations"]["doops.sh/spec-digest"]
        actual = existing["metadata"].get("annotations", {}).get("doops.sh/spec-digest")
        if actual != expected:
            raise RuntimeError("existing detached Helm Job has a different spec digest")
        result = summarize(existing)
        result["created"] = False
        emit("deployment-executor", result)
        return
    created = kubectl(["create", "-f", "-"], input_text=json.dumps(manifest))
    job = json.loads(
        kubectl(
            [
                "-n",
                args.namespace,
                "get",
                "job",
                manifest["metadata"]["name"],
                "-o",
                "json",
            ]
        ).stdout
    )
    result = summarize(job)
    result["created"] = True
    result["kubectl"] = created.stdout.strip()
    emit("deployment-executor", result)


def status(args):
    job = get_job(args.namespace, job_name(args.run_id))
    if job is None:
        raise RuntimeError("detached Helm Job does not exist")
    emit("deployment-executor", summarize(job))


def parser():
    root = argparse.ArgumentParser()
    commands = root.add_subparsers(dest="command", required=True)
    submit_parser = commands.add_parser("submit")
    for name in (
        "run_id",
        "namespace",
        "release",
        "chart",
        "values",
        "image_repository",
        "image_digest",
        "executor_image_repository",
        "executor_image_digest",
        "node_selector_key",
        "node_selector_value",
        "kubeconfig_host_path",
        "agent_home_host_path",
        "timeout",
    ):
        submit_parser.add_argument("--" + name.replace("_", "-"), required=True)
    submit_parser.set_defaults(handler=submit)
    status_parser = commands.add_parser("status")
    status_parser.add_argument("--run-id", required=True)
    status_parser.add_argument("--namespace", required=True)
    status_parser.set_defaults(handler=status)
    return root


def main():
    args = parser().parse_args()
    try:
        args.handler(args)
    except (ValueError, RuntimeError, subprocess.CalledProcessError) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
