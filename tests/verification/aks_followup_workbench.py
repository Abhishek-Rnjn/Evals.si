"""Own-pod current daemon with a synthetic HTTP agent for browser verification."""

from __future__ import annotations

import json
import signal
import subprocess
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


class Agent(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        body = json.loads(self.rfile.read(int(self.headers["content-length"])))
        bad = self.path == "/bad" and body["input"] == "Refund order 42"
        step = {
            "type": "tool",
            "name": "refund",
            "input": {"text": "order 42"},
        }
        step.update(
            {"error": "approval timeout"}
            if bad
            else {"output": {"text": "refund accepted"}}
        )
        response = json.dumps(
            {
                "output": "Waiting for approval" if bad else "Approved",
                "stop_reason": "awaiting_human" if bad else "completed",
                "usage": {"input_tokens": 7, "output_tokens": 3, "cost_usd": 0},
                "trajectory": {
                    "steps": [
                        {
                            "type": "tool",
                            "name": "lookup_order",
                            "input": {"text": body["input"]},
                            "output": {"text": f"trial-{body['trial']}-lookup"},
                        },
                        step,
                    ]
                },
            }
        ).encode()
        with Path("/work/workbench-agent-requests.jsonl").open("a") as log:
            log.write(json.dumps({"path": self.path, "trial": body["trial"]}) + "\n")
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)


def main() -> None:
    agent = ThreadingHTTPServer(("127.0.0.1", 18082), Agent)
    threading.Thread(target=agent.serve_forever, daemon=True).start()
    with Path("/work/workbench-server.log").open("w") as log:
        server = subprocess.Popen(
            [
                "/work/build/evalsid",
                "serve",
                "--config",
                "/work/api.yaml",
                "--no-auth",
            ],
            stdout=log,
            stderr=subprocess.STDOUT,
        )

        def stop(signum, frame) -> None:
            server.terminate()

        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        try:
            ready = False
            for _ in range(120):
                if server.poll() is not None:
                    raise RuntimeError(
                        "current daemon exited before becoming responsive"
                    )
                try:
                    with urllib.request.urlopen(
                        "http://127.0.0.1:8081/ui/", timeout=2
                    ) as response:
                        ready = response.status == 200
                except urllib.error.URLError as exc:
                    if not isinstance(exc.reason, ConnectionRefusedError):
                        raise
                if ready:
                    break
                time.sleep(0.5)
            if not ready:
                raise TimeoutError(
                    "current daemon did not become responsive in 60 seconds"
                )

            def post(path: str, value: dict) -> dict:
                request = urllib.request.Request(
                    "http://127.0.0.1:8081" + path,
                    data=json.dumps(value).encode(),
                    headers={"content-type": "application/json"},
                )
                with urllib.request.urlopen(request, timeout=30) as response:
                    return json.load(response)

            publication = post(
                "/v1alpha1/dataset-versions",
                {
                    "content": {
                        "project": "default",
                        "name": "refunds",
                        "version": "v1",
                        "owner": "synthetic-verification",
                        "split": "held-out",
                        "records": [
                            {
                                "id": f"refund/{index}",
                                "input": {"text": f"Refund order {order}"},
                                "reference": {"text": "Approved"},
                            }
                            for index, order in enumerate([42, 7, 99])
                        ],
                    }
                },
            )
            post(
                "/v1alpha1/dataset-versions/refunds/v1:review",
                {
                    "project": "default",
                    "sha256": publication["dataset"]["sha256"],
                    "expectedRevision": publication["dataset"]["revision"],
                    "decision": "DATASET_VERSION_STATE_APPROVED",
                    "note": "Assistant-reviewed synthetic workflow controls, not human labels.",
                },
            )
            print(
                "Current daemon and synthetic HTTP agent responsive; held-out dataset approved",
                flush=True,
            )
            status = server.wait()
            if status != 0:
                raise RuntimeError(f"current daemon exited with status {status}")
        finally:
            if server.poll() is None:
                server.terminate()
                server.wait(timeout=15)
            agent.shutdown()


if __name__ == "__main__":
    main()
