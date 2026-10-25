#!/usr/bin/env python3
"""Print what an agent did in a task, from `evalsi run --output` results.

python3 show.py results.json            # every task: stop reason, checker, scores
python3 show.py results.json calc       # one task, step by step
python3 show.py results.json calc 2     # its third trial
"""

from __future__ import annotations

import json
import sys
from typing import Any


def clip(text: Any, n: int = 100) -> str:
    s = " ".join(str(text or "").split())
    return s if len(s) <= n else s[: n - 1] + "…"


def verdict(check: dict[str, Any] | None) -> str:
    if not check or "passed" not in check:
        return "none"
    return "pass" if check["passed"] else "fail"


def scores(results: list[dict[str, Any]], record: str, trial: int) -> list[str]:
    out = []
    for r in results:
        if r["record_id"] != record or int(r.get("trial") or 0) != trial:
            continue
        if r["outcome"] != "scored":
            out.append(
                f"{r['evaluator']}: {r['outcome']} ({clip(r.get('reason') or r.get('error'), 60)})"
            )
            continue
        for s in r["scores"]:
            value = s.get("passed", s.get("number"))
            why = f"  ({clip(s['explanation'], 60)})" if s.get("explanation") else ""
            out.append(f"{s['name']}: {value}{why}")
    return out


def main() -> None:
    with open(sys.argv[1]) as f:
        data = json.load(f)
    want = sys.argv[2] if len(sys.argv) > 2 else None
    want_trial = int(sys.argv[3]) if len(sys.argv) > 3 else 0
    for rec in data["records"]:
        trial = int(((rec.get("provenance") or {}).get("run") or {}).get("trial") or 0)
        if want is None:
            agent = (rec.get("metadata") or {}).get("agent") or {}
            check = verdict(rec.get("check"))
            print(
                f"{rec['id']} trial {trial}: stop={agent.get('stop_reason')} "
                f"steps={agent.get('steps')} checker={check}"
            )
            print(f"  answer: {clip((rec.get('output') or {}).get('text'), 90)}")
            continue
        if rec["id"] != want or trial != want_trial:
            continue
        print(
            f"task {rec['id']}, trial {trial}: {clip((rec.get('input') or {}).get('text'), 120)}\n"
        )
        for n, step in enumerate(rec["trajectory"]["steps"], 1):
            if step["type"] == "llm":
                for m in step["output"]["messages"]:
                    calls = ", ".join(
                        f"{c['name']}({clip(c['arguments'], 60)})"
                        for c in m.get("tool_calls") or []
                    )
                    print(f"{n:>2} model  {clip(m.get('content'), 70)!r} {calls}")
            else:
                result = (step.get("output") or {}).get("text") or step.get("error") or ""
                print(f"{n:>2} {step['type']:<6} {step.get('name')}: {clip(result, 90)}")
        check = rec.get("check") or {}
        print(f"\nchecker: {verdict(check)} {clip(check.get('details'), 100)}")
        print("scores:")
        for line in scores(data["results"], rec["id"], trial):
            print(f"  {line}")
        return


if __name__ == "__main__":
    main()
