"""High-entropy completion canaries for controlled training-data probes.

The secret suffix belongs in the training corpus and the held-out reference,
never in the model's probe input. Recall is exposure evidence, not proof of a
particular training source or a general absence-of-contamination test.
"""

from __future__ import annotations

import secrets

from evalsi.types import Content, Record


def make_canaries(count: int = 10) -> list[Record]:
    if isinstance(count, bool) or not isinstance(count, int) or not 1 <= count <= 10000:
        raise ValueError("count must be an integer from 1 to 10000")
    return [
        Record(
            id=f"canary-{i}",
            input=Content(text=f"EVALSI_CANARY_{secrets.token_hex(16)}:"),
            reference=Content(text=secrets.token_hex(16)),
            metadata={"evalsi.canary": True},
        )
        for i in range(count)
    ]
