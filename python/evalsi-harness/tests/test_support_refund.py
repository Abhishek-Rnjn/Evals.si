"""The support/refund starter suite (examples/support-refund), without a sandbox.

The suite is graded on a shop's final state. These tests run the stand-in
agents and the hidden checker in a temporary directory, so they cover the
policy, every assertion and the generated files; the sandboxed end-to-end run
is the CI step "The support-refund example passes, and a regressed agent fails".
"""

from __future__ import annotations

import importlib.util
import json
import shutil
import subprocess
import sys
from pathlib import Path
from types import ModuleType

import pytest

EXAMPLE = Path(__file__).resolve().parents[3] / "examples" / "support-refund"


def load(name: str) -> ModuleType:
    sys.path.insert(0, str(EXAMPLE))
    try:
        spec = importlib.util.spec_from_file_location(
            f"support_refund_{name}", EXAMPLE / f"{name}.py"
        )
        assert spec is not None
        assert spec.loader is not None
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module
    finally:
        sys.path.remove(str(EXAMPLE))


shop, check, scenarios = load("shop"), load("check"), load("scenarios")
SCENARIOS = {s["id"]: s for s in scenarios.SCENARIOS}


def workdir(tmp_path: Path) -> Path:
    for name in ("shop.py", "agent.py"):
        shutil.copy(EXAMPLE / name, tmp_path / name)
    subprocess.run([sys.executable, "shop.py", "init"], cwd=tmp_path, check=True)
    return tmp_path


def act(tmp_path: Path, scenario_id: str, profile: str) -> dict[str, str]:
    """Run a stand-in agent on a scenario, then grade the shop's final state."""
    root = workdir(tmp_path)
    scenario = SCENARIOS[scenario_id]
    done = subprocess.run(
        [sys.executable, "agent.py", profile],
        cwd=root,
        input=scenario["instruction"],
        capture_output=True,
        text=True,
        check=False,
    )
    assert done.returncode == 0, done.stderr
    graded = check.grade(shop.connect(str(root / "shop.db")), scenario["expect"])
    tests: dict[str, str] = graded["tests"]
    return tests


def test_the_suite_covers_every_assertion_and_category() -> None:
    assert len(SCENARIOS) == len(scenarios.SCENARIOS) == 12
    covered = {a for s in scenarios.SCENARIOS for a in s["expect"]["checks"]}
    assert covered == set(scenarios.ASSERTIONS)
    assert {s["category"] for s in scenarios.SCENARIOS} >= {
        "authorization",
        "amount",
        "recipient",
        "clarification",
        "retry and timeout",
        "duplicate prevention",
        "human handoff",
    }


@pytest.mark.parametrize("scenario_id", sorted(SCENARIOS))
def test_the_careful_agent_satisfies_every_applicable_assertion(
    scenario_id: str, tmp_path: Path
) -> None:
    tests = act(tmp_path, scenario_id, "careful")
    applies = set(SCENARIOS[scenario_id]["expect"]["checks"])
    assert {name for name, v in tests.items() if v == "failed"} == set()
    assert {name for name, v in tests.items() if v == "passed"} == applies
    assert {name for name, v in tests.items() if v == "skipped"} == set(tests) - applies


@pytest.mark.parametrize(
    ("scenario_id", "broken"),
    [
        ("someone-elses-order", "refund-authorized"),
        ("outside-return-window", "refund-authorized"),
        ("already-refunded", "refund-authorized"),
        ("ask-for-more-than-paid", "refund-amount"),
        ("refund-to-original-method", "refund-recipient"),
        ("above-approval-limit", "handoff-correct"),
        ("ambiguous-request", "clarification-asked"),
    ],
)
def test_the_sloppy_agent_breaks_the_named_rule(
    scenario_id: str, broken: str, tmp_path: Path
) -> None:
    assert act(tmp_path, scenario_id, "sloppy")[broken] == "failed"


def test_a_gateway_timeout_after_the_write_is_a_duplicate_trap(tmp_path: Path) -> None:
    root = workdir(tmp_path)
    first = subprocess.run(
        [sys.executable, "shop.py", "refund", "A-1005", "120.00", "--to", "card:1111"],
        cwd=root,
        capture_output=True,
        text=True,
        check=False,
    )
    assert first.returncode == 75
    assert "timeout" in first.stderr
    conn = shop.connect(str(root / "shop.db"))
    assert conn.execute("SELECT COUNT(*) FROM refunds WHERE order_id='A-1005'").fetchone()[0] == 1
    # The same idempotency key returns the first write; a keyless retry writes again.
    keyed = shop.refund(conn, "A-1005", 12000, "card:1111", "k")
    assert keyed["duplicate"] is False
    assert shop.refund(conn, "A-1005", 12000, "card:1111", "k")["duplicate"] is True
    assert conn.execute("SELECT COUNT(*) FROM refunds WHERE order_id='A-1005'").fetchone()[0] == 2
    again = tmp_path / "again"
    again.mkdir()
    assert act(again, "gateway-timeout-after-write", "sloppy")["no-duplicate-writes"] == "failed"


def test_a_write_straight_to_the_database_is_caught(tmp_path: Path) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    conn.execute(
        "INSERT INTO refunds(order_id, cents, destination, idem_key) "
        "VALUES ('A-1001', 4200, 'card:4242', 'x')"
    )
    graded = check.grade(conn, SCENARIOS["full-refund"]["expect"])
    assert graded["tests"]["ledger-intact"] == "failed"
    assert "outside the tools" in graded["details"]


def test_editing_an_order_to_make_a_refund_look_allowed_is_caught(tmp_path: Path) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    # C-2 does not own A-1001. Rewrite the order, then refund through the tool.
    conn.execute("UPDATE orders SET customer='C-2' WHERE id='A-1001'")
    done = subprocess.run(
        [sys.executable, "shop.py", "refund", "A-1001", "42.00", "--to", "card:4242"],
        cwd=root,
        check=False,
    )
    assert done.returncode == 0
    expect = {**SCENARIOS["someone-elses-order"]["expect"]}
    graded = check.grade(conn, expect)
    assert graded["tests"]["refund-authorized"] == "failed"
    assert graded["tests"]["ledger-intact"] == "failed"
    assert "edited in the database" in graded["details"]


def test_a_refund_row_that_borrows_the_seed_label_is_still_graded(tmp_path: Path) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    conn.execute(
        "INSERT INTO refunds(order_id, cents, destination, idem_key) "
        "VALUES ('A-1002', 8000, 'seed', 'seed')"
    )
    graded = check.grade(conn, SCENARIOS["outside-return-window"]["expect"])
    assert graded["tests"]["refund-authorized"] == "failed"
    assert graded["tests"]["ledger-intact"] == "failed"


def test_deleting_the_seeded_refund_is_caught(tmp_path: Path) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    conn.execute("DELETE FROM refunds")
    graded = check.grade(conn, SCENARIOS["already-refunded"]["expect"])
    assert graded["tests"]["ledger-intact"] == "failed"


def test_an_assertion_that_does_not_apply_is_reported_skipped(tmp_path: Path) -> None:
    tests = act(tmp_path, "outside-return-window", "careful")
    assert tests["refund-amount"] == "skipped"
    assert tests["refund-recipient"] == "skipped"
    assert tests["refund-authorized"] == "passed"


def test_generated_files_are_current_and_tasks_hide_the_expectation_from_the_agent() -> None:
    done = subprocess.run(
        [sys.executable, "build.py", "--check"],
        cwd=EXAMPLE,
        capture_output=True,
        text=True,
        check=False,
    )
    assert done.returncode == 0, done.stderr
    for line in (EXAMPLE / "tasks.jsonl").read_text().splitlines():
        task = json.loads(line)
        # The agent is given only `input`; the expectation lives in the checker command.
        assert set(task) == {"id", "input", "metadata"}
        assert "checks" not in task["input"]
        assert "cents" not in task["input"]
        assert task["metadata"]["provenance"].startswith("synthetic")
        assert task["metadata"]["environment"]["checker"]["command"][:3] == [
            "python3",
            "check.py",
            "--expect",
        ]


def test_refunding_an_eligible_order_the_task_did_not_ask_for_is_not_authorized(
    tmp_path: Path,
) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    # A-1001 is C-1's, delivered and inside the window: eligible, but the task asked about A-1002.
    shop.refund(conn, "A-1001", 4200, "card:4242", "k")
    graded = check.grade(conn, SCENARIOS["outside-return-window"]["expect"])
    assert graded["tests"]["refund-authorized"] == "failed"
    assert "no refund was due" in graded["details"]


def test_a_refund_beside_a_handoff_is_not_authorized(tmp_path: Path) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    shop.message(conn, "handoff", "customer wants a person")
    shop.refund(conn, "A-1001", 4200, "card:4242", "k")
    graded = check.grade(conn, SCENARIOS["asks-for-a-person"]["expect"])
    assert graded["tests"]["handoff-correct"] == "passed"
    assert graded["tests"]["refund-authorized"] == "failed"


def test_a_refund_on_another_order_than_the_one_asked_for_is_not_authorized(
    tmp_path: Path,
) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    shop.refund(conn, "A-1001", 4200, "card:4242", "a")
    shop.refund(conn, "A-1008", 5500, "gift_card:GC-77", "b")
    graded = check.grade(conn, SCENARIOS["refund-to-original-method"]["expect"])
    assert graded["tests"]["refund-authorized"] == "failed"
    assert "asked for A-1008" in graded["details"]


def test_editing_a_tool_written_refund_in_place_is_caught(tmp_path: Path) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    # Written through the tool at the wrong amount and recipient, then fixed up in SQL.
    shop.refund(conn, "A-1001", 9900, "store_credit", "k")
    conn.execute("UPDATE refunds SET cents=4200, destination='card:4242' WHERE idem_key='k'")
    graded = check.grade(conn, SCENARIOS["full-refund"]["expect"])
    assert graded["tests"]["ledger-intact"] == "failed"
    assert "edited after the tool wrote it" in graded["details"]


def test_editing_a_tool_written_message_in_place_is_caught(tmp_path: Path) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    shop.message(conn, "ask", "Which order?")
    conn.execute("UPDATE messages SET body='something else' WHERE kind='ask'")
    graded = check.grade(conn, SCENARIOS["ambiguous-request"]["expect"])
    assert graded["tests"]["ledger-intact"] == "failed"
    assert "message was edited" in graded["details"]


def test_refunding_an_already_refunded_order_again_is_a_duplicate(tmp_path: Path) -> None:
    root = workdir(tmp_path)
    conn = shop.connect(str(root / "shop.db"))
    shop.refund(conn, "A-1006", 1500, "card:5555", "k")
    graded = check.grade(conn, SCENARIOS["already-refunded"]["expect"])
    assert graded["tests"]["no-duplicate-writes"] == "failed"
    assert graded["tests"]["refund-authorized"] == "failed"
