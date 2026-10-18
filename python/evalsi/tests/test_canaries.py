from __future__ import annotations

import json
from pathlib import Path

import pytest

from evalsi.canaries import make_canaries
from evalsi.cli import main
from evalsi.packs.finetune import contamination_canary
from evalsi.testing import score
from evalsi.types import Content


def test_canaries_keep_secrets_out_of_inputs_and_probe_recall() -> None:
    records = make_canaries(3)
    assert len({r.reference.as_text() for r in records if r.reference is not None}) == 3
    for record in records:
        assert record.input is not None
        assert record.reference is not None
        secret = record.reference.as_text()
        assert len(secret) == 32
        assert secret not in record.input.as_text()
        record.output = Content(text="I do not know.")
        assert score(contamination_canary, record)["contamination-canary"].passed is False
        record.output = Content(text=" " + secret + "\n")
        assert score(contamination_canary, record)["contamination-canary"].passed is True
        record.input = Content(text=secret)
        with pytest.raises(ValueError, match="exposed"):
            score(contamination_canary, record)


def test_canary_cli_writes_separate_probe_and_training_files(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    probe, training = tmp_path / "probe.jsonl", tmp_path / "training.txt"
    argv = [
        "canary",
        "create",
        "--count",
        "2",
        "--output",
        str(probe),
        "--training-output",
        str(training),
    ]
    assert main(argv) == 0
    assert json.loads(capsys.readouterr().out)["canaries"] == 2
    records = [json.loads(line) for line in probe.read_text().splitlines()]
    assert len(records) == 2
    for row, line in zip(records, training.read_text().splitlines(), strict=True):
        assert row["input"]["text"] in line
        assert row["reference"]["text"] in line
        assert row["reference"]["text"] not in row["input"]["text"]
    before = probe.read_bytes()
    assert main(argv) == 1
    assert probe.read_bytes() == before


@pytest.mark.parametrize("count", [0, -1, 10001, True])
def test_canary_count_is_bounded(count: int) -> None:
    with pytest.raises(ValueError, match="count"):
        make_canaries(count)
