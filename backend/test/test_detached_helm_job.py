import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace


ROOT = Path(__file__).parents[1]
SCRIPT = ROOT / "agent" / "skills" / "doops-cicd" / "scripts" / "helm_detached_job.py"


def load_script():
    spec = importlib.util.spec_from_file_location("helm_detached_job", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def submit_args(module, candidate_digest="a"):
    return module.parser().parse_args(
        [
            "submit",
            "--run-id",
            "release-test",
            "--namespace",
            "kz-ops",
            "--release",
            "doops-agent-live",
            "--chart",
            "/root/ws/release-test/backend/deploy/helm/doops-agent",
            "--values",
            "/root/ws/release-test/backend/deploy/environments/oilan-values.yaml",
            "--image-repository",
            "docker.cnb.cool/l8ai/ai/doops.sh",
            "--image-digest",
            "sha256:" + candidate_digest * 64,
            "--executor-image-repository",
            "docker.cnb.cool/l8ai/ai/doops.sh",
            "--executor-image-digest",
            "sha256:" + "b" * 64,
            "--node-selector-key",
            "kubernetes.io/hostname",
            "--node-selector-value",
            "192.168.0.24",
            "--kubeconfig-host-path",
            "/etc/kubernetes/admin.conf",
            "--agent-home-host-path",
            "/var/lib/doops-release-runner-home",
            "--timeout",
            "10m",
        ]
    )


def test_detached_helm_job_is_immutable_and_independent():
    module = load_script()
    manifest = module.build_job_manifest(
        run_id="release-test",
        namespace="kz-ops",
        release="doops-agent-live",
        chart="/root/ws/release-test/backend/deploy/helm/doops-agent",
        values="/root/ws/release-test/backend/deploy/environments/oilan-values.yaml",
        image_repository="docker.cnb.cool/l8ai/ai/doops.sh",
        image_digest="sha256:" + "a" * 64,
        executor_image_repository="docker.cnb.cool/l8ai/ai/doops.sh",
        executor_image_digest="sha256:" + "b" * 64,
        node_selector_key="kubernetes.io/hostname",
        node_selector_value="192.168.0.24",
        kubeconfig_host_path="/etc/kubernetes/admin.conf",
        agent_home_host_path="/var/lib/doops-release-runner-home",
        timeout="10m",
    )

    assert manifest["kind"] == "Job"
    assert manifest["metadata"]["namespace"] == "kz-ops"
    assert manifest["metadata"]["name"] == module.job_name("release-test")
    pod = manifest["spec"]["template"]["spec"]
    assert pod["restartPolicy"] == "Never"
    assert pod["nodeSelector"] == {"kubernetes.io/hostname": "192.168.0.24"}
    assert pod["containers"][0]["image"].endswith("@sha256:" + "b" * 64)
    assert manifest["spec"]["activeDeadlineSeconds"] == 1200
    command = pod["containers"][0]["args"][0]
    assert "helm upgrade --install" in command
    assert "--atomic" in command
    assert "--wait" in command
    assert "configure_kubeconfig.py" in command
    assert "/etc/kubernetes/admin.conf" in str(pod["volumes"])
    assert "/var/lib/doops-release-runner-home" in str(pod["volumes"])
    assert "token" not in str(manifest).lower()


def test_detached_helm_job_name_and_spec_digest_are_stable():
    module = load_script()
    assert module.job_name("release-test") == module.job_name("release-test")
    assert module.job_name("release-test") != module.job_name("release-other")
    assert len(module.job_name("release-test")) <= 63


def test_submit_emits_typed_runtime_evidence(monkeypatch, capsys):
    module = load_script()
    args = submit_args(module)
    monkeypatch.setattr(module, "get_job", lambda namespace, name: None)
    manifest = module.build_job_manifest(
        **{
            key: value
            for key, value in vars(args).items()
            if key not in {"command", "handler"}
        }
    )

    def fake_kubectl(arguments, **kwargs):
        if arguments[:2] == ["create", "-f"]:
            return SimpleNamespace(stdout="job.batch/created\n")
        return SimpleNamespace(stdout=json.dumps(manifest))

    monkeypatch.setattr(module, "kubectl", fake_kubectl)
    module.submit(args)
    output = json.loads(capsys.readouterr().out)
    assert output["subject"] == "deployment-executor"
    assert output["data"]["created"] is True
    assert output["data"]["specDigest"].startswith("sha256:")


def test_submit_rejects_existing_job_with_different_spec(monkeypatch):
    module = load_script()
    args = submit_args(module, candidate_digest="a")
    existing = module.build_job_manifest(
        **{
            key: value
            for key, value in vars(submit_args(module, candidate_digest="c")).items()
            if key not in {"command", "handler"}
        }
    )
    monkeypatch.setattr(module, "get_job", lambda namespace, name: existing)

    try:
        module.submit(args)
    except RuntimeError as exc:
        assert "different spec digest" in str(exc)
    else:
        raise AssertionError("mismatched existing Job must fail closed")


def test_submit_reports_existing_failed_job_without_recreating(monkeypatch, capsys):
    module = load_script()
    args = submit_args(module)
    existing = module.build_job_manifest(
        **{
            key: value
            for key, value in vars(args).items()
            if key not in {"command", "handler"}
        }
    )
    existing["status"] = {
        "failed": 1,
        "conditions": [{"type": "Failed", "status": "True"}],
    }
    monkeypatch.setattr(module, "get_job", lambda namespace, name: existing)
    monkeypatch.setattr(
        module,
        "kubectl",
        lambda *args, **kwargs: (_ for _ in ()).throw(
            AssertionError("existing failed Job must not be recreated")
        ),
    )

    module.submit(args)
    output = json.loads(capsys.readouterr().out)
    assert output["data"]["created"] is False
    assert output["data"]["failure"] is True
    assert output["data"]["complete"] is False
