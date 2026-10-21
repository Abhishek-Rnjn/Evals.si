"""A stand-in support agent, so the starter suite runs offline and free.

It reads the customer's message on standard input and acts through ``shop.py``
the way a real agent would act through its tools. Replace it with your own
agent: ``target.agent`` accepts any command, A2A, MCP or HTTP endpoint.

    python3 agent.py careful   # follows the refund policy
    python3 agent.py sloppy    # the same agent after a "harmless" prompt change

``sloppy`` is a regression to catch: it skips the ownership, delivery, window and
approval checks, retries a timed-out refund without an idempotency key, refunds
what was asked for even above what was paid, and refunds to store credit.
"""

from __future__ import annotations

import json
import re
import subprocess
import sys

SHOP = [sys.executable, "shop.py"]


def tool(*args: str) -> tuple[int, str]:
    done = subprocess.run([*SHOP, *args], capture_output=True, text=True, check=False)
    return done.returncode, done.stdout.strip() or done.stderr.strip()


def lookup(order: str) -> dict:
    code, out = tool("lookup_order", order)
    return json.loads(out) if code == 0 else {}


def requested_cents(message: str, order: dict) -> int:
    total = order["total_cents"]
    for item in order["items"]:
        if item["sku"] in message.lower() and re.search(r"damaged|only", message.lower()):
            return item["cents"]
    money = re.search(r"\$(\d+(?:\.\d\d)?)", message)
    return round(float(money.group(1)) * 100) if money else total


def careful(customer: str, message: str, order_id: str | None) -> str:
    if re.search(r"\b(person|human|manager)\b", message.lower()):
        tool("handoff", "Customer asked for a person")
        return "I have asked a colleague to take over."
    if not order_id:
        tool("ask_customer", "Which order would you like refunded?")
        return "Which order would you like refunded?"
    order = lookup(order_id)
    rules = order.get("policy", {})
    if not order or order["customer"] != customer:
        return "I can only refund your own orders."
    if order["status"] != "delivered" or order["refunded_cents"] > 0:
        return "That order is not eligible for a refund."
    if order["days_since_delivery"] > rules["window_days"]:
        return "That order is outside the return window."
    if order["total_cents"] > rules["human_approval_over_cents"]:
        tool("handoff", f"Refund of order {order_id} needs approval")
        return "A colleague will approve this refund."
    cents = min(requested_cents(message, order), order["total_cents"])
    key = f"refund-{order_id}-{cents}"
    args = ["refund", order_id, f"{cents / 100:.2f}", "--to", order["payment_method"], "--key", key]
    code, _ = tool(*args)
    while code == 75:  # timed out: the write may have happened, so ask the service
        if lookup(order_id)["refunded_cents"] >= cents:
            break
        code, _ = tool(*args)  # same key: the service refuses to write it twice
    return f"Refunded {cents / 100:.2f} to {order['payment_method']}."


def sloppy(customer: str, message: str, order_id: str | None) -> str:
    if not order_id:
        return "Sorry, I cannot help with that."
    order = lookup(order_id)
    if not order:
        return "I could not find that order."
    cents = requested_cents(message, order)
    destination = (
        "store_credit"
        if order["payment_method"].startswith("gift_card")
        else (order["payment_method"])
    )
    args = ["refund", order_id, f"{cents / 100:.2f}", "--to", destination]
    code, _ = tool(*args)
    for _ in range(2):
        if code != 75:
            break
        code, _ = tool(*args)  # no key: a second refund
    return f"Refunded {cents / 100:.2f}."


def main() -> int:
    profile = sys.argv[1] if len(sys.argv) > 1 else "careful"
    text = sys.stdin.read()
    who = re.search(r"\[customer (C-\d+)\]", text)
    order = re.search(r"\bA-\d{4}\b", text)
    act = {"careful": careful, "sloppy": sloppy}[profile]
    print(act(who.group(1) if who else "", text, order.group(0) if order else None))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
