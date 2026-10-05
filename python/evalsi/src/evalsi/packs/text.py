"""The ``text`` pack: reference-based text overlap metrics, pure Python.

Implementations follow the standard definitions (Papineni et al. 2002 for
BLEU, Lin 2004 for ROUGE, Popovic 2015 for chrF, the SQuAD script for token
F1). Scores are 0-1. With several references (a JSON list), the best
reference counts, except corpus BLEU, which uses them all as the standard
definition does.
"""

from __future__ import annotations

import math
import re
import string
from collections import Counter
from collections.abc import Sequence

from evalsi.evaluator import MetricSpec, Requirements, Scope, ScoreType, SkipRecord, evaluator
from evalsi.packs.core import _output_text, _references
from evalsi.registry import Pack
from evalsi.types import Record, Score

NUMBER = ScoreType.NUMBER
REFERENCE = Requirements(reference=True)
_TOKEN = re.compile(r"\w+", re.UNICODE)
_ARTICLES = re.compile(r"\b(a|an|the)\b")
_PUNCT = str.maketrans("", "", string.punctuation)


def tokenize(text: str) -> list[str]:
    return _TOKEN.findall(text.lower())


def _ngrams(tokens: Sequence[str], n: int) -> Counter[tuple[str, ...]]:
    return Counter(tuple(tokens[i : i + n]) for i in range(len(tokens) - n + 1))


def _bleu_stats(
    hyp: list[str], refs: list[list[str]], max_n: int
) -> tuple[list[int], list[int], int, int]:
    """Clipped n-gram matches, n-gram totals, hypothesis length, closest reference length."""
    matches, totals = [], []
    for n in range(1, max_n + 1):
        counts = _ngrams(hyp, n)
        max_ref: Counter[tuple[str, ...]] = Counter()
        for ref in refs:
            max_ref |= _ngrams(ref, n)
        matches.append(sum(min(c, max_ref[g]) for g, c in counts.items()))
        totals.append(max(len(hyp) - n + 1, 0))
    closest = min((abs(len(r) - len(hyp)), len(r)) for r in refs)[1]
    return matches, totals, len(hyp), closest


def _bleu(matches: list[int], totals: list[int], hyp_len: int, ref_len: int, smooth: bool) -> float:
    if hyp_len == 0:
        return 0.0
    log_precision = 0.0
    for i, (matched, total) in enumerate(zip(matches, totals, strict=True)):
        # Lin & Och (2004) add-one smoothing for n > 1.
        bump = 1 if smooth and i > 0 else 0
        if matched + bump == 0 or total + bump == 0:
            return 0.0
        log_precision += math.log((matched + bump) / (total + bump))
    brevity = 1.0 if hyp_len > ref_len else math.exp(1 - ref_len / hyp_len)
    return brevity * math.exp(log_precision / len(matches))


@evaluator(
    name="builtin/bleu",
    version="1.0.0",
    description="Sentence BLEU (up to 4-grams, add-one smoothing for n > 1).",
    requires=REFERENCE,
    outputs=[MetricSpec("bleu", NUMBER, min=0.0, max=1.0, higher_is_better=True)],
)
def bleu(record: Record, *, max_n: int = 4) -> Score:
    assert record.reference is not None
    refs = [tokenize(r) for r in _references(record.reference)]
    stats = _bleu_stats(tokenize(_output_text(record)), refs, max_n)
    return Score(number=_bleu(*stats, smooth=True))


@evaluator(
    name="builtin/corpus-bleu",
    version="1.0.0",
    description="Corpus BLEU over all records (n-gram counts pooled, no smoothing).",
    scope=Scope.DATASET,
    requires=REFERENCE,
    outputs=[MetricSpec("corpus-bleu", NUMBER, min=0.0, max=1.0, higher_is_better=True)],
)
def corpus_bleu(records: list[Record], *, max_n: int = 4) -> Score:
    if not records:
        raise SkipRecord("no records with references")
    matches, totals = [0] * max_n, [0] * max_n
    hyp_len = ref_len = 0
    for record in records:
        assert record.reference is not None
        refs = [tokenize(r) for r in _references(record.reference)]
        m, t, h, r = _bleu_stats(tokenize(_output_text(record)), refs, max_n)
        matches = [a + b for a, b in zip(matches, m, strict=True)]
        totals = [a + b for a, b in zip(totals, t, strict=True)]
        hyp_len, ref_len = hyp_len + h, ref_len + r
    return Score(number=_bleu(matches, totals, hyp_len, ref_len, smooth=False))


def _f1(overlap: int, hyp: int, ref: int) -> float:
    if overlap == 0 or hyp == 0 or ref == 0:
        return 0.0
    p, r = overlap / hyp, overlap / ref
    return 2 * p * r / (p + r)


def _lcs(a: Sequence[str], b: Sequence[str]) -> int:
    prev = [0] * (len(b) + 1)
    for x in a:
        cur = [0]
        for j, y in enumerate(b, start=1):
            cur.append(prev[j - 1] + 1 if x == y else max(prev[j], cur[j - 1]))
        prev = cur
    return prev[-1]


@evaluator(
    name="builtin/rouge",
    version="1.0.0",
    description="ROUGE-1, ROUGE-2 and ROUGE-L F1 against the best reference.",
    requires=REFERENCE,
    outputs=[
        MetricSpec("rouge1", NUMBER, min=0.0, max=1.0, higher_is_better=True),
        MetricSpec("rouge2", NUMBER, min=0.0, max=1.0, higher_is_better=True),
        MetricSpec("rougeL", NUMBER, min=0.0, max=1.0, higher_is_better=True),
    ],
)
def rouge(record: Record) -> list[Score]:
    assert record.reference is not None
    hyp = tokenize(_output_text(record))
    best = {"rouge1": 0.0, "rouge2": 0.0, "rougeL": 0.0}
    for ref_text in _references(record.reference):
        ref = tokenize(ref_text)
        for n in (1, 2):
            h, r = _ngrams(hyp, n), _ngrams(ref, n)
            score = _f1(sum((h & r).values()), sum(h.values()), sum(r.values()))
            best[f"rouge{n}"] = max(best[f"rouge{n}"], score)
        best["rougeL"] = max(best["rougeL"], _f1(_lcs(hyp, ref), len(hyp), len(ref)))
    return [Score(number=v, name=k) for k, v in best.items()]


def _chrf(hyp: str, ref: str, max_n: int, beta: float) -> float:
    hyp, ref = hyp.replace(" ", ""), ref.replace(" ", "")
    precisions, recalls = [], []
    for n in range(1, max_n + 1):
        hyp_grams = Counter(hyp[i : i + n] for i in range(len(hyp) - n + 1))
        ref_grams = Counter(ref[i : i + n] for i in range(len(ref) - n + 1))
        if not hyp_grams or not ref_grams:
            continue
        overlap = sum((hyp_grams & ref_grams).values())
        precisions.append(overlap / sum(hyp_grams.values()))
        recalls.append(overlap / sum(ref_grams.values()))
    if not precisions:
        return 0.0
    p, r = sum(precisions) / len(precisions), sum(recalls) / len(recalls)
    if p == 0 and r == 0:
        return 0.0
    return (1 + beta**2) * p * r / (beta**2 * p + r)


@evaluator(
    name="builtin/chrf",
    version="1.0.0",
    description="chrF: character n-gram F-score (n up to 6, beta 2).",
    requires=REFERENCE,
    outputs=[MetricSpec("chrf", NUMBER, min=0.0, max=1.0, higher_is_better=True)],
)
def chrf(record: Record, *, max_n: int = 6, beta: float = 2.0) -> Score:
    assert record.reference is not None
    out = _output_text(record)
    return Score(number=max(_chrf(out, r, max_n, beta) for r in _references(record.reference)))


def _squad_normalize(text: str) -> list[str]:
    text = text.lower().translate(_PUNCT)
    return _ARTICLES.sub(" ", text).split()


@evaluator(
    name="builtin/token-f1",
    version="1.0.0",
    description="SQuAD-style token F1 (lowercased, punctuation and articles removed).",
    requires=REFERENCE,
    outputs=[MetricSpec("token-f1", NUMBER, min=0.0, max=1.0, higher_is_better=True)],
)
def token_f1(record: Record) -> Score:
    assert record.reference is not None
    hyp = Counter(_squad_normalize(_output_text(record)))
    best = 0.0
    for ref_text in _references(record.reference):
        ref = Counter(_squad_normalize(ref_text))
        best = max(best, _f1(sum((hyp & ref).values()), sum(hyp.values()), sum(ref.values())))
    return Score(number=best)


PACK = Pack(
    name="text",
    description="Reference overlap: BLEU, corpus BLEU, ROUGE-1/2/L, chrF and token F1.",
    evaluators=[bleu, corpus_bleu, rouge, chrf, token_f1],
)
