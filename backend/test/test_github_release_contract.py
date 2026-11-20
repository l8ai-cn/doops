"""Lock the GitHub tag-release contract.

The public repository line publishes CLI/Gateway release assets from a tag through
`.github/workflows/release.yml`; GitHub Releases is the only install/publish path.
These checks assert structure: gated by CI, immutable, verified after publishing, and
built with the same builder identity the release scripts require. Wording is not locked.
"""

import subprocess
import tarfile
from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github" / "workflows"
RELEASE = WORKFLOWS / "release.yml"
CI = WORKFLOWS / "ci.yml"
BUILD_CLI = ROOT / "backend" / "scripts" / "build-cli.sh"
VERIFY = ROOT / "backend" / "scripts" / "verify-published-cli-release.sh"
INSTALL = ROOT / "backend" / "scripts" / "install.sh"
PACKAGE_SKILL = ROOT / "backend" / "scripts" / "package-skill-release.sh"

# builder identity backend/scripts/build-cli.sh --all insists on
RELEASE_BUILDER = "1.25.14"

# artifacts backend/scripts/build-cli.sh --all publishes
RELEASE_ASSETS = (
    "checksums.txt",
    "doops-darwin-amd64",
    "doops-darwin-arm64",
    "doops-gateway-linux-amd64",
    "doops-linux-amd64",
    "doops-linux-arm64",
    "doops-skill.tar.gz",
)


def load_workflow(path: Path):
    data = yaml.safe_load(path.read_text(encoding="utf-8"))
    return data


def triggers(data):
    # PyYAML resolves the YAML 1.1 key on to True.
    return data[True] if True in data else data["on"]


def steps_of(job):
    return job.get("steps", [])


def run_text(job):
    return "\n".join(str(step.get("run", "")) for step in steps_of(job))


def test_release_workflow_only_runs_for_tags_or_manual_dispatch():
    release = load_workflow(RELEASE)
    on = triggers(release)
    assert set(on) == {"push", "workflow_dispatch"}
    assert on["push"] == {"tags": ["v*"]}
    inputs = on["workflow_dispatch"]["inputs"]
    assert inputs["tag"]["required"] is True
    assert inputs["publish"]["type"] == "boolean"
    assert inputs["publish"]["default"] is False
    assert release["permissions"] == {"contents": "read"}
    assert release["concurrency"]["cancel-in-progress"] is False


def test_release_is_gated_by_the_reusable_ci_workflow():
    release = load_workflow(RELEASE)
    gate = release["jobs"]["gate"]
    assert gate["uses"] == "./.github/workflows/ci.yml"
    # A rejected tag fails in prepare; do not spend a full test run on it.
    needs = gate["needs"]
    assert needs == "prepare" or needs == ["prepare"]
    ci = load_workflow(CI)
    assert "workflow_call" in triggers(ci)
    # gitleaks scans full history on tag/dispatch events; only branch pushes and
    # pull requests carry a change range, so the release gate skips it.
    condition = ci["jobs"]["secret-scan"]["if"]
    assert "refs/tags/" in condition
    assert "github.event_name == 'pull_request'" in condition
    # The release contract tests themselves gate the release workflow.
    assert "contract-tests" in ci["jobs"]


def test_publish_requires_a_green_ci_and_stays_least_privileged():
    release = load_workflow(RELEASE)
    jobs = release["jobs"]
    assert jobs["build"]["needs"] == ["prepare", "gate"]
    publish = jobs["publish"]
    assert publish["needs"] == ["prepare", "build"]
    assert publish["permissions"] == {"contents": "write"}
    assert publish["if"] == "needs.prepare.outputs.publish == 'true'"
    runs = run_text(publish)
    assert "gh release view" in runs  # published releases are immutable
    assert "gh release create" in runs
    assert "--prerelease" in runs
    verify = jobs["verify"]
    assert verify["needs"] == ["prepare", "publish"]
    assert verify["permissions"] == {"contents": "read", "attestations": "read"}


def test_release_gate_tests_the_same_commit_as_the_asset_build():
    jobs = load_workflow(RELEASE)["jobs"]
    build_checkout = next(step for step in steps_of(jobs["build"])
                          if step.get("uses", "").startswith("actions/checkout@"))
    selected_commit = "${{ needs.prepare.outputs.commit }}"
    assert jobs["gate"]["with"]["source_commit"] == selected_commit
    assert build_checkout["with"]["ref"] == selected_commit

    ci = load_workflow(CI)
    source_input = triggers(ci)["workflow_call"]["inputs"]["source_commit"]
    assert source_input["required"] is True
    assert source_input["type"] == "string"
    checkouts = [step for job in ci["jobs"].values() for step in steps_of(job)
                 if step.get("uses", "").startswith("actions/checkout@")]
    assert checkouts
    # Release calls use the resolved tag SHA; standalone push/PR checks keep
    # their own event SHA instead of accidentally testing a default branch.
    for checkout in checkouts:
        assert checkout["with"]["ref"] == "${{ inputs.source_commit || github.sha }}"


def test_prepare_rejects_off_main_and_already_published_tags():
    runs = run_text(load_workflow(RELEASE)["jobs"]["prepare"])
    assert "refs/tags/" in runs
    assert "merge-base --is-ancestor" in runs
    assert "gh release view" in runs
    assert "GITHUB_OUTPUT" in runs


def test_build_uses_the_pinned_release_builder_and_verified_assets():
    build = load_workflow(RELEASE)["jobs"]["build"]
    setup_go = next(step for step in steps_of(build) if step.get("uses", "").startswith("actions/setup-go@"))
    assert setup_go["with"]["go-version"] == RELEASE_BUILDER
    runs = run_text(build)
    assert "backend/scripts/build-cli.sh --all" in runs
    assert "sha256sum --check --strict checksums.txt" in runs
    assert "doops.sh/cli-contract/v3" in runs
    upload = next(step for step in steps_of(build) if step.get("uses", "").startswith("actions/upload-artifact@"))
    assert upload["with"]["name"] == "release-assets"
    assert upload["with"]["if-no-files-found"] == "error"
    assert build["permissions"]["id-token"] == "write"
    assert any(step.get("uses", "").startswith("actions/attest-build-provenance@") for step in steps_of(build))


def test_verify_job_rechecks_the_published_release_and_installer():
    runs = run_text(load_workflow(RELEASE)["jobs"]["verify"])
    assert "DOOPS_RELEASE_ASSET_BASE=" in runs
    assert "backend/scripts/verify-published-cli-release.sh" in runs
    assert "gh attestation verify" in runs
    assert "backend/scripts/install.sh" in runs


def test_release_asset_names_match_the_build_script_contract():
    build_script = BUILD_CLI.read_text(encoding="utf-8")
    assert "checksums.txt" in build_script
    assert "doops-gateway" in build_script
    assert "doops-darwin" in build_script
    assert "doops-linux" in build_script
    assert "linux/amd64" in build_script
    for name in RELEASE_ASSETS:
        if name in ("checksums.txt", "doops-skill.tar.gz"):
            continue
        platform = name[len("doops-"):]
        if platform.startswith("gateway-"):
            platform = platform[len("gateway-"):]
        assert platform in build_script, name


def test_install_and_verify_scripts_use_github_release_assets_by_default():
    verify = VERIFY.read_text(encoding="utf-8")
    install = INSTALL.read_text(encoding="utf-8")
    for text in (verify, install):
        assert "DOOPS_RELEASE_ASSET_BASE" in text
    # GitHub Releases is the default; DOOPS_RELEASE_ASSET_BASE is the only mirror.
    assert 'GITHUB_RELEASE_ASSET_BASE="https://github.com/symansun/doops/releases/download"' in install
    assert 'RELEASE_ASSET_BASE="${DOOPS_RELEASE_ASSET_BASE:-${GITHUB_RELEASE_ASSET_BASE}}"' in install
    assert 'RELEASE_SOURCE="${RELEASE_ASSET_BASE}"' in install
    assert 'RELEASE_ASSET_BASE="${DOOPS_RELEASE_ASSET_BASE:-https://github.com/symansun/doops/releases/download}"' in verify


def test_build_packages_skill_bundle_and_checksums_it():
    build = load_workflow(RELEASE)["jobs"]["build"]
    runs = run_text(build)
    assert "backend/scripts/package-skill-release.sh" in runs
    script = PACKAGE_SKILL.read_text(encoding="utf-8")
    assert "skills/SKILL.md" in script
    assert "docs/*.md" in script
    assert "doops-skill.tar.gz" in script
    assert "checksums.txt" in script
    verify_runs = run_text(load_workflow(RELEASE)["jobs"]["verify"])
    assert "doops-skill.tar.gz" in verify_runs


def test_package_skill_release_script_builds_installer_layout(tmp_path):
    dist = tmp_path / "dist"
    dist.mkdir()
    (dist / "checksums.txt").write_text("a  doops-linux-amd64\n", encoding="utf-8")

    subprocess.run(["bash", str(PACKAGE_SKILL), str(dist)], check=True)
    subprocess.run(["bash", str(PACKAGE_SKILL), str(dist)], check=True)

    with tarfile.open(dist / "doops-skill.tar.gz", "r:gz") as archive:
        names = set(archive.getnames())
    assert "skills/SKILL.md" in names
    assert any(name.startswith("docs/") and name.endswith(".md") for name in names)
    assert "scripts/install.sh" in names

    migrations = {
        f"docs/migrations/{name}.sql"
        for name in ("machine_inventory", "cluster_inventory", "inventory_node_links")
    }
    assert {name for name in names if name.endswith(".sql")} == migrations
    unpacked = tmp_path / "unpacked"
    unpacked.mkdir()
    subprocess.run(["tar", "-xzf", dist / "doops-skill.tar.gz", "-C", unpacked], check=True)
    for path in migrations:
        assert (unpacked / path).read_bytes() == (ROOT / "backend" / path).read_bytes()

    lines = (dist / "checksums.txt").read_text(encoding="utf-8").splitlines()
    assert lines[0] == "a  doops-linux-amd64"
    skill_lines = [line for line in lines if line.endswith("  doops-skill.tar.gz")]
    assert len(skill_lines) == 1
