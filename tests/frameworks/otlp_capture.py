"""Captures the OTLP traces an agent framework exports, for internal/ingest's
fixture tests (internal/ingest/testdata/frameworks).

Each recorder (record_<framework>.py) starts a capture, points its framework's
exporter at it, runs the shared research question against the mock model, and
writes what arrived as one OTLP/JSON request: every export merged, trace and
span ids in hex as OTLP/JSON has them, and the resource attributes that
describe the recording machine (host, process, OS) dropped.
"""

from __future__ import annotations

import base64
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

from google.protobuf.json_format import MessageToDict
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest

# The question every recorder asks. The mock model's solutions.json answers it
# with one search_docs call and then a fixed report.
QUESTION = "Research how retrieval-augmented generation is evaluated and write a short report."
TOOL_RESULT = (
    "[rag-evaluation] RAG is evaluated in two parts: context quality (are the retrieved "
    "documents relevant) and answer quality (is the answer faithful to them)."
)

# A model name the frameworks know, so they use native tool calling; the mock
# answers the same whatever the name.
MODEL = "gpt-4o-mini"
MOCK_URL = os.environ.get("MOCK_MODEL_URL", "http://127.0.0.1:8124/v1")

FIXTURES = Path(__file__).resolve().parents[2] / "internal" / "ingest" / "testdata" / "frameworks"

_MACHINE = ("host.", "process.", "os.", "container.", "device.")


class Capture:
    """An OTLP/HTTP receiver on a free local port."""

    def __init__(self) -> None:
        self.exports: list[bytes] = []
        exports = self.exports

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self) -> None:
                exports.append(self.rfile.read(int(self.headers.get("Content-Length") or 0)))
                self.send_response(200)
                self.send_header("Content-Type", "application/x-protobuf")
                self.send_header("Content-Length", "0")
                self.end_headers()

            def log_message(self, *args: object) -> None:
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    @property
    def endpoint(self) -> str:
        return f"http://127.0.0.1:{self.server.server_port}/v1/traces"

    def wait(self, spans: int = 1, timeout: float = 15) -> None:
        """Waits until at least spans spans arrived, then a moment more for stragglers."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.count() >= spans:
                time.sleep(1)
                return
            time.sleep(0.1)
        raise SystemExit(f"only {self.count()} spans arrived")

    def count(self) -> int:
        n = 0
        for raw in self.exports:
            req = ExportTraceServiceRequest.FromString(raw)
            n += sum(len(ss.spans) for rs in req.resource_spans for ss in rs.scope_spans)
        return n

    def write(self, name: str) -> Path:
        merged = ExportTraceServiceRequest()
        for raw in self.exports:
            merged.resource_spans.extend(ExportTraceServiceRequest.FromString(raw).resource_spans)
        doc = MessageToDict(merged)
        for rs in doc.get("resourceSpans", []):
            attrs = rs.get("resource", {}).get("attributes", [])
            rs.setdefault("resource", {})["attributes"] = [
                a for a in attrs if not a["key"].startswith(_MACHINE)
            ]
        _hex_ids(doc)
        text = json.dumps(doc, indent=1, sort_keys=True) + "\n"
        for leak in {str(Path.home()), os.getcwd(), str(Path(__file__).resolve().parent)}:
            text = text.replace(leak, "/work")
        FIXTURES.mkdir(parents=True, exist_ok=True)
        out = FIXTURES / f"{name}.otlp.json"
        out.write_text(text)
        print(f"wrote {out} ({self.count()} spans)")
        return out


def _hex_ids(v: Any) -> None:
    if isinstance(v, dict):
        for k, val in v.items():
            if k in ("traceId", "spanId", "parentSpanId") and isinstance(val, str):
                v[k] = base64.b64decode(val).hex()
            else:
                _hex_ids(val)
    elif isinstance(v, list):
        for e in v:
            _hex_ids(e)


def search_docs(query: str) -> str:
    """Searches the documentation corpus and returns the matching passages."""
    return TOOL_RESULT


def tracer_provider(capture: Capture, service: str) -> Any:
    """An OTel SDK tracer provider that exports to capture (for instrumentors)."""
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor

    provider = TracerProvider(resource=Resource.create({"service.name": service}))
    provider.add_span_processor(SimpleSpanProcessor(OTLPSpanExporter(endpoint=capture.endpoint)))
    return provider
