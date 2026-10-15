"""``tool_search``: load tools on demand instead of putting every schema in the prompt.

An agent wired to several MCP servers can face hundreds of tools, and their
schemas alone can fill a context. With ``tool_search`` in ``tools.builtin``, the
MCP tools start out unloaded: the model sees one tool, ``tool_search``, and the
names of what exists. A search loads the best matches, and from the next call
on they are in the tool list like any other. Loading appends to the tool list,
so the part of the prompt before it keeps its cache.
"""

from __future__ import annotations

import re
from typing import TYPE_CHECKING, Any

from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import ToolResult

if TYPE_CHECKING:
    from evalsi_harness.loop import AgentLoop

MAX_LOADED = 5
NAMES_SHOWN = 60
_WORD = re.compile(r"[a-z0-9]+")


def words(text: str) -> list[str]:
    return _WORD.findall(text.lower().replace("_", " ").replace("-", " "))


def _summary(spec: ToolSpec) -> str:
    """The first line of a tool's description; MCP descriptions are optional."""
    lines = spec.description.strip().splitlines()
    return lines[0][:160] if lines else ""


def score(query: list[str], spec: ToolSpec) -> float:
    """Name matches count three times a description match; a query word that
    is part of a name word counts too (search finds search_issues)."""
    name, desc = words(spec.name), words(spec.description)
    total = 0.0
    for q in query:
        total += 3 * sum(1 for w in name if w == q) + sum(1 for w in desc if w == q)
        total += 1.5 * sum(1 for w in name if q != w and (q in w or w in q) and len(q) > 2)
    return total


class ToolSearchTool:
    def __init__(self, loop: AgentLoop) -> None:
        self.loop = loop
        names = sorted(loop.deferred)
        shown = ", ".join(names[:NAMES_SHOWN]) + (
            f", and {len(names) - NAMES_SHOWN} more" if len(names) > NAMES_SHOWN else ""
        )
        self.spec = ToolSpec(
            name="tool_search",
            description=(
                f"{len(names)} more tools exist but are not loaded, to keep this prompt short. "
                "Search them by what you need to do (for example 'create a calendar event'); the "
                f"best matches (up to {MAX_LOADED}) are loaded and usable from your next step. "
                f"Their names: {shown}."
            ),
            input_schema={
                "type": "object",
                "properties": {"query": {"type": "string"}},
                "required": ["query"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        query = args.get("query")
        if not isinstance(query, str) or not query.strip():
            return ToolResult("tool_search needs a query", is_error=True)
        q = words(query)
        pool = [self.loop.tools[n].spec for n in sorted(self.loop.deferred)]
        ranked = sorted(
            ((score(q, s), s) for s in pool if s.name not in self.loop.active),
            key=lambda pair: (-pair[0], pair[1].name),
        )
        hits = [s for sc, s in ranked if sc > 0][:MAX_LOADED]
        if not hits:
            loaded = sorted(self.loop.active)
            more = f" Already loaded: {', '.join(loaded)}." if loaded else ""
            return ToolResult(f"no unloaded tool matches {query!r}; try other words.{more}")
        for s in hits:
            self.loop.active.add(s.name)
        lines = [f"- {s.name}: {_summary(s)}".rstrip(": ") for s in hits]
        return ToolResult(
            "Loaded, and available from your next step:\n" + "\n".join(lines),
            metadata={"loaded": [s.name for s in hits]},
        )
