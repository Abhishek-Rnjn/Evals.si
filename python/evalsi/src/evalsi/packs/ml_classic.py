"""The ``ml-classic`` pack: metrics for classic models, without numpy.

Predictions come from the record's output and the truth from its reference:

- **Labels**: the output text (or ``label_field`` of a JSON output), against
  the reference text. Labels are compared as strings after trimming.
- **Scores** (for ROC-AUC, PR-AUC, log-loss and calibration):
  ``metadata[score_field]`` (or the same key of a JSON output), either the
  positive class's probability (binary) or a ``{label: probability}`` map.
- **Numbers** (regression): the output and reference parsed as numbers.
- **Rankings**: the output as a JSON list of item ids (or one id per line),
  against the reference as a list of relevant ids or ``{id: gain}``.

Classification and regression metrics are over the dataset (macro scores,
AUCs and R² are not means of per-record values); ranking metrics are per
record, so they get confidence intervals.
"""

from __future__ import annotations

import json
import math
from collections import Counter
from collections.abc import Sequence
from typing import Any

from evalsi.evaluator import MetricSpec, Requirements, Scope, ScoreType, SkipRecord, evaluator
from evalsi.packs.finetune import calibration_errors
from evalsi.registry import Pack
from evalsi.types import Content, Record, Score

NUMBER = ScoreType.NUMBER
REFERENCE = Requirements(reference=True)


# --- reading predictions --------------------------------------------------------


def _json_field(content: Content | None, field: str) -> Any:
    if content is None:
        return None
    if content.is_json and isinstance(content.json, dict):
        return content.json.get(field)
    if content.text is not None and content.text.lstrip().startswith("{"):
        try:
            data = json.loads(content.text)
        except ValueError:
            return None
        if isinstance(data, dict):
            return data.get(field)
    return None


def _label(content: Content | None, field: str) -> str | None:
    if content is None:
        return None
    value = _json_field(content, field)
    if value is None:
        if content.is_json and not isinstance(content.json, dict | list):
            value = content.json
        elif content.text is not None and not content.text.lstrip().startswith("{"):
            value = content.text
    if value is None:
        return None
    if isinstance(value, bool):
        return str(value).lower()
    if isinstance(value, float) and value.is_integer():
        value = int(value)
    return str(value).strip()


def _score(record: Record, field: str) -> float | dict[str, float] | None:
    value = record.metadata.get(field)
    if value is None:
        value = _json_field(record.output, field)
    if isinstance(value, bool):
        return None
    if isinstance(value, int | float):
        return float(value)
    if isinstance(value, dict):
        out: dict[str, float] = {}
        for k, v in value.items():
            if isinstance(v, int | float) and not isinstance(v, bool):
                out[str(k)] = float(v)
        return out or None
    return None


def _number(content: Content | None) -> float | None:
    if content is None:
        return None
    value: Any = content.json if content.is_json else content.text
    if isinstance(value, dict):
        value = value.get("value", value.get("prediction"))
    if isinstance(value, bool):
        return None
    try:
        out = float(value)
    except (TypeError, ValueError):
        return None
    return out if math.isfinite(out) else None


# --- classification ---------------------------------------------------------------


def confusion(pairs: Sequence[tuple[str, str]]) -> dict[str, dict[str, int]]:
    """``{truth: {prediction: count}}``."""
    out: dict[str, dict[str, int]] = {}
    for truth, pred in pairs:
        out.setdefault(truth, {}).setdefault(pred, 0)
        out[truth][pred] += 1
    return out


def prf(pairs: Sequence[tuple[str, str]], labels: Sequence[str]) -> dict[str, dict[str, float]]:
    """Per-label precision, recall, F1 and support (zero when undefined, as scikit-learn)."""
    tp: Counter[str] = Counter()
    fp: Counter[str] = Counter()
    fn: Counter[str] = Counter()
    for truth, pred in pairs:
        if truth == pred:
            tp[truth] += 1
        else:
            fp[pred] += 1
            fn[truth] += 1
    out = {}
    for label in labels:
        p_den, r_den = tp[label] + fp[label], tp[label] + fn[label]
        precision = tp[label] / p_den if p_den else 0.0
        recall = tp[label] / r_den if r_den else 0.0
        f1 = 2 * precision * recall / (precision + recall) if precision + recall else 0.0
        out[label] = {"precision": precision, "recall": recall, "f1": f1, "support": r_den}
    return out


def _pairs(records: list[Record], label_field: str) -> list[tuple[str, str]]:
    pairs = []
    for r in records:
        truth, pred = _label(r.reference, label_field), _label(r.output, label_field)
        if truth is not None and pred is not None:
            pairs.append((truth, pred))
    return pairs


@evaluator(
    name="builtin/classification",
    version="1.0.0",
    description=(
        "Accuracy, and precision, recall and F1 averaged over labels (macro) and over "
        "records (micro); per-label scores and the confusion matrix in metadata. With "
        "positive_label, binary precision, recall and F1 of that label too."
    ),
    scope=Scope.DATASET,
    requires=REFERENCE,
    outputs=[
        MetricSpec("accuracy", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("precision-macro", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("recall-macro", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("f1-macro", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("f1-micro", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("precision", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("recall", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("f1", NUMBER, min=0, max=1, higher_is_better=True),
    ],
)
def classification(
    records: list[Record],
    *,
    label_field: str = "label",
    positive_label: str = "",
    labels: list[str] | None = None,
) -> list[Score]:
    pairs = _pairs(records, label_field)
    if not pairs:
        raise SkipRecord("no records with both a predicted and a true label")
    seen = sorted({t for t, _ in pairs} | {p for _, p in pairs})
    names = [str(x) for x in labels] if labels else seen
    per = prf(pairs, names)
    n = len(pairs)
    correct = sum(t == p for t, p in pairs)
    in_scope = [(t, p) for t, p in pairs if t in names or p in names]
    tp = sum(1 for t, p in in_scope if t == p and t in names)
    fp = sum(1 for t, p in in_scope if t != p and p in names)
    fn = sum(1 for t, p in in_scope if t != p and t in names)
    micro_p = tp / (tp + fp) if tp + fp else 0.0
    micro_r = tp / (tp + fn) if tp + fn else 0.0
    micro_f1 = 2 * micro_p * micro_r / (micro_p + micro_r) if micro_p + micro_r else 0.0
    meta = {"n": n, "per_label": per, "confusion": confusion(pairs), "labels": names}
    scores = [
        Score(name="accuracy", number=correct / n, metadata=meta),
        Score(name="precision-macro", number=sum(v["precision"] for v in per.values()) / len(per)),
        Score(name="recall-macro", number=sum(v["recall"] for v in per.values()) / len(per)),
        Score(name="f1-macro", number=sum(v["f1"] for v in per.values()) / len(per)),
        Score(name="f1-micro", number=micro_f1),
    ]
    if positive_label:
        pos = prf(pairs, [str(positive_label)])[str(positive_label)]
        scores += [
            Score(name="precision", number=pos["precision"]),
            Score(name="recall", number=pos["recall"]),
            Score(name="f1", number=pos["f1"]),
        ]
    return scores


def roc_auc(scores: Sequence[float], positives: Sequence[bool]) -> float | None:
    """Area under the ROC curve: the Mann-Whitney statistic, ties counted half."""
    pos = sum(positives)
    neg = len(positives) - pos
    if pos == 0 or neg == 0:
        return None
    order = sorted(range(len(scores)), key=lambda i: scores[i])
    ranks = [0.0] * len(scores)
    i = 0
    while i < len(order):
        j = i
        while j + 1 < len(order) and scores[order[j + 1]] == scores[order[i]]:
            j += 1
        for k in range(i, j + 1):
            ranks[order[k]] = (i + j) / 2 + 1
        i = j + 1
    rank_sum = sum(r for r, p in zip(ranks, positives, strict=True) if p)
    return (rank_sum - pos * (pos + 1) / 2) / (pos * neg)


def average_precision(scores: Sequence[float], positives: Sequence[bool]) -> float | None:
    """Average precision (the step-wise area under the precision-recall curve)."""
    pos = sum(positives)
    if pos == 0:
        return None
    pairs = sorted(zip(scores, positives, strict=True), key=lambda x: -x[0])
    ap, tp, seen, i = 0.0, 0, 0, 0
    while i < len(pairs):
        j = i
        while j < len(pairs) and pairs[j][0] == pairs[i][0]:
            j += 1
        block = pairs[i:j]
        tp += sum(p for _, p in block)
        seen += len(block)
        hits = sum(p for _, p in block)
        if hits:
            ap += hits / pos * (tp / seen)
        i = j
    return ap


def _probabilities(
    records: list[Record], score_field: str, label_field: str, positive_label: str
) -> tuple[list[float], list[bool], list[dict[str, float]], list[str]]:
    """Binary (score, positive) pairs, or multiclass ({label: p}, truth) pairs."""
    binary_s: list[float] = []
    binary_y: list[bool] = []
    multi_p: list[dict[str, float]] = []
    multi_y: list[str] = []
    for r in records:
        truth = _label(r.reference, label_field)
        s = _score(r, score_field)
        if truth is None or s is None:
            continue
        if isinstance(s, dict):
            multi_p.append(s)
            multi_y.append(truth)
        else:
            if not positive_label:
                raise ValueError(
                    "a single score per record is the positive class's probability: "
                    "set positive_label"
                )
            binary_s.append(s)
            binary_y.append(truth == str(positive_label))
    return binary_s, binary_y, multi_p, multi_y


@evaluator(
    name="builtin/roc-auc",
    version="1.0.0",
    description=(
        "ROC-AUC, PR-AUC (average precision) and log-loss of predicted probabilities: "
        "metadata[score_field] is the positive class's probability (set positive_label) or "
        "a {label: probability} map (one-vs-rest, macro-averaged)."
    ),
    scope=Scope.DATASET,
    requires=Requirements(reference=True, output=False),
    outputs=[
        MetricSpec("roc-auc", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("pr-auc", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("log-loss", NUMBER, min=0, higher_is_better=False),
    ],
)
def roc_auc_eval(
    records: list[Record],
    *,
    score_field: str = "score",
    label_field: str = "label",
    positive_label: str = "",
    eps: float = 1e-15,
) -> list[Score]:
    bs, by, mp, my = _probabilities(records, score_field, label_field, positive_label)
    if bs:
        auc, ap = roc_auc(bs, by), average_precision(bs, by)
        clip = [min(max(p, eps), 1 - eps) for p in bs]
        loss = -sum(
            math.log(p) if y else math.log(1 - p) for p, y in zip(clip, by, strict=True)
        ) / len(clip)
        meta: dict[str, Any] = {"n": len(bs), "positives": sum(by)}
    elif mp:
        labels = sorted(set(my) | {k for p in mp for k in p})
        aucs, aps = [], []
        for label in labels:
            s = [p.get(label, 0.0) for p in mp]
            y = [t == label for t in my]
            a, b = roc_auc(s, y), average_precision(s, y)
            if a is not None and b is not None:
                aucs.append(a)
                aps.append(b)
        auc = sum(aucs) / len(aucs) if aucs else None
        ap = sum(aps) / len(aps) if aps else None
        losses = []
        for p, t in zip(mp, my, strict=True):
            total = sum(p.values()) or 1.0
            losses.append(-math.log(min(max(p.get(t, 0.0) / total, eps), 1 - eps)))
        loss = sum(losses) / len(losses)
        meta = {"n": len(mp), "labels": labels, "averaged_over": len(aucs)}
    else:
        raise SkipRecord(f"no records with metadata.{score_field} and a reference label")
    out = [Score(name="log-loss", number=loss, metadata=meta)]
    if auc is not None:
        out.insert(0, Score(name="roc-auc", number=auc))
    if ap is not None:
        out.insert(1, Score(name="pr-auc", number=ap))
    return out


@evaluator(
    name="builtin/classifier-calibration",
    version="1.0.0",
    description=(
        "Expected calibration error (equal-width bins) and Brier score of the positive "
        "class's predicted probability (metadata[score_field]) against the true label."
    ),
    scope=Scope.DATASET,
    requires=Requirements(reference=True, output=False),
    outputs=[
        MetricSpec("ece", NUMBER, min=0, max=1, higher_is_better=False),
        MetricSpec("brier", NUMBER, min=0, max=1, higher_is_better=False),
    ],
)
def classifier_calibration(
    records: list[Record],
    *,
    positive_label: str,
    score_field: str = "score",
    label_field: str = "label",
    bins: int = 10,
) -> list[Score]:
    bs, by, mp, my = _probabilities(records, score_field, label_field, positive_label)
    pairs = list(zip(bs, by, strict=True))
    for p, t in zip(mp, my, strict=True):
        if str(positive_label) in p:
            pairs.append((p[str(positive_label)], t == str(positive_label)))
    pairs = [(p, y) for p, y in pairs if 0 <= p <= 1]
    if not pairs:
        raise SkipRecord(f"no records with a probability in metadata.{score_field}")
    ece, brier = calibration_errors(pairs, bins)
    return [
        Score(name="ece", number=ece, metadata={"n": len(pairs), "bins": bins}),
        Score(name="brier", number=brier),
    ]


# --- regression -----------------------------------------------------------------------


@evaluator(
    name="builtin/regression",
    version="1.0.0",
    description=(
        "MAE, MSE, RMSE, R² and MAPE of numeric outputs against numeric references "
        "(MAPE leaves out references of zero)."
    ),
    scope=Scope.DATASET,
    requires=REFERENCE,
    outputs=[
        MetricSpec("mae", NUMBER, min=0, higher_is_better=False),
        MetricSpec("mse", NUMBER, min=0, higher_is_better=False),
        MetricSpec("rmse", NUMBER, min=0, higher_is_better=False),
        MetricSpec("r2", NUMBER, max=1, higher_is_better=True),
        MetricSpec("mape", NUMBER, min=0, higher_is_better=False),
    ],
)
def regression(records: list[Record]) -> list[Score]:
    pairs = []
    unparsed = 0
    for r in records:
        y, p = _number(r.reference), _number(r.output)
        if y is None or p is None:
            unparsed += 1
            continue
        pairs.append((y, p))
    if not pairs:
        raise SkipRecord("no records with numeric output and reference")
    n = len(pairs)
    errors = [p - y for y, p in pairs]
    mae = sum(abs(e) for e in errors) / n
    mse = sum(e * e for e in errors) / n
    mean_y = sum(y for y, _ in pairs) / n
    ss_tot = sum((y - mean_y) ** 2 for y, _ in pairs)
    meta = {"n": n, "unparsed": unparsed}
    out = [
        Score(name="mae", number=mae, metadata=meta),
        Score(name="mse", number=mse),
        Score(name="rmse", number=math.sqrt(mse)),
    ]
    if ss_tot > 0:
        out.append(Score(name="r2", number=1 - mse * n / ss_tot))
    nonzero = [(y, p) for y, p in pairs if y != 0]
    if nonzero:
        out.append(
            Score(name="mape", number=sum(abs((p - y) / y) for y, p in nonzero) / len(nonzero))
        )
    return out


# --- ranking ---------------------------------------------------------------------------


def _ranked(content: Content | None) -> list[str]:
    if content is None:
        return []
    value: Any = content.json if content.is_json else content.text
    if isinstance(value, str):
        stripped = value.strip()
        if stripped.startswith("["):
            try:
                value = json.loads(stripped)
            except ValueError:
                value = stripped.splitlines()
        else:
            value = [line for line in stripped.splitlines() if line.strip()]
    if not isinstance(value, list):
        return []
    out = []
    for entry in value:
        item = entry.get("id", entry.get("doc_id")) if isinstance(entry, dict) else entry
        if item is not None:
            out.append(str(item).strip())
    return out


def _gains(content: Content | None) -> dict[str, float]:
    if content is None:
        return {}
    value: Any = content.json if content.is_json else content.text
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except ValueError:
            value = [line for line in value.splitlines() if line.strip()]
    if isinstance(value, dict):
        return {str(k): float(v) for k, v in value.items() if isinstance(v, int | float) and v > 0}
    if isinstance(value, list):
        return {str(v).strip(): 1.0 for v in value}
    return {}


def ndcg(ranked: Sequence[str], gains: dict[str, float], k: int) -> float:
    def dcg(items: Sequence[float]) -> float:
        return sum((2**g - 1) / math.log2(i + 2) for i, g in enumerate(items))

    ideal = dcg(sorted(gains.values(), reverse=True)[:k])
    return dcg([gains.get(d, 0.0) for d in ranked[:k]]) / ideal if ideal else 0.0


@evaluator(
    name="builtin/ranking",
    version="1.0.0",
    description=(
        "Ranking quality per record: NDCG@k (graded gains when the reference maps ids to "
        "gains), MRR, average precision (MAP over records), recall@k and precision@k."
    ),
    requires=REFERENCE,
    outputs=[
        MetricSpec("ndcg", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("mrr", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("map", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("recall-at-k", NUMBER, min=0, max=1, higher_is_better=True),
        MetricSpec("precision-at-k", NUMBER, min=0, max=1, higher_is_better=True),
    ],
)
def ranking(record: Record, *, k: int = 10) -> list[Score]:
    ranked = _ranked(record.output)
    gains = _gains(record.reference)
    if not gains:
        raise SkipRecord("the reference names no relevant items")
    relevant = set(gains)
    rr = next((1 / (i + 1) for i, d in enumerate(ranked) if d in relevant), 0.0)
    hits, ap = 0, 0.0
    for i, d in enumerate(ranked):
        if d in relevant:
            hits += 1
            ap += hits / (i + 1)
    top = ranked[:k]
    found = sum(1 for d in top if d in relevant)
    return [
        Score(name="ndcg", number=ndcg(ranked, gains, k), metadata={"k": k}),
        Score(name="mrr", number=rr),
        Score(name="map", number=ap / len(relevant)),
        Score(name="recall-at-k", number=found / len(relevant)),
        Score(name="precision-at-k", number=found / k if k else 0.0),
    ]


PACK = Pack(
    name="ml-classic",
    description=(
        "Classic ML metrics: classification (accuracy, macro and micro F1, ROC-AUC, PR-AUC, "
        "log-loss, calibration), regression (MAE, MSE, RMSE, R², MAPE) and ranking (NDCG, "
        "MRR, MAP, recall@k)."
    ),
    evaluators=[classification, roc_auc_eval, classifier_calibration, regression, ranking],
)
