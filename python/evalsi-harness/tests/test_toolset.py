"""The optional editing and search tools."""

from __future__ import annotations

import asyncio
import shutil
import stat
from collections.abc import Awaitable, Callable
from pathlib import Path
from typing import Any

import pytest

from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.task import EnvironmentConfig
from evalsi_harness.testing import LocalSandboxClient
from evalsi_harness.tools import ToolResult
from evalsi_harness.toolset import (
    MUTATING,
    NAMES,
    ApplyPatchTool,
    EditFileTool,
    GlobTool,
    GrepTool,
    extra_tools,
    glob_regex,
)
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO, calls_then


def with_env(
    files: dict[str, str], body: Callable[[TaskEnvironment, Path], Awaitable[None]]
) -> None:
    async def go() -> None:
        client = LocalSandboxClient()
        sandbox = await client.create()
        env = TaskEnvironment(sandbox, EnvironmentConfig())
        root = Path(sandbox.root)
        for name, content in files.items():
            (root / name).parent.mkdir(parents=True, exist_ok=True)
            (root / name).write_text(content)
        try:
            await body(env, root)
        finally:
            await client.aclose()

    asyncio.run(go())


def call(tool: Any, **args: Any) -> ToolResult:
    result: ToolResult = (
        asyncio.get_event_loop_policy().new_event_loop().run_until_complete(tool.call(args))
    )
    return result


# --- edit_file ---------------------------------------------------------------------


def test_edit_file_replaces_one_unique_match() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        tool = EditFileTool(env)
        r = await tool.call(
            {"path": "a.py", "old_string": "return a - b", "new_string": "return a + b"}
        )
        assert not r.is_error
        assert "first at line 2" in r.content
        assert (root / "a.py").read_text() == "def add(a, b):\n    return a + b\n"

    with_env({"a.py": "def add(a, b):\n    return a - b\n"}, body)


def test_edit_file_refuses_ambiguity_and_absence_and_changes_nothing() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        tool = EditFileTool(env)
        text = (root / "a.txt").read_text()
        many = await tool.call({"path": "a.txt", "old_string": "x = 1", "new_string": "x = 2"})
        assert many.is_error
        assert "appears 3 times" in many.content
        missing = await tool.call({"path": "a.txt", "old_string": "nope", "new_string": "y"})
        assert missing.is_error
        assert "was not found" in missing.content
        nofile = await tool.call({"path": "zzz.txt", "old_string": "a", "new_string": "b"})
        assert "no such file" in nofile.content
        same = await tool.call({"path": "a.txt", "old_string": "x = 1", "new_string": "x = 1"})
        assert same.is_error
        empty = await tool.call({"path": "a.txt", "old_string": "", "new_string": "q"})
        assert empty.is_error
        bad = await tool.call({"path": "a.txt", "old_string": 1, "new_string": "q"})
        assert bad.is_error
        assert (root / "a.txt").read_text() == text

    with_env({"a.txt": "x = 1\ny\nx = 1\nz\nx = 1\n"}, body)


def test_replace_all_changes_every_match() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        r = await EditFileTool(env).call(
            {"path": "a.txt", "old_string": "x = 1", "new_string": "x = 2", "replace_all": True}
        )
        assert "3 replacements" in r.content
        assert (root / "a.txt").read_text().count("x = 2") == 3

    with_env({"a.txt": "x = 1\ny\nx = 1\nz\nx = 1\n"}, body)


def test_a_wrong_indentation_gets_a_hint() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        # The file's line is indented with spaces; old_string uses a tab.
        r = await EditFileTool(env).call(
            {"path": "a.py", "old_string": "\treturn a - b", "new_string": "\treturn a + b"}
        )
        assert "line 2" in r.content
        assert "indentation" in r.content

    with_env({"a.py": "def add(a, b):\n    return a - b\n"}, body)


def test_editing_a_script_keeps_it_executable() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        script = root / "run.sh"
        script.chmod(0o755)
        await EditFileTool(env).call({"path": "run.sh", "old_string": "one", "new_string": "two"})
        assert script.read_text() == "echo two\n"
        assert stat.S_IMODE(script.stat().st_mode) == 0o755

    with_env({"run.sh": "echo one\n"}, body)


# --- glob --------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("pattern", "path", "matches"),
    [
        ("*.py", "a.py", True),
        ("*.py", "src/a.py", False),
        ("**/*.py", "src/a.py", True),
        ("**/*.py", "a.py", True),
        ("src/**/*.py", "src/x/y/a.py", True),
        ("src/*.py", "src/x/a.py", False),
        ("a?.txt", "ab.txt", True),
        ("a?.txt", "a/.txt", False),
        ("file[0-9].txt", "file3.txt", True),
        ("file[!0-9].txt", "file3.txt", False),
        ("a.b", "axb", False),
        ("**", "any/where/at/all", True),
    ],
)
def test_glob_patterns(pattern: str, path: str, matches: bool) -> None:
    assert bool(glob_regex(pattern).match(path)) is matches


def test_glob_lists_sorted_files_and_hides_git_and_evalsi_files() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        tool = GlobTool(env)
        r = await tool.call({"pattern": "**/*.py"})
        assert r.content.splitlines() == ["a.py", "pkg/b.py", "pkg/sub/c.py"]
        assert r.metadata["matches"] == 3
        none = await tool.call({"pattern": "*.rs"})
        assert none.content == "no files match *.rs"
        assert (await tool.call({})).is_error

    with_env(
        {
            "a.py": "",
            "pkg/b.py": "",
            "pkg/sub/c.py": "",
            ".git/hooks/h.py": "",
            ".evalsi-out/0001.py": "",
            "pkg/.evalsi-patch-1.py": "",
        },
        body,
    )


def test_glob_caps_a_long_listing() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        r = await GlobTool(env).call({"pattern": "*.txt"})
        assert len(r.content.splitlines()) == 501
        assert "and 20 more" in r.content
        assert r.metadata["matches"] == 520

    with_env({f"f{i:04d}.txt": "" for i in range(520)}, body)


# --- grep --------------------------------------------------------------------------


def test_grep_finds_lines_and_respects_path_include_and_case() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        tool = GrepTool(env)
        r = await tool.call({"pattern": r"def \w+\("})
        assert sorted(r.content.splitlines()) == [
            "a.py:1:def add(a, b):",
            "pkg/b.py:1:def sub(a, b):",
        ]
        only = await tool.call({"pattern": "def", "path": "pkg"})
        assert only.content == "pkg/b.py:1:def sub(a, b):"
        inc = await tool.call({"pattern": "ADD", "ignore_case": True, "include": "*.md"})
        assert inc.content == "notes.md:1:add things"
        assert (await tool.call({"pattern": "zzzzz"})).content.startswith("no matches")
        # Hidden Evals.si and git files are not searched.
        assert (await tool.call({"pattern": "secret"})).content.startswith("no matches")
        # A pattern that starts with a dash is a pattern, not an option.
        assert (await tool.call({"pattern": "-x"})).content.startswith("no matches")
        bad = await tool.call({"pattern": "(unclosed"})
        assert bad.is_error
        assert "grep failed" in bad.content

    with_env(
        {
            "a.py": "def add(a, b):\n",
            "pkg/b.py": "def sub(a, b):\n",
            "notes.md": "add things\n",
            ".git/config": "secret\n",
            ".evalsi-out/0001.txt": "secret\n",
        },
        body,
    )


def test_grep_caps_matches_and_says_so() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        r = await GrepTool(env).call({"pattern": "line", "max_results": 5})
        assert len(r.content.splitlines()) == 6
        assert "95 more matches" in r.content

    with_env({"f.txt": "line\n" * 100}, body)


# --- apply_patch -------------------------------------------------------------------

PATCH = """\
--- a/a.py
+++ b/a.py
@@ -1,2 +1,2 @@
 def add(a, b):
-    return a - b
+    return a + b
--- a/new.txt
+++ b/new.txt
@@ -0,0 +1 @@
+hello
"""


@pytest.mark.skipif(not (shutil.which("git") or shutil.which("patch")), reason="no git or patch")
def test_apply_patch_changes_several_files_atomically() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        tool = ApplyPatchTool(env)
        r = await tool.call({"patch": PATCH})
        assert not r.is_error, r.content
        assert "a.py" in r.content
        assert (root / "a.py").read_text() == "def add(a, b):\n    return a + b\n"
        assert not list(root.glob(".evalsi-patch-*"))
        # A patch that does not apply leaves everything as it was.
        broken = PATCH.replace("-    return a - b", "-    return a * b").replace("hello", "hi")
        bad = await tool.call({"patch": broken})
        assert bad.is_error
        assert "nothing was changed" in bad.content
        assert (root / "a.py").read_text() == "def add(a, b):\n    return a + b\n"
        assert (root / "new.txt").read_text() == "hello\n"
        assert (await tool.call({"patch": "  "})).is_error

    with_env({"a.py": "def add(a, b):\n    return a - b\n"}, body)


def test_extra_tools_by_name_in_order_and_unknown_names_are_refused() -> None:
    async def body(env: TaskEnvironment, root: Path) -> None:
        tools = extra_tools(env, ["grep", "edit_file", "grep"])
        assert [t.spec.name for t in tools] == ["grep", "edit_file"]
        with pytest.raises(ValueError, match="unknown built-in tool"):
            extra_tools(env, ["rm"])
        assert set(NAMES) == {"edit_file", "glob", "grep", "apply_patch"}
        assert {"edit_file", "apply_patch"} == MUTATING

    with_env({}, body)


# --- through the harness -----------------------------------------------------------


def test_the_default_toolset_is_unchanged_and_the_extras_are_opt_in(
    model: Any, sandboxes: Any
) -> None:
    server = model(calls_then([{"text": "ok"}]))

    def offered(builtin: list[str] | None) -> list[str]:
        tools: dict[str, Any] = {"builtin": builtin} if builtin is not None else {}
        spec = spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": server.base_url,
                },
                "harness": {"builtin": {"tools": tools}},
                "environment": {},
            }
        )
        n = len(server.requests)
        run(spec, record(), sandboxes)
        return [t["function"]["name"] for t in server.requests[n]["tools"]]

    assert offered(None) == ["bash", "read_file", "write_file", "request_escalation"]
    assert offered(["glob", "edit_file"]) == [
        "bash",
        "read_file",
        "write_file",
        "request_escalation",
        "glob",
        "edit_file",
    ]


def test_edits_replay_into_the_same_file_tree(model: Any, sandboxes: Any, tmp_path: Path) -> None:
    """edit_file and apply_patch are re-executed on replay, so the checker grades the
    recorded tree and not an unedited one."""
    server = model(
        calls_then(
            [
                {"tool_calls": [("write_file", {"path": "out.txt", "content": "howdy\n"})]},
                {
                    "tool_calls": [
                        (
                            "edit_file",
                            {"path": "out.txt", "old_string": "howdy", "new_string": "hello"},
                        )
                    ]
                },
                {"text": "done"},
            ]
        )
    )

    def spec(mode: str) -> Any:
        return spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": server.base_url,
                },
                "harness": {
                    "builtin": {
                        "tools": {"builtin": ["edit_file"]},
                        "recording": {"mode": mode, "dir": "tapes"},
                    }
                },
                "environment": {"checker": CHECK_HELLO},
            }
        )

    first = run(spec("record"), record(), sandboxes, base_dir=tmp_path).record
    assert first is not None
    assert first.check is not None
    assert first.check.passed
    sent = len(server.requests)
    again = run(spec("replay"), record(), sandboxes, base_dir=tmp_path).record
    assert len(server.requests) == sent
    assert again is not None
    assert again.check is not None
    assert again.check.passed  # the edit ran again: out.txt says hello
