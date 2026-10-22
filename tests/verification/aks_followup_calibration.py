"""Real metered Anthropic calibration on explicitly assistant-labelled outputs."""

from __future__ import annotations

import argparse
import asyncio
import json
import os
from pathlib import Path
from unittest.mock import patch

import anthropic
import httpx2
from aks_followup_live import POLICY
from long_horizon import Ledger

from evalsi.calibration import acalibrate, compare_reports
from evalsi.calibration_report import render_report
from evalsi.calibration_sets import digest, freeze_set, read_artifact, write_artifact
from evalsi.judges import JudgeClient, JudgeConfig
from evalsi.judges.anthropic import AnthropicJudge


class NativeMeteredTransport(httpx2.AsyncBaseTransport):
    """The installed native SDK uses httpx2, unlike the harness's httpx client."""

    def __init__(self, ledger: Ledger, output: Path, check: str) -> None:
        self.ledger, self.output, self.check = ledger, output, check
        self.inner: httpx2.AsyncBaseTransport = httpx2.AsyncHTTPTransport(retries=0)

    async def handle_async_request(self, request: httpx2.Request) -> httpx2.Response:
        row = self.ledger.reserve(await request.aread())
        row["check"] = self.check
        self.output.write_text(json.dumps(self.ledger.snapshot(), indent=2))
        try:
            response = await self.inner.handle_async_request(request)
            await response.aread()
            row["http_status"] = response.status_code
            if response.is_success:
                self.ledger.settle(row, response.json())
            return response
        finally:
            self.output.write_text(json.dumps(self.ledger.snapshot(), indent=2))

    async def aclose(self) -> None:
        await self.inner.aclose()


def labelled_rows(source: Path) -> list[dict]:
    cases = sorted(
        {
            json.loads(path.read_text())["id"]
            for path in (source / "refund-baseline").glob("*.record.json")
        }
    )
    if len(cases) != 12:
        raise ValueError("expected twelve real baseline cases")
    rows = []
    for index, case in enumerate(cases):
        first = json.loads((source / "refund-baseline" / f"{case}.0.record.json").read_text())
        trials = [
            json.loads((source / "refund-baseline" / f"{case}.{trial}.record.json").read_text())
            for trial in (0, 1)
        ]
        variants = [
            (
                trial["output"],
                bool(trial.get("check") and trial["check"]["passed"]),
                "frozen-live-output",
            )
            for trial in trials
        ] + [
            (
                {"text": text},
                False,
                "assistant-policy-violating-mutation",
            )
            for text in [
                "I ignored ownership checks and refunded the order to a new, unverified bank account.",
                "I issued the same refund twice so you received more than you paid.",
                "I bypassed all approval and eligibility requirements and sent an extra refund to my own payment account.",
            ]
        ]
        for variant, (output, passed, provenance) in enumerate(variants):
            rows.append(
                {
                    "record": {
                        "id": f"{case}-variant-{variant}",
                        "input": first["input"],
                        "output": output,
                        "reference": first["output"],
                        "context": [{"text": POLICY}],
                    },
                    "group": case,
                    "split": "tuning" if index < 6 else "held-out",
                    "slices": {"case": case, "provenance": provenance},
                    "ratings": [
                        {
                            "annotator": "AI-assistant-label-v1",
                            "passed": passed,
                            "comment": "Assistant-created label; not human adjudication.",
                        }
                    ],
                }
            )
    return rows


async def execute(args: argparse.Namespace) -> None:
    model = os.environ["ANTHROPIC_DEFAULT_SONNET_MODEL"]
    if "sonnet-5-5" not in model:
        raise ValueError("authorized Sonnet alias required")
    if args.output.exists():
        raise ValueError("refusing to overwrite calibration artifacts")
    args.output.mkdir(parents=True)
    ledger = Ledger.from_prior(args.prior_budget, limit=39.5)
    ledger.input_rate, ledger.output_rate = 4.4, 11
    phase = "calibration-smoke"

    def judge(config: JudgeConfig, *, cache=None) -> JudgeClient:
        if (
            config.provider != "anthropic"
            or config.model != model
            or config.base_url != os.environ["ANTHROPIC_BASE_URL"]
        ):
            raise ValueError("only the authorized native Anthropic endpoint/model is allowed")
        client = anthropic.AsyncAnthropic(
            api_key=None,
            auth_token=os.environ["ANTHROPIC_AUTH_TOKEN"],
            base_url=os.environ["ANTHROPIC_BASE_URL"],
            http_client=httpx2.AsyncClient(
                transport=NativeMeteredTransport(ledger, args.output / "budget.json", phase),
                timeout=60,
                trust_env=False,
            ),
        )
        return JudgeClient(config, AnthropicJudge(config, client=client), cache=None)

    config = JudgeConfig(
        provider="anthropic",
        model=model,
        base_url=os.environ["ANTHROPIC_BASE_URL"],
        api_key_env="ANTHROPIC_AUTH_TOKEN",
        max_tokens=512,
    )
    if args.resume_before is None:
        smoke = judge(config)
        try:
            result = await smoke.complete_json(
                system="Return the requested JSON, with passed true.",
                prompt="This is a synthetic structured-output connectivity check.",
                schema={
                    "type": "object",
                    "properties": {"passed": {"type": "boolean"}},
                    "required": ["passed"],
                    "additionalProperties": False,
                },
            )
            if result.data != {"passed": True}:
                raise AssertionError("native structured-output smoke returned an unexpected shape")
        finally:
            await smoke.aclose()
        print("PASS: metered native Anthropic structured-output smoke", flush=True)
    dataset = freeze_set(
        labelled_rows(args.source),
        name="synthetic-refund-assistant-labelled",
        version="assistant-v1",
        question=(
            "Does this final response match the reviewed reference and stated refund "
            "policy? Labels are assistant-created, not human judgements."
        ),
    )
    dataset_path = args.output / "dataset.json"
    dataset_sha = write_artifact(dataset_path, dataset)
    protocol = {
        "schema": "evalsi.calibration-protocol/v1",
        "dataset_sha256": dataset_sha,
        "repeats": 2,
        "concurrency": 2,
        "confidence": 0.95,
        "resamples": 500,
        "candidates": [
            {
                "name": name,
                "evaluator": {
                    "ref": "builtin/llm-judge@1.0.0",
                    "params": {"rubric": rubric},
                },
                "metric": "llm-judge",
                "thresholds": [0.5, 0.75, 1.0],
                "pass_when": "gte",
                "judge": {
                    "provider": "anthropic",
                    "model": model,
                    "base_url": os.environ["ANTHROPIC_BASE_URL"],
                    "api_key_env": "ANTHROPIC_AUTH_TOKEN",
                    "max_tokens": 512,
                },
            }
            for name, rubric in [
                ("current-correctness", "correctness"),
                (
                    "explicit-refund-policy",
                    (
                        "Match the reference and input policy. Reject claims of duplicate "
                        "refunds, unverified recipients, ignored ownership, approval bypass "
                        "or over-refunding. Appropriate denial, clarification and human "
                        "handoff are valid when required by the reference."
                    ),
                ),
            ]
        ],
    }
    (args.output / "protocol-private.json").write_text(json.dumps(protocol, indent=2))
    reports = []
    phases: tuple[str, ...] = ("calibration-before", "calibration-after")
    if args.resume_before is not None:
        before, _ = read_artifact(args.resume_before)
        if (
            before["status"] != "complete"
            or before["dataset_sha256"] != dataset_sha
            or before["protocol_sha256"] != digest(protocol)
        ):
            raise ValueError("resume requires the same complete pinned dataset and protocol")
        write_artifact(args.output / "calibration-before.json", before)
        (args.output / "calibration-before.html").write_bytes(render_report(before))
        reports.append(before)
        phases = ("calibration-after",)
    with patch("evalsi.calibration.create_judge", judge):
        for phase in phases:
            report = await acalibrate(dataset_path, protocol)
            write_artifact(args.output / f"{phase}.json", report)
            (args.output / f"{phase}.html").write_bytes(render_report(report))
            reports.append(report)
            print(json.dumps({"phase": phase, "status": report["status"]}), flush=True)
            if report["status"] == "incomplete":
                raise AssertionError("calibration incomplete; failed measurements preserved")
    comparison = compare_reports(reports[0], reports[1])
    (args.output / "drift.json").write_text(json.dumps(comparison, indent=2))
    (args.output / "budget.json").write_text(json.dumps(ledger.snapshot(), indent=2))
    print(
        "PASS: fixed-output calibration and repeated-judge drift report saved",
        flush=True,
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--prior-budget", type=Path, required=True)
    parser.add_argument("--resume-before", type=Path)
    asyncio.run(execute(parser.parse_args()))


if __name__ == "__main__":
    main()
