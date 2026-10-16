"""Protected files: keeping an agent from editing what the checker relies on (H4).

``checker.protect`` lists glob patterns, relative to the workdir (``tests/**``,
``**/conftest.py``, ``pytest.ini``). Their contents are hashed when the trial
starts. Before the checker runs they are compared with what the agent left:

* a file that changed, a file that is gone and a file that is new under a
  protected pattern are *changes*;
* with ``on_change: fail`` (the default) the trial fails without running the
  checker, because the state it would grade is not one it can trust;
* with ``on_change: restore`` the originals are written back and the new files
  removed, then the checker runs, so a real fix still passes while the tampering
  stays on the trajectory as a guardrail step either way.

This covers the workdir only. An agent can also tamper outside it (a
``sitecustomize.py`` on the Python path, a shim earlier on ``PATH``, the
installed packages); closing that needs ``sandbox.read_only_root``, and running
the checker on a clean copy of the setup snapshot with only the agent's diff
applied is not built. Symlinks are never followed: they are
inventoried apart (path and target), a protected file replaced by a link counts as
deleted and the link as new, and restore removes links and directories standing
where an original was before writing it back.
"""

from __future__ import annotations

import asyncio
import hashlib
from dataclasses import dataclass, field

from evalsi.sandbox import SandboxError
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.toolset import GlobMatcher

MAX_FILES = 5000
MAX_LINKS = 500
MAX_FILE_BYTES = 8 * 1024 * 1024
MAX_TOTAL_BYTES = 64 * 1024 * 1024
LIST_LIMIT = 16 * 1024 * 1024
MODES = ("fail", "restore")

_LIST = "find . -type f -not -path './.git/*' -not -path './.evalsi-*' -print0"
_LIST_LINKS = "find . -type l -not -path './.git/*' -not -path './.evalsi-*' -print0"


class ProtectError(Exception):
    """The protected set could not be taken or checked (a cap, or the sandbox)."""


@dataclass
class Changes:
    modified: list[str] = field(default_factory=list)
    deleted: list[str] = field(default_factory=list)
    created: list[str] = field(default_factory=list)

    def __bool__(self) -> bool:
        return bool(self.modified or self.deleted or self.created)

    def describe(self, limit: int = 20) -> str:
        parts = []
        for label, names in (
            ("modified", self.modified),
            ("deleted", self.deleted),
            ("created", self.created),
        ):
            if names:
                shown = ", ".join(names[:limit])
                more = f" and {len(names) - limit} more" if len(names) > limit else ""
                parts.append(f"{label}: {shown}{more}")
        return "protected files changed; " + "; ".join(parts)

    def to_dict(self) -> dict[str, list[str]]:
        return {"modified": self.modified, "deleted": self.deleted, "created": self.created}


def validate_patterns(patterns: list[str]) -> None:
    for p in patterns:
        if not p or p.startswith("/") or ".." in p.split("/"):
            raise ValueError(f"protect pattern {p!r} must be relative to the workdir, without ..")
        GlobMatcher(p.rstrip("/") if p.endswith("/") else p)  # raises ValueError if too long


def _matcher(patterns: list[str]) -> list[tuple[GlobMatcher, str]]:
    out = []
    for p in patterns:
        base = p.rstrip("/")
        out.append((GlobMatcher(base), base))
    return out


def _selected(files: list[str], patterns: list[str]) -> list[str]:
    matchers = _matcher(patterns)
    chosen = []
    for path in files:
        if any(m.match(path) or path.startswith(base + "/") for m, base in matchers):
            chosen.append(path)
    return sorted(chosen)


async def _exec(env: TaskEnvironment, argv: list[str], what: str, timeout_s: float = 120) -> str:
    try:
        result = await env.sandbox.exec(
            argv, timeout_s=timeout_s, env=env.config.env, output_limit=LIST_LIMIT
        )
    except SandboxError as exc:
        raise ProtectError(f"{what}: {exc}") from exc
    if not result.ok:
        detail = result.stderr.strip()[-300:] or result.outcome
        raise ProtectError(f"{what}: {detail}")
    if result.truncated:
        raise ProtectError(f"{what}: the output was cut off (too many files)")
    return result.stdout


def _names(out: str) -> list[str]:
    names = [p.removeprefix("./") for p in out.split("\0") if p]
    for name in names:
        if "\n" in name:
            raise ProtectError(f"a file name contains a newline: {name!r}")
    return names


async def _listing(env: TaskEnvironment) -> list[str]:
    return _names(await _exec(env, ["sh", "-c", _LIST], "could not list the workdir"))


async def _links(env: TaskEnvironment, patterns: list[str]) -> dict[str, str]:
    """Symlinks under the protected patterns and where they point. They are never
    followed: ``find -type f`` does not list them, so they are inventoried apart."""
    paths = _selected(
        _names(await _exec(env, ["sh", "-c", _LIST_LINKS], "could not list the workdir")),
        patterns,
    )
    if len(paths) > MAX_LINKS:
        raise ProtectError(f"{len(paths)} symlinks match the protect patterns; at most {MAX_LINKS}")
    sem = asyncio.Semaphore(8)

    async def target(path: str) -> str:
        async with sem:
            out = await _exec(env, ["readlink", "--", path], f"could not read symlink {path}", 30)
            return out.rstrip("\n")

    return dict(zip(paths, await asyncio.gather(*(target(p) for p in paths)), strict=True))


async def _read(
    env: TaskEnvironment, paths: list[str], keep: bool
) -> tuple[dict[str, str], dict[str, bytes]]:
    """Hash every path, one request each: ``read_files(max_bytes)`` caps a whole
    request and drops what does not fit without saying so."""
    if len(paths) > MAX_FILES:
        raise ProtectError(
            f"{len(paths)} files match the protect patterns; at most {MAX_FILES} are supported"
        )
    sem = asyncio.Semaphore(8)

    async def one(path: str) -> tuple[str, bytes]:
        async with sem:
            try:
                got = await env.sandbox.read_files([path], max_bytes=MAX_FILE_BYTES + 1)
            except SandboxError as exc:
                raise ProtectError(f"could not read protected file {path}: {exc}") from exc
        data = got.get(path)
        if data is None or len(data) > MAX_FILE_BYTES:
            # Listed, but nothing came back: over the limit, or gone mid-read.
            raise ProtectError(
                f"protected file {path} could not be read in full "
                f"(over {MAX_FILE_BYTES // 1024 // 1024} MiB, or changed while being read)"
            )
        return path, data

    hashes: dict[str, str] = {}
    blobs: dict[str, bytes] = {}
    total = 0
    for path, data in await asyncio.gather(*(one(p) for p in paths)):
        total += len(data)
        if total > MAX_TOTAL_BYTES:
            raise ProtectError("the protected files total more than 64 MiB")
        hashes[path] = hashlib.sha256(data).hexdigest()
        if keep:
            blobs[path] = data
    return hashes, blobs


# Make room for a restored file: drop a symlink or directory standing at the path
# or at any parent (writing through a symlinked parent would restore nothing, and
# a file where a directory belongs would stop the write).
_CLEAR = """p="$1"
d=$(dirname -- "$p"); anc=""
while [ "$d" != "." ] && [ "$d" != "/" ]; do anc="$d
$anc"; d=$(dirname -- "$d"); done
printf '%s\n' "$anc" | while IFS= read -r a; do
  [ -n "$a" ] || continue
  if [ -L "$a" ] || { [ -e "$a" ] && [ ! -d "$a" ]; }; then rm -f -- "$a"; fi
done
if [ -L "$p" ]; then rm -f -- "$p"; elif [ -d "$p" ]; then rm -rf -- "$p"; fi
exit 0"""


@dataclass
class Baseline:
    patterns: list[str]
    mode: str
    hashes: dict[str, str]
    blobs: dict[str, bytes] = field(default_factory=dict)
    links: dict[str, str] = field(default_factory=dict)

    @classmethod
    async def take(cls, env: TaskEnvironment, patterns: list[str], mode: str) -> Baseline:
        paths = _selected(await _listing(env), patterns)
        hashes, blobs = await _read(env, paths, keep=mode == "restore")
        return cls(patterns, mode, hashes, blobs, await _links(env, patterns))

    async def compare(self, env: TaskEnvironment) -> Changes:
        paths = _selected(await _listing(env), self.patterns)
        now, _ = await _read(env, paths, keep=False)
        links = await _links(env, self.patterns)
        changes = Changes()
        for path, digest in self.hashes.items():
            if path not in now:
                changes.deleted.append(path)
            elif now[path] != digest:
                changes.modified.append(path)
        created = set(now) - set(self.hashes)
        for path, target in links.items():
            if path not in self.links:
                created.add(path)  # a new link: its target is code the checker may load
            elif self.links[path] != target:
                changes.modified.append(path)
        changes.deleted.extend(p for p in self.links if p not in links)
        changes.created = sorted(created)
        return changes

    async def restore(self, env: TaskEnvironment, changes: Changes) -> None:
        back = {p: self.blobs[p] for p in [*changes.modified, *changes.deleted] if p in self.blobs}
        relink = {
            p: self.links[p] for p in [*changes.modified, *changes.deleted] if p in self.links
        }
        # New files and links go first, then whatever stands where an original was.
        for path in changes.created:
            await _exec(env, ["rm", "-rf", "--", path], f"could not remove {path}", 30)
        for path in [*back, *relink]:
            await _exec(env, ["sh", "-c", _CLEAR, "sh", path], f"could not clear {path}", 30)
        try:
            if back:
                await env.sandbox.write_files(dict(back))
        except SandboxError as exc:
            raise ProtectError(f"could not restore protected files: {exc}") from exc
        for path, target in relink.items():
            await _exec(env, ["ln", "-s", "--", target, path], f"could not relink {path}", 30)
