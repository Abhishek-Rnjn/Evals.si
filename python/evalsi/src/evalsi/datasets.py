"""Loading records from JSONL, JSON, the Hugging Face Hub and importers.

A row's fields map onto a record by name (``id``, ``input``, ``output``,
``reference``, ``context``, ``usage``, ``metadata``). ``mapping`` renames
them, using dotted paths for nested fields::

    load_records("gsm8k.jsonl", mapping={"input": "question", "reference": "answer"})

Fields that are not consumed land in ``metadata``, so they can be used for
slicing and clustered standard errors.

Other formats come from importers: ``<scheme>://<path>?<options>`` hands the
path to the importer registered under that scheme in the ``evalsi.importers``
entry-point group, for example ``inspect://logs/run.eval`` from the Inspect AI
adapter. An importer is a callable ``(path, **options)`` returning records or
rows. ``hf://`` is the only scheme that is not a local file.
"""

from __future__ import annotations

import hashlib
import json
from collections.abc import Callable, Iterable, Iterator, Mapping
from importlib.metadata import entry_points
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse

from evalsi.types import Content, Record, TaskCheck, Trajectory, Usage

RECORD_FIELDS = (
    "id",
    "input",
    "output",
    "reference",
    "context",
    "usage",
    "metadata",
    "trajectory",
    "check",
)


class DatasetError(ValueError):
    pass


def load_records(
    source: str | Path | Iterable[Mapping[str, Any] | Record],
    *,
    mapping: Mapping[str, str] | None = None,
    limit: int | None = None,
) -> list[Record]:
    """Load records from a path, an ``hf://`` or importer URI, or an iterable of rows."""
    unknown = set(mapping or {}) - set(RECORD_FIELDS)
    if unknown:
        raise DatasetError(f"mapping has unknown record fields {sorted(unknown)}")
    rows: Iterable[Mapping[str, Any] | Record]
    if isinstance(source, str) and source.startswith("hf://"):
        rows = _hf_rows(source)
    elif isinstance(source, str) and "://" in source:
        rows = _imported_rows(source)
    elif isinstance(source, str | Path):
        rows = _file_rows(Path(source))
    else:
        rows = source
    records: list[Record] = []
    for index, row in enumerate(rows):
        if limit is not None and index >= limit:
            break
        records.append(
            row if isinstance(row, Record) else record_from_row(row, index, mapping or {})
        )
    seen: set[str] = set()
    for record in records:
        if record.id in seen:
            raise DatasetError(f"duplicate record id {record.id!r}")
        seen.add(record.id)
    return records


def record_from_row(row: Mapping[str, Any], index: int, mapping: Mapping[str, str]) -> Record:
    consumed: set[str] = set()

    def take(name: str) -> Any:
        path = mapping.get(name, name)
        value = _get_path(row, path)
        if value is not None:
            consumed.add(path.split(".", 1)[0])
        return value

    raw_context = take("context")
    if raw_context is None:
        context = []
    elif isinstance(raw_context, list):
        context = [Content.from_value(c) for c in raw_context]
    else:
        context = [Content.from_value(raw_context)]
    raw_usage = take("usage")
    raw_metadata = take("metadata")
    metadata: dict[str, Any] = dict(raw_metadata) if isinstance(raw_metadata, Mapping) else {}
    raw_trajectory = take("trajectory")
    raw_check = take("check")
    take("provenance")  # where a promoted trace came from; not needed for evaluation
    raw_id = take("id")
    record = Record(
        id=str(raw_id) if raw_id is not None else str(index),
        input=_content(take("input")),
        output=_content(take("output")),
        reference=_content(take("reference")),
        context=context,
        usage=Usage.from_dict(raw_usage) if isinstance(raw_usage, Mapping) else None,
        metadata=metadata,
        trajectory=Trajectory.from_dict(raw_trajectory)
        if isinstance(raw_trajectory, Mapping)
        else None,
        check=TaskCheck.from_dict(raw_check) if isinstance(raw_check, Mapping) else None,
    )
    for key, value in row.items():
        if key not in consumed:
            record.metadata.setdefault(key, value)
    return record


def _content(value: Any) -> Content | None:
    return None if value is None else Content.from_value(value)


def _get_path(row: Mapping[str, Any], path: str) -> Any:
    value: Any = row
    for part in path.split("."):
        if not isinstance(value, Mapping) or part not in value:
            return None
        value = value[part]
    return value


def _file_rows(path: Path) -> Iterator[Mapping[str, Any]]:
    if not path.exists():
        raise DatasetError(f"no such file: {path}")
    if path.suffix == ".json":
        data = json.loads(path.read_text(encoding="utf-8"))
        if not isinstance(data, list):
            raise DatasetError(f"{path}: a .json dataset must be a list of objects")
        yield from _objects(data, path)
        return
    with path.open(encoding="utf-8") as handle:
        for line_no, line in enumerate(handle, start=1):
            if not line.strip():
                continue
            try:
                row = json.loads(line)
            except json.JSONDecodeError as exc:
                raise DatasetError(f"{path}:{line_no}: invalid JSON: {exc}") from exc
            if not isinstance(row, dict):
                raise DatasetError(f"{path}:{line_no}: each line must be a JSON object")
            yield row


def _objects(data: list[Any], path: Path) -> Iterator[Mapping[str, Any]]:
    for i, row in enumerate(data):
        if not isinstance(row, dict):
            raise DatasetError(f"{path}[{i}]: each item must be a JSON object")
        yield row


def _hf_rows(ref: str) -> Iterator[Mapping[str, Any]]:
    """``hf://<repo>?config=<name>&split=<split>``; split defaults to ``test``."""
    try:
        import datasets
    except ImportError as exc:
        raise DatasetError("hf:// datasets need: pip install 'evalsi[hf]'") from exc
    parsed = urlparse(ref)
    repo = (parsed.netloc + parsed.path).strip("/")
    query = {k: v[-1] for k, v in parse_qs(parsed.query).items()}
    dataset = datasets.load_dataset(repo, query.get("config"), split=query.get("split", "test"))
    for row in dataset:
        yield dict(row)


Importer = Callable[..., Iterable[Mapping[str, Any] | Record]]


def find_importer(scheme: str) -> Importer:
    """The importer registered for ``scheme`` in the ``evalsi.importers`` entry points."""
    for ep in entry_points(group="evalsi.importers", name=scheme):
        importer: Importer = ep.load()
        return importer
    known = ", ".join(sorted({ep.name for ep in entry_points(group="evalsi.importers")}))
    raise DatasetError(
        f"no importer for {scheme}:// (installed: {known or 'none'}); importers ship "
        "with adapters, for example evalsi-adapter-inspect"
    )


def split_uri(uri: str) -> tuple[str, str, dict[str, str]]:
    """``scheme://path?k=v`` as (scheme, path, options)."""
    scheme, _, rest = uri.partition("://")
    path, _, query = rest.partition("?")
    return scheme, path, {k: v[-1] for k, v in parse_qs(query).items()}


def _imported_rows(uri: str) -> Iterable[Mapping[str, Any] | Record]:
    scheme, path, options = split_uri(uri)
    if not path:
        raise DatasetError(f"{uri}: no path after {scheme}://")
    return find_importer(scheme)(path, **options)


def records_hash(records: Iterable[Record]) -> str:
    """Content hash of a dataset, recorded in the run manifest."""
    digest = hashlib.sha256()
    for record in records:
        digest.update(json.dumps(record.to_dict(), sort_keys=True, default=str).encode())
        digest.update(b"\n")
    return digest.hexdigest()
