"""Optional editing and search tools for the built-in agent.

``bash``, ``read_file`` and ``write_file`` are always the default toolset. These
are offered only when a spec lists them (``harness.builtin.tools.builtin``), so
a comparison between models stays fair and old results stay comparable.

They depend on as little as possible in the task image: ``edit_file`` works
through the sandbox's file API, ``glob`` on ``find`` and Python's matching,
``grep`` on POSIX ``grep``, and ``apply_patch`` on ``git apply`` or ``patch``,
whichever the image has, with a clear error when it has neither.
"""

from __future__ import annotations

import re
import shlex
from typing import Any

from evalsi.sandbox import SandboxError
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import OUTPUT_LIMIT, Tool, ToolResult, _clip

# The tools that change the sandbox. On replay these run again, as bash does,
# so the replayed file tree is the recorded one and the checker grades it.
MUTATING = frozenset({"edit_file", "apply_patch"})
NAMES = ("edit_file", "glob", "grep", "apply_patch")
MAX_LISTED = 500
# Evals.si's own files never show up in searches, as they never show up in the diff.
_PRUNE = "-not -path '*/.git/*' -not -path '*/.evalsi-*'"


def _text(args: dict[str, Any], key: str) -> str | None:
    value = args.get(key)
    return value if isinstance(value, str) else None


def glob_regex(pattern: str) -> re.Pattern[str]:
    """A glob as a regular expression: ``*`` and ``?`` stay inside a path
    segment, ``**`` crosses them, ``[...]`` is a class."""
    out, i = [], 0
    while i < len(pattern):
        c = pattern[i]
        if pattern.startswith("**/", i):
            out.append("(?:.*/)?")
            i += 3
            continue
        if pattern.startswith("**", i):
            out.append(".*")
            i += 2
            continue
        if c == "*":
            out.append("[^/]*")
        elif c == "?":
            out.append("[^/]")
        elif c == "[":
            j = pattern.find("]", i + 2)
            if j == -1:
                out.append(re.escape(c))
            else:
                body = pattern[i + 1 : j]
                body = "^" + body[1:] if body.startswith("!") else body
                out.append(f"[{body.replace(chr(92), chr(92) * 2)}]")
                i = j
        else:
            out.append(re.escape(c))
        i += 1
    return re.compile("".join(out) + r"\Z")


class EditFileTool:
    def __init__(self, env: TaskEnvironment) -> None:
        self.env = env
        self.spec = ToolSpec(
            name="edit_file",
            description=(
                "Edit a file by replacing exact text. old_string must match the file exactly "
                "(whitespace included) and be unique, unless replace_all is set; the edit fails "
                "otherwise, so add surrounding lines to make it unique. Prefer this to rewriting "
                "a whole file."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Relative to the workdir."},
                    "old_string": {"type": "string"},
                    "new_string": {"type": "string"},
                    "replace_all": {"type": "boolean"},
                },
                "required": ["path", "old_string", "new_string"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        path, old, new = _text(args, "path"), _text(args, "old_string"), _text(args, "new_string")
        if not path or old is None or new is None:
            return ToolResult("edit_file needs path, old_string and new_string", is_error=True)
        if old == "":
            return ToolResult("old_string cannot be empty; use write_file to create a file", True)
        if old == new:
            return ToolResult("old_string and new_string are the same: nothing to change", True)
        try:
            files = await self.env.sandbox.read_files([path], max_bytes=4 << 20)
        except SandboxError as exc:
            return ToolResult(str(exc), is_error=True)
        if path not in files:
            return ToolResult(f"no such file: {path}", is_error=True)
        text = files[path].decode(errors="surrogateescape")
        count = text.count(old)
        if count == 0:
            return ToolResult(f"old_string was not found in {path}{_near_miss(text, old)}", True)
        if count > 1 and args.get("replace_all") is not True:
            return ToolResult(
                f"old_string appears {count} times in {path}; add surrounding lines to make it "
                "unique, or set replace_all to change every one",
                is_error=True,
            )
        line = text.count("\n", 0, text.index(old)) + 1
        edited = (
            text.replace(old, new) if args.get("replace_all") is True else text.replace(old, new, 1)
        )
        try:
            await self.env.sandbox.write_files(
                {path: edited.encode(errors="surrogateescape")}, mode=await self._mode(path)
            )
        except SandboxError as exc:
            return ToolResult(str(exc), is_error=True)
        n = count if args.get("replace_all") is True else 1
        return ToolResult(
            f"edited {path}: {n} replacement{'s' if n != 1 else ''}, first at line {line}"
        )

    async def _mode(self, path: str) -> int:
        """The file's mode, so editing a script does not drop its executable bit."""
        quoted = shlex.quote(path)
        result = await self.env.sandbox.exec(
            ["sh", "-c", f"stat -c %a -- {quoted} 2>/dev/null || stat -f %Lp -- {quoted}"]
        )
        try:
            return int(result.stdout.strip().splitlines()[0], 8)
        except (ValueError, IndexError):
            return 0o644


def _near_miss(text: str, old: str) -> str:
    """A hint when the first line of old_string exists with other whitespace."""
    first = next((ln.strip() for ln in old.splitlines() if ln.strip()), "")
    if not first:
        return ""
    for number, line in enumerate(text.splitlines(), 1):
        if line.strip() == first:
            return (
                f"; a line matching its first line without the surrounding whitespace is at "
                f"line {number}: {line!r}. Copy the exact text, indentation included"
            )
    return ""


class GlobTool:
    def __init__(self, env: TaskEnvironment) -> None:
        self.env = env
        self.spec = ToolSpec(
            name="glob",
            description=(
                "List files whose path matches a glob (* and ? stay inside a directory, ** "
                f"crosses directories, [abc] is a class), for example 'src/**/*.py'. Up to "
                f"{MAX_LISTED} paths, sorted, relative to the workdir."
            ),
            input_schema={
                "type": "object",
                "properties": {"pattern": {"type": "string"}},
                "required": ["pattern"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        pattern = _text(args, "pattern")
        if not pattern:
            return ToolResult("glob needs a pattern", is_error=True)
        regex = glob_regex(pattern.removeprefix("./"))
        result = await self.env.run(f"find . -type f {_PRUNE}", span=span)
        if not result.ok:
            return ToolResult(f"find failed: {result.stderr.strip()[:300]}", is_error=True)
        paths = sorted(
            p.removeprefix("./")
            for p in result.stdout.splitlines()
            if regex.match(p.removeprefix("./"))
        )
        if not paths:
            return ToolResult(f"no files match {pattern}")
        shown = paths[:MAX_LISTED]
        more = f"\n... and {len(paths) - MAX_LISTED} more" if len(paths) > MAX_LISTED else ""
        return ToolResult("\n".join(shown) + more, metadata={"matches": len(paths)})


class GrepTool:
    def __init__(self, env: TaskEnvironment) -> None:
        self.env = env
        self.spec = ToolSpec(
            name="grep",
            description=(
                "Search file contents with an extended regular expression. Returns path:line:text "
                "for matches, skipping binary files. Narrow with path (a file or directory) and "
                "include (a file-name glob such as '*.py')."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "pattern": {"type": "string"},
                    "path": {"type": "string", "description": "Default: the workdir."},
                    "include": {"type": "string", "description": "File-name glob, e.g. '*.py'."},
                    "ignore_case": {"type": "boolean"},
                    "max_results": {"type": "integer", "description": "Default 200."},
                },
                "required": ["pattern"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        pattern = _text(args, "pattern")
        if not pattern:
            return ToolResult("grep needs a pattern", is_error=True)
        path = _text(args, "path") or "."
        limit = args.get("max_results")
        limit = limit if isinstance(limit, int) and 0 < limit <= 2000 else 200
        flags = "-rnIE" + ("i" if args.get("ignore_case") is True else "")
        include = _text(args, "include")
        command = (
            # --include goes first: with both kinds, a file that matches neither is
            # searched unless the first option is an --include.
            f"grep {flags}"
            + (f" --include={shlex.quote(include)}" if include else "")
            + " --exclude-dir=.git --exclude-dir='.evalsi-*' --exclude='.evalsi-*'"
            + f" -e {shlex.quote(pattern)} -- {shlex.quote(path)}"
        )
        result = await self.env.run(command, span=span)
        if result.exit_code == 1 and not result.stdout:
            return ToolResult(f"no matches for {pattern!r} in {path}")
        if result.exit_code not in (0, 1):
            return ToolResult(f"grep failed: {result.stderr.strip()[:400]}", is_error=True)
        lines = [ln.removeprefix("./") for ln in result.stdout.splitlines()]
        shown = lines[:limit]
        more = (
            f"\n... {len(lines) - limit} more matches; narrow the search"
            if len(lines) > limit
            else ""
        )
        return ToolResult(_clip("\n".join(shown) + more), metadata={"matches": len(lines)})


class ApplyPatchTool:
    def __init__(self, env: TaskEnvironment) -> None:
        self.env = env
        self.spec = ToolSpec(
            name="apply_patch",
            description=(
                "Apply a unified diff (as git diff prints, paths relative to the workdir, "
                "-p1 style 'a/' and 'b/' prefixes) in one step, to change several files "
                "together. The patch is checked first and applied only if all of it applies."
            ),
            input_schema={
                "type": "object",
                "properties": {"patch": {"type": "string"}},
                "required": ["patch"],
            },
        )
        self._count = 0

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        patch = _text(args, "patch")
        if not patch or not patch.strip():
            return ToolResult("apply_patch needs a patch", is_error=True)
        if not patch.endswith("\n"):
            patch += "\n"
        self._count += 1
        name = f".evalsi-patch-{self._count}.diff"
        try:
            await self.env.sandbox.write_files({name: patch})
        except SandboxError as exc:
            return ToolResult(str(exc), is_error=True)
        q = shlex.quote(name)
        script = (
            f"if command -v git >/dev/null 2>&1; then "
            f"git apply --check --whitespace=nowarn {q} && git apply --whitespace=nowarn {q}; "
            f"elif command -v patch >/dev/null 2>&1; then "
            f"patch -p1 --dry-run --batch < {q} >/dev/null && patch -p1 --batch < {q}; "
            f"else echo 'neither git nor patch is installed in this image: use edit_file' >&2; "
            f"exit 127; fi"
        )
        result = await self.env.run(script, span=span)
        await self.env.run(f"rm -f {q}", span=span)
        if not result.ok:
            detail = (result.stderr or result.stdout).strip()[:1500]
            return ToolResult(f"the patch did not apply, and nothing was changed: {detail}", True)
        files = re.findall(r"^\+\+\+ (?:b/)?(\S+)", patch, flags=re.M)
        return ToolResult(
            f"applied the patch to {len(files)} file(s): {', '.join(files)}"[:OUTPUT_LIMIT]
        )


def extra_tools(env: TaskEnvironment, names: list[str]) -> list[Tool]:
    """The optional tools a spec asked for, in the order it listed them."""
    unknown = [n for n in names if n not in NAMES]
    if unknown:
        raise ValueError(f"unknown built-in tool(s) {unknown}; choose from {list(NAMES)}")
    made: dict[str, Tool] = {
        "edit_file": EditFileTool(env),
        "glob": GlobTool(env),
        "grep": GrepTool(env),
        "apply_patch": ApplyPatchTool(env),
    }
    return [made[n] for n in dict.fromkeys(names)]
