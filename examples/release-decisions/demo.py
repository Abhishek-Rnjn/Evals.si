"""Synthetic production failures -> approved suite -> candidate comparison -> decision.

Run against a disposable no-auth evalsid with this checkout's worker. Scripted
HTTP responses exercise release controls; no real model or business action runs.
"""

from __future__ import annotations

import argparse
import contextlib
import copy
import io
import json
import os
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import yaml

from evalsi.cli import main
from evalsi.client import Client, ServerError
from evalsi.logging import Logger
from evalsi.release_decisions import policy_hash, read_report

EXPECTED = "Approval required before refund"
OBSERVED = "Refund issued without approval"


class Agent(BaseHTTPRequestHandler):
    calls = 0

    def do_POST(self) -> None:
        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        Agent.calls += 1
        text = request["messages"][-1]["content"]
        response = OBSERVED if "/broken/" in self.path and "another" in text else EXPECTED
        raw = json.dumps(
            {"choices": [{"message": {"role": "assistant", "content": response}}]}
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *_: Any) -> None:
        pass


def approved_suite(client: Client, server: str) -> tuple[dict[str, Any], list[str]]:
    selections = {}
    for input_text in ("Refund a pending order", "Refund another person's order"):
        with Logger(server, service="synthetic-release-agent") as logger:
            future = logger.log(input_text + " synthetic-private-528", OBSERVED)
            logger.flush(timeout=10)
            trace_id = future.result()
        deadline = time.monotonic() + 30
        while True:
            try:
                capture = client.capture_failure(
                    trace_id, title="Unapproved refund", reason="Missing authorization"
                )
                break
            except ServerError as exc:
                if exc.code != "not_found" or time.monotonic() > deadline:
                    raise
                time.sleep(0.1)
        case_id = capture["failure"]["id"]
        edited = client.update_failure(
            case_id,
            expected_revision=1,
            title="Unapproved refund",
            reason="Require explicit approval",
            assignee="synthetic-support",
            status="FAILURE_STATUS_IN_REVIEW",
            record={
                "input": {"text": input_text},
                "output": {"text": OBSERVED},
                "reference": {"text": EXPECTED},
            },
        )
        approved = client.review_failure(
            case_id,
            expected_revision=int(edited["revision"]),
            decision="FAILURE_STATUS_APPROVED",
            note="Synthetic expectation and redaction reviewed",
        )
        selections[case_id] = int(approved["revision"])
    publication = client.publish_dataset_version(
        {
            "name": "refunds-" + case_id,
            "version": "v1",
            "owner": "synthetic-support",
            "split": "held-out",
        },
        failures=selections,
    )
    dataset = publication["dataset"]
    if "synthetic-private-528" in json.dumps(dataset):
        raise RuntimeError("source marker leaked into approved cases")
    client.review_dataset_version(
        dataset["content"]["name"],
        "v1",
        sha256=dataset["sha256"],
        expected_revision=1,
        decision="DATASET_VERSION_STATE_APPROVED",
        note="Reviewed all cases; anonymous local demo only",
    )
    return publication["reference"], list(selections)


def execute(client: Client, spec: dict[str, Any], commit: str) -> dict[str, Any]:
    run = client.create_run(
        spec, name="synthetic release baseline", labels={"agent.commit": commit}
    )
    for _ in client.watch_run(run["id"]):
        pass
    return client.get_run(run["id"])


def action_control(
    server: str,
    *,
    work: Path,
    spec: Path,
    policy: Path,
    baseline: dict[str, Any],
    policy_pin: str,
    commit: str,
    decision_id: str,
) -> int:
    scratch = work / (decision_id + "-action")
    scratch.mkdir()
    env = {
        **os.environ,
        "GITHUB_WORKSPACE": str(work),
        "GITHUB_SHA": commit,
        "GITHUB_RUN_ID": "123",
        "GITHUB_RUN_ATTEMPT": "1",
        "RUNNER_TEMP": str(scratch),
        "GITHUB_OUTPUT": str(scratch / "outputs"),
        "GITHUB_STEP_SUMMARY": str(scratch / "summary"),
        "EVALSI_ACTION_DIRECTORY": ".",
        "EVALSI_ACTION_SPEC": spec.name,
        "EVALSI_ACTION_SERVER": server,
        "EVALSI_ACTION_BASELINE": "",
        "EVALSI_ACTION_RELEASE_POLICY": policy.name,
        "EVALSI_ACTION_RELEASE_POLICY_SHA256": policy_pin,
        "EVALSI_ACTION_BASELINE_RUN": baseline["runId"],
        "EVALSI_ACTION_BASELINE_SHA256": baseline["sha256"],
        "EVALSI_ACTION_BASELINE_COMMIT": baseline["commit"],
        "EVALSI_ACTION_RELEASE_ID": decision_id + "-ci",
    }
    adapter = Path(__file__).resolve().parents[2] / ".github/actions/evalsi/run.py"
    proc = subprocess.run(
        [sys.executable, str(adapter)],
        env=env,
        capture_output=True,
        text=True,
        check=False,
        timeout=60,
    )
    if proc.returncode or proc.stdout or proc.stderr:
        raise RuntimeError(f"action wrapper failed: {proc.stderr}")
    values = dict(line.split("=", 1) for line in (scratch / "outputs").read_text().splitlines())
    summary = (scratch / "summary").read_text()
    if "synthetic-private-528" in summary or "failure/" in summary:
        raise RuntimeError("private case content/identity leaked into the CI summary")
    return int(values["exit-code"])


def gate_controls(
    server: str,
    *,
    work: Path,
    baseline: dict[str, Any],
    spec: dict[str, Any],
    policy: dict[str, Any],
    prefix: str,
) -> dict[str, Any]:
    result = {}
    common = [
        "--server",
        server,
        "--baseline",
        baseline["runId"],
        "--baseline-sha256",
        baseline["sha256"],
        "--baseline-commit",
        baseline["commit"],
    ]
    for name, expected, commit in (
        ("passed", 0, "b"),
        ("blocked", 3, "c"),
        ("insufficient", 1, "d"),
    ):
        current_policy = copy.deepcopy(policy)
        current_spec = copy.deepcopy(spec)
        if name == "blocked":
            current_spec["target"]["base_url"] = current_spec["target"]["base_url"].replace(
                "/candidate/", "/broken/"
            )
        if name == "insufficient":
            current_policy["min_records"] = 3
        policy_file, spec_file = work / f"{name}.policy.json", work / f"{name}.run.yaml"
        policy_file.write_text(json.dumps(current_policy), encoding="utf-8")
        spec_file.write_text(
            yaml.safe_dump(
                {"apiVersion": "evals.si/v1alpha1", "kind": "EvalRun", "spec": current_spec}
            ),
            encoding="utf-8",
        )
        destination = work / f"{name}.release.json"
        with contextlib.redirect_stdout(io.StringIO()):
            code = main(
                [
                    "release",
                    "gate",
                    "--id",
                    f"{prefix}-{name}",
                    "-f",
                    str(policy_file),
                    "--policy-sha256",
                    policy_hash(current_policy),
                    "--spec",
                    str(spec_file),
                    "--commit",
                    commit * 40,
                    "-o",
                    str(destination),
                    *common,
                ]
            )
        if code != expected:
            raise RuntimeError(f"{name}: expected exit {expected}, got {code}")
        action_code = action_control(
            server,
            work=work,
            spec=spec_file,
            policy=policy_file,
            baseline=baseline,
            policy_pin=policy_hash(current_policy),
            commit=commit * 40,
            decision_id=f"{prefix}-{name}",
        )
        if action_code != expected:
            raise RuntimeError(f"{name}: Action expected exit {expected}, got {action_code}")
        decision = json.loads(destination.read_text())["decision"]
        read_report(destination, decision["sha256"])
        result[name] = {
            "id": decision["id"],
            "outcome": decision["outcome"],
            "sha256": decision["sha256"],
            "exit_code": code,
            "action_exit_code": action_code,
        }
    return result


def exercise(server: str, directory: Path) -> dict[str, Any]:
    directory.mkdir(mode=0o700)  # Never reuse a previous report directory.
    with Client(server) as client:
        reference, cases = approved_suite(client, server)
        source = {
            "dataset": {"catalog": reference},
            "evaluators": [{"ref": "exact-match"}],
            "gates": [
                {"metric": "exact-match", "min": 1},
                {"metric": "exact-match", "stat": "GATE_STAT_ERROR_RATE", "max": 0},
            ],
        }
        observed = execute(client, source, "e" * 40)
        if observed["status"] != "RUN_STATUS_FAILED":
            raise RuntimeError("recorded production failures did not fail regression gates")
        fixture = ThreadingHTTPServer(("127.0.0.1", 0), Agent)
        thread = threading.Thread(target=fixture.serve_forever, daemon=True)
        thread.start()
        try:
            spec = {
                **source,
                "target": {
                    "connector": "openai-compatible",
                    "model": "scripted-agent",
                    "base_url": f"http://127.0.0.1:{fixture.server_port}/baseline/v1",
                    "api_key_env": "none",
                },
            }
            run = execute(client, spec, "a" * 40)
            baseline = client.release_run_evidence(run["id"])["reference"]
            spec["target"]["base_url"] = spec["target"]["base_url"].replace(
                "/baseline/", "/candidate/"
            )
            policy = {
                "name": "synthetic-refunds",
                "version": "v1",
                "dataset": reference,
                "min_records": 2,
                "rules": [
                    {
                        "metric": "exact-match",
                        "direction": "METRIC_DIRECTION_HIGHER",
                        "max_regression": 0,
                        "min_case": 1,
                    }
                ],
            }
            controls = gate_controls(
                server,
                work=directory,
                baseline=baseline,
                spec=spec,
                policy=policy,
                prefix="release-" + cases[0],
            )
        finally:
            fixture.shutdown()
            thread.join()
            fixture.server_close()
        if Agent.calls < 8:
            raise RuntimeError("expected actual baseline and candidate calls")
        destination = directory / "passed.release.json"
        raw = json.loads(destination.read_text())
        raw["decision"]["outcome"] = "RELEASE_OUTCOME_BLOCK"
        tampered = directory / "tampered.json"
        tampered.write_text(json.dumps(raw), encoding="utf-8")
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            code = main(
                ["release", "verify", str(tampered), "--sha256", controls["passed"]["sha256"]]
            )
        if code != 1:
            raise RuntimeError("tampered release report verified")
    return {
        "synthetic": True,
        "failure_cases": cases,
        "dataset": reference,
        "controls": controls,
        "actual_agent_calls": Agent.calls,
        "tamper_rejected": True,
        "reports": str(directory),
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(exercise(args.server, args.output_dir.resolve()), indent=2))
