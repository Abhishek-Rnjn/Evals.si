"""What a run spec may make the worker execute outside the sandbox.

Some spec fields name things the worker itself runs: stdio MCP server
commands, external harness commands, Python harness classes and checker
parsers. On your own machine (``evalsi run``) the spec is yours and is
trusted. On a server, specs come from API callers, so the worker allows only
what the server's config lists (``agents.trusted_commands`` and
``agents.trusted_python``); Python references into installed ``evalsi_*``
packages (adapters such as ``evalsi_swebench:parse_log``) are always allowed.
Everything an agent itself does runs in the sandbox and needs no trust.
"""

from __future__ import annotations

import json
import os
from collections.abc import Sequence
from dataclasses import dataclass

from evalsi_harness.task import TaskError

ENV = "EVALSI_AGENT_TRUST"


@dataclass
class Trust:
    # None means everything is trusted (embedded runs).
    commands: list[list[str]] | None = None
    python: list[str] | None = None

    @classmethod
    def from_env(cls) -> Trust:
        """The worker's policy, set by evalsid in EVALSI_AGENT_TRUST."""
        raw = os.environ.get(ENV)
        if not raw:
            return cls()
        data = json.loads(raw)
        return cls(
            commands=[list(c) for c in data.get("commands") or []],
            python=list(data.get("python") or []),
        )

    @property
    def restricted(self) -> bool:
        return self.commands is not None

    def check_command(self, argv: Sequence[str], what: str) -> None:
        if self.commands is None or list(argv) in self.commands:
            return
        raise TaskError(
            f"{what}: the command {list(argv)} would run on the server's worker, outside the "
            "sandbox; list it in the server config under agents.trusted_commands to allow it"
        )

    def check_python(self, ref: str, what: str) -> None:
        if self.python is None or ref in self.python or ref.startswith("evalsi_"):
            return
        raise TaskError(
            f"{what} {ref!r} would run on the server's worker; list it in the server config "
            "under agents.trusted_python to allow it"
        )


__all__ = ["ENV", "Trust"]
