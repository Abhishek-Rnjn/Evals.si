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
applied is not built. Symlinks are not followed: a protected file replaced by
a symlink counts as deleted.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field

from evalsi.sandbox import SandboxError
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.toolset import GlobMatcher

MAX_FILES = 5000
MAX_FILE_BYTES = 8 * 1024 * 1024
MAX_TOTAL_BYTES = 64 * 1024 * 1024
LIST_LIMIT = 16 * 1024 * 1024
MODES = ("fail", "restore")

_LIST = "find . -type f -not -path './.git/*' -not -path './.evalsi-*' -print0"


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


async def _listing(env: TaskEnvironment) -> list[str]:
    try:
        result = await env.sandbox.exec(
            ["sh", "-c", _LIST], timeout_s=120, env=env.config.env, output_limit=LIST_LIMIT
        )
    except SandboxError as exc:
        raise ProtectError(f"could not list the workdir: {exc}") from exc
    if not result.ok:
        raise ProtectError(f"could not list the workdir: {result.stderr.strip()[-300:]}")
    if result.truncated:
        raise ProtectError("the workdir has too many files to scan for protected ones")
    return [p.removeprefix("./") for p in result.stdout.split("\0") if p]


async def _read(
    env: TaskEnvironment, paths: list[str], keep: bool
) -> tuple[dict[str, str], dict[str, bytes]]:
    if len(paths) > MAX_FILES:
        raise ProtectError(
            f"{len(paths)} files match the protect patterns; at most {MAX_FILES} are supported"
        )
    hashes: dict[str, str] = {}
    blobs: dict[str, bytes] = {}
    total = 0
    for start in range(0, len(paths), 200):
        chunk = paths[start : start + 200]
        try:
            got = await env.sandbox.read_files(chunk, max_bytes=MAX_FILE_BYTES + 1)
        except SandboxError as exc:
            raise ProtectError(f"could not read protected files: {exc}") from exc
        for path, data in got.items():
            if len(data) > MAX_FILE_BYTES:
                raise ProtectError(
                    f"protected file {path} is over {MAX_FILE_BYTES // 1024 // 1024} MiB"
                )
            total += len(data)
            if total > MAX_TOTAL_BYTES:
                raise ProtectError("the protected files total more than 64 MiB")
            hashes[path] = hashlib.sha256(data).hexdigest()
            if keep:
                blobs[path] = data
    return hashes, blobs


@dataclass
class Baseline:
    patterns: list[str]
    mode: str
    hashes: dict[str, str]
    blobs: dict[str, bytes] = field(default_factory=dict)

    @classmethod
    async def take(cls, env: TaskEnvironment, patterns: list[str], mode: str) -> Baseline:
        paths = _selected(await _listing(env), patterns)
        hashes, blobs = await _read(env, paths, keep=mode == "restore")
        return cls(patterns, mode, hashes, blobs)

    async def compare(self, env: TaskEnvironment) -> Changes:
        paths = _selected(await _listing(env), self.patterns)
        now, _ = await _read(env, paths, keep=False)
        changes = Changes()
        for path, digest in self.hashes.items():
            if path not in now:
                changes.deleted.append(path)
            elif now[path] != digest:
                changes.modified.append(path)
        changes.created = sorted(set(now) - set(self.hashes))
        return changes

    async def restore(self, env: TaskEnvironment, changes: Changes) -> None:
        back = {p: self.blobs[p] for p in [*changes.modified, *changes.deleted]}
        try:
            if back:
                await env.sandbox.write_files(dict(back))
            for path in changes.created:
                await env.sandbox.exec(["rm", "-f", "--", path], timeout_s=30, env=env.config.env)
        except SandboxError as exc:
            raise ProtectError(f"could not restore protected files: {exc}") from exc
