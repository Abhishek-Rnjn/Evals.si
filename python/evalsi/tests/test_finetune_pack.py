from __future__ import annotations

from pathlib import Path

import pytest

from conftest import make_record
from evalsi.evaluator import SkipRecord
from evalsi.packs import finetune
from evalsi.packs.text import tokenize
from evalsi.testing import score


def test_diversity_detects_mode_collapse() -> None:
    varied = [
        make_record(t, id=str(i))
        for i, t in enumerate(
            [
                "the cat sat on the mat",
                "a dog ran across the park quickly",
                "rain fell over quiet hills tonight",
                "new ideas grow from careful questions",
            ]
        )
    ]
    collapsed = [make_record("the cat sat on the mat", id=str(i)) for i in range(4)]
    v, c = score(finetune.diversity, varied), score(finetune.diversity, collapsed)
    assert v["distinct-2"].number is not None
    assert c["distinct-2"].number is not None
    assert v["distinct-2"].number > 0.9 > c["distinct-2"].number
    assert c["distinct-1"].number == pytest.approx(5 / 24)
    assert c["self-bleu"].number == pytest.approx(1.0)
    assert v["self-bleu"].number is not None
    assert v["self-bleu"].number < 0.2
    assert v["entropy"].number is not None
    assert c["entropy"].number is not None
    assert v["entropy"].number > c["entropy"].number


def test_diversity_groups() -> None:
    records = [
        make_record("same answer", id="a1", metadata={"prompt": "p1"}),
        make_record("same answer", id="a2", metadata={"prompt": "p1"}),
        make_record("one thing", id="b1", metadata={"prompt": "p2"}),
        make_record("other stuff", id="b2", metadata={"prompt": "p2"}),
        make_record("alone", id="c1", metadata={"prompt": "p3"}),  # a group of one is dropped
    ]
    got = score(finetune.diversity, records, group_by="prompt")
    assert got["distinct-1"].number == pytest.approx((2 / 4 + 4 / 4) / 2)
    assert got["distinct-1"].metadata == {"groups": 2, "outputs": 4}
    with pytest.raises(SkipRecord):
        score(finetune.diversity, records[-1:], group_by="prompt")


def test_calibration() -> None:
    def rec(i: int, conf: float, correct: bool) -> object:
        return make_record("x", id=str(i), metadata={"confidence": conf, "correct": correct})

    perfect = [rec(0, 1.0, True), rec(1, 0.0, False)]
    got = score(finetune.calibration, perfect)  # type: ignore[arg-type]
    assert got["ece"].number == 0.0
    assert got["brier"].number == 0.0
    overconfident = [rec(i, 0.9, i < 5) for i in range(10)]
    got = score(finetune.calibration, overconfident)  # type: ignore[arg-type]
    assert got["ece"].number == pytest.approx(0.4)
    assert got["brier"].number == pytest.approx((5 * 0.01 + 5 * 0.81) / 10)
    # Correctness from the reference when metadata.correct is absent.
    by_ref = [
        make_record("Paris", "paris", id="1", metadata={"confidence": 0.8}),
        make_record("Lyon", "paris", id="2", metadata={"confidence": 0.8}),
    ]
    assert score(finetune.calibration, by_ref)["ece"].number == pytest.approx(0.3)
    with pytest.raises(SkipRecord):
        score(finetune.calibration, [make_record("x")])


def test_contamination(tmp_path: Path) -> None:
    corpus = tmp_path / "train.jsonl"
    corpus.write_text(
        '{"text": "the quick brown fox jumps over the lazy dog near the river bank"}\n'
    )
    leaked = make_record(
        None, "near the river bank", input="the quick brown fox jumps over the lazy dog"
    )
    fresh = make_record(None, "paris", input="what is the capital city of france then")
    a = score(finetune.contamination, leaked, training_data=str(corpus), n=4)
    b = score(finetune.contamination, fresh, training_data=str(corpus), n=4)
    assert a["ngram-overlap"].number == 1.0
    assert a["contaminated"].passed is True
    assert b["ngram-overlap"].number == 0.0
    assert b["contaminated"].passed is False
    assert "min-k-prob" not in a
    with_lp = make_record(
        None,
        "x",
        input="the quick brown fox jumps",
        metadata={"logprobs": [-0.1, -5.0, -0.2, -3.0, -0.1]},
    )
    got = score(finetune.contamination, with_lp, training_data=str(corpus), n=2, k=0.4)
    assert got["min-k-prob"].number == pytest.approx(-4.0)
    with pytest.raises(SkipRecord):
        score(
            finetune.contamination, make_record(None, "x", input="short"), training_data=str(corpus)
        )
    with pytest.raises(ValueError, match="not a file"):
        score(finetune.contamination, leaked, training_data=str(tmp_path / "nope"))


def test_reward_hacking() -> None:
    # The reward climbs with length while the held-out grade does not.
    records = [
        make_record(
            " ".join(["word"] * (5 * i + 1)),
            id=str(i),
            metadata={
                "reward": 0.2 * i,
                "heldout": 0.5 if i % 2 else 0.3,
                "logprob": -1.0,
                "ref_logprob": -1.5,
            },
        )
        for i in range(6)
    ]
    got = score(finetune.reward_hacking, records)
    assert got["reward-length-pearson"].number == pytest.approx(1.0)
    assert got["reward-heldout-gap"].number == pytest.approx((0.2 * 15) / 6 - 0.4)
    assert got["kl-to-reference"].number == pytest.approx(0.5)
    assert got["reward-heldout-spearman"].number is not None
    with pytest.raises(SkipRecord):
        score(finetune.reward_hacking, [make_record("x")])


def test_statistics_helpers() -> None:
    assert finetune.spearman([1, 2, 3, 4], [10, 20, 30, 40]) == pytest.approx(1.0)
    assert finetune.spearman([1, 2, 3, 4], [4, 3, 2, 1]) == pytest.approx(-1.0)
    assert finetune.pearson([1, 1, 1], [1, 2, 3]) is None
    assert finetune._ranks([3, 1, 3]) == [1.5, 0, 1.5]
    assert finetune.distinct_n([tokenize("a a a")], 1) == pytest.approx(1 / 3)
    assert finetune.entropy([tokenize("a b")]) == pytest.approx(1.0)
