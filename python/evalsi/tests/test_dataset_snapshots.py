from __future__ import annotations

import json
import os
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any

import grpc
import pytest
import yaml

from evalsi.cli import main
from evalsi.convert import record_from_proto
from evalsi.dataset_snapshots import (
    IDENTITY_KEY,
    Snapshot,
    create_snapshot,
    diff_snapshots,
    read_snapshot,
)
from evalsi.datasets import DatasetError, load_records


def source(path: Path, rows: list[dict[str, Any]] | None = None) -> Path:
    rows = (
        rows
        if rows is not None
        else [
            {
                "id": "france",
                "input": "private-input-849",
                "output": "Paris",
                "reference": "Paris",
                "provenance": {"trace_id": "approved-trace", "reviewer": "team-ml"},
            },
            {"id": "japan", "output": "Tokyo", "reference": "Tokyo"},
        ]
    )
    path.write_text("".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8")
    return path


def snapshot(src: Path, dest: Path, version: str = "v1") -> Snapshot:
    return create_snapshot(
        src, dest, name="support", version=version, owner="team-ml", tags=["regression", "approved"]
    )


def test_snapshot_round_trip_preserves_pin_provenance_and_source(tmp_path: Path) -> None:
    src, dest = source(tmp_path / "source.jsonl"), tmp_path / "snapshot.json"
    saved = snapshot(src, dest)
    assert read_snapshot(dest, expected=saved.sha256) == saved
    records = load_records(f"snapshot://{dest}?sha256={saved.sha256}")
    identity = records[0].metadata[IDENTITY_KEY]
    assert identity["sha256"] == saved.sha256
    assert identity["split"] == "held-out"
    assert identity["owner"] == "team-ml"
    assert identity["provenance"]["trace_id"] == "approved-trace"
    assert records[0].input is not None
    assert records[0].input.text == "private-input-849"
    assert "metadata" not in saved.records[0]  # Importing does not mutate the snapshot.
    if os.name == "posix":
        assert dest.stat().st_mode & 0o777 == 0o600
    # Editing the original source cannot change the published dataset.
    src.write_text("invalid source")
    assert read_snapshot(dest).sha256 == saved.sha256


def test_pinned_run_and_replacement_fail_before_target_execution(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    dest = tmp_path / "snapshot with spaces.json"
    src = source(tmp_path / "source.jsonl")
    saved = snapshot(src, dest)
    spec: dict[str, Any] = {
        "apiVersion": "evals.si/v1alpha1",
        "kind": "EvalRun",
        "spec": {
            "dataset": {"uri": f"snapshot://{dest.name}?sha256={saved.sha256}"},
            "evaluators": [{"ref": "exact-match"}],
            "gates": [{"metric": "exact-match", "min": 1}],
        },
    }
    path = tmp_path / "run.yaml"
    path.write_text(yaml.safe_dump(spec))
    output = tmp_path / "result.json"
    assert main(["run", "-f", str(path), "--output", str(output)]) == 0
    result = json.loads(output.read_text())
    assert result["records"][0]["metadata"][IDENTITY_KEY]["sha256"] == saved.sha256
    # Even a replacement with its own valid internal hash fails the approved external pin.
    source(src, [{"id": "france", "output": "wrong", "reference": "Paris"}])
    other = tmp_path / "replacement.json"
    replacement = snapshot(src, other)
    dest.write_bytes(other.read_bytes())
    assert read_snapshot(dest).sha256 == replacement.sha256
    spec["spec"]["target"] = {
        "connector": "openai-compatible",
        "model": "never-called",
        "base_url": "http://unused",
        "api_key_env": "none",
    }
    path.write_text(yaml.safe_dump(spec))

    def fail_if_called(config: Any) -> None:
        pytest.fail("target must not be created before the pin is verified")

    monkeypatch.setattr("evalsi.run.create_target", fail_if_called)
    assert main(["run", "-f", str(path)]) == 1


@pytest.mark.parametrize(
    "attributes",
    [{"team": "support"}, {IDENTITY_KEY: {"sha256": "0" * 64, "provenance": {"fake": True}}}],
    ids=["ordinary-metadata", "conflicting-identity"],
)
def test_snapshot_metadata_remapping_is_rejected_locally_and_by_worker(
    tmp_path: Path, attributes: dict[str, Any], capsys: pytest.CaptureFixture[str]
) -> None:
    from test_worker import with_worker

    from evalsi.plugin.v1alpha1 import evaluator_plugin_pb2 as pb
    from evalsi.registry import default_registry
    from evalsi.v1alpha1 import run_pb2
    from evalsi.worker import EvaluatorPlugin

    dest = tmp_path / "snapshot.json"
    saved = snapshot(
        source(
            tmp_path / "source.jsonl",
            [{"id": "one", "question": "hello", "attributes": attributes}],
        ),
        dest,
    )
    uri = f"snapshot://{dest}?sha256={saved.sha256}"
    mapping = {"metadata": "attributes"}
    with pytest.raises(DatasetError, match="metadata remapping"):
        load_records(uri, mapping=mapping)
    # Other mappings and an explicit identity mapping still preserve the verified identity.
    record = load_records(uri, mapping={"input": "question", "metadata": "metadata"})[0]
    assert record.input is not None
    assert record.input.as_text() == "hello"
    assert record.metadata[IDENTITY_KEY]["sha256"] == saved.sha256

    spec = tmp_path / "run.yaml"
    spec.write_text(
        yaml.safe_dump(
            {
                "apiVersion": "evals.si/v1alpha1",
                "kind": "EvalRun",
                "spec": {
                    "dataset": {"uri": uri, "mapping": mapping},
                    "evaluators": [{"ref": "exact-match"}],
                },
            }
        )
    )
    output = tmp_path / "result.json"
    assert main(["run", "-f", str(spec), "--output", str(output)]) == 1
    assert "metadata remapping" in capsys.readouterr().err
    assert not output.exists()

    async def call(stub: Any) -> list[Any]:
        source = run_pb2.DatasetSource(uri=uri, mapping=mapping)
        return [chunk async for chunk in stub.LoadDataset(pb.LoadDatasetRequest(source=source))]

    with pytest.raises(grpc.aio.AioRpcError, match="metadata remapping") as info:
        with_worker(EvaluatorPlugin(default_registry()), call, tmp_path)
    assert info.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_reordered_nested_keys_keep_pinned_execution_inputs_identical(tmp_path: Path) -> None:
    rows: list[dict[str, Any]] = [
        {"id": "structured", "input": {"z": [{"y": 2, "a": 1}], "a": {"z": 3, "b": 4}}},
        {
            "id": "chat",
            "input": [
                {"role": "user", "content": "use the tool"},
                {
                    "role": "assistant",
                    "tool_calls": [
                        {"name": "first", "arguments": {"z": {"y": 2, "a": 1}, "a": [2, 1]}},
                        {"name": "second", "arguments": '{"z":2,"a":1}'},
                    ],
                },
            ],
        },
    ]
    dest = tmp_path / "snapshot.json"
    saved = snapshot(source(tmp_path / "source.jsonl", rows), dest)
    uri = f"snapshot://{dest}?sha256={saved.sha256}"
    before = load_records(uri)
    assert [r.to_dict() for r in load_records(saved.records)] == [
        {k: v for k, v in r.to_dict().items() if k != "metadata"} for r in before
    ]

    def reverse_keys(value: Any) -> Any:
        if isinstance(value, dict):
            return {key: reverse_keys(value[key]) for key in reversed(value)}
        if isinstance(value, list):
            return [reverse_keys(item) for item in value]
        return value

    dest.write_text(json.dumps(reverse_keys(json.loads(dest.read_text())), indent=4))
    assert read_snapshot(dest, expected=saved.sha256).sha256 == saved.sha256
    after = load_records(uri)
    assert [r.id for r in after] == ["structured", "chat"]
    assert [r.to_dict() for r in before] == [r.to_dict() for r in after]
    assert before[0].input is not None
    assert after[0].input is not None
    assert (
        before[0].input.as_text()
        == after[0].input.as_text()
        == '{"a":{"b":4,"z":3},"z":[{"a":1,"y":2}]}'
    )
    assert after[1].input is not None
    assert after[1].input.messages is not None
    messages = after[1].input.messages
    assert [m.role for m in messages] == ["user", "assistant"]
    calls = messages[1].tool_calls
    assert [c.name for c in calls] == ["first", "second"]
    assert calls[0].arguments == '{"a": [2, 1], "z": {"a": 1, "y": 2}}'
    assert calls[1].arguments == '{"z":2,"a":1}'


@pytest.mark.parametrize("existing", ["file", "directory", "symlink"])
def test_snapshot_never_overwrites_existing_path(tmp_path: Path, existing: str) -> None:
    src, dest = source(tmp_path / "source.jsonl"), tmp_path / "snapshot.json"
    if existing == "file":
        dest.write_text("keep")
    elif existing == "directory":
        dest.mkdir()
    else:
        target = tmp_path / "target"
        target.write_text("keep")
        dest.symlink_to(target)
    with pytest.raises(DatasetError, match="never replaced"):
        snapshot(src, dest)
    assert not list(tmp_path.glob(".evalsi-snapshot-*"))
    if existing != "directory":
        assert dest.read_text() == "keep"


def test_symlinked_output_ancestor_and_racing_publishers(tmp_path: Path) -> None:
    src = source(tmp_path / "source.jsonl")
    real = tmp_path / "real"
    real.mkdir()
    alias = tmp_path / "alias"
    alias.symlink_to(real, target_is_directory=True)
    with pytest.raises(DatasetError, match="symlinked ancestors"):
        snapshot(src, alias / "out.json")
    assert not list(real.iterdir())
    dest = real / "out.json"

    def publish() -> bool:
        try:
            snapshot(src, dest)
        except DatasetError:
            return False
        return True

    with ThreadPoolExecutor(max_workers=2) as pool:
        assert sorted(pool.map(lambda _: publish(), range(2))) == [False, True]
    assert read_snapshot(dest).manifest["records"] == 2
    assert list(real.iterdir()) == [dest]


@pytest.mark.parametrize(
    "content",
    [
        "{}",
        '{"id":1}',
        '{"id":""}',
        '{"id":"a"}\n{"id":"a"}',
        '{"id":"a","id":"b"}',
        '{"id":"a","output":NaN}',
        '{"id":"a","output":1e999}',
        '{"id":"a","metadata":{"evalsi.dataset":{}}}',
        '{"id":"a","provenance":"not an object"}',
    ],
)
def test_invalid_source_cannot_publish(tmp_path: Path, content: str) -> None:
    src = tmp_path / "source.jsonl"
    src.write_text(content)
    dest = tmp_path / "snapshot.json"
    with pytest.raises(DatasetError):
        snapshot(src, dest)
    assert not dest.exists()


def test_missing_pins_unknown_options_and_tampered_snapshot(tmp_path: Path) -> None:
    dest = tmp_path / "snapshot.json"
    saved = snapshot(source(tmp_path / "source.jsonl"), dest)
    for query in ("", "?sha256=not-a-digest", f"?sha256={saved.sha256}&ignore_hash=true"):
        with pytest.raises(DatasetError, match=r"sha256|SHA-256"):
            load_records(f"snapshot://{dest}{query}")
    document = json.loads(dest.read_text())
    document["records"][0]["reference"] = "tampered"
    dest.write_text(json.dumps(document))
    with pytest.raises(DatasetError, match="SHA-256 mismatch"):
        read_snapshot(dest)


def test_metadata_is_hashed_and_reformatting_keeps_the_same_pin(tmp_path: Path) -> None:
    src, a, b = source(tmp_path / "source.jsonl"), tmp_path / "a.json", tmp_path / "b.json"
    first = snapshot(src, a)
    second = create_snapshot(
        src, b, name="support", version="v1", owner="other-team", split="tuning"
    )
    assert first.sha256 != second.sha256
    diff = diff_snapshots(first, second)
    assert diff["metadata_changed"] == ["owner", "split", "tags"]
    assert not diff["changed"]
    document = json.loads(a.read_text())
    a.write_text(json.dumps(document, indent=4))
    assert read_snapshot(a, expected=first.sha256).sha256 == first.sha256


def test_size_and_encoding_limits_are_enforced(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from evalsi import dataset_snapshots

    invalid = tmp_path / "invalid.jsonl"
    invalid.write_bytes(b"\xff")
    with pytest.raises(DatasetError, match="UTF-8"):
        snapshot(invalid, tmp_path / "snapshot.json")
    monkeypatch.setattr(dataset_snapshots, "MAX_BYTES", 16)
    invalid.write_bytes(b" " * 17)
    with pytest.raises(DatasetError, match="exceeds"):
        snapshot(invalid, tmp_path / "snapshot.json")


def test_cli_diff_and_verify_do_not_print_payloads(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    src, a, b = tmp_path / "source.jsonl", tmp_path / "a.json", tmp_path / "b.json"
    first = snapshot(source(src), a)
    rows = json.loads("[" + ",".join(src.read_text().splitlines()) + "]")
    rows[0]["reference"] = "private-reference-849"
    rows[1]["id"] = "new"
    snapshot(source(src, rows), b, "v2")
    diff = diff_snapshots(read_snapshot(a), read_snapshot(b))
    assert diff["added"] == ["new"]
    assert diff["removed"] == ["japan"]
    assert diff["changed"][0]["fields"] == ["reference"]
    assert "version" in diff["metadata_changed"]
    for fmt in ("json", "table"):
        assert main(["dataset", "diff", str(a), str(b), "--format", fmt, "--fail-on-change"]) == 3
        text = capsys.readouterr().out
        assert "private-input-849" not in text
        assert "private-reference-849" not in text
    assert main(["dataset", "diff", str(a), str(a), "--fail-on-change"]) == 0
    capsys.readouterr()
    assert main(["dataset", "verify", str(a), "--sha256", first.sha256, "--format", "json"]) == 0
    assert json.loads(capsys.readouterr().out)["sha256"] == first.sha256
    assert main(["dataset", "verify", str(a), "--sha256", "0" * 64]) == 1


def test_snapshot_loads_through_real_worker_and_rejects_pin_mismatch(tmp_path: Path) -> None:
    from test_worker import with_worker

    from evalsi.plugin.v1alpha1 import evaluator_plugin_pb2 as pb
    from evalsi.registry import default_registry
    from evalsi.v1alpha1 import run_pb2
    from evalsi.worker import EvaluatorPlugin

    dest = tmp_path / "snapshot.json"
    saved = snapshot(source(tmp_path / "source.jsonl"), dest)

    async def call(stub: Any) -> list[Any]:
        source = run_pb2.DatasetSource(uri=f"snapshot://{dest}?sha256={saved.sha256}")
        return [c async for c in stub.LoadDataset(pb.LoadDatasetRequest(source=source))]

    chunks = with_worker(EvaluatorPlugin(default_registry()), call, tmp_path)
    record = record_from_proto(chunks[0].records[0])
    assert record.metadata[IDENTITY_KEY]["provenance"]["trace_id"] == "approved-trace"
    saved_hash = saved.sha256
    dest.write_text(dest.read_text().replace(saved_hash, "0" * 64))
    with pytest.raises(grpc.aio.AioRpcError) as info:
        with_worker(EvaluatorPlugin(default_registry()), call, tmp_path)
    assert info.value.code() == grpc.StatusCode.INVALID_ARGUMENT


def test_checked_in_example_reproduces_snapshots_and_gate_controls(tmp_path: Path) -> None:
    root = Path(__file__).resolve().parents[3] / "examples/dataset-snapshots"
    for version, expected in (("v1", 0), ("v2", 3)):
        built = tmp_path / f"{version}.json"
        saved = create_snapshot(
            root / f"source-{version}.jsonl",
            built,
            name="support-refunds",
            version=version,
            owner="example-maintainers",
            tags=["synthetic"],
        )
        approved = read_snapshot(root / f"support-{version}.evalsi.json")
        assert saved.sha256 == approved.sha256
        assert (
            main(
                [
                    "run",
                    "-f",
                    str(root / f"gate-{version}.yaml"),
                    "--output",
                    str(tmp_path / f"{version}-results.json"),
                ]
            )
            == expected
        )
