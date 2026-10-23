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
from evalsi_harness import background
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import OUTPUT_LIMIT, Tool, ToolResult, _clip

# The tools that change the sandbox. On replay these run again, as bash does,
# so the replayed file tree is the recorded one and the checker grades it.
MUTATING = frozenset({"edit_file", "apply_patch", "memory"}) | background.MUTATING
# "background" is a group: process_start, process_output, process_write,
# process_list and process_kill (see background.py).
NAMES = ("edit_file", "glob", "grep", "apply_patch", "memory", "background", "shell")
# Offered by the agent loop itself, which owns what they need.
LOOP_NAMES = ("session_search", "delegate", "tool_search", "code_mode")
MAX_LISTED = 500
# Evals.si's own files never show up in searches, as they never show up in the diff.
_PRUNE = "-not -path '*/.git/*' -not -path '*/.evalsi-*'"


def _text(args: dict[str, Any], key: str) -> str | None:
    value = args.get(key)
    return value if isinstance(value, str) else None


MAX_GLOB_PATTERN = 120
MAX_GLOB_PATH = 1024


class GlobMatcher:
    """A glob matched without backtracking: ``*`` and ``?`` stay inside a path
    segment, ``**`` crosses them (``**/`` also matches no directory at all) and
    ``[abc]`` / ``[!abc]`` is a class.

    The pattern comes from the model, so it is run as a small automaton, in time
    proportional to pattern length times path length. A regular expression made
    of the same wildcards can take exponential time and stall the worker."""

    def __init__(self, pattern: str) -> None:
        if len(pattern) > MAX_GLOB_PATTERN:
            raise ValueError(f"a glob is limited to {MAX_GLOB_PATTERN} characters")
        # Tokens: ("lit", c), ("one",), ("star",), ("any",), ("class", chars, negated).
        self.tokens: list[tuple[Any, ...]] = []
        self.skip: dict[int, int] = {}  # an optional group, for "**/"
        i = 0
        while i < len(pattern):
            c = pattern[i]
            if pattern.startswith("**/", i):
                self.skip[len(self.tokens)] = len(self.tokens) + 2
                self.tokens += [("any",), ("lit", "/")]
                i += 3
                continue
            if pattern.startswith("**", i):
                self.tokens.append(("any",))
                i += 2
                continue
            if c == "*":
                self.tokens.append(("star",))
            elif c == "?":
                self.tokens.append(("one",))
            elif c == "[" and (j := pattern.find("]", i + 2)) != -1:
                body = pattern[i + 1 : j]
                negated = body[:1] in ("!", "^")
                self.tokens.append(("class", _class_chars(body[1:] if negated else body), negated))
                i = j
            else:
                self.tokens.append(("lit", c))
            i += 1

    def _close(self, states: set[int]) -> set[int]:
        out, todo = set(states), list(states)
        while todo:
            n = todo.pop()
            if n >= len(self.tokens):
                continue
            nxt = []
            if self.tokens[n][0] in ("star", "any"):
                nxt.append(n + 1)  # matches nothing
            if n in self.skip:
                nxt.append(self.skip[n])
            for m in nxt:
                if m not in out:
                    out.add(m)
                    todo.append(m)
        return out

    def match(self, path: str) -> bool:
        if len(path) > MAX_GLOB_PATH:
            return False
        states = self._close({0})
        for ch in path:
            nxt: set[int] = set()
            for n in states:
                if n >= len(self.tokens):
                    continue
                t = self.tokens[n]
                kind = t[0]
                if kind == "lit":
                    if ch == t[1]:
                        nxt.add(n + 1)
                elif kind == "one":
                    if ch != "/":
                        nxt.add(n + 1)
                elif kind == "class":
                    if ch != "/" and ((ch in t[1]) != t[2]):
                        nxt.add(n + 1)
                elif kind == "star":
                    if ch != "/":
                        nxt.add(n)
                else:  # any
                    nxt.add(n)
            states = self._close(nxt)
            if not states:
                return False
        return len(self.tokens) in states


def _class_chars(body: str) -> frozenset[str]:
    """The characters of a class body such as ``a-c_`` (ranges expanded)."""
    chars: set[str] = set()
    i = 0
    while i < len(body):
        if i + 2 < len(body) and body[i + 1] == "-":
            lo, hi = ord(body[i]), ord(body[i + 2])
            chars.update(chr(c) for c in range(lo, min(hi, lo + 256) + 1))
            i += 3
        else:
            chars.add(body[i])
            i += 1
    return frozenset(chars)


def glob_regex(pattern: str) -> GlobMatcher:
    """Kept for callers of the old name: a matcher with ``match(path)``."""
    return GlobMatcher(pattern)


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
        try:
            regex = GlobMatcher(pattern.removeprefix("./"))
        except ValueError as exc:
            return ToolResult(str(exc), is_error=True)
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


MEMORY_DIR = ".evalsi-memory"
MEMORY_FILE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}\Z")
MEMORY_MAX_BYTES = 64 * 1024
MEMORY_MAX_FILES = 100


class MemoryTool:
    """Notes that outlive context management. They are files in the sandbox
    (so a replay rebuilds them), under ``.evalsi-memory/``, which Evals.si's
    own naming keeps out of the agent's diff."""

    def __init__(self, env: TaskEnvironment) -> None:
        self.env = env
        self.spec = ToolSpec(
            name="memory",
            description=(
                "Keep notes that must survive your context being shortened: decisions, what you "
                "have verified, what is left to do. Actions: list, read (name), write (name, "
                "content), append (name, content), delete (name). Read your notes again after "
                "the conversation is summarized."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "action": {
                        "type": "string",
                        "enum": ["list", "read", "write", "append", "delete"],
                    },
                    "name": {"type": "string", "description": "A file name such as progress.md."},
                    "content": {"type": "string"},
                },
                "required": ["action"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        action, name = _text(args, "action"), _text(args, "name")
        if action == "list":
            return await self._list()
        if action not in ("read", "write", "append", "delete"):
            return ToolResult("action must be list, read, write, append or delete", is_error=True)
        if not name or not MEMORY_FILE.match(name):
            return ToolResult(
                "name must be a plain file name (letters, digits, dots, dashes, underscores)", True
            )
        path = f"{MEMORY_DIR}/{name}"
        try:
            if action == "read":
                files = await self.env.sandbox.read_files([path], max_bytes=MEMORY_MAX_BYTES)
                if path not in files:
                    return ToolResult(f"no note named {name}", is_error=True)
                return ToolResult(files[path].decode(errors="replace"))
            if action == "delete":
                result = await self.env.run(f"rm -f -- {shlex.quote(path)}", span=span)
                return ToolResult(
                    f"deleted {name}" if result.ok else "could not delete", not result.ok
                )
            content = _text(args, "content")
            if content is None:
                return ToolResult(f"{action} needs content", is_error=True)
            if action == "append":
                files = await self.env.sandbox.read_files([path], max_bytes=MEMORY_MAX_BYTES)
                existing = files.get(path, b"").decode(errors="replace")
                content = (
                    existing + ("" if not existing or existing.endswith("\n") else "\n") + content
                )
            if len(content.encode()) > MEMORY_MAX_BYTES:
                return ToolResult(f"a note is limited to {MEMORY_MAX_BYTES} bytes", is_error=True)
            if await self._count() >= MEMORY_MAX_FILES and not await self._exists(path):
                return ToolResult(f"at most {MEMORY_MAX_FILES} notes; delete one first", True)
            await self.env.sandbox.write_files({path: content})
        except SandboxError as exc:
            return ToolResult(str(exc), is_error=True)
        return ToolResult(f"{'appended to' if action == 'append' else 'wrote'} {name}")

    async def _names(self) -> list[str]:
        files = await self.env.sandbox.read_files([MEMORY_DIR], max_bytes=MEMORY_MAX_BYTES)
        return sorted(p.removeprefix(MEMORY_DIR + "/") for p in files)

    async def _count(self) -> int:
        return len(await self._names())

    async def _exists(self, path: str) -> bool:
        return path.removeprefix(MEMORY_DIR + "/") in await self._names()

    async def _list(self) -> ToolResult:
        try:
            names = await self._names()
        except SandboxError as exc:
            return ToolResult(str(exc), is_error=True)
        return ToolResult("\n".join(names) if names else "no notes yet")


def extra_tools(env: TaskEnvironment, names: list[str]) -> list[Tool]:
    """The optional tools a spec asked for, in the order it listed them."""
    names = [n for n in names if n not in LOOP_NAMES]
    unknown = [n for n in names if n not in NAMES]
    if unknown:
        raise ValueError(f"unknown built-in tool(s) {unknown}; choose from {[*NAMES, *LOOP_NAMES]}")
    made: dict[str, Tool] = {
        "edit_file": EditFileTool(env),
        "glob": GlobTool(env),
        "grep": GrepTool(env),
        "apply_patch": ApplyPatchTool(env),
        "memory": MemoryTool(env),
    }
    tools: list[Tool] = []
    for n in dict.fromkeys(names):
        if n == "background":
            tools += background.process_tools(env)
        elif n == "shell":
            tools += background.shell_tools(env)
        else:
            tools.append(made[n])
    return tools
