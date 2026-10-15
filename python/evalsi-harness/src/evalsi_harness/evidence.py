"""The ``evidence-state`` context strategy, after LongHarness (arXiv 2610.13075).

For long-document and corpus questions, the agent keeps two structured states
outside the chat history instead of free-text notes:

- an **evidence list**: source-linked statements, each citing a passage the
  agent has actually read. A correction supersedes or retracts the old entry
  instead of appending next to it, so the list never holds two answers to one
  question;
- a **conclusion list**: claims with the evidence that supports them and the
  open gaps that steer the next search.

The state is shown to the model on every call (not stored in the
conversation), and when the context fills up the conversation is cut down to
the task and the latest turns: the state is what carries over, and it costs a
few lines, not a summary the model has to trust. The paper's ablations found
structured state beat free-text notes by 6.8 to 7.8 points, and an
append-only evidence list cost up to 10.0, on one benchmark with one run per
setting, so this is a strategy to compare with ``compact``, not a new default.
"""

from __future__ import annotations

import re
import time
from dataclasses import dataclass, field
from typing import Any

from evalsi_harness.archive import Archive
from evalsi_harness.context import (
    ContextConfig,
    ContextResult,
    ContextStats,
    ContextView,
    Message,
    _context_step,
    clear_tool_results,
    split_for_compaction,
)
from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import ToolResult

MIN_QUOTE = 8
MAX_QUOTE = 600
SHOWN_QUOTE = 240
MAX_ENTRIES = 200
STATE_HEADER = "[Your evidence state, kept outside the conversation. It is current; trust it.]"


def _squash(text: str) -> str:
    return re.sub(r"\s+", " ", text).strip()


@dataclass
class Evidence:
    id: str
    statement: str
    source: str
    quote: str
    status: str = "active"  # active, superseded or retracted
    note: str = ""  # superseded by E5 / the reason it was retracted


@dataclass
class Claim:
    id: str
    claim: str
    status: str = "open"  # open or supported
    support: list[str] = field(default_factory=list)
    gaps: list[str] = field(default_factory=list)


class EvidenceState:
    """The evidence list and the conclusion list of one run."""

    def __init__(self, archive: Archive) -> None:
        self.archive = archive
        self.evidence: dict[str, Evidence] = {}
        self.claims: dict[str, Claim] = {}
        self._n = 0
        self._c = 0

    def __bool__(self) -> bool:
        return bool(self.evidence or self.claims)

    def counts(self) -> dict[str, int]:
        by = [e.status for e in self.evidence.values()]
        return {
            "evidence_active": by.count("active"),
            "evidence_superseded": by.count("superseded"),
            "evidence_retracted": by.count("retracted"),
            "claims": len(self.claims),
            "claims_open": sum(1 for c in self.claims.values() if c.status == "open"),
        }

    def was_read(self, quote: str) -> bool:
        """The quote is in something the agent was shown: a tool result, or the task."""
        needle = _squash(quote)
        return any(
            needle in _squash(e.text) for e in self.archive.entries if e.role in ("tool", "user")
        )

    def render(self) -> str:
        lines = [STATE_HEADER]
        active = [e for e in self.evidence.values() if e.status == "active"]
        lines.append("Evidence:" if active else "Evidence: none yet.")
        for e in active:
            quote = e.quote if len(e.quote) <= SHOWN_QUOTE else e.quote[:SHOWN_QUOTE] + "..."
            lines.append(f' {e.id} [{e.source}] {e.statement}\n    "{quote}"')
        gone = [e for e in self.evidence.values() if e.status != "active"]
        if gone:
            lines.append("No longer valid: " + "; ".join(f"{e.id} ({e.note})" for e in gone))
        if self.claims:
            lines.append("Conclusions:")
            for c in self.claims.values():
                tail = f" supported by {', '.join(c.support)}" if c.support else ""
                lines.append(f" {c.id} {c.status.upper()}: {c.claim}{tail}")
                for gap in c.gaps:
                    lines.append(f"    open gap: {gap}")
        open_gaps = [g for c in self.claims.values() for g in c.gaps]
        if open_gaps:
            lines.append("Next: search to close the open gaps before anything else.")
        return "\n".join(lines)

    # --- changes; each returns an error message, or None when it worked ---

    def _check(self, statement: str, source: str, quote: str) -> str | None:
        if not statement.strip() or not source.strip():
            return "an entry needs a statement and a source"
        q = _squash(quote)
        if not MIN_QUOTE <= len(q) <= MAX_QUOTE:
            return f"the quote must be {MIN_QUOTE} to {MAX_QUOTE} characters, copied verbatim"
        if not self.was_read(q):
            return (
                "that passage was not found in anything you have read: copy the quote exactly "
                "from a tool result (or the task), or read the source first"
            )
        return None

    def add(self, statement: str, source: str, quote: str) -> tuple[str | None, str | None]:
        if len(self.evidence) >= MAX_ENTRIES:
            return (
                None,
                f"the list holds {MAX_ENTRIES} entries; retract the ones you no longer need",
            )
        if (err := self._check(statement, source, quote)) is not None:
            return None, err
        self._n += 1
        eid = f"E{self._n}"
        self.evidence[eid] = Evidence(eid, statement.strip(), source.strip(), _squash(quote))
        return eid, None

    def supersede(
        self, old: str, statement: str, source: str, quote: str
    ) -> tuple[str | None, str | None]:
        entry = self.evidence.get(old)
        if entry is None or entry.status != "active":
            return None, f"{old} is not an active entry"
        new, err = self.add(statement, source, quote)
        if new is None:
            return None, err
        entry.status, entry.note = "superseded", f"superseded by {new}"
        self._repoint(old, new)
        return new, None

    def retract(self, eid: str, reason: str) -> str | None:
        entry = self.evidence.get(eid)
        if entry is None or entry.status != "active":
            return f"{eid} is not an active entry"
        if not reason.strip():
            return "say why it is retracted"
        entry.status, entry.note = "retracted", f"retracted: {reason.strip()[:120]}"
        for c in self.claims.values():
            if eid in c.support:
                c.support.remove(eid)
                if not c.support:
                    c.status = "open"
                    c.gaps.append(f"{eid} was retracted; the claim needs new support")
        return None

    def _repoint(self, old: str, new: str) -> None:
        for c in self.claims.values():
            c.support = [new if s == old else s for s in c.support]

    def claim(
        self, cid: str | None, claim: str, support: list[str], gaps: list[str]
    ) -> tuple[str | None, str | None]:
        bad = [s for s in support if s not in self.evidence or self.evidence[s].status != "active"]
        if bad:
            return None, f"{', '.join(bad)} are not active evidence entries"
        if cid is None:
            self._c += 1
            cid = f"C{self._c}"
            self.claims[cid] = Claim(cid, claim.strip())
        elif cid not in self.claims:
            return None, f"there is no conclusion {cid}"
        c = self.claims[cid]
        if claim.strip():
            c.claim = claim.strip()
        c.support = list(dict.fromkeys(support))
        c.gaps = [g.strip() for g in gaps if g.strip()]
        # Supported means supported by something and with nothing left open.
        c.status = "supported" if c.support and not c.gaps else "open"
        return cid, None


def _strings(value: Any) -> list[str]:
    return [v for v in value if isinstance(v, str)] if isinstance(value, list) else []


class EvidenceTool:
    def __init__(self, state: EvidenceState) -> None:
        self.state = state
        self.spec = ToolSpec(
            name="evidence",
            description=(
                "Keep your findings in two lists that are shown to you on every turn and survive "
                "your context being cut down. Evidence: a statement, its source and a quote "
                "copied verbatim from something you read (entries whose quote was not read are "
                "refused). Conclusions: a claim, the evidence ids that support it, and open gaps "
                "still to close. Actions: add (statement, source, quote), supersede (id, "
                "statement, source, quote: a corrected entry replaces the old one), retract (id, "
                "reason), claim (claim, support, gaps; give id to update one), view."
            ),
            input_schema={
                "type": "object",
                "properties": {
                    "action": {
                        "type": "string",
                        "enum": ["add", "supersede", "retract", "claim", "view"],
                    },
                    "id": {"type": "string"},
                    "statement": {"type": "string"},
                    "source": {"type": "string"},
                    "quote": {"type": "string"},
                    "reason": {"type": "string"},
                    "claim": {"type": "string"},
                    "support": {"type": "array", "items": {"type": "string"}},
                    "gaps": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["action"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        s = self.state

        def text(key: str) -> str:
            v = args.get(key)
            return v if isinstance(v, str) else ""

        action = text("action")
        if action == "view":
            return ToolResult(s.render())
        if action == "add":
            eid, err = s.add(text("statement"), text("source"), text("quote"))
            return ToolResult(f"added {eid}" if eid else err or "", is_error=eid is None)
        if action == "supersede":
            eid, err = s.supersede(text("id"), text("statement"), text("source"), text("quote"))
            msg = f"{text('id')} is superseded by {eid}" if eid else err or ""
            return ToolResult(msg, is_error=eid is None)
        if action == "retract":
            err = s.retract(text("id"), text("reason"))
            return ToolResult(err or f"retracted {text('id')}", is_error=err is not None)
        if action == "claim":
            if not text("claim") and not text("id"):
                return ToolResult("a conclusion needs a claim", is_error=True)
            cid, err = s.claim(
                text("id") or None,
                text("claim"),
                _strings(args.get("support")),
                _strings(args.get("gaps")),
            )
            if cid is None:
                return ToolResult(err or "", is_error=True)
            return ToolResult(f"{cid} is {s.claims[cid].status}")
        return ToolResult("action must be add, supersede, retract, claim or view", is_error=True)


class EvidenceStateStrategy:
    """Shows the state on every call, clears tool results when the window
    fills, and then cuts the conversation to the task and the latest turns: the
    state, not a summary, carries over."""

    name = "evidence-state"

    def __init__(self, config: ContextConfig, state: EvidenceState) -> None:
        self.config = config
        self.state = state
        self.stats = ContextStats()

    async def prepare(
        self, messages: list[Message], view: ContextView, *, parent: str = ""
    ) -> ContextResult:
        c = self.config
        window = c.window_tokens
        transient: list[Message] = (
            [{"role": "user", "content": self.state.render()}] if self.state else []
        )

        def size(m: list[Message]) -> int:
            return view.tokens([*m, *transient])

        before = size(messages)
        if before < c.trigger * window:
            return ContextResult(messages, [], transient)
        started = time.perf_counter()
        out, cleared, removed = clear_tool_results(
            messages, lambda m: size(m) <= c.target * window, c.keep_recent
        )
        steps = []
        if cleared:
            self.stats.clears += 1
            self.stats.results_cleared += cleared
            self.stats.chars_removed += removed
            steps.append(
                _context_step(
                    "clear-tool-results",
                    parent,
                    started,
                    {
                        "tokens_before": before,
                        "tokens_after": size(out),
                        "results_cleared": cleared,
                        "chars_removed": removed,
                    },
                )
            )
        if size(out) > c.trigger * window:
            tail = split_for_compaction(out, c.keep_turns)
            if tail is not None:
                dropped = out[1:tail]
                turns = sum(1 for m in dropped if m.get("role") == "assistant")
                kept = [out[0], *out[tail:]]
                self.stats.compactions += 1
                self.stats.turns_summarized += turns
                steps.append(
                    _context_step(
                        "evidence-state",
                        parent,
                        started,
                        {
                            "tokens_before": size(out),
                            "tokens_after": size(kept),
                            "messages_dropped": len(dropped),
                            "turns_dropped": turns,
                            **self.state.counts(),
                        },
                    )
                )
                out = kept
        self.stats.tokens_removed += max(before - size(out), 0)
        return ContextResult(out, steps, transient)
