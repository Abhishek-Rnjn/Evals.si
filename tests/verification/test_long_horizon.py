from __future__ import annotations

import asyncio
import json
import sys
from pathlib import Path

import httpx
import pytest
from evalsi.targets import TargetConfig
from evalsi_harness.context import SUMMARY_SYSTEM

ROOT = Path(__file__).parent
sys.path.insert(0, str(ROOT))
from horizon_task import check, current, expected_action, release_code, submit
from long_horizon import BudgetExhausted, ConciseSummaryModel, Ledger, MeteredTransport


@pytest.mark.parametrize(
    ("values", "expected"),
    [
        ([300, 700, 4, 25, 90], "scale_database"),
        ([250, 700, 4, 25, 90], "repair_database_network"),
        ([50, 20, 0, 20, 90], "rollback_api"),
        ([50, 20, 0, 19, 61], "add_consumers"),
        ([50, 20, 0, 19, 60], "observe"),
    ],
)
def test_policy_precedence_and_boundaries(values, expected):
    keys = [
        "database_latency_ms",
        "queue_depth",
        "database_reconnects",
        "api_errors",
        "oldest_job_seconds",
    ]
    assert (
        expected_action("OBSERVATION " + json.dumps(dict(zip(keys, values))))
        == expected
    )


def test_checker_requires_every_incident_evidence_and_retained_memory():
    state = {"count": 3, "seed": "unit", "decisions": []}
    assert not check(state)["passed"]
    for index in range(3):
        page = current(state)
        submit(
            state,
            {
                "incident_id": index,
                "action": expected_action(page["observation"]),
                "evidence": page["observation"],
                "release_code": release_code("unit"),
            },
        )
    assert check(state)["passed"]
    state["decisions"][-1]["release_code"] = "wrong"
    assert not check(state)["passed"]
    state["decisions"][-1]["release_code"] = release_code("unit")
    state["decisions"][0]["evidence"] = "invented"
    assert not check(state)["passed"]


def test_out_of_order_or_duplicate_submission_is_rejected():
    state = {"count": 2, "seed": "unit", "decisions": []}
    with pytest.raises(ValueError, match="currently displayed"):
        submit(state, {"incident_id": 1})
    assert not state["decisions"]
    with pytest.raises(TypeError, match="must be an integer"):
        submit(state, {"incident_id": False})


def test_checker_uses_the_required_95_percent_threshold():
    state = {"count": 20, "seed": "threshold", "decisions": []}
    for index in range(20):
        page = current(state)
        submit(
            state,
            {
                "incident_id": index,
                "action": expected_action(page["observation"]),
                "evidence": page["observation"],
                "release_code": release_code("threshold"),
            },
        )
    for index in range(2):
        correct = state["decisions"][index]["action"]
        state["decisions"][index]["action"] = (
            "observe" if correct != "observe" else "rollback_api"
        )
        assert check(state)["passed"] is (index == 0)


def test_budget_reserves_before_request_and_refuses_overshoot():
    ledger = Ledger(limit=0.0011)
    with pytest.raises(BudgetExhausted):
        ledger.reserve(json.dumps({"max_tokens": 512}).encode())
    assert ledger.attempts == []
    assert ledger.charged == 0.001


def test_usage_settlement_counts_cache_tokens_and_preserves_failed_attempt():
    ledger = Ledger()
    body = json.dumps({"max_tokens": 512}).encode()
    first = ledger.reserve(body)
    failed = ledger.reserve(body)
    ledger.settle(
        first,
        {
            "model": "haiku",
            "usage": {
                "input_tokens": 100,
                "cache_creation_input_tokens": 200,
                "cache_read_input_tokens": 300,
                "output_tokens": 10,
            },
        },
    )
    assert first["charged_usd"] == pytest.approx((600 * 2.2 + 10 * 5.5) / 1e6)
    assert ledger.charged == pytest.approx(
        0.001 + first["charged_usd"] + failed["reserved_usd"]
    )


@pytest.mark.parametrize(
    "usage", [{}, {"input_tokens": -1, "output_tokens": 1}, {"input_tokens": 1}]
)
def test_missing_or_invalid_usage_keeps_full_reservation(usage):
    ledger = Ledger()
    row = ledger.reserve(json.dumps({"max_tokens": 512}).encode())
    before = ledger.charged
    with pytest.raises(ValueError):
        ledger.settle(row, {"usage": usage})
    assert ledger.charged == before


def test_unexpected_over_reservation_usage_is_recorded_before_stopping():
    ledger = Ledger()
    row = ledger.reserve(json.dumps({"max_tokens": 512}).encode())
    with pytest.raises(ValueError, match="exceeds the pre-request estimate"):
        ledger.settle(row, {"usage": {"input_tokens": 100_000, "output_tokens": 1}})
    assert row["charged_usd"] == pytest.approx((100_000 * 2.2 + 5.5) / 1e6)
    assert ledger.charged == pytest.approx(0.001 + row["charged_usd"])


def test_budget_refusal_prevents_http_attempt(tmp_path):
    calls = []

    def unexpected_request(request):
        calls.append(request)
        raise AssertionError("over-budget requests must not reach the transport")

    async def request():
        transport = MeteredTransport(Ledger(limit=0.0011), tmp_path / "budget.json")
        transport.inner = httpx.MockTransport(unexpected_request)
        async with httpx.AsyncClient(transport=transport) as client:
            with pytest.raises(BudgetExhausted):
                await client.post(
                    "https://example.invalid/v1/messages", json={"max_tokens": 512}
                )

    asyncio.run(request())
    assert not calls


def test_failed_http_attempts_each_retain_a_reservation(tmp_path):
    ledger = Ledger()

    async def request():
        transport = MeteredTransport(ledger, tmp_path / "budget.json")
        transport.inner = httpx.MockTransport(
            lambda req: httpx.Response(429, json={"error": "rate_limit"})
        )
        async with httpx.AsyncClient(transport=transport) as client:
            for _ in range(2):
                response = await client.post(
                    "https://example.invalid/v1/messages", json={"max_tokens": 512}
                )
                assert response.status_code == 429

    asyncio.run(request())
    assert len(ledger.attempts) == 2
    assert ledger.charged == pytest.approx(
        0.001 + sum(row["reserved_usd"] for row in ledger.attempts)
    )


def test_restart_carries_failed_reservations_without_resetting_allowance(tmp_path):
    prior = Ledger()
    prior.reserve(json.dumps({"max_tokens": 512}).encode())
    path = tmp_path / "prior.json"
    path.write_text(json.dumps(prior.snapshot()))
    resumed = Ledger.from_prior(path)
    assert resumed.charged == pytest.approx(prior.charged)
    assert resumed.limit == 14.5
    assert resumed.carried_attempts == 1
    assert resumed.attempts == prior.attempts
    invalid = prior.snapshot()
    invalid["conservative_estimated_usd"] = 0
    path.write_text(json.dumps(invalid))
    with pytest.raises(ValueError, match="understates"):
        Ledger.from_prior(path)


def test_concise_summary_profile_does_not_change_agent_instructions():
    bodies = []

    def respond(request):
        bodies.append(json.loads(request.content))
        return httpx.Response(
            200,
            json={
                "content": [{"type": "text", "text": "summary"}],
                "stop_reason": "end_turn",
                "usage": {"input_tokens": 10, "output_tokens": 2},
            },
        )

    async def request():
        client = httpx.AsyncClient(transport=httpx.MockTransport(respond))
        model = ConciseSummaryModel(
            TargetConfig(connector="anthropic", model="haiku", api_key_env="none"),
            client,
        )
        try:
            await model.complete(
                [{"role": "user", "content": "test"}], [], system="agent policy"
            )
            await model.complete(
                [{"role": "user", "content": "test"}], [], system=SUMMARY_SYSTEM
            )
        finally:
            await model.aclose()

    asyncio.run(request())
    assert bodies[0]["system"][0]["text"] == "agent policy"
    assert "120 words" in bodies[1]["system"][0]["text"]


def test_switching_to_sonnet_preserves_haiku_charges_and_rate_provenance(tmp_path):
    prior = Ledger()
    body = json.dumps({"max_tokens": 1024}).encode()
    prior.reserve(body)
    path = tmp_path / "prior.json"
    path.write_text(json.dumps(prior.snapshot()))
    sonnet = Ledger.from_prior(path)
    sonnet.input_rate, sonnet.output_rate = 4.4, 11.0
    row = sonnet.reserve(body)
    sonnet.settle(row, {"usage": {"input_tokens": 100, "output_tokens": 10}})
    assert sonnet.charged == pytest.approx(prior.charged + 0.00055)
    assert sonnet.attempts[0]["input_rate"] == 2.2
    assert sonnet.attempts[1]["input_rate"] == 4.4
    path.write_text(json.dumps(sonnet.snapshot()))
    carried = Ledger.from_prior(path)
    assert carried.charged == pytest.approx(sonnet.charged)
    assert carried.attempts == sonnet.attempts


def test_legacy_haiku_ledger_carries_explicit_historical_rates(tmp_path):
    prior = Ledger()
    prior.reserve(json.dumps({"max_tokens": 512}).encode())
    snapshot = prior.snapshot()
    for row in snapshot["attempts"]:
        row.pop("input_rate")
        row.pop("output_rate")
    path = tmp_path / "prior.json"
    path.write_text(json.dumps(snapshot))
    carried = Ledger.from_prior(path)
    assert carried.charged == pytest.approx(prior.charged)
    assert carried.attempts[0]["input_rate"] == 2.2
    assert carried.attempts[0]["output_rate"] == 5.5


@pytest.mark.parametrize("limit", [19.5, 36.0, 39.5])
def test_explicit_budget_increase_carries_spend_without_resetting(tmp_path, limit):
    prior = Ledger()
    prior.reserve(json.dumps({"max_tokens": 1024}).encode())
    path = tmp_path / "prior.json"
    path.write_text(json.dumps(prior.snapshot()))
    raised = Ledger.from_prior(path, limit=limit)
    assert raised.limit == limit
    assert raised.charged == pytest.approx(prior.charged)
    assert raised.attempts == prior.attempts
    path.write_text(json.dumps(raised.snapshot()))
    with pytest.raises(ValueError, match="nondecreasing"):
        Ledger.from_prior(path)
    with pytest.raises(ValueError, match="supported allowance"):
        Ledger.from_prior(path, limit=99)


def test_followup_budget_increase_retains_uncertain_reservations_and_caps_next_attempt(
    tmp_path,
):
    prior = Ledger(limit=36)
    body = json.dumps({"max_tokens": 1024}).encode()
    prior.reserve(body)
    path = tmp_path / "prior.json"
    path.write_text(json.dumps(prior.snapshot()))
    raised = Ledger.from_prior(path, limit=39.5)
    assert raised.charged == pytest.approx(prior.charged)
    assert raised.attempts == prior.attempts
    assert "input_tokens" not in raised.attempts[0]
    raised.charged = raised.limit - 0.000001
    before = raised.snapshot()
    with pytest.raises(BudgetExhausted):
        raised.reserve(body)
    assert raised.snapshot() == before
