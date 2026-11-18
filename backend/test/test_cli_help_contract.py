"""The top-level help must list exactly the commands main() dispatches.

The help text lives in a raw string inside main(), so it drifts silently when a
command is added or renamed. Compare it against the dispatch switch instead of
trusting review.
"""

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
MAIN = ROOT / "backend" / "cli" / "cli" / "doops" / "main.go"

# Aliases that deliberately share another command's entry.
ALIASES = {"rm"}


def _help_commands(source: str) -> list[str]:
    start = source.index("可用命令:")
    end = source.index("选项:", start)
    block = source[start:end]
    return re.findall(r"^  ([a-z][a-z0-9]*)\s+", block, re.MULTILINE)


def _dispatched_commands(source: str) -> set[str]:
    names: set[str] = set()
    for match in re.finditer(r'^\tcase ((?:"[a-z][a-z0-9]*"(?:, )?)+):', source, re.MULTILINE):
        names.update(re.findall(r'"([a-z][a-z0-9]*)"', match.group(1)))
    return names


def test_help_lists_every_dispatched_command():
    source = MAIN.read_text(encoding="utf-8")
    listed = _help_commands(source)
    dispatched = _dispatched_commands(source)

    assert listed, "failed to parse the top-level help block"
    assert dispatched, "failed to parse the dispatch switch"

    missing = sorted(dispatched - ALIASES - set(listed))
    assert not missing, f"dispatched but absent from help: {missing}"

    unknown = sorted(set(listed) - dispatched)
    assert not unknown, f"listed in help but never dispatched: {unknown}"

    assert len(listed) == len(set(listed)), f"duplicate help entries: {listed}"


def test_help_block_uses_spaces_not_tabs():
    source = MAIN.read_text(encoding="utf-8")
    start = source.index("可用命令:")
    end = source.index("选项:", start)
    block = source[start:end]
    assert "\t" not in block, "help entries must align with spaces, not tabs"
