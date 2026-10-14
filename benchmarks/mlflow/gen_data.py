"""Generate a deterministic QA/RAG dataset for the Evals.si vs MLflow benchmark.

Every record has a question, an expected answer, the answer a pipeline gave, and
three retrieved passages. About 60% of answers match exactly, 10% differ only in
case, spacing or punctuation, 10% contain the expected answer inside a longer
sentence, and 20% are wrong, so the tools' matching rules can be compared.

    python gen_data.py --n 1000 --out data-1000.jsonl
"""

from __future__ import annotations

import argparse
import json
import random

CITIES = ["Paris", "Tokyo", "Canberra", "Ottawa", "Cairo", "Lima", "Oslo", "Nairobi", "Hanoi", "Quito"]


def make_records(n: int, seed: int = 42) -> list[dict]:
    rng = random.Random(seed)
    records = []
    for i in range(n):
        answer = f"{rng.choice(CITIES)} {rng.randint(100, 999)}"
        roll = rng.random()
        if roll < 0.60:
            output = answer
        elif roll < 0.70:
            output = rng.choice([answer.upper(), f"  {answer}  ", f"{answer}.", answer.lower()])
        elif roll < 0.80:
            output = f"The answer is {answer} according to the documents."
        else:
            output = f"{rng.choice(CITIES)} {rng.randint(1000, 1999)}"
        context = [
            f"Background note {rng.randint(1, 9999)} about an unrelated topic.",
            f"The record for item {i} says the answer is {answer}.",
            f"Another passage {rng.randint(1, 9999)} with filler text.",
        ]
        records.append(
            {
                "id": f"q{i}",
                "input": f"What is the answer for item {i}?",
                "output": output,
                "reference": answer,
                "context": context,
            }
        )
    return records


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--n", type=int, required=True)
    parser.add_argument("--out", required=True)
    parser.add_argument("--seed", type=int, default=42)
    args = parser.parse_args()
    with open(args.out, "w") as f:
        for rec in make_records(args.n, args.seed):
            f.write(json.dumps(rec) + "\n")


if __name__ == "__main__":
    main()
