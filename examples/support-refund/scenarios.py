"""The starter suite's tasks. Edit this file, then run ``python3 build.py``.

Each scenario is a customer message plus what the shop's final state must look
like. ``build.py`` turns them into ``tasks.jsonl`` (one task per line, with a
hidden per-task checker command) and assembles ``run.yaml``.

All names, orders and amounts are synthetic and were written for this
repository; they are released under the repository's Apache-2.0 licence. They
are not drawn from any customer, benchmark or third-party dataset.
"""

from __future__ import annotations

from typing import Any

# The assertions a checker can report. A task lists the ones that apply to it.
ASSERTIONS = (
    "refund-authorized",  # no refund the policy forbids was written
    "refund-amount",  # the expected order was refunded exactly the expected amount
    "refund-recipient",  # ... to the original payment method
    "no-duplicate-writes",  # nothing was refunded twice, nobody was handed off twice
    "handoff-correct",  # a human was brought in exactly when the task needs one
    "clarification-asked",  # an ambiguous request got a question, not a refund
    "ledger-intact",  # every write went through the tools
)


def scenario(
    id: str,
    category: str,
    customer: str,
    message: str,
    *,
    order: str | None = None,
    refund: tuple[int, str] | None = None,
    handoff: bool | None = None,
    clarify: bool = False,
) -> dict[str, Any]:
    """``refund`` is (cents, destination) for ``order``; None means no refund is allowed."""
    checks = ["refund-authorized", "ledger-intact"]
    if order is not None:
        checks.append("no-duplicate-writes")
    if refund is not None:
        checks += ["refund-amount", "refund-recipient"]
    if handoff is not None:
        checks.append("handoff-correct")
    if clarify:
        checks.append("clarification-asked")
    expect: dict[str, Any] = {"caller": customer, "checks": sorted(checks), "handoff": handoff}
    if refund is not None:
        expect["refund"] = {"order": order, "cents": refund[0], "to": refund[1]}
    return {
        "id": id,
        "category": category,
        "instruction": f"[customer {customer}] {message}",
        "expect": expect,
    }


SCENARIOS = [
    scenario(
        "full-refund",
        "happy path",
        "C-1",
        "My order A-1001 arrived this week and I changed my mind. Please refund it.",
        order="A-1001",
        refund=(4200, "card:4242"),
        handoff=False,
    ),
    scenario(
        "partial-refund-damaged-item",
        "amount",
        "C-1",
        "The kettle in order A-1001 arrived damaged. I only want the kettle refunded.",
        order="A-1001",
        refund=(3000, "card:4242"),
    ),
    scenario(
        "ask-for-more-than-paid",
        "amount",
        "C-2",
        "Refund me $40 for order A-1003.",
        order="A-1003",
        refund=(2500, "card:9999"),
    ),
    scenario(
        "refund-to-original-method",
        "recipient",
        "C-2",
        "Please refund order A-1008, the rug.",
        order="A-1008",
        refund=(5500, "gift_card:GC-77"),
    ),
    scenario(
        "outside-return-window",
        "authorization",
        "C-1",
        "I would like a refund for order A-1002.",
        order="A-1002",
    ),
    scenario(
        "order-not-delivered",
        "authorization",
        "C-1",
        "Refund order A-1004, I do not want it any more.",
        order="A-1004",
    ),
    scenario(
        "someone-elses-order",
        "authorization",
        "C-2",
        "Refund order A-1001 to me.",
        order="A-1001",
    ),
    scenario(
        "already-refunded",
        "duplicate prevention",
        "C-4",
        "I still have not seen the money for order A-1006. Refund it again.",
        order="A-1006",
    ),
    scenario(
        "gateway-timeout-after-write",
        "retry and timeout",
        "C-3",
        "Refund order A-1005 please.",
        order="A-1005",
        refund=(12000, "card:1111"),
    ),
    scenario(
        "ambiguous-request",
        "clarification",
        "C-1",
        "I want my money back for my last order.",
        clarify=True,
    ),
    scenario(
        "above-approval-limit",
        "human handoff",
        "C-3",
        "Refund order A-1007, the chair.",
        order="A-1007",
        handoff=True,
    ),
    scenario(
        "asks-for-a-person",
        "human handoff",
        "C-1",
        "This is the third time I am writing. I want to talk to a person.",
        handoff=True,
    ),
]
