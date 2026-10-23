"""Interjections: scripted messages from the user while an agent works (H4).

An interjection is timed by a step count (after the agent's ``at_step``-th model
call) or by an event:

* ``tool_call``: after a tool call whose name matches the glob ``match`` (any when
  empty) and, when ``pattern`` is set, whose JSON arguments match that regular
  expression;
* ``file_write``: after a tool call that wrote a path matching the glob ``match``
  (``write_file``, ``edit_file`` and ``apply_patch``, and the file tools of CLI agents
  such as ``Write`` and ``Edit``; a shell command's writes cannot be told apart);
* ``stop``: the first time the agent tries to finish.

An event interjection fires once, and ``at_step`` (when set) is the earliest model
call it may fire after. The built-in loop delivers it at the start of the next model
call. A CLI agent runs to completion, so ``cli.resume_command`` delivers it after the
run (see connectors.py).
"""

from __future__ import annotations

import fnmatch
import json
import re
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

from evalsi_harness.toolset import GlobMatcher

EVENTS = ("tool_call", "file_write", "stop")
MAX_PATTERN = 300

_WRITERS = frozenset(
    {
        "write_file",
        "edit_file",
        "apply_patch",
        "write",
        "edit",
        "multiedit",
        "notebookedit",
        "create_file",
        "str_replace_editor",
        "str_replace_based_edit_tool",
        "replace",
    }
)
_PATH_KEYS = ("path", "file_path", "filepath", "filename", "file", "notebook_path")
_PATCH_PATH = re.compile(
    r"^(?:\+\+\+ (?:b/)?|\*\*\* (?:Update|Add|Delete) File: )(\S+)", re.MULTILINE
)


@dataclass(frozen=True)
class Interjection:
    at_step: int
    message: str
    on: str = ""
    match: str = ""
    pattern: str = ""

    @property
    def timed_by_event(self) -> bool:
        return bool(self.on)

    def describe(self) -> str:
        if not self.on:
            return f"after model call {self.at_step}"
        detail = " ".join(x for x in (self.match, f"/{self.pattern}/" if self.pattern else "") if x)
        return f"on {self.on}" + (f" {detail}" if detail else "")


def validate(items: list[Interjection]) -> None:
    """Raise ValueError when an interjection cannot be delivered as declared."""
    for j in items:
        if not j.message.strip():
            raise ValueError("an interjection needs a message")
        if not j.on:
            if j.at_step < 1:
                raise ValueError("an interjection without an event needs at_step of at least 1")
            if j.match or j.pattern:
                raise ValueError("match and pattern apply only with an event (on)")
            continue
        if j.on not in EVENTS:
            raise ValueError(f"interjection on must be one of {', '.join(EVENTS)}")
        if j.at_step < 0:
            raise ValueError("at_step cannot be negative")
        if j.on == "stop" and (j.match or j.pattern):
            raise ValueError("a stop interjection takes no match or pattern")
        if j.on == "file_write" and j.pattern:
            raise ValueError("a file_write interjection takes match (a path glob), not pattern")
        if j.on == "file_write" and not j.match:
            raise ValueError("a file_write interjection needs match, a path glob")
        if len(j.pattern) > MAX_PATTERN:
            raise ValueError(f"pattern is limited to {MAX_PATTERN} characters")
        if j.pattern:
            try:
                re.compile(j.pattern)
            except re.error as exc:
                raise ValueError(f"pattern is not a valid regular expression: {exc}") from exc
        if j.match:
            GlobMatcher(j.match)  # raises ValueError if too long


def _args(raw: Any) -> dict[str, Any]:
    if isinstance(raw, Mapping):
        return dict(raw)
    try:
        value = json.loads(raw or "{}")
    except (TypeError, ValueError):
        return {}
    return value if isinstance(value, dict) else {}


def written_paths(name: str, raw_args: Any) -> list[str]:
    """The files a file-writing tool call names, relative as the tool gave them."""
    if name.lower() not in _WRITERS:
        return []
    args = _args(raw_args)
    paths = [str(args[k]) for k in _PATH_KEYS if isinstance(args.get(k), str)]
    for key in ("patch", "diff", "input"):
        if isinstance(args.get(key), str):
            paths += _PATCH_PATH.findall(args[key])
    return [p.removeprefix("./") for p in paths]


def _path_matches(glob: str, path: str) -> bool:
    base = path.rsplit("/", 1)[-1]
    matcher = GlobMatcher(glob)
    return matcher.match(path) or ("/" not in glob and matcher.match(base))


def call_matches(j: Interjection, name: str, raw_args: Any) -> bool:
    """Whether a (successful) tool call is the event ``j`` waits for."""
    if j.on == "tool_call":
        if j.match and not fnmatch.fnmatchcase(name, j.match):
            return False
        return (
            not j.pattern
            or re.search(j.pattern, raw_args if isinstance(raw_args, str) else json.dumps(raw_args))
            is not None
        )
    if j.on == "file_write":
        return any(_path_matches(j.match, p) for p in written_paths(name, raw_args))
    return False


def from_proto(items: Any) -> list[Interjection]:
    out = [Interjection(int(i.at_step), i.message, i.event, i.match, i.pattern) for i in items]
    validate(out)
    return out
