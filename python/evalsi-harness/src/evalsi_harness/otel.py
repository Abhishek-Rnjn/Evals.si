"""OTel by default: every step of a task becomes a span.

When ``OTEL_EXPORTER_OTLP_ENDPOINT`` (or ``..._TRACES_ENDPOINT``) is set, a
task's trajectory is exported over OTLP/HTTP (JSON) as one trace: an
``invoke_agent`` root span and a child per step, using the OTel GenAI
semantic conventions and tagged ``evalsi.run_id``, ``evalsi.trial`` and
``evalsi.task_id``, so offline runs appear in the same trace views as
production (and in Evals.si's own ingest, whose mappers read the same
attributes). Headers come from ``OTEL_EXPORTER_OTLP_HEADERS``.
"""

from __future__ import annotations

import json
import logging
import os
import secrets
import time
from typing import Any
from urllib.parse import unquote

import httpx

from evalsi.types import Step

logger = logging.getLogger(__name__)

_OPERATION = {"llm": "chat", "tool": "execute_tool", "agent": "invoke_agent"}


def endpoint() -> str:
    traces = os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
    if traces:
        return traces
    base = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT")
    return base.rstrip("/") + "/v1/traces" if base else ""


def _headers() -> dict[str, str]:
    out = {"Content-Type": "application/json"}
    for pair in os.environ.get("OTEL_EXPORTER_OTLP_HEADERS", "").split(","):
        key, sep, value = pair.partition("=")
        if sep and key.strip():
            out[key.strip()] = unquote(value.strip())
    return out


def _attr(key: str, value: Any) -> dict[str, Any]:
    if isinstance(value, bool):
        v: dict[str, Any] = {"boolValue": value}
    elif isinstance(value, int):
        v = {"intValue": str(value)}
    elif isinstance(value, float):
        v = {"doubleValue": value}
    elif isinstance(value, str):
        v = {"stringValue": value}
    else:
        v = {"stringValue": json.dumps(value)}
    return {"key": key, "value": v}


def spans(
    steps: list[Step],
    *,
    task_id: str,
    trial: int,
    run_id: str,
    agent: str,
    started_ns: int,
    ended_ns: int,
) -> tuple[str, list[dict[str, Any]]]:
    """The trace id and OTLP spans for a task's steps."""
    trace_id = secrets.token_hex(16)
    root = secrets.token_hex(8)
    tags = [_attr("evalsi.task_id", task_id), _attr("evalsi.trial", trial)]
    if run_id:
        tags.append(_attr("evalsi.run_id", run_id))
    out = [
        {
            "traceId": trace_id,
            "spanId": root,
            "name": f"invoke_agent {agent}",
            "kind": 1,
            "startTimeUnixNano": str(started_ns),
            "endTimeUnixNano": str(ended_ns),
            "attributes": [
                _attr("gen_ai.operation.name", "invoke_agent"),
                _attr("gen_ai.agent.name", agent),
                *tags,
            ],
        }
    ]
    cursor = started_ns
    for step in steps:
        duration = int((step.duration_ms or 0) * 1e6)
        attrs = [*tags]
        op = _OPERATION.get(step.type)
        if op:
            attrs.append(_attr("gen_ai.operation.name", op))
        if step.type == "tool":
            attrs.append(_attr("gen_ai.tool.name", step.name))
            if step.input is not None:
                attrs.append(_attr("gen_ai.tool.call.arguments", step.input.as_text()))
            if step.output is not None:
                attrs.append(_attr("gen_ai.tool.call.result", step.output.as_text()))
        elif step.type == "llm":
            attrs.append(_attr("gen_ai.request.model", step.name))
            if step.output is not None:
                attrs.append(_attr("gen_ai.output.messages", json.dumps(step.output.to_dict())))
        elif step.type == "guardrail":
            attrs.append(_attr("openinference.span.kind", "GUARDRAIL"))
        if step.usage is not None:
            if step.usage.input_tokens is not None:
                attrs.append(_attr("gen_ai.usage.input_tokens", step.usage.input_tokens))
            if step.usage.output_tokens is not None:
                attrs.append(_attr("gen_ai.usage.output_tokens", step.usage.output_tokens))
        for key, value in step.attributes.items():
            if key.startswith(("evalsi.", "gen_ai.")) and value is not None:
                attrs.append(_attr(key, value))
        span: dict[str, Any] = {
            "traceId": trace_id,
            "spanId": step.span_id if len(step.span_id) == 16 else secrets.token_hex(8),
            "parentSpanId": root,
            "name": f"{op or step.type} {step.name}".strip(),
            "kind": 1,
            "startTimeUnixNano": str(cursor),
            "endTimeUnixNano": str(cursor + duration),
            "attributes": attrs,
        }
        if step.error:
            span["status"] = {"code": 2, "message": step.error[:200]}
        out.append(span)
        cursor += duration
    return trace_id, out


async def export(steps: list[Step], **kwargs: Any) -> str:
    """Exports a task's steps if an endpoint is configured; returns the trace id or ''."""
    url = endpoint()
    if not url:
        return ""
    kwargs.setdefault("ended_ns", time.time_ns())
    trace_id, span_list = spans(steps, **kwargs)
    body = {
        "resourceSpans": [
            {
                "resource": {
                    "attributes": [
                        _attr("service.name", os.environ.get("OTEL_SERVICE_NAME", "evalsi-harness"))
                    ]
                },
                "scopeSpans": [{"scope": {"name": "evalsi-harness"}, "spans": span_list}],
            }
        ]
    }
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            response = await client.post(url, json=body, headers=_headers())
        if response.status_code >= 400:
            logger.warning("OTLP export to %s failed: HTTP %s", url, response.status_code)
            return ""
    except httpx.HTTPError as exc:
        logger.warning("OTLP export to %s failed: %s", url, exc)
        return ""
    return trace_id
