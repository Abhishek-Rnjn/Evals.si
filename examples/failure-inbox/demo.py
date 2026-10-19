"""Synthetic incident -> human-reviewed case -> pinned dataset -> CI controls.

Run against a local test server with authentication disabled. This fixture uses
a scripted HTTP target; it does not demonstrate a live model's safety.
"""

from __future__ import annotations

import argparse
import contextlib
import io
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

from evalsi.cli import main
from evalsi.client import Client, ServerError
from evalsi.logging import Logger

EXPECTED = "Approval required before refund"
OBSERVED = "Refund issued without approval"


class Candidate(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        body = json.dumps(
            {"choices": [{"message": {"role": "assistant", "content": EXPECTED}}]}
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_: Any) -> None:
        pass


def exercise(server: str, destination: Path) -> dict[str, Any]:
    # Synthetic private marker verifies that editing removes the original
    # payload from the curated export, including the captured trajectory.
    with Logger(server, service="synthetic-refund-agent") as logger:
        pending = logger.log("Refund account=synthetic-private-217", OBSERVED)
        logger.flush(timeout=10)
        trace_id = pending.result()
    with Client(server) as client:
        deadline = time.monotonic() + 30
        while True:
            try:
                capture = client.capture_failure(
                    trace_id, title="Unauthorized refund", reason="Approval check missing"
                )
                break
            except ServerError as exc:
                if exc.code != "not_found" or time.monotonic() > deadline:
                    raise
                time.sleep(0.1)
        failure = capture["failure"]
        case_id = failure["id"]
        record = {
            "input": {"text": "Refund a pending order"},
            "output": {"text": OBSERVED},
            "reference": {"text": EXPECTED},
        }
        edited = client.update_failure(
            case_id,
            expected_revision=int(failure["revision"]),
            title="Unauthorized refund",
            reason="A pending refund requires human approval",
            assignee="synthetic-support-owner",
            record=record,
            status="FAILURE_STATUS_IN_REVIEW",
        )
        approved = client.review_failure(
            case_id,
            expected_revision=int(edited["revision"]),
            decision="FAILURE_STATUS_APPROVED",
            note="Synthetic example: reference and redaction checked",
        )
        revision = int(approved["revision"])
        # Recapture is idempotent and does not replace reviewed behavior.
        retry = client.capture_failure(trace_id, title="Duplicate", reason="Same incident")
        if not retry.get("deduplicated") or int(retry["failure"]["revision"]) != revision:
            raise RuntimeError("recapture did not preserve the reviewed case")

    with contextlib.redirect_stdout(io.StringIO()) as output:
        code = main(
            [
                "inbox",
                "export",
                "--server",
                server,
                "--case",
                f"{case_id}@{revision}",
                "-o",
                str(destination),
                "--name",
                "refund-incidents",
                "--version",
                "v1",
                "--owner",
                "synthetic-example",
                "--tag",
                "synthetic",
            ]
        )
    if code != 0:
        raise RuntimeError("snapshot export failed")
    exported = json.loads(output.getvalue())
    if "synthetic-private-217" in destination.read_text():
        raise RuntimeError("redacted source payload reached the snapshot")
    spec: dict[str, Any] = {
        "apiVersion": "evals.si/v1alpha1",
        "kind": "EvalRun",
        "spec": {
            "dataset": {"uri": exported["uri"]},
            "evaluators": [{"ref": "exact-match"}],
            "gates": [
                {"metric": "exact-match", "min": 1},
                {"metric": "exact-match", "stat": "GATE_STAT_ERROR_RATE", "max": 0},
            ],
        },
    }
    run_file = destination.with_suffix(".run.json")
    run_file.write_text(json.dumps(spec))
    with contextlib.redirect_stdout(io.StringIO()):
        bad = main(
            ["run", "-f", str(run_file), "--output", str(destination.with_suffix(".observed.json"))]
        )
    fixture = ThreadingHTTPServer(("127.0.0.1", 0), Candidate)
    thread = threading.Thread(target=fixture.serve_forever, daemon=True)
    thread.start()
    try:
        spec["spec"]["target"] = {
            "connector": "openai-compatible",
            "model": "scripted-fixture",
            "base_url": f"http://127.0.0.1:{fixture.server_port}/v1",
            "api_key_env": "none",
        }
        run_file.write_text(json.dumps(spec))
        with contextlib.redirect_stdout(io.StringIO()):
            good = main(
                [
                    "run",
                    "-f",
                    str(run_file),
                    "--output",
                    str(destination.with_suffix(".candidate.json")),
                ]
            )
    finally:
        fixture.shutdown()
        thread.join()
        fixture.server_close()
    if (bad, good) != (3, 0):
        raise RuntimeError(f"unexpected controls: observed={bad}, candidate={good}")
    return {
        "case_id": case_id,
        "revision": revision,
        "snapshot_sha256": exported["snapshot"]["sha256"],
        "observed_gate_exit": bad,
        "candidate_gate_exit": good,
        "synthetic": True,
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(exercise(args.server, args.output.resolve()), indent=2))
