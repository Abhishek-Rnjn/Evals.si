"""Conservative exact-input and record-ID overlap diagnostics between pinned splits."""

from __future__ import annotations

from typing import Any

from evalsi.dataset_snapshots import Snapshot, _hash
from evalsi.datasets import DatasetError, load_records
from evalsi.types import Content


def _has_input(content: Content | None) -> bool:
    if content is None:
        return False
    if content.text is not None:
        return bool(content.text.strip())
    if content.messages is not None:
        return any(m.content.strip() or m.tool_calls for m in content.messages)
    return content.json not in (None, "", {}, [])


def _input_index(snapshot: Snapshot) -> tuple[dict[str, list[str]], list[str]]:
    index: dict[str, list[str]] = {}
    missing = []
    for record in load_records(snapshot.records):
        if not _has_input(record.input):
            missing.append(record.id)
            continue
        assert record.input is not None
        fingerprint = _hash(record.input.to_dict())
        index.setdefault(fingerprint, []).append(record.id)
    return index, sorted(missing)


def split_overlap(tuning: Snapshot, held_out: Snapshot) -> dict[str, Any]:
    """Report shared IDs and exact normalized inputs; never infer semantic contamination.

    Input groups keep the output bounded by the number of records, even when many
    cases share an input. No Cartesian product of all matching pairs is generated.
    """
    if tuning.manifest["split"] != "tuning" or held_out.manifest["split"] != "held-out":
        raise DatasetError("overlap requires a tuning snapshot and a held-out snapshot")
    a_ids = {r["id"] for r in tuning.records}
    b_ids = {r["id"] for r in held_out.records}
    shared_ids = sorted(a_ids & b_ids)
    a, a_missing = _input_index(tuning)
    b, b_missing = _input_index(held_out)
    inputs = [
        {"input_sha256": digest, "tuning_ids": sorted(a[digest]), "held_out_ids": sorted(b[digest])}
        for digest in sorted(a.keys() & b.keys())
    ]
    a_matched, b_matched = set(shared_ids), set(shared_ids)
    for group in inputs:
        a_matched.update(group["tuning_ids"])
        b_matched.update(group["held_out_ids"])
    complete = not a_missing and not b_missing
    overlap = bool(shared_ids or inputs)
    fields = ("name", "version", "sha256", "split", "records")
    return {
        "schema": "evalsi.dataset-overlap/v1",
        "tuning": {key: tuning.info()[key] for key in fields},
        "held_out": {key: held_out.info()[key] for key in fields},
        "status": "incomplete" if not complete else "overlap" if overlap else "disjoint",
        "complete": complete,
        "overlap": overlap,
        "shared_ids": shared_ids,
        "shared_inputs": inputs,
        "matched_records": {"tuning": len(a_matched), "held_out": len(b_matched)},
        "input_coverage": {
            "tuning": {
                "total": len(a_ids),
                "compared": len(a_ids) - len(a_missing),
                "missing_ids": a_missing,
            },
            "held_out": {
                "total": len(b_ids),
                "compared": len(b_ids) - len(b_missing),
                "missing_ids": b_missing,
            },
        },
        "method": "shared record IDs or exact normalized full inputs; "
        "outputs, references, context and metadata are excluded",
    }
