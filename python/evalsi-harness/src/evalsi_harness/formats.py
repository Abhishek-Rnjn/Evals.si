"""Readers for the output of command-line agents that report what they did.

A CLI agent's standard output is normally just its answer. Some agents can also
print a structured account of the run; ``output_format`` on the spec says which,
and the matching reader turns it into the answer and the trajectory.

``dsh-json``: newline-delimited events from ``dsh --profile headless --json``
(DeepSeek Harness, 0.2.x)::

    {"type":"session","sessionId":"...","cwd":"..."}
    {"type":"status","phase":"turn_start","turn":1}
    {"type":"status","phase":"step_start","turn":1,"step":1}
    {"type":"tool_call","callId":"...","tool":"bash","input":{...}}
    {"type":"tool_result","callId":"...","status":"completed","result":"..."}
    {"type":"text","text":"..."}
    {"type":"status","phase":"step_end","turn":1,"step":1,"usage":{"inputTokens":..,"outputTokens":..}}
    {"type":"status","phase":"turn_end","turn":1,"reason":{"kind":"completed"}}
    {"type":"final","text":"..."}

A step is one model call: it becomes an ``llm`` step carrying the text and the
tool calls the model made, followed by one ``tool`` step per call with its
result. A tool result whose status is not ``completed``, or a turn that ends
in an error, is recorded as that step's error.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any

from evalsi.types import Content, Message, Step, ToolCall, Usage
from evalsi_harness.events import span_id

FORMATS = ("", "dsh-json")


@dataclass
class ParsedRun:
    """What a structured output format says about a run."""

    answer: str
    steps: list[Step] = field(default_factory=list)
    usage: Usage = field(default_factory=Usage)
    # Why the run failed, when the agent said so; empty otherwise.
    error: str = ""
    session_id: str = ""


def parse_output(fmt: str, stdout: str) -> ParsedRun | None:
    """Read ``stdout`` as ``fmt``; None for the empty format (plain output)."""
    if fmt == "":
        return None
    if fmt == "dsh-json":
        return parse_dsh_events(stdout)
    raise ValueError(
        f"unknown output_format {fmt!r}; use one of {', '.join(repr(f) for f in FORMATS)}"
    )


def _events(stdout: str) -> list[dict[str, Any]]:
    out = []
    for raw in stdout.splitlines():
        line = raw.strip()
        if not line.startswith("{"):
            continue  # a warning or banner the agent printed beside the events
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(event, dict):
            out.append(event)
    return out


def _tokens(usage: Any) -> tuple[int | None, int | None]:
    if not isinstance(usage, dict):
        return None, None
    return usage.get("inputTokens"), usage.get("outputTokens")


def _result_text(result: Any) -> str:
    if isinstance(result, str):
        return result
    return json.dumps(result, ensure_ascii=False)


def parse_dsh_events(stdout: str) -> ParsedRun:
    run = ParsedRun(answer="")
    texts: list[str] = []
    calls: list[dict[str, Any]] = []
    results: dict[str, dict[str, Any]] = {}
    total_in = total_out = 0
    seen_usage = False
    final: str | None = None

    for event in _events(stdout):
        kind = event.get("type")
        if kind == "session":
            run.session_id = str(event.get("sessionId", ""))
        elif kind == "text":
            texts.append(str(event.get("text", "")))
        elif kind == "tool_call":
            calls.append(event)
        elif kind == "tool_result":
            results[str(event.get("callId", ""))] = event
        elif kind == "final":
            final = str(event.get("text", ""))
        elif kind == "status":
            phase = event.get("phase")
            if phase == "step_start":
                texts, calls = [], []
            elif phase == "step_end":
                tokens_in, tokens_out = _tokens(event.get("usage"))
                if tokens_in is not None or tokens_out is not None:
                    seen_usage = True
                    total_in += tokens_in or 0
                    total_out += tokens_out or 0
                run.steps.extend(_model_step(texts, calls, results, tokens_in, tokens_out))
                texts, calls = [], []
            elif phase == "turn_end":
                reason = event.get("reason") or {}
                if reason.get("kind") == "error":
                    err = reason.get("error") or {}
                    code = err.get("code")
                    message = str(err.get("message", "the turn ended in an error"))
                    run.error = f"{code}: {message}" if code else message
                elif reason.get("kind") not in (None, "completed"):
                    run.error = f"the turn ended: {reason.get('kind')}"

    run.answer = final if final is not None else "\n".join(t for t in texts if t)
    if seen_usage:
        run.usage = Usage(input_tokens=total_in, output_tokens=total_out)
    return run


def _model_step(
    texts: list[str],
    calls: list[dict[str, Any]],
    results: dict[str, dict[str, Any]],
    tokens_in: int | None,
    tokens_out: int | None,
) -> list[Step]:
    tool_calls = [
        ToolCall(
            id=str(c.get("callId", "")),
            name=str(c.get("tool", "")),
            arguments=json.dumps(c.get("input", {}), ensure_ascii=False),
        )
        for c in calls
    ]
    steps = [
        Step(
            type="llm",
            name="dsh",
            output=Content(
                messages=[
                    Message(
                        role="assistant",
                        content="\n".join(t for t in texts if t),
                        tool_calls=tool_calls,
                    )
                ]
            ),
            usage=Usage(input_tokens=tokens_in, output_tokens=tokens_out)
            if tokens_in is not None or tokens_out is not None
            else None,
            span_id=span_id(),
        )
    ]
    for call, tc in zip(calls, tool_calls, strict=True):
        result = results.get(tc.id)
        failed = result is not None and result.get("status") != "completed"
        steps.append(
            Step(
                type="tool",
                name=tc.name,
                input=Content(text=tc.arguments),
                output=Content(text=_result_text(result.get("result", "")) if result else ""),
                error=(_result_text(result.get("result", "")) or str(result.get("status")))
                if failed and result
                else "",
                span_id=span_id(),
                parent_span_id=steps[0].span_id,
                attributes={"dsh.call_id": call.get("callId", "")},
            )
        )
    return steps
