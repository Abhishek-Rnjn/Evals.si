"""A custom evaluator: does the answer cite its sources correctly?

For a RAG answer such as "Paris is the capital [1]. It lies on the Seine [2].",
the evaluator checks that every ``[n]`` marker points at a retrieved context
passage (1 <= n <= len(context)) and that there are at least ``min_citations``
of them. It is deterministic, needs no judge, and costs nothing to run.
"""

from __future__ import annotations

import re

from evalsi import (
    MetricSpec,
    Pack,
    Record,
    Requirements,
    Score,
    ScoreType,
    SkipRecord,
    evaluator,
)

_MARKER = re.compile(r"\[(\d+)\]")


@evaluator(
    name="acme/valid-citations",
    version="1.0.0",
    description="Every [n] citation in the output points at a retrieved passage.",
    requires=Requirements(output=True, context=True),
    outputs=[MetricSpec("valid-citations", ScoreType.PASSED, higher_is_better=True)],
)
def valid_citations(record: Record, *, min_citations: int = 1) -> Score:
    # Requirements already skip records with no output or no context. Raise
    # SkipRecord for anything else the evaluator cannot judge.
    assert record.output is not None
    text = record.output.as_text()
    if not text.strip():
        raise SkipRecord("empty output")

    cited = [int(n) for n in _MARKER.findall(text)]
    available = len(record.context)
    dangling = sorted({n for n in cited if not 1 <= n <= available})

    if dangling:
        return Score(
            passed=False,
            explanation=f"cites {dangling}, but only {available} passage(s) were retrieved",
        )
    if len(cited) < min_citations:
        return Score(
            passed=False,
            explanation=f"{len(cited)} citation(s), expected at least {min_citations}",
        )
    return Score(passed=True, explanation=f"{len(cited)} valid citation(s)")


# A pack makes the evaluator available by name (`--evaluators acme/valid-citations`,
# servers, online policies) once this package is installed; see pyproject.toml.
PACK = Pack(name="acme", description="Acme checks.", evaluators=[valid_citations])
