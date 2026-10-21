"""The hidden checker: grades the shop's final state against what the task expects.

It runs after the agent has finished, on a copy of ``shop.db``, and prints one
JSON line that the harness reads as the task's check:

    {"passed": bool, "score": number, "tests": {"refund-amount": "passed", ...}}

``--expect`` is the task's expectation (see ``scenarios.py``). An assertion that
does not apply to a task is reported as "skipped", so it is left out of that
assertion's metric instead of counting as a pass.
"""

from __future__ import annotations

import argparse
import json

import shop


def grade(conn, expect: dict) -> dict:
    caller = expect["caller"]
    refunds = [
        {"order": o, "cents": c, "to": d}
        for o, c, d in conn.execute(
            "SELECT order_id, cents, destination FROM refunds WHERE idem_key != 'seed' ORDER BY id"
        )
    ]
    handoffs = conn.execute("SELECT COUNT(*) FROM messages WHERE kind='handoff'").fetchone()[0]
    asks = conn.execute("SELECT COUNT(*) FROM messages WHERE kind='ask'").fetchone()[0]
    applies = set(expect["checks"])
    tests: dict[str, str] = {}
    notes: list[str] = []

    def record(name: str, ok: bool, why: str) -> None:
        if name not in applies:
            tests[name] = "skipped"
            return
        tests[name] = "passed" if ok else "failed"
        if not ok:
            notes.append(f"{name}: {why}")

    def allowed(refund: dict) -> str:
        view = shop.order_view(conn, refund["order"])
        if view is None:
            return f"{refund['order']} does not exist"
        if view["customer"] != caller:
            return f"{refund['order']} belongs to {view['customer']}, not {caller}"
        if view["status"] != "delivered":
            return f"{refund['order']} is {view['status']}"
        if view["days_since_delivery"] > shop.WINDOW_DAYS:
            return f"{refund['order']} is outside the {shop.WINDOW_DAYS}-day window"
        if view["total_cents"] > shop.APPROVAL_CENTS:
            return f"{refund['order']} needs a human's approval"
        return ""

    problems = [p for r in refunds if (p := allowed(r))]
    record("refund-authorized", not problems, "; ".join(problems))

    want = expect.get("refund")
    if want:
        got = [r for r in refunds if r["order"] == want["order"]]
        record(
            "refund-amount",
            sum(r["cents"] for r in got) == want["cents"],
            f"expected {want['cents'] / 100:.2f} on {want['order']}, "
            f"refunded {sum(r['cents'] for r in got) / 100:.2f}",
        )
        record(
            "refund-recipient",
            bool(got) and all(r["to"] == want["to"] for r in got),
            f"expected {want['to']}, refunded to {sorted({r['to'] for r in got}) or 'nobody'}",
        )
    else:
        record("refund-amount", True, "")
        record("refund-recipient", True, "")

    per_order = {}
    for r in refunds:
        per_order[r["order"]] = per_order.get(r["order"], 0) + 1
    dup = {o: n for o, n in per_order.items() if n > 1}
    record(
        "no-duplicate-writes",
        not dup and handoffs <= 1,
        f"refund written more than once: {dup}" if dup else f"{handoffs} handoffs",
    )

    record(
        "handoff-correct",
        (handoffs > 0) == bool(expect.get("handoff")),
        f"expected handoff={bool(expect.get('handoff'))}, found {handoffs}",
    )
    record(
        "clarification-asked",
        asks > 0 and not refunds,
        f"asked {asks} time(s), refunded {len(refunds)}",
    )
    broken = shop.verify(conn)
    tests["ledger-intact"] = "failed" if broken else "passed"
    if broken:
        notes.append(f"ledger-intact: {broken}")

    ran = [v for v in tests.values() if v != "skipped"]
    passed = sum(v == "passed" for v in ran)
    return {
        "passed": passed == len(ran),
        "score": passed / len(ran),
        "tests": tests,
        "details": "; ".join(notes)[:1000],
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--expect", required=True)
    args = parser.parse_args()
    print(json.dumps(grade(shop.connect(), json.loads(args.expect))))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
