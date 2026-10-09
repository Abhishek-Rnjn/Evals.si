"""Builds the deep-research suite's datasets from the demo agent's corpus.

    python3 build_deep_research.py            # rewrite the files
    python3 build_deep_research.py --check    # fail if they are out of date

``deep-research.jsonl`` is for an agent that has its own search tool (the Deep
Agents service): each record carries the documents the question should draw on
as ``context``, which ``citation-accuracy`` checks the report's [n] against.
``deep-research-docs.jsonl`` is for a command-line agent: the same questions,
with the documents as files in its workdir.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

HERE = Path(__file__).parent
CORPUS = HERE.parent / "deepagents" / "corpus"

QUESTIONS = [
    {
        "id": "rag-evaluation",
        "question": "Research how retrieval-augmented generation is evaluated and write a short report.",
        "docs": ["rag-overview", "rag-evaluation"],
        "reference": "A RAG system is evaluated in two parts: context quality (is the needed information retrieved: context precision and recall) and answer quality (faithfulness to the retrieved passages, and relevance to the question). Scoring them separately shows whether a bad answer came from retrieval or generation.",
    },
    {
        "id": "agent-evaluation",
        "question": "Research how AI agents are evaluated and write a short report.",
        "docs": ["agent-evaluation"],
        "reference": "An agent is evaluated on its end state (a checker grades the final environment, e.g. tests pass) and on its trajectory (tool errors, loops, wasted steps, policy violations). Because agents are stochastic, tasks run several times: pass@k says the agent can solve it, pass^k says it solves it reliably.",
    },
    {
        "id": "rag-overview",
        "question": "Research what retrieval-augmented generation is and why it reduces unsupported claims, and write a short report.",
        "docs": ["rag-overview"],
        "reference": "RAG grounds a model's answer in documents fetched at question time: a retriever selects passages and the generator answers only from them, so every statement can be traced to a source.",
    },
]

DOCS_NOTE = (
    "The documents are the files under docs/. Use only them, and cite each claim as [n] "
    "with a numbered list of the file names under a 'Sources' heading."
)


def doc(name: str) -> str:
    return (CORPUS / f"{name}.md").read_text(encoding="utf-8").strip()


def render() -> dict[str, str]:
    plain, with_files = [], []
    for q in QUESTIONS:
        plain.append(
            {
                "id": q["id"],
                "input": q["question"],
                "reference": q["reference"],
                "context": [doc(d) for d in q["docs"]],
            }
        )
        with_files.append(
            {
                "id": q["id"],
                "input": f"{q['question']} {DOCS_NOTE}",
                "reference": q["reference"],
                "context": [doc(d) for d in q["docs"]],
                "metadata": {"environment": {"files": {f"docs/{d}.md": doc(d) for d in q["docs"]}}},
            }
        )
    dump = lambda rows: "".join(json.dumps(r, ensure_ascii=False) + "\n" for r in rows)  # noqa: E731
    return {"deep-research.jsonl": dump(plain), "deep-research-docs.jsonl": dump(with_files)}


def main() -> int:
    files = render()
    if "--check" in sys.argv:
        stale = [n for n, text in files.items() if not (HERE / n).exists() or (HERE / n).read_text() != text]
        if stale:
            print(f"out of date: {', '.join(stale)}; run build_deep_research.py", file=sys.stderr)
            return 1
        return 0
    for name, text in files.items():
        (HERE / name).write_text(text)
        print(f"wrote {name}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
