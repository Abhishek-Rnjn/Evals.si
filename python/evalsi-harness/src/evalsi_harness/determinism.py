"""The determinism record of one agent run: what a later replay or rerun needs
to know to tell "the agent changed" from "the world under it changed".

It is written with the run, in the record's ``metadata["determinism"]``:

- ``seeds``: what the harness seeds (the simulated user, fault injection) and
  the sampling settings of the target (a temperature; no provider takes a seed
  for chat, so there is none to record).
- ``models``: for each model the run called, the name asked for and the names
  the provider answered with (the ``model`` field of its responses, a dated
  snapshot where the provider gives one) and OpenAI's ``system_fingerprint``.
- ``harness``: the harness name and version, the profile (``name@version``),
  the context strategy, the optional tools.
- ``tools``: the versions of what the run ran on (Python, the harness, the SDK)
  and the MCP servers' own reported name and version.
- ``image``: the sandbox image as the spec named it and the digest the sandbox
  reported (empty when the rung reports none), and the isolation it ran under.

``evalsi determinism --against`` (see ``evalsi.determinism.compare_runs``)
compares two runs' trajectories and records, and names the first step where
they differ.
"""

from __future__ import annotations

import platform
from typing import Any

from evalsi_harness._version import __version__

RECORD_VERSION = 1


def served_models(spend: dict[str, set[str]], fingerprints: dict[str, set[str]]) -> dict[str, Any]:
    """The ``models`` part: each requested name with what the provider answered."""
    out: dict[str, Any] = {}
    for name in sorted(set(spend) | set(fingerprints)):
        entry: dict[str, Any] = {"served": sorted(spend.get(name, set()))}
        if fingerprints.get(name):
            entry["system_fingerprints"] = sorted(fingerprints[name])
        out[name] = entry
    return out


def build(
    *,
    seeds: dict[str, Any],
    models: dict[str, Any],
    harness: dict[str, Any],
    mcp_servers: dict[str, dict[str, Any]],
    image: dict[str, Any],
    extra_versions: dict[str, str] | None = None,
) -> dict[str, Any]:
    """The record. Empty parts are left out, so a run that had no MCP server or
    no sandbox does not carry placeholders that look like data."""
    from evalsi._version import __version__ as sdk_version

    tools: dict[str, Any] = {
        "python": platform.python_version(),
        "evalsi_harness": __version__,
        "evalsi": sdk_version,
        **(extra_versions or {}),
    }
    if mcp_servers:
        tools["mcp_servers"] = mcp_servers
    record: dict[str, Any] = {
        "version": RECORD_VERSION,
        "seeds": {k: v for k, v in seeds.items() if v is not None},
        "models": models,
        "harness": harness,
        "tools": tools,
    }
    if image:
        record["image"] = image
    return record
