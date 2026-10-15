"""The unabridged history of a run, and the tool that searches it.

Context management shortens what the model is sent: old tool results become
stubs, early turns become a summary. The archive keeps every message exactly
as it was, so an agent that needs a detail it no longer sees can search for it
(``session_search``) and read the whole entry back.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Any

from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import OUTPUT_LIMIT, ToolResult, _clip

SNIPPET = 120


@dataclass
class ArchiveEntry:
    index: int
    role: str
    name: str  # the tool, for a tool result or a call
    text: str


class Archive:
    def __init__(self) -> None:
        self.entries: list[ArchiveEntry] = []

    def add(self, message: dict[str, Any]) -> None:
        """Keeps a conversation message as it is now, before anything shortens it."""
        role = str(message.get("role"))
        if role == "tool":
            self.entries.append(
                ArchiveEntry(
                    len(self.entries),
                    "tool",
                    str(message.get("name") or message.get("tool_call_id") or ""),
                    str(message.get("content") or ""),
                )
            )
            return
        text = str(message.get("content") or "")
        for call in message.get("tool_calls") or []:
            fn = call.get("function") or {}
            text += f"\n[call {fn.get('name')}({fn.get('arguments')})]"
        self.entries.append(ArchiveEntry(len(self.entries), role, "", text.strip()))


class SessionSearchTool:
    """``session_search``: find and re-read what the conversation no longer shows."""

    def __init__(self, archive: Archive) -> None:
        self.archive = archive
        self.spec = ToolSpec(
            name="session_search",
            description=(
                "Search everything said and done earlier in this task, including tool outputs "
                "that were cleared from your context or folded into a summary. Give a query (a "
                "regular expression, case-insensitive) to list matching entries with a snippet "
                "and their numbers, or an entry number as index to read it in full."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "query": {"type": "string"},
                    "index": {"type": "integer", "description": "An entry number from a search."},
                    "max_results": {"type": "integer", "description": "Default 8."},
                },
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        entries = self.archive.entries
        index = args.get("index")
        if isinstance(index, int) and not isinstance(index, bool):
            if not 0 <= index < len(entries):
                return ToolResult(
                    f"there is no entry {index}; entries run from 0 to {len(entries) - 1}", True
                )
            e = entries[index]
            label = f"{e.role} {e.name}".strip()
            return ToolResult(_clip(f"[entry {e.index}, {label}]\n{e.text}", OUTPUT_LIMIT))
        query = args.get("query")
        if not isinstance(query, str) or not query.strip():
            return ToolResult("give a query to search for, or an index to read", is_error=True)
        limit = args.get("max_results")
        limit = limit if isinstance(limit, int) and 0 < limit <= 50 else 8
        try:
            pattern = re.compile(query, re.IGNORECASE)
        except re.error:
            pattern = re.compile(re.escape(query), re.IGNORECASE)
        hits: list[str] = []
        total = 0
        for e in reversed(entries):  # newest first: what you lost most recently
            m = pattern.search(e.text)
            if m is None:
                continue
            total += 1
            if len(hits) < limit:
                start = max(m.start() - SNIPPET // 2, 0)
                snippet = e.text[start : m.end() + SNIPPET].replace("\n", " ")
                label = f"{e.role} {e.name}".strip()
                hits.append(f"#{e.index} [{label}, {len(e.text)} chars] ...{snippet}...")
        if not hits:
            return ToolResult(f"no entry matches {query!r}")
        more = f"\n({total - limit} more; narrow the query)" if total > limit else ""
        return ToolResult("\n".join(hits) + more, metadata={"matches": total})
