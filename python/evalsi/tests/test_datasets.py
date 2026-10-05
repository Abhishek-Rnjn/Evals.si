from __future__ import annotations

import json
from pathlib import Path

import pytest

from evalsi.datasets import DatasetError, load_records, records_hash


def write_jsonl(path: Path, rows: list[dict[str, object]]) -> Path:
    path.write_text("\n".join(json.dumps(r) for r in rows) + "\n\n", encoding="utf-8")
    return path


def test_jsonl_with_default_field_names(tmp_path: Path) -> None:
    path = write_jsonl(
        tmp_path / "d.jsonl",
        [
            {
                "id": "a",
                "input": "q",
                "output": "x",
                "reference": "x",
                "usage": {"latency_ms": 5},
                "metadata": {"topic": "t"},
                "extra": 1,
            },
            {"input": "q2", "output": "y"},
        ],
    )
    first, second = load_records(path)
    assert first.id == "a"
    assert first.output is not None
    assert first.output.text == "x"
    assert first.usage is not None
    assert first.usage.latency_ms == 5
    assert first.metadata == {"topic": "t", "extra": 1}
    assert second.id == "1"
    assert second.reference is None


def test_mapping_and_dotted_paths() -> None:
    rows = [{"q": "2+2?", "gold": {"answer": "4"}, "resp": "4", "docs": ["d1", "d2"]}]
    (record,) = load_records(
        rows,
        mapping={"input": "q", "reference": "gold.answer", "output": "resp", "context": "docs"},
    )
    assert record.input is not None
    assert record.input.text == "2+2?"
    assert record.reference is not None
    assert record.reference.text == "4"
    assert [c.text for c in record.context] == ["d1", "d2"]
    assert record.metadata == {}


def test_json_file_and_limit(tmp_path: Path) -> None:
    path = tmp_path / "d.json"
    path.write_text(json.dumps([{"output": str(i)} for i in range(5)]), encoding="utf-8")
    assert [r.id for r in load_records(path, limit=2)] == ["0", "1"]


@pytest.mark.parametrize(
    ("content", "message"),
    [
        ('{"output": "a"}\nnot json\n', "invalid JSON"),
        ("[1, 2]\n", "must be a JSON object"),
        ('{"id": "x"}\n{"id": "x"}\n', "duplicate record id"),
    ],
)
def test_bad_files(tmp_path: Path, content: str, message: str) -> None:
    path = tmp_path / "d.jsonl"
    path.write_text(content, encoding="utf-8")
    with pytest.raises(DatasetError, match=message):
        load_records(path)


def test_missing_file_and_bad_mapping(tmp_path: Path) -> None:
    with pytest.raises(DatasetError, match="no such file"):
        load_records(tmp_path / "nope.jsonl")
    with pytest.raises(DatasetError, match="unknown record fields"):
        load_records([], mapping={"answer": "x"})


def test_records_hash_is_stable_and_content_sensitive() -> None:
    a = load_records([{"output": "x"}])
    b = load_records([{"output": "x"}])
    c = load_records([{"output": "y"}])
    assert records_hash(a) == records_hash(b) != records_hash(c)


def test_importer_uris(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    from evalsi import datasets
    from evalsi.run import _resolve_uri

    calls: list[tuple[str, dict[str, str]]] = []

    def fake(path: str, **options: str) -> list[dict[str, str]]:
        calls.append((path, options))
        return [{"id": "a", "output": "x"}]

    monkeypatch.setattr(datasets, "find_importer", lambda scheme: fake)
    records = load_records("fake://logs/run.eval?scores=false&limit=3")
    assert [r.id for r in records] == ["a"]
    assert calls == [("logs/run.eval", {"scores": "false", "limit": "3"})]

    assert _resolve_uri("fake://run.eval?x=1", tmp_path) == f"fake://{tmp_path}/run.eval?x=1"
    assert _resolve_uri("fake:///abs/run.eval", tmp_path) == "fake:///abs/run.eval"
    assert _resolve_uri("hf://org/ds?split=test", tmp_path) == "hf://org/ds?split=test"
    with pytest.raises(DatasetError, match="needs a scheme"):
        _resolve_uri("/etc/passwd", tmp_path)


def test_unknown_importer_names_installed_ones() -> None:
    with pytest.raises(DatasetError, match=r"no importer for nope://"):
        load_records("nope://x")
