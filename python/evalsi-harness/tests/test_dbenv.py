"""A database-backed tool environment (H4, decision 0032)."""

from __future__ import annotations

from collections.abc import Callable
from typing import Any

import pytest

from evalsi_harness.task import TaskError
from evalsi_harness.testing import LocalSandboxClient, ScriptedModelServer
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import calls_then, target

Model = Callable[..., ScriptedModelServer]

ID = {"type": "object", "properties": {"id": {"type": "string"}}, "required": ["id"]}
MOVE = {
    "type": "object",
    "properties": {
        "id": {"type": "string"},
        "amount": {"type": "integer", "minimum": 1, "maximum": 1000},
    },
    "required": ["id", "amount"],
    "additionalProperties": False,
}


def database(**override: Any) -> dict[str, Any]:
    db: dict[str, Any] = {
        "schema": "CREATE TABLE accounts (id TEXT PRIMARY KEY, balance INTEGER NOT NULL "
        "CHECK (balance >= 0));",
        "seed": "INSERT INTO accounts VALUES ('a', 100), ('b', 0);",
        "tools": [
            {
                "name": "get_balance",
                "description": "Balance of one account.",
                "sql": "SELECT balance FROM accounts WHERE id = :id",
                "parameters": ID,
            },
            {
                "name": "withdraw",
                "mode": "write",
                "sql": "UPDATE accounts SET balance = balance - :amount WHERE id = :id",
                "parameters": MOVE,
                "min_rows": 1,
            },
            {
                "name": "deposit",
                "mode": "write",
                "sql": "UPDATE accounts SET balance = balance + :amount WHERE id = :id",
                "parameters": MOVE,
                "min_rows": 1,
            },
        ],
        "assertions": [
            {
                "name": "balances",
                "sql": "SELECT balance FROM accounts ORDER BY id",
                "expect": [[70], [30]],
            },
            {"name": "total", "sql": "SELECT SUM(balance) FROM accounts", "expect": 100},
        ],
    }
    db.update(override)
    return db


def spec_with(server: ScriptedModelServer, db: dict[str, Any]) -> Any:
    return spec_of(
        {
            "target": target(server),
            "harness": {"builtin": {}},
            "environment": {"database": db},
        }
    )


def move_script(amount: int = 30) -> Any:
    return calls_then(
        [
            {"tool_calls": [("get_balance", {"id": "a"})]},
            {"tool_calls": [("withdraw", {"id": "a", "amount": amount})]},
            {"tool_calls": [("deposit", {"id": "b", "amount": amount})]},
            {"text": "done"},
        ]
    )


def test_the_agent_moves_money_and_the_assertions_hold(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    server = model(move_script())
    out = run(spec_with(server, database()), record("Move 30 from a to b"), sandboxes)
    assert out.error == ""
    rec = out.record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed
    assert rec.check.tests == {"balances": "passed", "total": "passed"}
    assert sandboxes.created == []  # a database-only task needs no sandbox
    assert "get_balance" in [t["function"]["name"] for t in server.requests[0]["tools"]]
    # The read's result reached the model as rows.
    assert '"balance": 100' in str(server.requests[1])


def test_every_write_is_recorded_on_its_step_and_in_the_metadata(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    rec = run(spec_with(model(move_script()), database()), record("x"), sandboxes).record
    assert rec is not None
    summary = rec.metadata["database"]
    assert [w["tool"] for w in summary["writes"]] == ["withdraw", "deposit"]
    assert summary["writes"][0]["arguments"] == {"id": "a", "amount": 30}
    assert summary["writes"][0]["rows_changed"] == 1
    assert summary["initial_digest"] != summary["final_digest"]
    steps = [s for s in rec.trajectory.steps if s.attributes.get("evalsi.tool.db_write")]  # type: ignore[union-attr]
    assert [s.attributes["evalsi.tool.db_write"] for s in steps] == [1, 2]


def test_a_wrong_end_state_fails_with_the_difference(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    script = calls_then([{"tool_calls": [("withdraw", {"id": "a", "amount": 30})]}, {"text": "x"}])
    rec = run(spec_with(model(script), database()), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert not rec.check.passed
    assert rec.check.tests["balances"] == "failed"
    assert "[[70], [0]]" in rec.check.details
    assert rec.check.score == 0.0


def test_rejected_calls_change_nothing_and_are_still_recorded(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    script = calls_then(
        [
            {"tool_calls": [("withdraw", {"id": "a", "amount": 0})]},  # below minimum
            {"tool_calls": [("withdraw", {"id": "zzz", "amount": 5})]},  # matches no row
            {"tool_calls": [("withdraw", {"id": "b", "amount": 5})]},  # CHECK constraint
            {"text": "done"},
        ]
    )
    server = model(script)
    rec = run(spec_with(server, database()), record("x"), sandboxes).record
    assert rec is not None
    summary = rec.metadata["database"]
    # All three attempts are recorded, none applied.
    assert [(w["tool"], w["applied"]) for w in summary["writes"]] == [("withdraw", False)] * 3
    assert "must be at least 1" in summary["writes"][0]["error"]
    assert "must change at least 1" in summary["writes"][1]["error"]
    assert summary["initial_digest"] == summary["final_digest"]
    sent = str(server.requests[-1])
    assert "must be at least 1" in sent
    assert "CHECK constraint" in sent
    assert rec.check is not None
    assert not rec.check.passed


def test_unknown_parameters_are_refused_when_the_schema_says_so(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    script = calls_then(
        [{"tool_calls": [("withdraw", {"id": "a", "amount": 5, "note": "x"})]}, {"text": "x"}]
    )
    server = model(script)
    run(spec_with(server, database()), record("x"), sandboxes)
    assert "not a known parameter" in str(server.requests[-1])


def test_ddl_is_never_allowed_from_a_write_tool(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    tools = [
        {
            "name": "drop",
            "mode": "write",
            "sql": "DELETE FROM accounts WHERE id = :id",
            "parameters": ID,
        }
    ]
    db = database(
        tools=tools, assertions=[{"name": "n", "sql": "SELECT COUNT(*) FROM accounts", "expect": 1}]
    )
    script = calls_then([{"tool_calls": [("drop", {"id": "a"})]}, {"text": "x"}])
    rec = run(spec_with(model(script), db), record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed


@pytest.mark.parametrize(
    ("override", "message"),
    [
        ({"schema": "CREATE TABL x"}, "schema or seed"),
        ({"tools": [{"name": "a", "sql": "SELECT * FROM nope"}]}, "tool a"),
        ({"tools": [{"name": "a", "sql": "DELETE FROM accounts"}]}, "not authorized"),
        (
            {"tools": [{"name": "a", "mode": "write", "sql": "DROP TABLE accounts"}]},
            "not authorized",
        ),
        (
            {"tools": [{"name": "a", "sql": "SELECT 1"}, {"name": "a", "sql": "SELECT 2"}]},
            "duplicate database tool",
        ),
        ({"tools": [{"name": "a", "sql": "SELECT 1", "mode": "admin"}]}, "mode must be"),
        (
            {"assertions": [{"name": "x", "sql": "SELECT * FROM missing", "expect": 1}]},
            "assertion x",
        ),
    ],
)
def test_a_broken_declaration_is_refused_when_the_task_is_read(
    sandboxes: LocalSandboxClient, override: dict[str, Any], message: str
) -> None:
    spec = spec_of({"environment": {"database": database(**override)}})
    with pytest.raises(TaskError, match=message):
        run(spec, record("x"), sandboxes)


def test_the_database_can_sit_beside_a_sandbox_checker(
    model: Model, sandboxes: LocalSandboxClient
) -> None:
    spec = spec_of(
        {
            "target": target(model(move_script())),
            "harness": {"builtin": {}},
            "environment": {"database": database(), "checker": {"command": ["true"]}},
        }
    )
    rec = run(spec, record("x"), sandboxes).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed
    assert len(sandboxes.created) == 1


def test_a_runaway_query_is_stopped(model: Model, sandboxes: LocalSandboxClient) -> None:
    loop = "WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM n) SELECT COUNT(*) FROM n"
    tools = [{"name": "spin", "sql": loop, "parameters": {"type": "object", "properties": {}}}]
    db = database(tools=tools, assertions=[])
    server = model(calls_then([{"tool_calls": [("spin", {})]}, {"text": "x"}]))
    run(spec_with(server, db), record("x"), sandboxes)
    assert "interrupted" in str(server.requests[-1])
