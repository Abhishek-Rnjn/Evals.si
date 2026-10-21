"""Opt-in real-pod/Claude run; reserve estimated spend before every HTTP attempt."""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import shutil
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import httpx
import yaml
from evalsi.targets import TargetConfig
from evalsi_harness.cassette import Cassette, CassetteModel
from evalsi_harness.context import SUMMARY_SYSTEM, ContextConfig
from evalsi_harness.events import Event, FinalEvent, StepEvent
from evalsi_harness.hooks import Continue, Hook, HookContext, Hooks
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import AnthropicChat, Message, ModelTurn, ToolSpec
from evalsi_harness.tools import FunctionTool
from horizon_task import ACTIONS, POLICY, current


class BudgetExhausted(RuntimeError):
    pass


@dataclass
class Ledger:
    limit: float = 14.5
    input_rate: float = 2.2
    output_rate: float = 5.5
    charged: float = 0.001
    attempts: list[dict[str, Any]] = field(default_factory=list)
    carried_attempts: int = 0

    @classmethod
    def from_prior(cls, path: Path, *, limit: float = 14.5) -> Ledger:
        prior = json.loads(path.read_text())
        ledger = cls(limit=limit)
        if (
            limit not in (14.5, 19.5, 36.0, 39.5)
            or prior["allowance_usd"] not in (14.5, 19.5, 36.0, 39.5)
            or prior["allowance_usd"] > limit
        ) or (
            prior["input_including_all_cache_per_million"],
            prior["output_per_million"],
        ) not in ((2.2, 5.5), (4.4, 11.0)):
            raise ValueError(
                "prior ledger must use recognized rates and a nondecreasing supported allowance"
            )
        ledger.charged = prior["conservative_estimated_usd"]
        ledger.attempts = prior["attempts"]
        for row in ledger.attempts:
            row.setdefault("input_rate", prior["input_including_all_cache_per_million"])
            row.setdefault("output_rate", prior["output_per_million"])
        ledger.carried_attempts = len(ledger.attempts)
        minimum = 0.001 + sum(row["charged_usd"] for row in ledger.attempts)
        if ledger.charged < minimum - 0.000001:
            raise ValueError("prior ledger understates its charged attempts")
        return ledger

    def reserve(self, body: bytes) -> dict[str, Any]:
        request = json.loads(body)
        maximum = request["max_tokens"]
        if not isinstance(maximum, int) or not 1 <= maximum <= 1024:
            raise ValueError(
                "all responses, including summaries, must be capped at 1024 tokens"
            )
        # UTF-8 bytes plus overhead is deliberately pessimistic, not a provider token count.
        upper = (
            (len(body) + 4096) * self.input_rate + maximum * self.output_rate
        ) / 1e6
        if self.charged + upper > self.limit:
            raise BudgetExhausted(
                "next HTTP attempt would exceed the conservative cost allowance"
            )
        row = {
            "attempt": len(self.attempts) + 1,
            "reserved_usd": upper,
            "charged_usd": upper,
            "input_rate": self.input_rate,
            "output_rate": self.output_rate,
        }
        self.charged += upper
        self.attempts.append(row)
        return row

    def settle(self, row: dict[str, Any], payload: dict[str, Any]) -> None:
        usage = payload.get("usage")
        if not isinstance(usage, dict):
            raise TypeError(
                "provider omitted usage; reservation retained and run stopped"
            )
        keys = (
            "input_tokens",
            "cache_creation_input_tokens",
            "cache_read_input_tokens",
            "output_tokens",
        )
        tokens = {key: usage.get(key, 0) for key in keys}
        if any(type(value) is not int for value in tokens.values()):
            raise TypeError("provider returned noninteger usage; reservation retained")
        if any(value < 0 for value in tokens.values()):
            raise ValueError("provider returned invalid usage; reservation retained")
        if "input_tokens" not in usage or "output_tokens" not in usage:
            raise ValueError(
                "provider omitted required token counts; reservation retained"
            )
        actual = (
            sum(tokens[key] for key in keys[:-1]) * self.input_rate
            + tokens["output_tokens"] * self.output_rate
        ) / 1e6
        self.charged += actual - row["charged_usd"]
        row.update(tokens)
        row.update(charged_usd=actual, model=payload.get("model", "unreported"))
        if actual > row["reserved_usd"]:
            raise ValueError(
                "reported usage exceeds the pre-request estimate; stop and investigate"
            )

    def snapshot(self) -> dict[str, Any]:
        return {
            "conservative_estimated_usd": round(self.charged, 8),
            "allowance_usd": self.limit,
            "input_including_all_cache_per_million": self.input_rate,
            "output_per_million": self.output_rate,
            "smoke_allowance_included_usd": min(0.001, self.charged),
            "gateway_invoice_guaranteed": False,
            "carried_attempts": self.carried_attempts,
            "attempts": self.attempts,
        }


class ConciseSummaryModel(AnthropicChat):
    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolSpec],
        *,
        system: str = "",
        max_tokens: int = 0,
    ) -> ModelTurn:
        if system == SUMMARY_SYSTEM:
            system += (
                "\nUse at most 120 words. Preserve the approved release code and the "
                "current incident ID. Do not enumerate past incidents or repeat logs."
            )
        return await super().complete(
            messages, tools, system=system, max_tokens=max_tokens
        )


class MeteredTransport(httpx.AsyncBaseTransport):
    def __init__(self, ledger: Ledger, output: Path) -> None:
        self.ledger = ledger
        self.output = output
        self.inner: httpx.AsyncBaseTransport = httpx.AsyncHTTPTransport(retries=0)

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        row = self.ledger.reserve(await request.aread())
        self.output.write_text(json.dumps(self.ledger.snapshot(), indent=2) + "\n")
        try:
            response = await self.inner.handle_async_request(request)
            await response.aread()
            row["http_status"] = response.status_code
            if response.is_success:
                self.ledger.settle(row, response.json())
            return response
        finally:
            # Failed or ambiguous attempts retain their whole reservation, including retries.
            self.output.write_text(json.dumps(self.ledger.snapshot(), indent=2) + "\n")

    async def aclose(self) -> None:
        await self.inner.aclose()


async def execute(args: argparse.Namespace) -> int:
    from evalsi.sandbox.client import SandboxClient, SandboxSpec

    settings = yaml.safe_load(args.config.read_text())
    count = settings["task"]["incidents"]
    if not 1 <= count <= 300:
        raise ValueError("fixture count must be between 1 and 300")
    if not os.environ.get("EVALSI_SANDBOX_ADDR", "").startswith("tls://"):
        raise ValueError(
            "an explicit mutual-TLS sandbox pool is required; no local fallback"
        )
    model_env = (
        "ANTHROPIC_DEFAULT_HAIKU_MODEL"
        if args.model_family == "haiku"
        else "ANTHROPIC_DEFAULT_SONNET_MODEL"
    )
    model_name = os.environ.get(model_env, "")
    expected_alias = "haiku" if args.model_family == "haiku" else "sonnet-5-5"
    if not args.replay and expected_alias not in model_name.lower():
        raise ValueError(
            f"set {model_env} to an authorized {expected_alias} model with the documented pricing basis"
        )
    if not args.replay and (
        not os.environ.get("ANTHROPIC_AUTH_TOKEN")
        or not os.environ.get("ANTHROPIC_BASE_URL", "").startswith("https://")
    ):
        raise ValueError("terminal gateway HTTPS URL and bearer token are required")
    if args.replay and args.prior_budget:
        raise ValueError(
            "offline replay makes no provider calls; do not carry a paid ledger"
        )
    if args.resume and not args.prior_budget:
        raise ValueError(
            "paid resume requires --prior-budget to preserve the total allowance"
        )
    if args.output.exists() and any(args.output.iterdir()):
        raise ValueError(
            "output directory must be new or empty; refusing to overwrite evidence"
        )
    args.output.mkdir(parents=True, exist_ok=True)
    live = None
    if args.replay:
        ledger = Ledger(charged=0.0)
        if args.model_family == "sonnet":
            ledger.input_rate, ledger.output_rate = 4.4, 11.0
    else:
        ledger = (
            Ledger.from_prior(args.prior_budget, limit=args.budget_limit)
            if args.prior_budget
            else Ledger(limit=args.budget_limit)
        )
        if args.model_family == "sonnet":
            ledger.input_rate, ledger.output_rate = 4.4, 11.0
        transport = MeteredTransport(ledger, args.output / "budget.json")
        http = httpx.AsyncClient(transport=transport, timeout=60, trust_env=False)
        target = TargetConfig(
            connector="anthropic",
            model=model_name,
            base_url=os.environ["ANTHROPIC_BASE_URL"],
            api_key_env="ANTHROPIC_AUTH_TOKEN",
            max_tokens=1024,
            thinking="disabled",
        )
        live = ConciseSummaryModel(target, http)
    context = ContextConfig(**settings["context"])
    if args.resume:
        shutil.copyfile(args.resume, args.output / "cassette.jsonl")
    cassette = Cassette(
        args.replay or args.output / "cassette.jsonl",
        "replay" if args.replay else "resume" if args.resume else "record",
        context=context.describe(),
        hooks=["horizon-completion-v1"] if args.completion_guard else [],
    )
    model = CassetteModel(cassette, live)
    started = time.monotonic()
    turns: list[dict[str, Any]] = []

    def emit(event: Event) -> None:
        if isinstance(event, StepEvent):
            row = {"name": event.step.name, "type": event.step.type}
            turns.append(row)

    config = LoopConfig(
        max_steps=340,
        max_usd=args.budget_limit,
        input_per_mtok=ledger.input_rate,
        output_per_mtok=ledger.output_rate,
        wall_clock_s=3600,
        parallel=False,
        truncation_retry_tokens=1024,
        system=POLICY,
        context=context,
    )
    summary: dict[str, Any] = {
        "requested_model": model_name if not args.replay else "offline-replay",
        "offline_model_replay": bool(args.replay),
        "resuming_cassette": bool(args.resume),
        "completion_guard": args.completion_guard,
        "fixture": "synthetic paginated multi-service incident triage",
        "requested_incidents": count,
        "context_window_configured": context.window_tokens,
        "single_200k_token_context_tested": False,
        "summary_profile": "120-word summaries retaining release code and current incident",
    }
    try:
        async with SandboxClient(os.environ["EVALSI_SANDBOX_ADDR"]) as client:
            summary["pool_probe"] = await client.probe()
            if not any(
                rung["driver"] == "pod" and rung["available"]
                for rung in summary["pool_probe"]
            ):
                raise RuntimeError("the real pod rung is unavailable")
            async with await client.create(
                SandboxSpec(
                    image="python:3.13-slim",
                    min_isolation="namespaced",
                    memory_mb=128,
                    files={
                        "horizon_task.py": Path(__file__)
                        .with_name("horizon_task.py")
                        .read_text()
                    },
                )
            ) as sandbox:
                summary["isolation"] = sandbox.isolation.to_dict()

                async def remote(*command: str) -> dict[str, Any]:
                    result = await sandbox.exec(
                        ["python", "horizon_task.py", *command], timeout_s=30
                    )
                    if not result.ok:
                        raise RuntimeError(
                            f"fixture command failed: {result.stderr[:300]}"
                        )
                    return json.loads(result.stdout)

                first = await remote(
                    "init", "--count", str(count), "--seed", settings["task"]["seed"]
                )

                async def decide(**decision: Any) -> str:
                    page = await remote("submit", "--decision", json.dumps(decision))
                    completed = page.get("decisions", page.get("incident_id", 0))
                    if completed % 10 == 0:
                        print(
                            json.dumps(
                                {
                                    "submitted": completed,
                                    "estimated_usd": round(ledger.charged, 4),
                                }
                            ),
                            flush=True,
                        )
                    return json.dumps(page, sort_keys=True)

                tool = FunctionTool(
                    "submit_incident",
                    decide,
                    description="Submit a decision and verbatim evidence for the displayed incident. Returns the next incident or COMPLETE.",
                    input_schema={
                        "type": "object",
                        "properties": {
                            "incident_id": {"type": "integer"},
                            "action": {"type": "string", "enum": list(ACTIONS)},
                            "evidence": {"type": "string"},
                            "release_code": {"type": "string"},
                        },
                        "required": ["incident_id", "action", "evidence"],
                        "additionalProperties": False,
                    },
                )

                class CompletionGuard:
                    async def on_stop(
                        self, ctx: HookContext, final: FinalEvent
                    ) -> Continue | None:
                        state = json.loads(
                            (await sandbox.read_files(["triage-state.json"]))[
                                "triage-state.json"
                            ]
                        )
                        if len(state["decisions"]) == state["count"]:
                            return None
                        return Continue(
                            f"Task incomplete: {len(state['decisions'])}/{state['count']} submitted. "
                            "Repeated observations are intentional; incident IDs determine order. "
                            "Continue using the runbook. No correct actions are supplied here. "
                            "The current incident is:\n"
                            + json.dumps(current(state), sort_keys=True)
                        )

                hooks = (
                    Hooks([Hook("horizon-completion-v1", CompletionGuard(), {})])
                    if args.completion_guard
                    else None
                )
                loop = AgentLoop(model, [tool], config, emit, hooks=hooks)
                try:
                    final = await loop.run(
                        [{"role": "user", "content": json.dumps(first, sort_keys=True)}]
                    )
                except asyncio.CancelledError:
                    summary["stop_reason"] = "interrupted"
                    raise
                finally:
                    checked = await remote("check")
                    summary.update(
                        checker=checked,
                        model_turns=loop.state.steps,
                        tool_calls=loop.state.tool_calls,
                        context_stats=loop.strategy.stats.to_dict(),
                        seconds=round(time.monotonic() - started, 3),
                    )
                    (args.output / "decisions.json").write_bytes(
                        (await sandbox.read_files(["triage-state.json"]))[
                            "triage-state.json"
                        ]
                    )
                summary.update(
                    stop_reason=final.stop_reason,
                    error=final.error,
                    usage=final.usage.to_dict(),
                )
                summary["passed"] = (
                    checked["passed"]
                    and loop.state.steps >= count
                    and final.stop_reason == "completed"
                )
    finally:
        cassette.save()
        await model.aclose()
        summary["budget"] = ledger.snapshot()
        summary["events"] = turns
        (args.output / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(
        json.dumps(
            {
                key: value
                for key, value in summary.items()
                if key not in ("events", "budget")
            }
        ),
        flush=True,
    )
    return 0 if summary.get("passed") else 1


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--config", type=Path, default=Path(__file__).with_name("long-horizon.yaml")
    )
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--prior-budget", type=Path)
    parser.add_argument(
        "--budget-limit", type=float, choices=[14.5, 19.5, 36.0, 39.5], default=14.5
    )
    parser.add_argument("--model-family", choices=["haiku", "sonnet"], default="haiku")
    parser.add_argument("--completion-guard", action="store_true")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--replay", type=Path)
    mode.add_argument("--resume", type=Path)
    args = parser.parse_args()
    raise SystemExit(asyncio.run(execute(args)))


if __name__ == "__main__":
    main()
