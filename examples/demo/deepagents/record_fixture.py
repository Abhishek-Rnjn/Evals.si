"""Records the OTLP export of one demo run, for internal/ingest's fixture test.

Needs the packages in requirements.txt (pandas included, which MLflow's LangChain autolog wants)
and the mock model running:

    python3 ../mock-model/server.py --port 8124 &
    python3 record_fixture.py ../../../internal/ingest/testdata/deepagents-research.otlp.pb

It runs the research question once with MLflow's LangChain autolog and its OTLP
exporter pointed at a local receiver, and writes the first export request as is.
"""

from __future__ import annotations

import os
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

QUESTION = "Research how retrieval-augmented generation is evaluated and write a short report."


def main() -> None:
    out = sys.argv[1]
    exports: list[bytes] = []

    class Receiver(BaseHTTPRequestHandler):
        def do_POST(self) -> None:  # noqa: N802
            exports.append(self.rfile.read(int(self.headers.get("Content-Length") or 0)))
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def log_message(self, *args: object) -> None:
            pass

    receiver = HTTPServer(("127.0.0.1", 0), Receiver)
    threading.Thread(target=receiver.serve_forever, daemon=True).start()
    work = tempfile.mkdtemp()
    os.environ.update(
        OPENAI_API_KEY="unused",
        OPENAI_BASE_URL=os.environ.get("OPENAI_BASE_URL", "http://127.0.0.1:8124/v1"),
        DEMO_MODEL="openai:mock",
        MLFLOW_TRACKING_URI=f"file://{work}/mlruns",
        MLFLOW_ALLOW_FILE_STORE="true",  # only to have somewhere to log; the OTLP export is what is kept
        MLFLOW_ENABLE_OTLP_EXPORTER="true",
        OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=f"http://127.0.0.1:{receiver.server_port}/v1/traces",
        OTEL_EXPORTER_OTLP_TRACES_PROTOCOL="http/protobuf",
        OTEL_SERVICE_NAME="evalsi-demo-deepagents",
        OTEL_BSP_SCHEDULE_DELAY="200",  # export soon after the run, not after 5 s
    )
    import agent
    import mlflow

    agent.enable_tracing()
    agent.build_agent().invoke({"messages": [{"role": "user", "content": QUESTION}]})
    mlflow.flush_trace_async_logging()
    for _ in range(100):
        if exports:
            break
        time.sleep(0.1)
    if not exports:
        sys.exit("no OTLP export arrived")
    with open(out, "wb") as f:
        f.write(exports[0])
    print(f"wrote {out} ({len(exports[0])} bytes)")


if __name__ == "__main__":
    main()
