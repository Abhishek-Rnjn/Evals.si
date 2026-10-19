from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

from evalsi.cli import main
from evalsi.dataset_overlap import split_overlap
from evalsi.dataset_snapshots import Snapshot, create_snapshot
from evalsi.datasets import DatasetError


def make_split(tmp_path: Path, split: str, rows: list[dict[str, Any]]) -> tuple[Path, Snapshot]:
    source, destination = tmp_path / f"{split}.json", tmp_path / f"{split}.snapshot.json"
    source.write_text(json.dumps(rows))
    return destination, create_snapshot(
        source, destination, name="support", version="v1", owner="test-team", split=split
    )


def test_overlaps_include_renamed_inputs_and_changed_labels(tmp_path: Path) -> None:
    _, tuning = make_split(
        tmp_path,
        "tuning",
        [
            {"id": "same-id", "input": "old input"},
            {"id": "original", "input": "private-input-849", "output": "old", "reference": "yes"},
        ],
    )
    _, held_out = make_split(
        tmp_path,
        "held-out",
        [
            {"id": "same-id", "input": "changed input"},
            {
                "id": "renamed",
                "input": {"text": "private-input-849"},
                "output": "different",
                "reference": "no",
                "metadata": {"source": "elsewhere"},
                "context": ["different context deliberately excluded from input matching"],
            },
        ],
    )
    report = split_overlap(tuning, held_out)
    assert report["shared_ids"] == ["same-id"]
    assert report["matched_records"] == {"tuning": 2, "held_out": 2}
    assert report["shared_inputs"][0]["tuning_ids"] == ["original"]
    assert report["shared_inputs"][0]["held_out_ids"] == ["renamed"]
    assert report["status"] == "overlap"
    assert "private-input-849" not in json.dumps(report)


def test_full_chat_and_structured_inputs_are_compared(tmp_path: Path) -> None:
    _, tuning = make_split(
        tmp_path,
        "tuning",
        [
            {"id": "a", "input": [{"role": "user", "content": "q"}]},
            {"id": "b", "input": {"x": 1, "y": [2, 3]}},
        ],
    )
    _, held_out = make_split(
        tmp_path,
        "held-out",
        [
            {"id": "c", "input": {"messages": [{"role": "user", "content": "q"}]}},
            {"id": "d", "input": {"json": {"y": [2, 3], "x": 1}}},
            {
                "id": "e",
                "input": [
                    {"role": "system", "content": "changed"},
                    {"role": "user", "content": "q"},
                ],
            },
        ],
    )
    report = split_overlap(tuning, held_out)
    assert len(report["shared_inputs"]) == 2
    assert report["matched_records"]["held_out"] == 2
    assert report["complete"]


@pytest.mark.parametrize("missing", [None, "", "  ", {}, []])
def test_incomplete_coverage_cannot_pass_strict_check(
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
    missing: Any,
) -> None:
    tuning_path, tuning = make_split(tmp_path, "tuning", [{"id": "a", "input": "known"}])
    held_path, held = make_split(tmp_path, "held-out", [{"id": "b", "input": missing}])
    args = [
        "dataset",
        "overlap",
        "--tuning",
        str(tuning_path),
        "--held-out",
        str(held_path),
        "--tuning-sha256",
        tuning.sha256,
        "--held-out-sha256",
        held.sha256,
        "--format",
        "json",
    ]
    assert main(args) == 0  # A diagnostic report does not enforce a policy.
    report = json.loads(capsys.readouterr().out)
    assert report["status"] == "incomplete"
    assert report["input_coverage"]["held_out"]["missing_ids"] == ["b"]
    assert main([*args, "--fail-on-overlap"]) == 1


@pytest.mark.parametrize("overlaps", [True, False])
def test_cli_pinned_positive_negative_and_privacy_controls(
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
    overlaps: bool,
) -> None:
    tuning_path, tuning = make_split(tmp_path, "tuning", [{"id": "a", "input": "secret-q-849"}])
    held_path, held = make_split(
        tmp_path,
        "held-out",
        [
            {"id": "b", "input": "secret-q-849" if overlaps else "other"},
        ],
    )
    args = [
        "dataset",
        "overlap",
        "--tuning",
        str(tuning_path),
        "--held-out",
        str(held_path),
        "--tuning-sha256",
        tuning.sha256,
        "--held-out-sha256",
        held.sha256,
        "--fail-on-overlap",
    ]
    for fmt in ("json", "table"):
        assert main([*args, "--format", fmt]) == (3 if overlaps else 0)
        output = capsys.readouterr().out
        assert "secret-q-849" not in output
        assert tuning.sha256 in output if fmt == "json" else "input coverage" in output
    # Replacing a file or passing the wrong trusted digest cannot pass the policy.
    index = args.index("--held-out-sha256") + 1
    args[index] = "0" * 64
    assert main(args) == 1


def test_identical_inputs_are_grouped_without_cartesian_expansion(tmp_path: Path) -> None:
    _, tuning = make_split(
        tmp_path, "tuning", [{"id": f"a{i}", "input": "same"} for i in range(100)]
    )
    _, held_out = make_split(
        tmp_path, "held-out", [{"id": f"b{i}", "input": "same"} for i in range(100)]
    )
    report = split_overlap(tuning, held_out)
    assert len(report["shared_inputs"]) == 1
    assert len(report["shared_inputs"][0]["tuning_ids"]) == 100
    assert report["matched_records"] == {"tuning": 100, "held_out": 100}


def test_split_labels_must_match_their_roles(tmp_path: Path) -> None:
    _, tuning = make_split(tmp_path, "tuning", [{"id": "a", "input": "q"}])
    with pytest.raises(DatasetError, match="tuning snapshot and a held-out"):
        split_overlap(tuning, tuning)


def test_incomplete_coverage_takes_precedence_over_known_overlap(tmp_path: Path) -> None:
    _, tuning = make_split(tmp_path, "tuning", [{"id": "same", "input": "known"}])
    _, held_out = make_split(tmp_path, "held-out", [{"id": "same"}])
    report = split_overlap(tuning, held_out)
    assert report["overlap"]
    assert not report["complete"]
    assert report["status"] == "incomplete"


def test_checked_in_split_policy_runs_both_controls(tmp_path: Path) -> None:
    from evalsi.dataset_snapshots import read_snapshot

    root = Path(__file__).resolve().parents[3] / "examples/dataset-snapshots"
    for source, name, version in (
        ("tuning-source.jsonl", "tuning-v1.evalsi.json", "v1"),
        ("tuning-leaked-source.jsonl", "tuning-leaked.evalsi.json", "leaked"),
    ):
        saved = create_snapshot(
            root / source,
            tmp_path / name,
            name="support-refunds-tuning",
            version=version,
            owner="example-maintainers",
            split="tuning",
            tags=["synthetic"],
        )
        assert saved.sha256 == read_snapshot(root / name).sha256
    proc = subprocess.run(
        ["bash", str(root / "check-splits.sh")],
        check=False,
        capture_output=True,
        text=True,
        env={
            **os.environ,
            "PATH": str(Path(sys.executable).parent) + os.pathsep + os.environ["PATH"],
        },
    )
    assert proc.returncode == 0, proc.stderr
    assert "status: disjoint" in proc.stdout
    assert "status: overlap" in proc.stdout
