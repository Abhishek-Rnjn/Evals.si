"""Live delegation and model-cassette side effects against owned AKS PostgreSQL."""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import secrets
import subprocess
from pathlib import Path

import httpx
from aks_followup_live import TaggedTransport
from evalsi.packs.agent_checks import handoff_correctness
from evalsi.targets import TargetConfig
from evalsi.types import Record, Trajectory
from evalsi_harness.cassette import Cassette, CassetteModel
from evalsi_harness.context import ContextConfig
from evalsi_harness.events import Event, StepEvent
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import AnthropicChat
from evalsi_harness.subagent import SUBAGENT_SYSTEM
from evalsi_harness.tools import FunctionTool, Tool
from long_horizon import Ledger

KUBECTL = [
    "/opt/homebrew/bin/kubectl",
    "--kubeconfig",
    "./kube-aks.yaml",
    "-n",
    "evalsi-verify-20261021-followup",
]


def sql(statement: str) -> str:
    result = subprocess.run(
        [
            *KUBECTL,
            "exec",
            "postgres",
            "--",
            "psql",
            "-U",
            "postgres",
            "-d",
            "evalsi_followup",
            "-v",
            "ON_ERROR_STOP=1",
            "-Atqc",
            statement,
        ],
        capture_output=True,
        text=True,
        check=True,
        timeout=30,
    )
    return result.stdout.strip()


class TracedTransport(TaggedTransport):
    def __init__(self, ledger: Ledger, output: Path, check: str, trace: list) -> None:
        super().__init__(ledger, output / "budget.json", check)
        self.directory, self.trace = output, trace

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.trace.append(json.loads(await request.aread()))
        (self.directory / "request-bodies.json").write_text(
            json.dumps(self.trace, indent=2)
        )
        return await super().handle_async_request(request)


async def execute(args: argparse.Namespace) -> None:
    model_name = os.environ["ANTHROPIC_DEFAULT_SONNET_MODEL"]
    if "sonnet-5-5" not in model_name:
        raise ValueError("authorized Sonnet alias required")
    if args.output.exists():
        raise ValueError("refusing to overwrite lifecycle evidence")
    args.output.mkdir(parents=True)
    ledger = Ledger.from_prior(args.prior_budget, limit=39.5)
    ledger.input_rate, ledger.output_rate = 4.4, 11
    run = "followup-" + secrets.token_hex(8)
    sql(
        "CREATE TABLE IF NOT EXISTS followup_lifecycle_effects "
        "(id BIGSERIAL PRIMARY KEY, scope TEXT NOT NULL, stable_key TEXT, "
        "UNIQUE(scope,stable_key));"
        "CREATE TABLE IF NOT EXISTS followup_lifecycle_orders "
        "(run_id TEXT PRIMARY KEY, customer TEXT, status TEXT, amount INTEGER);"
        f"INSERT INTO followup_lifecycle_orders VALUES ('{run}','C-1','delivered',4200);"
    )
    trace: list[dict] = []
    rows: list[dict] = []
    context = ContextConfig(strategy="none")

    async def trial(
        name: str,
        mode: str,
        tape: Path,
        prompt: str,
        tools: list[Tool],
        *,
        delegate: bool = False,
        fork_to: Path | None = None,
    ) -> dict:
        before = ledger.charged
        attempts = len(ledger.attempts)
        live = None
        if mode in ("record", "fork"):
            live = AnthropicChat(
                TargetConfig(
                    connector="anthropic",
                    model=model_name,
                    base_url=os.environ["ANTHROPIC_BASE_URL"],
                    api_key_env="ANTHROPIC_AUTH_TOKEN",
                    max_tokens=512,
                    thinking="disabled",
                ),
                httpx.AsyncClient(
                    transport=TracedTransport(ledger, args.output, name, trace),
                    timeout=60,
                    trust_env=False,
                ),
            )
        cassette = Cassette(
            tape,
            mode,
            context=context.describe(),
            branch_at_step=1 if mode == "fork" else 0,
            fork_to=fork_to,
        )
        model = CassetteModel(cassette, live)
        steps = []

        def emit(event: Event) -> None:
            if isinstance(event, StepEvent):
                steps.append(event.step)

        loop = AgentLoop(
            model,
            tools,
            LoopConfig(
                max_steps=10,
                wall_clock_s=180,
                context=context,
                parallel=False,
                delegate_steps=6 if delegate else 0,
                truncation_retry_tokens=1024,
            ),
            emit,
        )
        try:
            final = await loop.run([{"role": "user", "content": prompt}])
        finally:
            cassette.save()
            await model.aclose()
            (args.output / "budget.json").write_text(
                json.dumps(ledger.snapshot(), indent=2)
            )
        record = Record(id=name, trajectory=Trajectory(steps=steps))
        (args.output / f"{name}.{mode}.json").write_text(
            json.dumps(record.to_dict(), indent=2)
        )
        row = {
            "check": name,
            "mode": mode,
            "stop_reason": final.stop_reason,
            "subagents": loop.state.subagents,
            "subagent_steps": loop.state.subagent_steps,
            "new_http_attempts": len(ledger.attempts) - attempts,
            "incremental_estimated_usd": ledger.charged - before,
        }
        if delegate:
            row["handoff_correctness"] = handoff_correctness(record).passed
            if not row["handoff_correctness"] or loop.state.subagents != 1:
                raise AssertionError(
                    "expected one real delegate with valid linked handoff"
                )
        if final.stop_reason != "completed":
            raise AssertionError(f"live lifecycle task did not complete: {row}")
        if mode in ("replay", "resume") and row["new_http_attempts"]:
            raise AssertionError("offline lifecycle control made a provider request")
        rows.append(row)
        (args.output / "results.json").write_text(json.dumps(rows, indent=2))
        print(json.dumps(row), flush=True)
        return row

    async def read_order() -> str:
        order = await asyncio.to_thread(
            sql,
            f"SELECT customer,status,amount FROM followup_lifecycle_orders WHERE run_id='{run}'",
        )
        return order + "\n" + "BACKGROUND_NOISE_NOT_FOR_PARENT " * 200

    await trial(
        "live-delegation",
        "record",
        args.output / "delegate.jsonl",
        "Use the delegate tool exactly once. Ask the subagent to call read_order "
        "and report only customer, delivery status and amount, without background "
        "noise. Do not call read_order in the parent. Then answer those three fields.",
        [FunctionTool("read_order", read_order, input_schema={"type": "object"})],
        delegate=True,
    )

    def is_child(request: dict) -> bool:
        system = request.get("system", "")
        text = (
            system
            if isinstance(system, str)
            else "\n".join(block.get("text", "") for block in system)
        )
        return SUBAGENT_SYSTEM in text

    child = [r for r in trace if is_child(r)]
    parent = [r for r in trace if not is_child(r)]
    if not any("BACKGROUND_NOISE_NOT_FOR_PARENT" in str(r["messages"]) for r in child):
        raise AssertionError("no real child request consumed the order evidence")
    if any("BACKGROUND_NOISE_NOT_FOR_PARENT" in str(r["messages"]) for r in parent):
        raise AssertionError("subagent raw evidence leaked into parent context")

    def refund_tool(scope: str, key: str, keyed: bool) -> Tool:
        async def record_refund(stable_key: str) -> str:
            if stable_key != key:
                raise ValueError(
                    "the synthetic experiment requires its exact stable key"
                )
            value = f"'{key}'" if keyed else "NULL"
            result = await asyncio.to_thread(
                sql,
                "INSERT INTO followup_lifecycle_effects(scope,stable_key) "
                f"VALUES ('{scope}',{value}) ON CONFLICT DO NOTHING RETURNING id",
            )
            return (
                "write committed" if result else "already committed; key deduplicated"
            )

        return FunctionTool(
            "record_refund",
            record_refund,
            input_schema={
                "type": "object",
                "properties": {"stable_key": {"type": "string"}},
                "required": ["stable_key"],
                "additionalProperties": False,
            },
        )

    for keyed in (True, False):
        name = "external-keyed" if keyed else "external-unkeyed"
        scope = run + ("-keyed" if keyed else "-unkeyed")
        key = run
        tools = [refund_tool(scope, key, keyed)]
        prompt = (
            f"Call record_refund exactly once with stable_key '{key}', then finish. "
            "This is a synthetic experiment, not a real payment."
        )
        tape = args.output / f"{name}.jsonl"
        await trial(name, "record", tape, prompt, tools)
        source_hash = hashlib.sha256(tape.read_bytes()).hexdigest()
        counts = [
            int(
                sql(
                    f"SELECT count(*) FROM followup_lifecycle_effects WHERE scope='{scope}'"
                )
            )
        ]
        if counts[0] != 1:
            raise AssertionError("expected exactly one initial external write")
        await trial(name, "replay", tape, prompt, tools)
        counts.append(
            int(
                sql(
                    f"SELECT count(*) FROM followup_lifecycle_effects WHERE scope='{scope}'"
                )
            )
        )
        if counts[1] != (1 if keyed else 2):
            raise AssertionError(
                f"external replay did not demonstrate expected effects: {counts}"
            )
        if keyed:
            await trial(name, "resume", tape, prompt, tools)
            fork = args.output / "external-keyed.fork.jsonl"
            await trial(name, "fork", tape, prompt, tools, fork_to=fork)
            if hashlib.sha256(tape.read_bytes()).hexdigest() != source_hash:
                raise AssertionError("fork/resume changed the original cassette")
            counts.append(
                int(
                    sql(
                        f"SELECT count(*) FROM followup_lifecycle_effects WHERE scope='{scope}'"
                    )
                )
            )
            if counts[-1] != 1:
                raise AssertionError("keyed resume/fork duplicated the external write")
        rows.append(
            {
                "check": name + "-database-evidence",
                "external_write_counts": counts,
                "model_only_cassette_tools_reexecuted": True,
                "harness_exactly_once_claimed": False,
            }
        )
    (args.output / "results.json").write_text(json.dumps(rows, indent=2))
    print(
        "PASS: real delegation, fork/resume and external keyed/unkeyed replay controls"
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--prior-budget", type=Path, required=True)
    asyncio.run(execute(parser.parse_args()))


if __name__ == "__main__":
    main()
