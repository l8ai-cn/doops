import json
import hashlib
import os
import subprocess
import tempfile
from pathlib import Path

import pytest


ROOT = Path(__file__).resolve().parents[1]
INSTALL = ROOT / "scripts" / "install.sh"
BUILD = ROOT / "scripts" / "build-cli.sh"
RELEASE_BUILD = ROOT / "scripts" / "build-cli-release.sh"
PUBLISH = ROOT / "scripts" / "publish-cli-release.mjs"
VERIFY_PUBLISHED = ROOT / "scripts" / "verify-published-cli-release.sh"
BUILDER_IMAGE = (
    "docker.io/library/golang:1.25-alpine@"
    "sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59"
)


def test_cli_install_downloads_named_release_asset_and_replaces_binary_atomically():
    text = INSTALL.read_text()

    assert '--version is required' in text
    assert 'GITHUB_RELEASE_ASSET_BASE="https://github.com/symansun/doops/releases/download"' in text
    assert 'RELEASE_ASSET_BASE="${DOOPS_RELEASE_ASSET_BASE:-${GITHUB_RELEASE_ASSET_BASE}}"' in text
    assert '"${RELEASE_ASSET_BASE}/${RELEASE_TAG}/checksums.txt"' in text
    assert '"${RELEASE_ASSET_BASE}/${RELEASE_TAG}/${artifact}"' in text
    assert 'DOOPS_RELEASE_ASSET_BASE must be an https URL' in text
    assert 'curl --fail --location --silent --show-error' in text
    assert 'install -m 0755 "${candidate}" "${replacement}"' in text
    assert 'mv -f "${replacement}" "${CLI_OUT}"' in text
    assert 'receipt="${receipt_dir}/install.txt"' in text
    assert '"source=${RELEASE_SOURCE}"' in text
    assert 'RELEASE_SOURCE="${RELEASE_ASSET_BASE}"' in text
    assert "skill_commit" in text
    assert '"${candidate}" version' in text
    assert "doops.sh/cli-contract/v3" in text
    assert "${CLI_OUT} version" in text
    assert "\n  doops list" not in text
    assert 'skills/doops-cli/bin' not in text
    assert 'backend/cli/bin' not in text
    assert '"${candidate}" --contract' in text
    assert '${CLI_OUT} -session setup login --gateway ${TARGET_GATEWAY:-<gateway-url>}' in text
    assert 'DOOPS_CONFIG= "${candidate}" list' in text
    assert "python3 - " not in text
    assert text.index('Existing doops config rejected by release CLI') < text.index(
        'install -m 0755 "${candidate}" "${replacement}"'
    )


def test_cli_release_builds_to_ignored_distribution_directory():
    build = BUILD.read_text()
    release = RELEASE_BUILD.read_text()

    assert 'expected_builder="linux/amd64 go1.25.14"' in build
    assert "run scripts/build-cli-release.sh instead" in build
    assert f'BUILDER_IMAGE="{BUILDER_IMAGE}"' in release
    assert "--platform linux/amd64" in release
    assert 'RELEASE_DIR="${DOOPS_CLI_RELEASE_DIR:-${REPO_ROOT}/dist/cli}"' in build
    assert 'RELEASE_DIR="${DOOPS_CLI_RELEASE_DIR:-${REPO_ROOT}/dist/cli}"' in release
    assert '-X main.cliBuildVersion=${CLI_VERSION}' in build
    assert '-X main.cliBuildCommit=${CLI_COMMIT}' in build


def test_installer_rejects_unsupported_target_fields_before_replacing_cli():
    with tempfile.TemporaryDirectory() as raw_home:
        home = Path(raw_home)
        fake_bin, release_env = prepare_fake_cli_release(Path(raw_home), "v-test")
        cli = home / ".local" / "bin" / "doops"
        cli.parent.mkdir(parents=True)
        cli.write_text("existing-cli")
        config = home / ".agent" / "skills" / "doops" / "config.json"
        config.parent.mkdir(parents=True)
        config.write_text(
            json.dumps(
                {
                    "servers": [
                        {
                            "name": "legacy",
                            "gateway": "https://gateway.example.com",
                            "cluster": "prod",
                            "instance": "node",
                            "token": "retired",
                        }
                    ]
                }
            )
        )

        result = subprocess.run(
            [
                "bash",
                str(INSTALL),
                "--version",
                "v-test",
                "--project",
                str(ROOT.parent),
            ],
            env={
                **os.environ,
                **release_env,
                "HOME": str(home),
                "PATH": f"{fake_bin}:{os.environ['PATH']}",
            },
            text=True,
            capture_output=True,
            check=False,
        )

        assert result.returncode != 0
        assert 'unknown field "token"' in result.stderr
        assert cli.read_text() == "existing-cli"


def test_installer_requires_complete_gateway_route_before_replacing_cli():
    with tempfile.TemporaryDirectory() as raw_home:
        home = Path(raw_home)
        cli = home / ".local" / "bin" / "doops"
        cli.parent.mkdir(parents=True)
        cli.write_text("existing-cli")

        result = subprocess.run(
            [
                "bash",
                str(INSTALL),
                "--version",
                "unused",
                "--project",
                str(ROOT.parent),
                "--target-name",
                "incomplete",
                "--target-gateway",
                "https://gateway.example.com",
            ],
            env={**os.environ, "HOME": str(home)},
            text=True,
            capture_output=True,
            check=False,
        )

        assert result.returncode == 2
        assert (
            "Explicit route mode requires --target-name, --target-gateway, --target-cluster, and --target-instance"
            in result.stderr
        )
        assert cli.read_text() == "existing-cli"


def test_installer_gateway_only_mode_prepares_first_login_without_route(tmp_path):
    repo, release_commit = prepare_synthetic_repo(
        tmp_path / "source",
        skill="release skill\n",
        docs={"release.md": "release docs\n"},
    )
    home = tmp_path / "home"
    project = tmp_path / "project"
    home.mkdir()
    project.mkdir()
    fake_bin, release_env = prepare_fake_cli_release(
        tmp_path / "release",
        "v-test",
        commit=release_commit,
    )

    result = subprocess.run(
        [
            "bash",
            str(repo / "backend" / "scripts" / "install.sh"),
            "--version",
            "v-test",
            "--project",
            str(project),
            "--target-gateway",
            "https://gateway.example.com",
        ],
        env={
            **os.environ,
            **release_env,
            "HOME": str(home),
            "PATH": f"{fake_bin}:{os.environ['PATH']}",
        },
        text=True,
        capture_output=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert not (home / ".agent" / "skills" / "doops" / "config.json").exists()
    assert "login --gateway https://gateway.example.com" in result.stdout
    assert "no local route was written" in result.stdout


def test_installer_rejects_legacy_only_config_before_replacing_any_artifact(tmp_path):
    home = tmp_path / "home"
    project = tmp_path / "project"
    project.mkdir()
    old_cli = home / ".local/bin/doops"
    old_cli.parent.mkdir(parents=True)
    old_cli.write_text("existing-cli")
    old_skill = project / ".agent/skills/doops/SKILL.md"
    old_skill.parent.mkdir(parents=True)
    old_skill.write_text("existing-skill")
    legacy = home / ".config/doops/config.json"
    legacy.parent.mkdir(parents=True)
    legacy.write_text('{"servers":[{"name":"legacy","ip":"192.0.2.1"}]}')
    fake_bin, release_env = prepare_fake_cli_release(tmp_path, "v-test")
    result = run_installer(
        ROOT.parent, home, project, fake_bin,
        {**release_env, "DOOPS_CONFIG": str(tmp_path / "isolated/config.json")},
    )
    assert result.returncode != 0
    assert "retired" in result.stderr
    assert old_cli.read_text() == "existing-cli"
    assert old_skill.read_text() == "existing-skill"
    assert not (home / ".local/share/doops/install.txt").exists()
    assert not (home / ".agent/skills/doops/config.json").exists()


def test_installer_places_release_asset_only_in_standard_cli_path():
    with tempfile.TemporaryDirectory() as raw_root:
        root = Path(raw_root)
        home = root / "home"
        fixtures = root / "fixtures"
        fake_bin = root / "bin"
        project = root / "project"
        home.mkdir()
        fixtures.mkdir()
        fake_bin.mkdir()
        project.mkdir()

        platform, arch = host_platform()
        artifact_name = f"doops-{platform[0]}-{arch}"
        artifact = fixtures / artifact_name
        release_commit = git_commit(ROOT.parent)
        artifact.write_text(
            "#!/usr/bin/env bash\n"
            "if [[ \"${1:-}\" == version ]]; then\n"
            f"  printf 'release=v-test commit={release_commit}\\n'\n"
            "elif [[ \"${1:-}\" == --contract ]]; then\n"
            "  printf 'doops.sh/cli-contract/v3\\n'\n"
            "fi\n"
        )
        artifact.chmod(0o755)
        checksum = hashlib.sha256(artifact.read_bytes()).hexdigest()
        (fixtures / "checksums.txt").write_text(f"{checksum}  {artifact_name}\n")

        curl = fake_bin / "curl"
        curl.write_text(
            """#!/usr/bin/env bash
set -euo pipefail
out=""
url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --*) shift ;;
    *) url="$1"; shift ;;
  esac
done
cp "${DOOPS_TEST_RELEASE_FIXTURES}/${url##*/}" "${out}"
"""
        )
        curl.chmod(0o755)

        result = subprocess.run(
            [
                "bash",
                str(INSTALL),
                "--version",
                "v-test",
                "--project",
                str(project),
            ],
            env={
                **os.environ,
                "HOME": str(home),
                "PATH": f"{fake_bin}:{os.environ['PATH']}",
                "DOOPS_TEST_RELEASE_FIXTURES": str(fixtures),
            },
            text=True,
            capture_output=True,
            check=False,
        )

        assert result.returncode == 0, result.stderr
        installed = home / ".local" / "bin" / "doops"
        assert installed.read_bytes() == artifact.read_bytes()
        receipt = (home / ".local" / "share" / "doops" / "install.txt").read_text()
        assert "release=v-test" in receipt
        assert f"artifact={artifact_name}" in receipt
        assert f"sha256={checksum}" in receipt
        assert f"commit={release_commit}" in receipt
        assert "contract=doops.sh/cli-contract/v3" in receipt
        assert f"path={installed}" in receipt
        assert not (home / ".agent" / "skills" / "doops" / "doops-cli" / "bin").exists()

        installed.write_text("keep-existing-cli")
        artifact.write_text(
            "#!/usr/bin/env bash\n"
            "printf 'release=dev commit=unknown\\n'\n"
        )
        artifact.chmod(0o755)
        bad_checksum = hashlib.sha256(artifact.read_bytes()).hexdigest()
        (fixtures / "checksums.txt").write_text(f"{bad_checksum}  {artifact_name}\n")
        rejected = subprocess.run(
            [
                "bash",
                str(INSTALL),
                "--version",
                "v-test",
                "--project",
                str(project),
            ],
            env={
                **os.environ,
                "HOME": str(home),
                "PATH": f"{fake_bin}:{os.environ['PATH']}",
                "DOOPS_TEST_RELEASE_FIXTURES": str(fixtures),
            },
            text=True,
            capture_output=True,
            check=False,
        )
        assert rejected.returncode != 0
        assert "identity mismatch" in rejected.stderr
        assert installed.read_text() == "keep-existing-cli"


def test_installer_stages_skill_and_docs_from_the_release_commit():
    with tempfile.TemporaryDirectory() as raw_root:
        root = Path(raw_root)
        repo, release_commit = prepare_synthetic_repo(
            root / "source",
            skill="release skill\n",
            docs={"release.md": "release docs\n"},
        )
        home = root / "home"
        project = root / "project"
        home.mkdir()
        project.mkdir()
        fake_bin, release_env = prepare_fake_cli_release(
            root / "release",
            "v-test",
            commit=release_commit,
        )

        result = run_installer(
            repo,
            home,
            project,
            fake_bin,
            release_env,
        )

        assert result.returncode == 0, result.stderr
        installed_skill = project / ".agent" / "skills" / "doops" / "SKILL.md"
        installed_doc = project / ".agent" / "skills" / "doops" / "docs" / "release.md"
        assert installed_skill.read_text() == "release skill\n"
        assert installed_doc.read_text() == "release docs\n"
        receipt = (home / ".local" / "share" / "doops" / "install.txt").read_text()
        assert f"skill_commit={release_commit}" in receipt


def test_installer_uses_release_commit_when_worktree_skill_and_docs_are_updated():
    with tempfile.TemporaryDirectory() as raw_root:
        root = Path(raw_root)
        repo, release_commit = prepare_synthetic_repo(
            root / "source",
            skill="release skill\n",
            docs={"release.md": "release docs\n"},
        )
        (repo / "backend" / "skills" / "SKILL.md").write_text("working tree skill\n")
        (repo / "backend" / "docs" / "release.md").write_text("working tree docs\n")
        home = root / "home"
        project = root / "project"
        home.mkdir()
        project.mkdir()
        fake_bin, release_env = prepare_fake_cli_release(
            root / "release",
            "v-test",
            commit=release_commit,
        )

        result = run_installer(
            repo,
            home,
            project,
            fake_bin,
            release_env,
        )

        assert result.returncode == 0, result.stderr
        installed_skill = project / ".agent" / "skills" / "doops" / "SKILL.md"
        installed_doc = project / ".agent" / "skills" / "doops" / "docs" / "release.md"
        assert installed_skill.read_text() == "release skill\n"
        assert installed_doc.read_text() == "release docs\n"


@pytest.mark.parametrize("has_migrations", [True, False], ids=["inventory-release", "historical-release"])
def test_installer_updates_existing_agent_copies_without_touching_runtime_state(tmp_path, has_migrations):
    sql_names = ("machine_inventory.sql", "cluster_inventory.sql", "inventory_node_links.sql")
    docs = {"release.md": "release docs\n"}
    if has_migrations:
        docs.update({f"migrations/{name}": (ROOT / "docs/migrations" / name).read_text() for name in sql_names})
        docs["migrations/unrelated.sql"] = "-- outside the operator inventory bundle\n"
    repo, commit = prepare_synthetic_repo(
        tmp_path / "source",
        skill="release skill\n",
        docs=docs,
    )
    home = tmp_path / "home"
    project = tmp_path / "project"
    home.mkdir()
    project.mkdir()
    destinations = [
        home / ".agent/skills/doops",
        home / ".codex/skills/doops",
        home / ".agents/skills/doops",
        project / ".agent/skills/doops",
        project / ".codex/skills/doops",
        project / ".agents/skills/doops",
    ]
    for destination in destinations:
        (destination / "docs/migrations").mkdir(parents=True)
        (destination / "docs/migrations/retired.sql").write_text("-- stale copy\n")
        (destination / "SKILL.md").write_text("outdated skill\n")
        (destination / "docs/retired.md").write_text("outdated contract\n")
        (destination / "config.json").write_text('{"servers":[]}\n')
        (destination / "auth.json").write_text('{"tokens":{}}\n')
        (destination / "history.jsonl").write_text("synthetic history\n")
    (repo / "backend/skills/SKILL.md").write_text("unreleased working tree\n")
    working_migrations = repo / "backend/docs/migrations"
    working_migrations.mkdir(exist_ok=True)
    for name in sql_names:
        (working_migrations / name).write_text("-- unreleased migration\n")
    fake_bin, release_env = prepare_fake_cli_release(tmp_path / "release", "v-test", commit)

    result = run_installer(repo, home, project, fake_bin, release_env)

    assert result.returncode == 0, result.stderr
    for destination in destinations:
        assert (destination / "SKILL.md").read_text() == "release skill\n"
        installed_migrations = destination / "docs/migrations"
        if has_migrations:
            assert {path.name for path in installed_migrations.iterdir()} == set(sql_names)
            for name in sql_names:
                assert (installed_migrations / name).read_bytes() == (ROOT / "docs/migrations" / name).read_bytes()
        else:
            assert not installed_migrations.exists()
        expected_docs = {"migrations", "release.md"} if has_migrations else {"release.md"}
        assert {path.name for path in (destination / "docs").iterdir()} == expected_docs
        assert (destination / "docs/release.md").read_text() == "release docs\n"
        assert (destination / "config.json").read_text() == '{"servers":[]}\n'
        assert (destination / "auth.json").read_text() == '{"tokens":{}}\n'
        assert (destination / "history.jsonl").read_text() == "synthetic history\n"
        assert list(destination.glob(".doops-install.*")) == []
    receipt = (home / ".local/share/doops/install.txt").read_text()
    assert f"skill_commit={commit}" in receipt
    assert receipt.count("skill_path=") == len(destinations)


def test_installer_replaces_development_file_links_without_editing_source(tmp_path):
    repo, commit = prepare_synthetic_repo(
        tmp_path / "source",
        skill="release skill\n",
        docs={"release.md": "release docs\n"},
    )
    home = tmp_path / "home"
    project = tmp_path / "project"
    home.mkdir()
    project.mkdir()
    destination = project / ".agent/skills/doops"
    destination.mkdir(parents=True)
    (destination / "SKILL.md").symlink_to(repo / "backend/skills/SKILL.md")
    (destination / "docs").symlink_to(repo / "backend/docs", target_is_directory=True)
    (repo / "backend/skills/SKILL.md").write_text("development skill\n")
    (repo / "backend/docs/release.md").write_text("development docs\n")
    fake_bin, release_env = prepare_fake_cli_release(tmp_path / "release", "v-test", commit)

    result = run_installer(repo, home, project, fake_bin, release_env)

    assert result.returncode == 0, result.stderr
    assert not (destination / "SKILL.md").is_symlink()
    assert not (destination / "docs").is_symlink()
    assert (destination / "SKILL.md").read_text() == "release skill\n"
    assert (destination / "docs/release.md").read_text() == "release docs\n"
    assert (repo / "backend/skills/SKILL.md").read_text() == "development skill\n"
    assert (repo / "backend/docs/release.md").read_text() == "development docs\n"


@pytest.mark.parametrize("invalid_type", ["directory-link", "skill-fifo", "docs-file", "receipt-directory"])
def test_installer_rejects_invalid_existing_copy_before_replacing_release(tmp_path, invalid_type):
    repo, commit = prepare_synthetic_repo(
        tmp_path / "source",
        skill="release skill\n",
        docs={"release.md": "release docs\n"},
    )
    home = tmp_path / "home"
    project = tmp_path / "project"
    project.mkdir()
    old_cli = home / ".local/bin/doops"
    old_cli.parent.mkdir(parents=True)
    old_cli.write_text("existing-cli")
    old_skill = project / ".agent/skills/doops/SKILL.md"
    old_skill.parent.mkdir(parents=True)
    old_skill.write_text("existing-skill")
    invalid = home / ".codex/skills/doops"
    invalid.parent.mkdir(parents=True)
    if invalid_type == "directory-link":
        invalid.symlink_to(repo / "backend/skills", target_is_directory=True)
        expected_error = "Skill directory must be a real directory"
    elif invalid_type == "receipt-directory":
        invalid = home / ".local/share/doops/install.txt"
        invalid.mkdir(parents=True)
        expected_error = "Install receipt has an unexpected type"
    else:
        invalid.mkdir()
        if invalid_type == "skill-fifo":
            os.mkfifo(invalid / "SKILL.md")
        else:
            (invalid / "docs").write_text("not a docs directory\n")
        expected_error = "Skill files have an unexpected type"
    fake_bin, release_env = prepare_fake_cli_release(tmp_path / "release", "v-test", commit)

    result = run_installer(repo, home, project, fake_bin, release_env)

    assert result.returncode != 0
    assert expected_error in result.stderr
    assert old_cli.read_text() == "existing-cli"
    assert old_skill.read_text() == "existing-skill"
    if invalid_type == "directory-link":
        assert invalid.is_symlink()
    elif invalid_type == "skill-fifo":
        assert (invalid / "SKILL.md").is_fifo()
    elif invalid_type == "docs-file":
        assert (invalid / "docs").read_text() == "not a docs directory\n"
    else:
        assert invalid.is_dir()
    assert not (home / ".local/share/doops/install.txt").is_file()
    assert list(old_skill.parent.glob(".doops-install.*")) == []


def test_installer_deduplicates_home_project_and_does_not_create_unused_clients(tmp_path):
    repo, commit = prepare_synthetic_repo(
        tmp_path / "source",
        skill="release skill\n",
        docs={"release.md": "release docs\n"},
    )
    home = tmp_path / "home"
    home.mkdir()
    fake_bin, release_env = prepare_fake_cli_release(tmp_path / "release", "v-test", commit)

    result = run_installer(repo, home, home, fake_bin, release_env)

    assert result.returncode == 0, result.stderr
    receipt = (home / ".local/share/doops/install.txt").read_text()
    assert receipt.count("skill_path=") == 1
    assert not (home / ".codex").exists()
    assert not (home / ".agents").exists()


@pytest.mark.parametrize("alias", ["trailing-slash", "parent-link"])
def test_installer_deduplicates_physical_skill_paths(tmp_path, alias):
    repo, commit = prepare_synthetic_repo(
        tmp_path / "source", skill="release skill\n", docs={"release.md": "release docs\n"}
    )
    home = tmp_path / "home"
    destination = home / ".agent/skills/doops"
    (destination / "docs").mkdir(parents=True)
    (destination / "SKILL.md").write_text("old skill\n")
    (destination / "docs/old.md").write_text("old docs\n")
    project = home
    if alias == "parent-link":
        project = tmp_path / "project"
        project.mkdir()
        (project / ".agent").symlink_to(home / ".agent", target_is_directory=True)
    fake_bin, release_env = prepare_fake_cli_release(tmp_path / "release", "v-test", commit)
    moves = tmp_path / "moves"
    (fake_bin / "mv").write_text(
        '#!/usr/bin/env bash\n'
        'printf "%s\\n" "${!#}" >> "$DOOPS_TEST_MOVES"\n'
        'exec /bin/mv "$@"\n'
    )
    (fake_bin / "mv").chmod(0o755)

    result = run_installer(
        repo, str(home) + "/" if alias == "trailing-slash" else home,
        project, fake_bin, {**release_env, "DOOPS_TEST_MOVES": str(moves)},
    )

    assert result.returncode == 0, result.stderr
    receipt = (home / ".local/share/doops/install.txt").read_text()
    assert [line for line in receipt.splitlines() if line.startswith("skill_path=")] == [
        f"skill_path={destination.resolve()}"
    ]
    assert moves.read_text().splitlines().count(str(destination.resolve() / "docs")) == 1
    assert (destination / "SKILL.md").read_text() == "release skill\n"
    assert sorted(path.name for path in (destination / "docs").iterdir()) == ["release.md"]
    assert (destination / "docs/release.md").read_text() == "release docs\n"
    assert not list(home.rglob(".doops-install.*"))


@pytest.mark.parametrize("failure", ["second-docs", "receipt", "receipt-write"])
@pytest.mark.parametrize("existing", [False, True])
def test_installer_restores_all_artifacts_after_replacement_failure(tmp_path, failure, existing):
    repo, commit = prepare_synthetic_repo(
        tmp_path / "source", skill="release skill\n", docs={"release.md": "release docs\n"}
    )
    home = tmp_path / "home"
    project = tmp_path / "project"
    destinations = [
        project / ".agent/skills/doops",
        home / ".agent/skills/doops",
        home / ".codex/skills/doops",
    ]
    cli = home / ".local/bin/doops"
    receipt = home / ".local/share/doops/install.txt"
    originals = {}
    for directory in [cli.parent, receipt.parent, *destinations]:
        directory.mkdir(parents=True)
    for destination in destinations:
        for name, content in [
            ("config.json", '{"servers":[]}\n'),
            ("auth.json", '{"tokens":{}}\n'),
            ("history.jsonl", "synthetic history\n"),
        ]:
            path = destination / name
            path.write_text(content)
            originals[path] = content.encode()
    if existing:
        cli.write_text("old cli\n")
        cli.chmod(0o750)
        receipt.write_text("old receipt\n")
        receipt.chmod(0o640)
        originals.update({cli: b"old cli\n", receipt: b"old receipt\n"})
        for destination in destinations:
            (destination / "docs").mkdir()
            (destination / "SKILL.md").write_text("old skill\n")
            (destination / "docs/old.md").write_text("old docs\n")
            originals[destination / "SKILL.md"] = b"old skill\n"
            originals[destination / "docs/old.md"] = b"old docs\n"
    fake_bin, release_env = prepare_fake_cli_release(tmp_path / "release", "v-test", commit)
    marker = tmp_path / "fault-injected"
    (fake_bin / "mv").write_text(
        """#!/usr/bin/env bash
set -euo pipefail
args=("$@")
source="${args[${#args[@]}-2]}"
destination="${args[${#args[@]}-1]}"
if { [[ "$DOOPS_TEST_FAILURE" == second-docs && "$destination" == "$DOOPS_TEST_SECOND/docs" && "$source" == */.doops-install.*/docs ]]; } ||
   { [[ "$DOOPS_TEST_FAILURE" == receipt && "$destination" == "$DOOPS_TEST_RECEIPT" && "$source" == */.doops-install.*/install.txt ]]; }; then
  touch "$DOOPS_TEST_FAULT"
  echo "injected replacement failure" >&2
  exit 74
fi
if [[ "$DOOPS_TEST_FAILURE" == receipt-write && "$destination" == "$DOOPS_TEST_SECOND/docs" && "$source" == */.doops-install.*/docs ]]; then
  /bin/mv "$@"
  for stage in "${DOOPS_TEST_RECEIPT%/*}"/.doops-install.*; do
    [[ -d "$stage" ]] || continue
    mkdir "$stage/install.txt"
    touch "$DOOPS_TEST_FAULT"
  done
  exit 0
fi
exec /bin/mv "$@"
"""
    )
    (fake_bin / "mv").chmod(0o755)

    result = run_installer(repo, home, project, fake_bin, {
        **release_env,
        "DOOPS_TEST_FAILURE": failure,
        "DOOPS_TEST_SECOND": str(destinations[1].resolve()),
        "DOOPS_TEST_RECEIPT": str(receipt.resolve()),
        "DOOPS_TEST_FAULT": str(marker),
    })

    assert marker.exists(), result.stdout + result.stderr
    assert result.returncode != 0
    for path, content in originals.items():
        assert path.read_bytes() == content, path
    if existing:
        assert cli.stat().st_mode & 0o777 == 0o750
        assert receipt.stat().st_mode & 0o777 == 0o640
    else:
        assert not cli.exists()
        assert not receipt.exists()
        for destination in destinations:
            assert not (destination / "SKILL.md").exists()
            assert not (destination / "docs").exists()
    for destination in destinations:
        assert not (destination / "docs/release.md").exists()
    assert not list(home.rglob(".doops-install.*"))
    assert not list(project.rglob(".doops-install.*"))


@pytest.mark.parametrize("fail_recovery", [False, True])
def test_installer_restores_development_links_or_keeps_failed_backup(tmp_path, fail_recovery):
    repo, commit = prepare_synthetic_repo(
        tmp_path / "source", skill="release skill\n", docs={"release.md": "release docs\n"}
    )
    home = tmp_path / "home"
    project = tmp_path / "project"
    destination = project / ".agent/skills/doops"
    destination.mkdir(parents=True)
    source_skill = repo / "backend/skills/SKILL.md"
    source_docs = repo / "backend/docs"
    (destination / "SKILL.md").symlink_to(source_skill)
    (destination / "docs").symlink_to(source_docs, target_is_directory=True)
    source_skill.write_text("development skill\n")
    (source_docs / "release.md").write_text("development docs\n")
    old_cli = home / ".local/bin/doops"
    old_cli.parent.mkdir(parents=True)
    source_cli = tmp_path / "development-cli"
    source_cli.write_text("old cli\n")
    old_cli.symlink_to(source_cli)
    fake_bin, release_env = prepare_fake_cli_release(tmp_path / "release", "v-test", commit)
    failed_restore = tmp_path / "failed-restore"
    (fake_bin / "mv").write_text(
        """#!/usr/bin/env bash
set -euo pipefail
args=("$@")
source="${args[${#args[@]}-2]}"
destination="${args[${#args[@]}-1]}"
if [[ "$destination" == */.local/share/doops/install.txt ]]; then
  exit 74
fi
if [[ "$DOOPS_TEST_FAIL_RECOVERY" == true && "$source" == */previous-docs ]]; then
  printf '%s\\n' "$source" > "$DOOPS_TEST_FAILED_RESTORE"
  exit 74
fi
exec /bin/mv "$@"
"""
    )
    (fake_bin / "mv").chmod(0o755)

    result = run_installer(repo, home, project, fake_bin, {
        **release_env, "DOOPS_TEST_FAILED_RESTORE": str(failed_restore),
        "DOOPS_TEST_FAIL_RECOVERY": "true" if fail_recovery else "false",
    })

    assert result.returncode != 0
    if fail_recovery:
        assert failed_restore.exists(), result.stdout + result.stderr
        backup = Path(failed_restore.read_text().strip())
        assert backup.is_symlink()
        assert backup.resolve() == source_docs.resolve()
        assert str(backup) in result.stderr
        assert "recovery" in result.stderr.lower()
    else:
        assert (destination / "docs").is_symlink()
        assert (destination / "docs").resolve() == source_docs.resolve()
        assert not list(home.rglob(".doops-install.*"))
        assert not list(project.rglob(".doops-install.*"))
    assert (destination / "SKILL.md").is_symlink()
    assert (destination / "SKILL.md").resolve() == source_skill.resolve()
    assert source_skill.read_text() == "development skill\n"
    assert (source_docs / "release.md").read_text() == "development docs\n"
    assert old_cli.is_symlink()
    assert old_cli.resolve() == source_cli.resolve()
    assert old_cli.read_text() == "old cli\n"


def test_installer_preserves_existing_installation_when_release_commit_is_missing():
    with tempfile.TemporaryDirectory() as raw_root:
        root = Path(raw_root)
        repo, _ = prepare_synthetic_repo(
            root / "source",
            skill="release skill\n",
            docs={"release.md": "release docs\n"},
        )
        home = root / "home"
        project = root / "project"
        home.mkdir()
        project.mkdir()
        old_cli = home / ".local" / "bin" / "doops"
        old_cli.parent.mkdir(parents=True)
        old_cli.write_text("old cli\n")
        skill_dest = project / ".agent" / "skills" / "doops"
        (skill_dest / "docs").mkdir(parents=True)
        (skill_dest / "SKILL.md").write_text("old skill\n")
        (skill_dest / "docs" / "old.md").write_text("old docs\n")
        config = home / ".agent" / "skills" / "doops" / "config.json"
        config.parent.mkdir(parents=True)
        config.write_text('{"servers":[]}\n')
        fake_bin, release_env = prepare_fake_cli_release(
            root / "release",
            "v-test",
            commit="b" * 40,
        )

        result = run_installer(
            repo,
            home,
            project,
            fake_bin,
            release_env,
        )

        assert result.returncode != 0
        assert "unavailable in local git objects" in result.stderr
        assert old_cli.read_text() == "old cli\n"
        assert (skill_dest / "SKILL.md").read_text() == "old skill\n"
        assert (skill_dest / "docs" / "old.md").read_text() == "old docs\n"
        assert config.read_text() == '{"servers":[]}\n'


def test_installer_preserves_existing_installation_when_release_skill_or_docs_are_missing():
    for missing in ("skill", "docs", "migration"):
        with tempfile.TemporaryDirectory() as raw_root:
            root = Path(raw_root)
            docs = {} if missing == "docs" else {"release.md": "release docs\n"}
            if missing == "migration":
                docs["migrations/machine_inventory.sql"] = "-- incomplete inventory release\n"
            repo, release_commit = prepare_synthetic_repo(
                root / "source",
                skill=None if missing == "skill" else "release skill\n",
                docs=docs,
            )
            home = root / "home"
            project = root / "project"
            home.mkdir()
            project.mkdir()
            old_cli = home / ".local" / "bin" / "doops"
            old_cli.parent.mkdir(parents=True)
            old_cli.write_text("old cli\n")
            skill_dest = project / ".agent" / "skills" / "doops"
            (skill_dest / "docs").mkdir(parents=True)
            (skill_dest / "SKILL.md").write_text("old skill\n")
            (skill_dest / "docs" / "old.md").write_text("old docs\n")
            config = home / ".agent" / "skills" / "doops" / "config.json"
            config.parent.mkdir(parents=True)
            config.write_text('{"servers":[]}\n')
            fake_bin, release_env = prepare_fake_cli_release(
                root / "release",
                "v-test",
                commit=release_commit,
            )

            result = run_installer(
                repo,
                home,
                project,
                fake_bin,
                release_env,
            )

            assert result.returncode != 0
            assert ("cluster_inventory.sql" if missing == "migration" else "missing") in result.stderr
            assert old_cli.read_text() == "old cli\n"
            assert (skill_dest / "SKILL.md").read_text() == "old skill\n"
            assert (skill_dest / "docs" / "old.md").read_text() == "old docs\n"
            assert config.read_text() == '{"servers":[]}\n'


def test_installer_validates_with_release_cli_when_path_python_is_broken():
    with tempfile.TemporaryDirectory() as raw_root:
        root = Path(raw_root)
        home = root / "home"
        project = root / "project"
        home.mkdir()
        project.mkdir()
        fake_bin, release_env = prepare_fake_cli_release(root, "v-test")
        marker = root / "python-invoked"
        python = fake_bin / "python3"
        python.write_text(
            "#!/usr/bin/env bash\n"
            f"touch {marker}\n"
            "exit 137\n"
        )
        python.chmod(0o755)
        config = home / ".agent" / "skills" / "doops" / "config.json"
        config.parent.mkdir(parents=True)
        config.write_text(
            json.dumps(
                {
                    "servers": [
                        {
                            "name": "gw",
                            "gateway": "https://gateway.example.com",
                            "cluster": "prod",
                            "instance": "node",
                        }
                    ]
                }
            )
        )

        result = subprocess.run(
            [
                "bash",
                str(INSTALL),
                "--version",
                "v-test",
                "--project",
                str(project),
            ],
            env={
                **os.environ,
                **release_env,
                "HOME": str(home),
                "PATH": f"{fake_bin}:{os.environ['PATH']}",
            },
            text=True,
            capture_output=True,
            check=False,
        )

        assert result.returncode == 0, result.stderr
        assert not marker.exists()
        assert (home / ".local" / "bin" / "doops").exists()


def test_release_verifier_uses_github_asset_default():
    verify = VERIFY_PUBLISHED.read_text()

    assert 'RELEASE_ASSET_BASE="${DOOPS_RELEASE_ASSET_BASE:-https://github.com/symansun/doops/releases/download}"' in verify
    assert '"${RELEASE_ASSET_BASE}/${RELEASE_TAG}/${name}"' in verify
    assert "must be an https URL" in verify
    assert "Published CLI identity mismatch" in verify
    assert "Published CLI identity marker missing" in verify


def prepare_fake_cli_release(root: Path, version: str, commit: str | None = None):
    fixtures = root / "fixtures"
    fake_bin = root / "bin"
    fixtures.mkdir(parents=True, exist_ok=True)
    fake_bin.mkdir(parents=True, exist_ok=True)
    platform, arch = host_platform()
    artifact_name = f"doops-{platform[0]}-{arch}"
    artifact = fixtures / artifact_name
    release_commit = commit or git_commit(ROOT.parent)
    artifact.write_text(
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        "case \"${1:-}\" in\n"
        "  version)\n"
        f"    printf 'release={version} commit={release_commit}\\n'\n"
        "    ;;\n"
        "  --contract)\n"
        "    printf 'doops.sh/cli-contract/v3\\n'\n"
        "    ;;\n"
        "  list)\n"
        "    config=\"${DOOPS_CONFIG:-${HOME}/.agent/skills/doops/config.json}\"\n"
        "    if [[ -z \"${DOOPS_CONFIG:-}\" && ! -f \"$config\" && -f \"${HOME}/.config/doops/config.json\" ]]; then\n"
        "      printf 'legacy config is retired\\n' >&2\n"
        "      exit 1\n"
        "    fi\n"
        "    if [[ -f \"$config\" ]] && grep -q '\"token\"' \"$config\"; then\n"
        "      printf 'Error loading config: json: unknown field \"token\"\\n' >&2\n"
        "      exit 1\n"
        "    fi\n"
        "    ;;\n"
        "esac\n"
    )
    artifact.chmod(0o755)
    checksum = hashlib.sha256(artifact.read_bytes()).hexdigest()
    (fixtures / "checksums.txt").write_text(f"{checksum}  {artifact_name}\n")
    curl = fake_bin / "curl"
    curl.write_text(
        """#!/usr/bin/env bash
set -euo pipefail
out=""
url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --*) shift ;;
    *) url="$1"; shift ;;
  esac
done
cp "${DOOPS_TEST_RELEASE_FIXTURES}/${url##*/}" "${out}"
"""
    )
    curl.chmod(0o755)
    return fake_bin, {"DOOPS_TEST_RELEASE_FIXTURES": str(fixtures)}


def host_platform():
    platform = subprocess.run(
        ["bash", "-c", "uname -s | tr '[:upper:]' '[:lower:]'; uname -m"],
        check=True,
        text=True,
        capture_output=True,
    ).stdout.splitlines()
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[
        platform[1]
    ]
    return platform, arch


def git_commit(repo: Path):
    return subprocess.run(
        ["git", "-C", str(repo), "rev-parse", "HEAD"],
        check=True,
        text=True,
        capture_output=True,
    ).stdout.strip()


def prepare_synthetic_repo(root: Path, skill: str | None, docs: dict[str, str]):
    script = root / "backend" / "scripts" / "install.sh"
    script.parent.mkdir(parents=True)
    script.write_bytes(INSTALL.read_bytes())
    script.chmod(0o755)
    if skill is not None:
        skill_path = root / "backend" / "skills" / "SKILL.md"
        skill_path.parent.mkdir(parents=True, exist_ok=True)
        skill_path.write_text(skill)
    for name, content in docs.items():
        doc_path = root / "backend" / "docs" / name
        doc_path.parent.mkdir(parents=True, exist_ok=True)
        doc_path.write_text(content)

    subprocess.run(["git", "-C", str(root), "init", "-q"], check=True)
    subprocess.run(
        ["git", "-C", str(root), "config", "user.email", "test@example.com"],
        check=True,
    )
    subprocess.run(
        ["git", "-C", str(root), "config", "user.name", "Install Test"],
        check=True,
    )
    # CI nodes may enable commit signing globally; this fixture repo is unsigned.
    subprocess.run(
        ["git", "-C", str(root), "config", "commit.gpgsign", "false"],
        check=True,
    )
    subprocess.run(["git", "-C", str(root), "add", "backend"], check=True)
    subprocess.run(
        ["git", "-C", str(root), "commit", "-q", "-m", "release sources"],
        check=True,
    )
    return root, git_commit(root)


def run_installer(
    repo: Path,
    home: Path,
    project: Path,
    fake_bin: Path,
    release_env: dict[str, str],
):
    return subprocess.run(
        [
            "bash",
            str(repo / "backend" / "scripts" / "install.sh"),
            "--version",
            "v-test",
            "--project",
            str(project),
        ],
        env={
            **os.environ,
            **release_env,
            "HOME": str(home),
            "PATH": f"{fake_bin}:{os.environ['PATH']}",
        },
        text=True,
        capture_output=True,
        check=False,
    )


def test_scripts_install_wrapper_execs_backend_installer():
    root = Path(__file__).resolve().parents[2]
    wrapper = root / "scripts" / "install.sh"
    assert wrapper.is_file(), wrapper
    assert wrapper.stat().st_mode & 0o111, "wrapper must be executable"
    text = wrapper.read_text(encoding="utf-8")
    assert "backend/scripts/install.sh" in text
