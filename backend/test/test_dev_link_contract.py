import os
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
DEV_LINK = ROOT / "scripts" / "dev-link.sh"
BUILD = ROOT / "scripts" / "build-cli.sh"


def test_dev_link_builds_the_repository_and_points_the_installed_cli_at_it():
    text = DEV_LINK.read_text()

    assert 'DOOPS_CLI_OUT="${BUILD_OUT}" bash "${REPO_ROOT}/backend/scripts/build-cli.sh"' in text
    assert 'ln -sfn "${BUILD_OUT}" "${CLI_OUT}"' in text
    assert 'BUILD_OUT="${REPO_ROOT}/bin/doops"' in text
    assert 'CLI_OUT="${HOME}/.local/bin/doops"' in text


def test_dev_link_keeps_the_release_install_as_the_only_supported_entrypoint():
    text = DEV_LINK.read_text()

    # The development link must never become a second install path: the
    # supported user entrypoint stays ~/.local/bin/doops installed by
    # install.sh from GitHub Release assets.
    assert 'backend/scripts/install.sh' in text
    # A real install product is preserved, never deleted.
    assert 'mv "${CLI_OUT}" "${CLI_OUT}.release"' in text
    assert 'rm -f "${CLI_OUT}"' not in text
    assert 'rm -rf "${CLI_OUT}"' not in text


def test_dev_link_links_the_project_skill_to_the_repository_sources():
    text = DEV_LINK.read_text()

    assert 'ln -sfn "${REPO_ROOT}/backend/skills/SKILL.md" "${SKILL_DIR}/SKILL.md"' in text
    assert 'ln -sfn "${REPO_ROOT}/backend/docs" "${SKILL_DIR}/docs"' in text
    # Backups stay outside the Skill tree so retired instructions are not loaded.
    assert 'backup_skill_path "${SKILL_DIR}/SKILL.md"' in text
    assert 'backup_skill_path "${SKILL_DIR}/docs"' in text
    assert '${PROJECT_PATH}/.local/doops-dev-link/backup.XXXXXX' in text


def test_unlink_removes_retired_guidance_from_skill_tree_and_preserves_sources(tmp_path):
    project = tmp_path / "project"
    skill = project / ".agent/skills/doops"
    skill.mkdir(parents=True)
    source = tmp_path / "source"
    source.mkdir()
    (source / "SKILL.md").write_text("current skill\n")
    (source / "docs").mkdir()
    (source / "docs/current.md").write_text("current docs\n")
    (skill / "SKILL.md").symlink_to(source / "SKILL.md")
    (skill / "docs").symlink_to(source / "docs", target_is_directory=True)
    (skill / "SKILL.md.release").write_text("retired skill\n")
    (skill / "docs.release/docs").mkdir(parents=True)
    (skill / "docs.release/docs/retired.md").write_text("retired Helm gate\n")
    (skill / "SKILL.md.devlink-backup").symlink_to(source / "SKILL.md")
    (skill / "docs.devlink-backup").symlink_to(source / "docs")
    # Isolate HOME instead of rewriting the script under test.
    env = {**os.environ, "HOME": str(tmp_path / "home")}
    result = subprocess.run(
        ["bash", str(DEV_LINK), "--unlink", "--project", str(project)],
        capture_output=True, text=True, env=env,
    )
    assert result.returncode == 0, result.stderr
    assert list(skill.iterdir()) == []
    backups = list((project / ".local/doops-dev-link").iterdir())
    assert len(backups) == 1
    backup = backups[0]
    assert (backup / "SKILL.md.release").read_text() == "retired skill\n"
    assert (backup / "docs.release/docs/retired.md").read_text() == "retired Helm gate\n"
    assert (backup / "SKILL.md").is_symlink()
    assert (backup / "docs").is_symlink()
    assert (source / "SKILL.md").read_text() == "current skill\n"
    assert (source / "docs/current.md").read_text() == "current docs\n"
    # Repeating unlink must neither nest a backup nor recreate stale guidance.
    again = subprocess.run(
        ["bash", str(DEV_LINK), "--unlink", "--project", str(project)],
        capture_output=True, text=True, env=env,
    )
    assert again.returncode == 0, again.stderr
    assert list(skill.iterdir()) == []
    assert list((project / ".local/doops-dev-link").iterdir()) == backups


def test_link_preserves_existing_links_and_repeated_link_is_stable(tmp_path):
    repo = tmp_path / "repo"
    scripts = repo / "backend/scripts"
    scripts.mkdir(parents=True)
    script = scripts / "dev-link.sh"
    script.write_text(DEV_LINK.read_text())
    (scripts / "build-cli.sh").write_text(
        '#!/bin/sh\nprintf \'#!/bin/sh\\nprintf "release=dev commit=synthetic\\\\n"\\n\' '
        '> "$DOOPS_CLI_OUT"\nchmod +x "$DOOPS_CLI_OUT"\n'
    )
    (repo / "backend/skills").mkdir()
    (repo / "backend/skills/SKILL.md").write_text("current\n")
    (repo / "backend/docs").mkdir()
    project = tmp_path / "project"
    skill = project / ".agent/skills/doops"
    skill.mkdir(parents=True)
    # Even a dangling link must be preserved without following its target.
    for name in ("SKILL.md", "docs"):
        (skill / name).symlink_to(tmp_path / "old" / name)
    env = {**os.environ, "HOME": str(tmp_path / "home")}
    command = ["bash", str(script), "--project", str(project)]
    first = subprocess.run(command, capture_output=True, text=True, env=env)
    assert first.returncode == 0, first.stderr
    backups = list((project / ".local/doops-dev-link").iterdir())
    assert len(backups) == 1
    for name in ("SKILL.md", "docs"):
        assert (backups[0] / name).readlink() == tmp_path / "old" / name
    assert (skill / "SKILL.md").resolve() == repo / "backend/skills/SKILL.md"
    assert (skill / "docs").resolve() == repo / "backend/docs"
    again = subprocess.run(command, capture_output=True, text=True, env=env)
    assert again.returncode == 0, again.stderr
    assert list((project / ".local/doops-dev-link").iterdir()) == backups
    assert sorted(path.name for path in skill.iterdir()) == ["SKILL.md", "docs"]


def test_dev_link_does_not_publish_or_touch_a_target():
    text = DEV_LINK.read_text()

    for forbidden in (
        "git tag",
        "git push",
        "helm upgrade",
        "doops upgrade",
        "--apply",
    ):
        assert forbidden not in text, f"dev-link must not contain {forbidden}"


def test_dev_link_unlink_restores_the_release_entrypoint():
    text = DEV_LINK.read_text()

    assert "--unlink" in text
    assert 'unlink_path "${CLI_OUT}"' in text
    assert "正式版本请用 backend/scripts/install.sh 安装" in text


def test_dev_link_reports_the_built_commit():
    text = DEV_LINK.read_text()

    assert '"${BUILD_OUT}" version' in text
    assert 'commit=' in text


def test_dev_link_warns_that_a_dev_cli_cannot_upgrade_an_agent():
    # The upgrade path verifies the Agent's release_commit against the CLI's
    # own commit, so a development CLI (release=dev, commit=HEAD) would fail
    # the check and trigger an automatic rollback. The script must say so.
    text = DEV_LINK.read_text()

    assert "release_commit" in text
    assert "Agent 升级命令" in text
    assert "同 commit 的正式 CLI" in text


def test_dev_link_script_is_executable_and_valid_bash():
    assert os.access(DEV_LINK, os.X_OK), "dev-link.sh must be executable"

    result = subprocess.run(
        ["bash", "-n", str(DEV_LINK)], capture_output=True, text=True
    )
    assert result.returncode == 0, result.stderr

    result = subprocess.run(
        ["bash", str(DEV_LINK), "--help"], capture_output=True, text=True
    )
    assert result.returncode == 0, result.stderr
    assert "--build-only" in result.stdout
    assert "--unlink" in result.stdout


def test_build_cli_script_writes_the_repository_bin_path_the_dev_link_uses():
    text = BUILD.read_text()

    assert 'DEFAULT_OUT="${DOOPS_CLI_OUT:-${REPO_ROOT}/bin/doops}"' in text
    assert 'DOOPS_CLI_OUT' in text
