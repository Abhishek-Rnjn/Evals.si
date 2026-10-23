"""Checker isolation from the agent's diff (H4).

``checker.isolation: diff`` grades the agent's changes to the workdir, and only
those, in a sandbox the agent never ran in:

* a fresh sandbox is built for the same environment (the setup snapshot where
  the rung has one, a new setup otherwise): it is the setup baseline;
* both workdirs are inventoried the same way. When the baseline's workdir is a
  git repository, ``git`` lists every file it would commit (``git add -A`` into a
  temporary index, so tracked, untracked and modified files are in and ignored
  files are out); otherwise every regular file is hashed with ``sha256sum``. When
  the agent broke its repository, both sides fall back to hashing;
* the difference is the agent's diff: files added, modified (content or the
  execute bit) and deleted. A change is *rejected* when its path matches
  ``checker.protect``, a workdir path in ``checker.files``, ``checker.exclude``,
  or falls outside ``checker.outputs`` when that is set; symlinks and other
  non-regular files are rejected too, since they could point the checker at
  something outside the diff;
* the accepted changes are applied to the fresh sandbox (contents read through
  the sandbox API, never through a command the agent could have replaced) and the
  checker runs there.

The applied diff (a unified diff, cut at 256 KiB) becomes a ``checker:diff`` step,
and every rejected path a ``checker_diff_rejected`` policy event, so an agent that
edited the tests is visible on its trajectory even when its fix passes. The
inventory comes from commands that run in the agent's sandbox; a forged listing
can only hide the agent's own changes or name paths that are filtered like any
other, because paths are checked to be canonical and inside the workdir.

What is left behind: anything outside the workdir (installed packages, shims on
``PATH``), ignored files in a repository, and symlinks. Services are not
supported (their state is never snapshotted), as with ``isolation: clean``.
"""

from __future__ import annotations

import difflib
import posixpath
from dataclasses import dataclass, field

from evalsi.sandbox import SandboxError
from evalsi.types import Content, Step, TaskCheck
from evalsi_harness.checker import run_checker
from evalsi_harness.environment import EnvironmentPool, TaskEnvironment
from evalsi_harness.events import PolicyEvent, StepEvent, span_id
from evalsi_harness.protect import (
    LIST_LIMIT,
    ProtectError,
    _exec,
    _read,
    _selected,
)
from evalsi_harness.task import CheckerConfig, Task

DIFF_LIMIT = 256 * 1024
MAX_CHANGES = 5000
MAX_REJECTED_EVENTS = 50

# A temporary index from the current one, every change added to it, then listed:
# "<mode> <blob> <stage>\t<path>\0". Exit 4: git is missing, broken or not at the
# workdir's root (the fallback hashes files instead).
GIT_LIST = r"""command -v git >/dev/null 2>&1 || exit 4
g() { git -c safe.directory='*' -c core.quotepath=off "$@"; }
[ "$(g rev-parse --is-inside-work-tree 2>/dev/null)" = true ] || exit 4
[ -z "$(g rev-parse --show-prefix 2>/dev/null)" ] || exit 4
idx=$(mktemp) || exit 4
trap 'rm -f "$idx"' EXIT
cp "$(g rev-parse --git-path index)" "$idx" 2>/dev/null || rm -f "$idx"
GIT_INDEX_FILE=$idx g add -A -- . ':(exclude).evalsi-*' >/dev/null 2>&1 || exit 4
GIT_INDEX_FILE=$idx g ls-files -s -z || exit 4"""

# Every regular file, hashed; then every symlink, after a marker.
FILE_LIST = r"""command -v sha256sum >/dev/null 2>&1 || { echo 'sha256sum is missing' >&2; exit 5; }
find . \( -path ./.git -o -name '.evalsi-*' \) -prune -o -type f -print0 |
  xargs -0 -r sha256sum -- || exit 5
printf '@@evalsi-links\n'
find . \( -path ./.git -o -name '.evalsi-*' \) -prune -o ! -type d ! -type f -print0"""


@dataclass
class Entry:
    digest: str
    executable: bool = False
    regular: bool = True


@dataclass
class Inventory:
    method: str  # "git" or "files"
    entries: dict[str, Entry] = field(default_factory=dict)


@dataclass
class Changes:
    added: list[str] = field(default_factory=list)
    modified: list[str] = field(default_factory=list)
    deleted: list[str] = field(default_factory=list)
    rejected: list[tuple[str, str]] = field(default_factory=list)

    def to_dict(self) -> dict[str, object]:
        return {
            "added": self.added,
            "modified": self.modified,
            "deleted": self.deleted,
            "rejected": [{"path": p, "reason": r} for p, r in self.rejected],
        }


def _canonical(path: str) -> bool:
    parts = path.split("/")
    return bool(path) and not path.startswith("/") and all(p not in ("", ".", "..") for p in parts)


def _parse_git(out: str) -> dict[str, Entry]:
    entries: dict[str, Entry] = {}
    for item in out.split("\0"):
        if not item:
            continue
        meta, sep, path = item.partition("\t")
        fields = meta.split()
        if not sep or len(fields) != 3:
            raise ProtectError(f"git listed an unreadable entry: {item[:120]!r}")
        mode, blob, _stage = fields
        entries[path] = Entry(
            digest=blob,
            executable=mode == "100755",
            # 120000 is a symlink and 160000 a submodule: never applied.
            regular=mode in ("100644", "100755"),
        )
    return entries


def _unescape(name: str) -> str:
    out, i = [], 0
    while i < len(name):
        if name[i] == "\\" and i + 1 < len(name):
            out.append({"n": "\n", "\\": "\\"}.get(name[i + 1], name[i + 1]))
            i += 2
        else:
            out.append(name[i])
            i += 1
    return "".join(out)


def _parse_files(out: str) -> dict[str, Entry]:
    hashed, marker, links = out.partition("@@evalsi-links\n")
    if not marker:
        raise ProtectError("the file listing was cut short")
    entries: dict[str, Entry] = {}
    for line in hashed.splitlines():
        if not line:
            continue
        escaped = line.startswith("\\")  # GNU sha256sum escapes odd names
        body = line[1:] if escaped else line
        digest, sep, name = body.partition("  ")
        if not sep or len(digest) != 64:
            raise ProtectError(f"sha256sum printed an unreadable line: {line[:120]!r}")
        name = _unescape(name) if escaped else name
        entries[name.removeprefix("./")] = Entry(digest=digest)
    for name in links.split("\0"):
        if name:
            entries[name.removeprefix("./")] = Entry(digest="link:" + name, regular=False)
    return entries


async def inventory(env: TaskEnvironment, method: str) -> Inventory:
    """The workdir's files by ``method`` ("git" or "files"). Raises ProtectError when
    git is unusable (for "git") or the listing cannot be taken."""
    if method == "git":
        try:
            result = await env.sandbox.exec(
                ["sh", "-c", GIT_LIST], timeout_s=300, env=env.config.env, output_limit=LIST_LIMIT
            )
        except SandboxError as exc:
            raise ProtectError(f"git listing: {exc}") from exc
        if not result.ok or result.truncated:
            raise ProtectError("git could not list the workdir")
        return Inventory("git", _parse_git(result.stdout))
    out = await _exec(env, ["sh", "-c", FILE_LIST], "could not hash the workdir", 300)
    inv = Inventory("files", _parse_files(out))
    # The execute bit, which sha256sum does not see.
    regular = sorted(p for p, e in inv.entries.items() if e.regular)
    for start in range(0, len(regular), 500):
        chunk = regular[start : start + 500]
        listed = await _exec(
            env,
            [
                "sh",
                "-c",
                'for f; do [ -x "$f" ] && printf "%s\\0" "$f"; done; exit 0',
                "sh",
                *chunk,
            ],
            "could not read file modes",
        )
        for name in listed.split("\0"):
            if name in inv.entries:
                inv.entries[name].executable = True
    return inv


def _workdir_paths(files: dict[str, str], workdir: str) -> list[str]:
    """The checker's files that land in the workdir, relative to it."""
    out = []
    root = posixpath.normpath(workdir or "/")
    for name in files:
        if name.startswith("/"):
            norm = posixpath.normpath(name)
            if norm.startswith(root.rstrip("/") + "/"):
                out.append(norm[len(root.rstrip("/")) + 1 :])
        else:
            out.append(posixpath.normpath(name))
    return out


def compare(base: Inventory, now: Inventory, checker: CheckerConfig, workdir: str) -> Changes:
    """What the agent changed against the baseline, split into what may be applied
    and what is rejected (with the reason)."""
    changes = Changes()
    hidden = _workdir_paths(checker.files, workdir)
    guarded = [
        ("protected", checker.protect),
        ("a checker file", hidden),
        ("excluded", checker.exclude),
    ]
    candidates: list[tuple[str, str]] = []
    for path, entry in now.entries.items():
        old = base.entries.get(path)
        if old is None:
            candidates.append((path, "added"))
        elif (old.digest, old.executable, old.regular) != (
            entry.digest,
            entry.executable,
            entry.regular,
        ):
            candidates.append((path, "modified"))
    candidates += [(p, "deleted") for p in base.entries if p not in now.entries]
    if len(candidates) > MAX_CHANGES:
        raise ProtectError(f"the agent changed {len(candidates)} files; at most {MAX_CHANGES}")
    for path, kind in sorted(candidates):
        reason = ""
        if not _canonical(path):
            reason = "not a canonical path in the workdir"
        else:
            for label, patterns in guarded:
                if patterns and _selected([path], patterns):
                    reason = label
                    break
            if not reason and checker.outputs and not _selected([path], checker.outputs):
                reason = "outside the declared outputs"
            current = now.entries.get(path)
            if not reason and kind != "deleted" and current is not None and not current.regular:
                reason = "not a regular file (a symlink or special file)"
        if reason:
            changes.rejected.append((path, f"{kind}, {reason}"))
        else:
            getattr(changes, kind).append(path)
    return changes


def _unified(path: str, old: bytes | None, new: bytes | None) -> str:
    try:
        a = (old or b"").decode("utf-8").splitlines(keepends=True)
        b = (new or b"").decode("utf-8").splitlines(keepends=True)
    except UnicodeDecodeError:
        return f"Binary files a/{path} and b/{path} differ\n"
    lines = difflib.unified_diff(
        a,
        b,
        fromfile="/dev/null" if old is None else f"a/{path}",
        tofile="/dev/null" if new is None else f"b/{path}",
    )
    return "".join(
        line if line.endswith("\n") else line + "\n\\ No newline at end of file\n" for line in lines
    )


_LINKED = """for d in "$@"; do
  if [ -L "$d" ]; then printf '%s' "$d"; exit 3; fi
done
exit 0"""


async def _symlinked_ancestor(env: TaskEnvironment, paths: list[str]) -> str:
    dirs: set[str] = set()
    for path in paths:
        parts = path.split("/")[:-1]
        dirs.update("/".join(parts[: n + 1]) for n in range(len(parts)))
    if not dirs:
        return ""
    result = await env.sandbox.exec(
        ["sh", "-c", _LINKED, "sh", *sorted(dirs)], timeout_s=60, env=env.config.env
    )
    return result.stdout if result.exit_code == 3 else ""


@dataclass
class DiffResult:
    check: TaskCheck
    info: dict[str, object] = field(default_factory=dict)
    events: list[PolicyEvent | StepEvent] = field(default_factory=list)


async def run_diff_checker(
    pool: EnvironmentPool, agent_env: TaskEnvironment, checker: CheckerConfig, task: Task
) -> DiffResult:
    """Grade the agent's diff in a fresh sandbox of the same environment."""
    assert task.environment is not None
    try:
        fresh = await pool.acquire(task.environment)
    except Exception as exc:  # a TaskError or SandboxError: the check cannot run
        return DiffResult(TaskCheck(passed=False, error=f"diff checker sandbox: {exc}"))
    try:
        return await _graded(fresh, agent_env, checker, task)
    except SandboxError as exc:
        return DiffResult(TaskCheck(passed=False, error=f"diff checker could not run: {exc}"))
    finally:
        await fresh.sandbox.destroy()


async def _graded(
    fresh: TaskEnvironment, agent_env: TaskEnvironment, checker: CheckerConfig, task: Task
) -> DiffResult:
    info: dict[str, object] = {"isolation": "diff"}
    try:
        # The fresh sandbox is the setup state, so its method is the one to trust.
        try:
            base = await inventory(fresh, "git")
            try:
                now = await inventory(agent_env, "git")
            except ProtectError:
                info["git_fallback"] = "the agent's repository could not be read"
                base = await inventory(fresh, "files")
                now = await inventory(agent_env, "files")
        except ProtectError:
            base = await inventory(fresh, "files")
            now = await inventory(agent_env, "files")
        info["method"] = base.method
        changes = compare(base, now, checker, agent_env.workdir)
        written = [*changes.added, *changes.modified]
        _, blobs = await _read(agent_env, written, keep=True)
        _, olds = await _read(fresh, [*changes.modified, *changes.deleted], keep=True)
    except ProtectError as exc:
        return DiffResult(
            TaskCheck(passed=False, error=f"the agent's diff could not be taken: {exc}")
        )
    info.update(changes.to_dict())
    events: list[PolicyEvent | StepEvent] = []
    for path, reason in changes.rejected[:MAX_REJECTED_EVENTS]:
        events.append(PolicyEvent(kind="checker_diff_rejected", detail=f"{path}: {reason}"))
    if len(changes.rejected) > MAX_REJECTED_EVENTS:
        events.append(
            PolicyEvent(
                kind="checker_diff_rejected",
                detail=f"and {len(changes.rejected) - MAX_REJECTED_EVENTS} more paths",
            )
        )
    patch = "".join(
        _unified(p, olds.get(p), blobs.get(p) if p not in changes.deleted else None)
        for p in sorted([*written, *changes.deleted])
    )
    info["diff_bytes"] = len(patch.encode())
    if len(patch) > DIFF_LIMIT:
        patch = patch[:DIFF_LIMIT] + f"\n[diff truncated at {DIFF_LIMIT} bytes]\n"
    info["diff"] = patch
    events.append(
        StepEvent(
            Step(
                type="guardrail",
                name="checker:diff",
                output=Content(text=patch),
                span_id=span_id(),
                attributes={
                    "evalsi.checker.diff.method": base.method,
                    "evalsi.checker.diff.added": len(changes.added),
                    "evalsi.checker.diff.modified": len(changes.modified),
                    "evalsi.checker.diff.deleted": len(changes.deleted),
                    "evalsi.checker.diff.rejected": len(changes.rejected),
                },
            )
        )
    )
    linked = await _symlinked_ancestor(fresh, [*written, *changes.deleted])
    if linked:
        # Setup left a link where a directory should be: writing through it would put
        # a change outside the paths that were checked.
        check = TaskCheck(passed=False, error=f"a changed path crosses a symlink: {linked}")
        return DiffResult(check, info, events)
    if changes.deleted:
        await _exec(fresh, ["rm", "-f", "--", *changes.deleted], "could not apply deletions")
    if blobs:
        await fresh.sandbox.write_files(dict(blobs))
    executable = [p for p in written if now.entries[p].executable]
    plain = [p for p in changes.modified if not now.entries[p].executable]
    if executable:
        await _exec(fresh, ["chmod", "a+x", "--", *executable], "could not set execute bits")
    if plain:
        await _exec(fresh, ["chmod", "a-x", "--", *plain], "could not clear execute bits")
    return DiffResult(await run_checker(fresh, checker, task), info, events)
