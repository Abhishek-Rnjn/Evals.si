"""Readers for the structured output of coding-agent CLIs (H3).

Each reader turns one CLI's event stream into a ``ParsedRun``: the answer, one
``llm`` step per model message with the tool calls it made, one ``tool`` step
per call with its result, and token counts.

* ``claude-stream-json``: ``claude -p --output-format stream-json --verbose``.
  Sub-agent messages carry ``parent_tool_use_id``; their steps are parented to
  the tool step of the call that started them.
* ``codex-json``: ``codex exec --json``. One event per *item* (a message, a
  command, a file change, an MCP call); usage arrives once per turn.
* ``gemini-json``: ``gemini --output-format stream-json`` (a single
  ``--output-format json`` object is read as an answer with totals).
* ``pi-json``: ``pi --mode json``. Whole messages arrive as ``message_end``.

A reader never raises on an event it does not know; it skips it. A stream with
no recognizable event at all is an error on the run, so a wrong ``output_format``
is not mistaken for an agent that did nothing.

Each reader was checked against the published type definitions of the package
named in ``VERSIONS`` (Claude's agent SDK, Codex's SDK, Gemini CLI's core, pi's
docs and types). The fixtures are written from those types, not captured from a
CLI run; see docs/LEFTOVERS.md.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

from evalsi.types import Content, Message, Step, ToolCall, Usage
from evalsi_harness.events import span_id

# The CLI release each reader's fixture was written against.
# Each names the published type definitions the reader was checked against.
VERSIONS = {
    "claude-stream-json": "@anthropic-ai/claude-agent-sdk 0.3.300",
    "codex-json": "@openai/codex-sdk 0.162.1",
    "gemini-json": "@google/gemini-cli-core 0.64.0",
    "pi-json": "@mariozechner/pi-coding-agent 0.73.1",
}

# Bounds on what a hostile or runaway agent can make a reader hold.
MAX_EVENTS = 200_000
MAX_TEXT = 200_000


@dataclass
class Parsed:
    """The shared result; ``formats.ParsedRun`` is this type."""

    answer: str
    steps: list[Step] = field(default_factory=list)
    usage: Usage = field(default_factory=Usage)
    error: str = ""
    session_id: str = ""
    model: str = ""
    # Tool names the agent used, in order, for "did the mechanism run" checks.
    tools: list[str] = field(default_factory=list)
    # The output held none of the format's events: the wrong format, not a quiet agent.
    wrong_format: bool = False


def events(stdout: str) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for raw in stdout.splitlines():
        line = raw.strip()
        if not line.startswith("{"):
            continue  # a banner or warning beside the events
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(event, dict):
            out.append(event)
            if len(out) >= MAX_EVENTS:
                break
    return out


def _clip(text: str) -> str:
    if len(text) <= MAX_TEXT:
        return text
    return text[:MAX_TEXT] + f"\n[{len(text) - MAX_TEXT} more characters not kept]"


def _text(value: Any) -> str:
    """A tool result or message body as text, whatever shape the CLI used."""
    if value is None:
        return ""
    if isinstance(value, str):
        return _clip(value)
    if isinstance(value, list):
        parts = []
        for item in value:
            if isinstance(item, dict) and isinstance(item.get("text"), str):
                parts.append(item["text"])
            elif isinstance(item, str):
                parts.append(item)
            else:
                parts.append(json.dumps(item, ensure_ascii=False))
        return _clip("\n".join(parts))
    return _clip(json.dumps(value, ensure_ascii=False))


def _int(value: Any) -> int | None:
    return value if isinstance(value, int) and not isinstance(value, bool) else None


def _llm(  # noqa: PLR0917
    name: str,
    text: str,
    calls: list[ToolCall],
    usage: Usage | None,
    parent: str = "",
    attributes: dict[str, Any] | None = None,
) -> Step:
    return Step(
        type="llm",
        name=name,
        output=Content(messages=[Message(role="assistant", content=text, tool_calls=calls)]),
        usage=usage,
        span_id=span_id(),
        parent_span_id=parent,
        attributes=attributes or {},
    )


def _tool(  # noqa: PLR0917
    name: str,
    call_id: str,
    args: str,
    output: str,
    error: str,
    parent: str,
    attributes: dict[str, Any] | None = None,
) -> Step:
    return Step(
        type="tool",
        name=name,
        input=Content(text=args),
        output=Content(text=output),
        error=error,
        span_id=span_id(),
        parent_span_id=parent,
        attributes={"call_id": call_id, **(attributes or {})},
    )


def _args(value: Any) -> str:
    return value if isinstance(value, str) else json.dumps(value or {}, ensure_ascii=False)


def _finish(run: Parsed, seen: bool, tin: int, tout: int, cost: float | None = None) -> Parsed:
    if seen or cost is not None:
        run.usage = Usage(
            input_tokens=tin if seen else None,
            output_tokens=tout if seen else None,
            cost_usd=cost,
        )
    run.tools = [s.name for s in run.steps if s.type == "tool"]
    return run


def _unrecognized(run: Parsed, fmt: str, evs: list[dict[str, Any]], known: set[str]) -> None:
    if not any(e.get("type") in known for e in evs) and not run.error:
        run.error = f"no {fmt} events in the output; is output_format right for this agent?"
        run.wrong_format = True


# --- Claude Code -----------------------------------------------------------------


def _claude_usage(u: Any) -> Usage | None:
    if not isinstance(u, dict):
        return None
    tokens_in, tokens_out = _int(u.get("input_tokens")), _int(u.get("output_tokens"))
    if tokens_in is None and tokens_out is None:
        return None
    # Cache reads and writes are input the model was sent.
    extra = sum(
        _int(u.get(k)) or 0 for k in ("cache_read_input_tokens", "cache_creation_input_tokens")
    )
    return Usage(
        input_tokens=None if tokens_in is None else tokens_in + extra, output_tokens=tokens_out
    )


def _claude_model_usage(table: Any) -> tuple[int, int] | None:
    """The result's per-model totals, which include sub-agents and compaction."""
    if not isinstance(table, dict) or not table:
        return None
    tin = tout = 0
    for entry in table.values():
        if not isinstance(entry, dict):
            continue
        tin += sum(
            _int(entry.get(k)) or 0
            for k in ("inputTokens", "cacheReadInputTokens", "cacheCreationInputTokens")
        )
        tout += _int(entry.get("outputTokens")) or 0
    return tin, tout


def parse_claude(stdout: str) -> Parsed:  # noqa: PLR0915
    run = Parsed(answer="")
    evs = events(stdout)
    pending: dict[str, Step] = {}  # tool_use id -> its tool step
    # One model response arrives as several ``assistant`` events, one per
    # content block, all with the same message id and the same usage.
    by_message: dict[str, Step] = {}
    result_text: str | None = None
    last_text = ""
    cost: float | None = None
    totals: tuple[int, int] | None = None

    for e in evs:
        kind = e.get("type")
        if e.get("session_id") and not run.session_id:
            run.session_id = str(e["session_id"])
        if kind == "system" and e.get("subtype") == "init":
            run.model = str(e.get("model", ""))
        elif kind == "assistant":
            msg = e.get("message") or {}
            blocks = msg.get("content") or []
            if not isinstance(blocks, list):
                continue
            body = "\n".join(
                str(b.get("text", ""))
                for b in blocks
                if isinstance(b, dict) and b.get("type") == "text"
            )
            calls = [
                ToolCall(
                    id=str(b.get("id", "")),
                    name=str(b.get("name", "")),
                    arguments=_args(b.get("input")),
                )
                for b in blocks
                if isinstance(b, dict) and b.get("type") == "tool_use"
            ]
            parent_call = str(e.get("parent_tool_use_id") or "")
            ident = str(msg.get("id") or "")
            llm = by_message.get(f"{parent_call}/{ident}") if ident else None
            usage = _claude_usage(msg.get("usage"))
            if llm is None:
                parent = pending[parent_call].span_id if parent_call in pending else ""
                attrs = {"claude.parent_tool_use_id": parent_call} if parent_call else {}
                llm = _llm("claude", body, calls, usage, parent, attrs)
                run.steps.append(llm)
                if ident:
                    by_message[f"{parent_call}/{ident}"] = llm
            else:
                assert llm.output is not None
                assert llm.output.messages is not None
                first = llm.output.messages[0]
                first.content = "\n".join(t for t in (str(first.content), body) if t)
                first.tool_calls.extend(calls)
                llm.usage = usage or llm.usage
            if body and not parent_call:
                last_text = body
            for call in calls:
                tool = _tool(call.name, call.id, call.arguments, "", "", llm.span_id)
                run.steps.append(tool)
                pending[call.id] = tool
        elif kind == "user":
            blocks = (e.get("message") or {}).get("content")
            if not isinstance(blocks, list):
                continue
            for b in blocks:
                if not isinstance(b, dict) or b.get("type") != "tool_result":
                    continue
                hit = pending.get(str(b.get("tool_use_id", "")))
                if hit is None:
                    continue
                text = _text(b.get("content"))
                hit.output = Content(text=text)
                if b.get("is_error"):
                    hit.error = text or "the tool reported an error"
        elif kind == "result":
            if isinstance(e.get("result"), str):
                result_text = e["result"]
            if isinstance(e.get("total_cost_usd"), int | float):
                cost = float(e["total_cost_usd"])
            totals = _claude_model_usage(e.get("modelUsage")) or totals
            if e.get("is_error") or str(e.get("subtype", "success")) != "success":
                sub = str(e.get("subtype", "error"))
                detail = result_text or ""
                run.error = f"{sub}: {detail}" if detail else sub

    run.answer = result_text if result_text is not None else last_text
    _unrecognized(run, "claude-stream-json", evs, {"system", "assistant", "user", "result"})
    if totals is None:
        # No result totals (the run was cut off): add up each response once.
        usages = [s.usage for s in run.steps if s.type == "llm" and s.usage is not None]
        if usages:
            totals = (
                sum(u.input_tokens or 0 for u in usages),
                sum(u.output_tokens or 0 for u in usages),
            )
    return _finish(run, totals is not None, *(totals or (0, 0)), cost)


# --- Codex -----------------------------------------------------------------------


def _codex_item(item: dict[str, Any], parent: str) -> list[Step]:
    kind = item.get("type")
    ident = str(item.get("id", ""))
    failed = item.get("status") == "failed"
    if kind == "command_execution":
        code = item.get("exit_code")
        bad = failed or (isinstance(code, int) and code != 0)
        out = _text(item.get("aggregated_output"))
        return [
            _tool(
                "command",
                ident,
                _args({"command": item.get("command", "")}),
                out,
                (out or f"exit {code}") if bad else "",
                parent,
                {"codex.exit_code": code},
            )
        ]
    if kind == "file_change":
        changes = item.get("changes") or []
        return [
            _tool(
                "file_change",
                ident,
                _args({"changes": changes}),
                "\n".join(
                    f"{c.get('kind', '')} {c.get('path', '')}"
                    for c in changes
                    if isinstance(c, dict)
                ),
                "the patch was not applied" if failed else "",
                parent,
            )
        ]
    if kind == "mcp_tool_call":
        result = item.get("result")
        if isinstance(result, dict) and "content" in result:
            result = result["content"]
        err = item.get("error")
        message = err.get("message") if isinstance(err, dict) else err
        return [
            _tool(
                f"{item.get('server', '')}.{item.get('tool', '')}".strip("."),
                ident,
                _args(item.get("arguments")),
                _text(result),
                str(message or "the call failed") if failed or message else "",
                parent,
            )
        ]
    if kind == "web_search":
        return [_tool("web_search", ident, _args({"query": item.get("query", "")}), "", "", parent)]
    return []


def parse_codex(stdout: str) -> Parsed:
    run = Parsed(answer="")
    evs = events(stdout)
    message_texts: list[str] = []
    # A command or file change is something the model did after the message
    # before it, so it hangs under that message's step.
    current: Step | None = None
    tin = tout = 0
    seen = False
    for e in evs:
        kind = e.get("type")
        if kind == "thread.started":
            run.session_id = str(e.get("thread_id", ""))
        elif kind == "item.completed":
            item = e.get("item") or {}
            if not isinstance(item, dict):
                continue
            if item.get("type") == "agent_message":
                text = _text(item.get("text"))
                message_texts.append(text)
                current = _llm("codex", text, [], None)
                run.steps.append(current)
            elif item.get("type") == "error":
                run.error = run.error or str(item.get("message", "an item failed"))
            else:
                for tool in _codex_item(item, ""):
                    if current is None:
                        current = _llm("codex", "", [], None)
                        run.steps.append(current)
                    assert current.output is not None
                    assert current.output.messages is not None
                    tool.parent_span_id = current.span_id
                    current.output.messages[0].tool_calls.append(
                        ToolCall(
                            id=str(tool.attributes.get("call_id", "")),
                            name=tool.name,
                            arguments=tool.input.as_text() if tool.input else "",
                        )
                    )
                    run.steps.append(tool)
        elif kind == "turn.completed":
            u = e.get("usage") or {}
            a, b = _int(u.get("input_tokens")), _int(u.get("output_tokens"))
            if a is not None or b is not None:
                seen = True
                tin += a or 0
                tout += b or 0
        elif kind == "turn.failed":
            err = e.get("error") or {}
            detail = err.get("message") if isinstance(err, dict) else err
            run.error = str(detail or "the turn failed")
        elif kind == "error":
            run.error = run.error or str(e.get("message", "error"))
    run.answer = message_texts[-1] if message_texts else ""
    _unrecognized(
        run,
        "codex-json",
        evs,
        {"thread.started", "turn.started", "item.completed", "turn.completed"},
    )
    return _finish(run, seen, tin, tout)


# --- Gemini CLI ------------------------------------------------------------------


def parse_gemini(stdout: str) -> Parsed:  # noqa: PLR0915
    run = Parsed(answer="")
    # ``--output-format json`` prints one object, ``{"response", "stats"}``,
    # usually indented over many lines, so the whole output is tried first.
    whole: Any = None
    if len(stdout) <= MAX_TEXT * 4:
        try:
            whole = json.loads(stdout)
        except json.JSONDecodeError:
            whole = None
    evs = [whole] if isinstance(whole, dict) else events(stdout)
    if len(evs) == 1 and "response" in evs[0] and "type" not in evs[0]:
        only = evs[0]
        run.answer = _text(only.get("response"))
        stats = only.get("stats") or {}
        total = _stats(stats)
        run.usage = Usage(input_tokens=total[0], output_tokens=total[1])
        if isinstance(only.get("error"), dict):
            run.error = str(only["error"].get("message", "error"))
        return run

    chunks: list[str] = []
    calls: list[ToolCall] = []
    tool_steps: dict[str, Step] = {}
    order: list[Step] = []
    tin = tout = 0
    seen = False

    def flush() -> None:
        nonlocal chunks, calls, order
        if not chunks and not calls:
            return
        llm = _llm("gemini", "".join(chunks), calls, None)
        for s in order:
            s.parent_span_id = llm.span_id
        run.steps.append(llm)
        run.steps.extend(order)
        chunks, calls, order = [], [], []

    for e in evs:
        kind = e.get("type")
        if kind == "init":
            run.session_id = str(e.get("session_id", ""))
            run.model = str(e.get("model", ""))
        elif kind == "message":
            if e.get("role") == "assistant":
                if order:  # text after tool results starts a new model call
                    flush()
                chunks.append(str(e.get("content", "")))
        elif kind == "tool_use":
            ident = str(e.get("tool_id", ""))
            name = str(e.get("tool_name", ""))
            args = _args(e.get("parameters"))
            calls.append(ToolCall(id=ident, name=name, arguments=args))
            step = _tool(name, ident, args, "", "", "")
            tool_steps[ident] = step
            order.append(step)
        elif kind == "tool_result":
            done = tool_steps.get(str(e.get("tool_id", "")))
            if done is None:
                continue
            done.output = Content(text=_text(e.get("output")))
            if e.get("status") not in (None, "success"):
                err = e.get("error")
                done.error = str(
                    err.get("message") if isinstance(err, dict) else err or "the tool failed"
                )
        elif kind == "error":
            if e.get("severity") != "warning":
                run.error = run.error or str(e.get("message", "error"))
        elif kind == "result":
            stats = e.get("stats") or {}
            a, b = _stats(stats)
            if a is not None or b is not None:
                seen = True
                tin, tout = a or 0, b or 0
            if e.get("status") not in (None, "success"):
                err = e.get("error")
                run.error = run.error or str(
                    err.get("message") if isinstance(err, dict) else err or e.get("status")
                )
    answer = "".join(chunks)
    flush()
    run.answer = answer
    _unrecognized(run, "gemini-json", evs, {"init", "message", "tool_use", "tool_result", "result"})
    return _finish(run, seen, tin, tout)


def _stats(stats: Any) -> tuple[int | None, int | None]:
    """Totals from a stream's ``result`` stats or a json object's session metrics."""
    if not isinstance(stats, dict):
        return None, None
    if isinstance(stats.get("models"), dict) and "input_tokens" not in stats:
        tin = tout = 0
        seen = False
        for entry in stats["models"].values():
            tokens = entry.get("tokens") if isinstance(entry, dict) else None
            if isinstance(tokens, dict):
                seen = True
                tin += _int(tokens.get("prompt")) or 0
                tout += _int(tokens.get("candidates")) or 0
        return (tin, tout) if seen else (None, None)
    return _int(stats.get("input_tokens")), _int(stats.get("output_tokens"))


# --- pi --------------------------------------------------------------------------


def parse_pi(stdout: str) -> Parsed:  # noqa: PLR0915
    run = Parsed(answer="")
    evs = events(stdout)
    by_call: dict[str, Step] = {}
    tin = tout = 0
    seen = False
    cost = 0.0
    saw_cost = False
    answer = ""
    for e in evs:
        kind = e.get("type")
        if kind == "session":
            run.session_id = str(e.get("id", ""))
        elif kind == "message_end":
            msg = e.get("message") or {}
            if msg.get("role") != "assistant":
                continue
            blocks = msg.get("content") or []
            if isinstance(blocks, str):
                blocks = [{"type": "text", "text": blocks}]
            body = "\n".join(
                str(b.get("text", ""))
                for b in blocks
                if isinstance(b, dict) and b.get("type") == "text"
            )
            calls = [
                ToolCall(
                    id=str(b.get("id", "")),
                    name=str(b.get("name", "")),
                    arguments=_args(b.get("arguments")),
                )
                for b in blocks
                if isinstance(b, dict) and b.get("type") == "toolCall"
            ]
            u = msg.get("usage") or {}
            a, b_ = _int(u.get("input")), _int(u.get("output"))
            extra = (_int(u.get("cacheRead")) or 0) + (_int(u.get("cacheWrite")) or 0)
            usage = None
            if a is not None or b_ is not None:
                seen = True
                tin += (a or 0) + extra
                tout += b_ or 0
                usage = Usage(input_tokens=(a or 0) + extra, output_tokens=b_)
            total = (u.get("cost") or {}).get("total") if isinstance(u.get("cost"), dict) else None
            if isinstance(total, int | float):
                saw_cost = True
                cost += float(total)
            llm = _llm("pi", body, calls, usage)
            run.model = run.model or str(msg.get("model", ""))
            run.steps.append(llm)
            if msg.get("stopReason") == "error":
                run.error = run.error or str(msg.get("errorMessage", "the model call failed"))
            for call in calls:
                step = _tool(call.name, call.id, call.arguments, "", "", llm.span_id)
                run.steps.append(step)
                by_call[call.id] = step
            if not calls:
                answer = body
        elif kind == "tool_execution_end":
            done = by_call.get(str(e.get("toolCallId", "")))
            if done is None:
                continue
            result = e.get("result")
            content = result.get("content") if isinstance(result, dict) else result
            done.output = Content(text=_text(content))
            if e.get("isError"):
                done.error = done.output.as_text() or "the tool reported an error"
    run.answer = answer
    _unrecognized(run, "pi-json", evs, {"session", "message_end", "agent_start", "turn_start"})
    return _finish(run, seen, tin, tout, cost if saw_cost else None)
