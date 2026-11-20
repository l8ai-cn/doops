import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

import pytest


AGENT = Path(__file__).resolve().parents[1] / "agent"
SCRIPTS = AGENT / "skills/k8s/scripts"
CANARY = "SYNTHETIC_PRIVATE_CONTENT"
NS = ["--namespace", "apps", "--output", "json"]
DEPLOY = ["kubectl", "get", "deployment", "api", *NS]
RS = ["kubectl", "get", "replicasets", *NS]
PODS = ["kubectl", "get", "pods", *NS]
EVENTS = ["kubectl", "get", "events", "--field-selector", "type=Warning", *NS]
HISTORY = ["helm", "history", "api", "--namespace", "apps", "--max", "5", "--output", "json"]


@pytest.fixture(scope="module")
def scripts(tmp_path_factory):
    root = tmp_path_factory.mktemp("agent-image")
    wrapper = root / "doops-shell"
    subprocess.run(["go", "build", "-o", wrapper, "./cmd/doops-shell"], cwd=AGENT, check=True)
    installed = shutil.copytree(SCRIPTS, root / "skills/k8s/scripts")
    # Relocate only the image mount path in this temporary copy. Production has no override.
    helper = installed / "_diagnostic.py"
    source = helper.read_text()
    assert '["/app/doops-shell", "--argv", tool, *args]' in source
    helper.write_text(source.replace("/app/doops-shell", str(wrapper)))
    return installed


@pytest.fixture
def run(scripts, tmp_path):
    # A stub records only actual process starts and rejects unexpected argv.
    stub = tmp_path / "kubectl"
    stub.write_text(f"#!{sys.executable}\n" + """
import json, os, sys
from pathlib import Path
assert not any(k in os.environ for k in ('DO_AGENT_MUTATION_AUTHORITY_SECRET', 'DO_AGENT_MUTATION_AUTHORITY_TOKEN'))
argv = [Path(sys.argv[0]).name, *sys.argv[1:]]
with open('calls', 'a') as log:
    print(json.dumps(argv), file=log)
responses = json.loads(Path('responses').read_text())
response = next(r for r in responses if r['argv'] == argv)
sys.stdout.write(response['out'])
sys.stderr.write(response['err'])
sys.exit(response['code'])
""")
    stub.chmod(0o755)
    (tmp_path / "helm").symlink_to(stub)
    # A lookup of doops-shell through PATH would bypass the real dispatcher and fail these tests.
    (tmp_path / "doops-shell").symlink_to(stub)

    def invoke(script, responses, args=None, deny=None):
        if args is None:
            args = ["-n", "apps", "deployment/api" if script == "workload-status" else "api"]
        (tmp_path / "responses").write_text(json.dumps(responses))
        calls = tmp_path / "calls"
        calls.write_text("")
        env = {"PATH": str(tmp_path) + os.pathsep + os.defpath, "HOME": str(tmp_path),
               "DO_AGENT_MUTATION_AUTHORITY_TOKEN": CANARY, "DO_AGENT_MUTATION_AUTHORITY_SECRET": CANARY}
        if deny:
            env["DOOPS_COMMAND_DENY_RULES"] = json.dumps(
                [{"name": "test_read", "match": deny, "hint": "coordinate with the operator"}])
        result = subprocess.run([scripts / script, *args], cwd=tmp_path, env=env, capture_output=True,
                                text=True, timeout=10)
        assert CANARY not in result.stdout + result.stderr
        return result, [json.loads(line) for line in calls.read_text().splitlines()]

    return invoke


def reply(argv, obj, err="", code=0):
    return dict(argv=argv, out=json.dumps(obj), err=err, code=code)


def owner(kind, uid, control=True):
    return dict(kind=kind, uid=uid, name="api", controller=control)


def meta(name, uid, *owners):
    return dict(name=name, uid=uid, namespace="apps", ownerReferences=owners, labels={"app": "api"})


def workload(kind="Deployment"):
    return dict(kind=kind, metadata=dict(meta("api", "w"), generation=3),
                spec=dict(replicas=2, template={"spec": {"containers": [
                    {"name": "app", "image": "app:v2", "env": [{"name": "PRIVATE", "value": CANARY}]}]}}),
                status=dict(observedGeneration=2, readyReplicas=1, updatedReplicas=2,
                            desiredNumberScheduled=3, numberReady=2, updatedNumberScheduled=1))


def event(kind, uid, second, reason="BackOff", event_type="Warning"):
    return dict(type=event_type, metadata=meta("event", "e"), reason=reason, message=CANARY,
                involvedObject=dict(kind=kind, namespace="apps", uid=uid, name="api"),
                lastTimestamp=f"2026-01-01T12:00:{second:02d}Z")


def test_workload_images_pods_and_recent_warning_events_by_owner_uid(run):
    rs = [{"metadata": meta("api-old", "old", owner("Deployment", "w"))},
          {"metadata": meta("api-new", "new", owner("Deployment", "w"))},
          {"metadata": meta("api-other", "other", owner("Deployment", "recreated"))},
          {"metadata": meta("api-hint", "hint", owner("Deployment", "w", False))}]
    ready = dict(phase="Running", conditions=[dict(type="Ready", status="True")], containerStatuses=[
        dict(name="app", image="app:v1", imageID="sha256:111")])
    broken = dict(phase="Running", containerStatuses=[dict(name="app", image="app:v2", imageID="sha256:222",
                  state={"waiting": {"reason": "CrashLoopBackOff", "message": CANARY}})])
    pods = [dict(metadata=meta("api-1", "p1", owner("ReplicaSet", "old")), status=ready),
            dict(metadata=meta("api-2", "p2", owner("ReplicaSet", "new")), status=broken),
            dict(metadata=meta("api-3", "p3", owner("ReplicaSet", "other")), status=broken),
            dict(metadata=meta("api-4", "p4", owner("ReplicaSet", "hint")), status=broken),
            dict(metadata=meta("api-5", "p5", owner("ReplicaSet", "new", False)), status=broken),
            dict(metadata=meta("api-6", "p6"), status=broken)]
    events = [event("Deployment", "w", i, reason=f"Reason{i}") for i in range(12)]
    events += [event("ReplicaSet", "old", 20), event("Pod", "p2", 1),
               event("Pod", "p3", 50), event("Deployment", "recreated", 51),
               event("ReplicaSet", "other", 52), event("Deployment", "w", 53, event_type="Normal")]
    events[13]["series"] = dict(count=8, lastObservedTime="2026-01-01T12:00:20.500000Z")
    events.append(dict(event("Pod", "p2", 54), metadata={"namespace": "other"}))
    result, calls = run("workload-status", [reply(DEPLOY, workload()), reply(RS, {"items": rs}),
                                            reply(PODS, {"items": pods}), reply(EVENTS, {"items": events})])
    assert result.returncode == 0 and result.stderr == ""
    assert calls == [DEPLOY, RS, PODS, EVENTS]
    data = json.loads(result.stdout)
    assert (data["desired"], data["ready"], data["updated"]) == (2, 1, 2)
    assert data["images"] == [{"name": "app", "image": "app:v2"}]
    assert data["podImages"] == [dict(name="app", image="app:v1", imageID="sha256:111"),
                                 dict(name="app", image="app:v2", imageID="sha256:222")]
    assert data["pods"] == dict(total=2, ready=1, succeeded=0, terminating=0, abnormalCount=1,
                                abnormal=[dict(name="api-2", phase="Running", reasons=["CrashLoopBackOff"])])
    warnings = data["warningEvents"]
    assert warnings[:2] == [dict(object="Pod/api-2", reason="BackOff", count=8, time="2026-01-01T12:00:20.500000Z"),
                            dict(object="ReplicaSet/api-old", reason="BackOff", count=1, time="2026-01-01T12:00:20Z")]
    assert [w["reason"] for w in warnings[2:]] == [f"Reason{i}" for i in range(11, 3, -1)]


@pytest.mark.parametrize("kind,counts", [("StatefulSet", (2, 1, 2)), ("DaemonSet", (3, 2, 1))])
def test_direct_controllers_ignore_same_name_and_bound_abnormal_samples(run, kind, counts):
    target = kind.lower()
    get = ["kubectl", "get", target, "api", *NS]
    pods = [dict(metadata=meta(f"pod-{i}", f"p{i}", owner(kind, "w")), status={"phase": "Pending"})
            for i in range(21)]
    pods += [dict(metadata=meta("complete", "s", owner(kind, "w")), status={"phase": "Succeeded"}),
             dict(metadata=dict(meta("gone", "t", owner(kind, "w")), deletionTimestamp="2026-01-01T12:00:00Z")),
             dict(metadata=meta("foreign", "p", owner(kind, "recreated")))]
    result, calls = run("workload-status", [reply(get, workload(kind)), reply(PODS, {"items": pods}),
                                            reply(EVENTS, {"items": []})], ["-n", "apps", f"{target}/api"])
    assert result.returncode == 0 and calls == [get, PODS, EVENTS]
    data = json.loads(result.stdout)
    assert (data["desired"], data["ready"], data["updated"]) == counts
    summary = data["pods"]
    assert (summary["total"], summary["abnormalCount"], len(summary["abnormal"])) == (23, 21, 20)
    assert summary["succeeded"] == summary["terminating"] == 1 and data["warningEvents"] == []


def test_release_projects_only_revision_metadata(run):
    history = [dict(revision=i, status="pending-upgrade" if i == 5 else "superseded", chart=f"api-1.{i}",
                    updated="2026-01-01T00:00:00Z", description=CANARY, values=CANARY, config=CANARY,
                    notes=CANARY, manifest=CANARY, hooks=[CANARY]) for i in range(1, 6)]
    result, calls = run("release-status", [reply(HISTORY, history)])
    assert result.returncode == 0 and not result.stderr and calls == [HISTORY]
    data = json.loads(result.stdout)
    assert data["current"] == dict(revision=5, status="pending-upgrade", chart="api-1.5", updated="2026-01-01T00:00:00Z")
    assert [row["revision"] for row in data["previous"]] == [4, 3, 2, 1]
    assert all(row.keys() == data["current"].keys() for row in data["previous"])


@pytest.mark.parametrize("deny,started", [(r"^kubectl get replicasets\b", [DEPLOY]),
                                          (r"^kubectl get pods\b", [DEPLOY, RS]),
                                          (r"^kubectl get events\b", [DEPLOY, RS, PODS]),
                                          (r"^helm history\b", [])])
def test_policy_denial_stops_native_process_sequence(run, deny, started):
    script = "release-status" if "helm" in deny else "workload-status"
    result, calls = run(script, [reply(DEPLOY, workload()), reply(RS, {"items": []}), reply(PODS, {"items": []})],
                        deny=deny)
    assert calls == started
    assert result.returncode == 1 and result.stdout == ""
    assert "blocked_command(test_read): next: coordinate with the operator" in result.stderr


@pytest.mark.parametrize("script", ["release-status", "workload-status"])
def test_native_failure_keeps_exit_status_and_redacted_cause(run, script):
    steps = [reply(DEPLOY, workload()), reply(RS, {"items": []}), reply(PODS, {"items": []})]
    argv = EVENTS if script == "workload-status" else HISTORY
    responses = steps if script == "workload-status" else []
    responses.append(reply(argv, {"private": CANARY},
                           f"Forbidden: read permission denied\nurl=https://user:{CANARY}@api.invalid\n"
                           f'Authorization: Bearer {CANARY}\npassword="{CANARY}"', 37))
    result, calls = run(script, responses)
    assert calls == [r["argv"] for r in responses]
    assert result.stdout == "" and result.returncode == 37
    assert f"{' '.join(argv[:3])} failed: Forbidden: read permission denied" in result.stderr


@pytest.mark.parametrize("private", [f"token: |\n  {CANARY}", f'values: {{"db": "{CANARY}"}}',
                                     f"upstream --token {CANARY}", f"password=\"two\n{CANARY}\""])
def test_success_warning_redacts_private_material_without_corrupting_json(run, private):
    history = [dict(revision=1, status="deployed", chart="api-1", updated="2026-01-01T00:00:00Z")]
    result, calls = run("release-status", [reply(HISTORY, history, "Warning: upstream authentication changed\n" + private)])
    assert calls == [HISTORY] and result.returncode == 0 and json.loads(result.stdout)["current"] == history[0]
    assert "upstream authentication changed" in result.stderr and "[REDACTED]" in result.stderr


@pytest.mark.parametrize("items", [{}, [dict(event("Deployment", "w", 1), lastTimestamp=CANARY)]])
def test_malformed_event_list_cannot_look_like_no_warnings(run, items):
    steps = [reply(DEPLOY, workload()), reply(RS, {"items": []}), reply(PODS, {"items": []}),
             reply(EVENTS, {"items": items, "private": CANARY})]
    result, calls = run("workload-status", steps)
    assert calls == [DEPLOY, RS, PODS, EVENTS]
    assert result.returncode != 0 and result.stdout == "" and "kubectl get events" in result.stderr


@pytest.mark.parametrize("out", [CANARY, "{}", "[]", '[{"revision":1,"status":{}}]'])
def test_invalid_release_response_never_prints_native_stdout(run, out):
    result, calls = run("release-status", [dict(argv=HISTORY, out=out, err="", code=0)])
    assert result.returncode != 0 and result.stdout == "" and result.stderr and calls == [HISTORY]


@pytest.mark.parametrize("script,args", [("workload-status", ["deployment/api"]), ("release-status", ["-n", "apps"]),
                                         ("workload-status", ["-n", "apps", "secret/api"]),
                                         ("workload-status", ["-n", "apps", "deployment/--all"]),
                                         ("release-status", ["-n", "-A", "api"])])
def test_invalid_scope_starts_no_native_tool(run, script, args):
    result, calls = run(script, [], args)
    assert result.returncode == 2 and result.stdout == "" and not calls
