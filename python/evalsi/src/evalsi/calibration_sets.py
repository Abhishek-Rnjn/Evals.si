"""Portable human-labelled calibration sets, kept separate from evaluator inputs.

These are content pins, not signatures or attestations of who supplied a rating.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from collections.abc import Iterable, Mapping
from pathlib import Path
from typing import Any, cast

from evalsi.datasets import load_records

SET_SCHEMA = "evalsi.calibration-set/v1"
MAX_BYTES = 64 * 1024 * 1024


def canonical(value: Any) -> bytes:
    return json.dumps(
        value, sort_keys=True, ensure_ascii=False, separators=(",", ":"), allow_nan=False
    ).encode("utf-8")


def digest(value: Any) -> str:
    return hashlib.sha256(canonical(value)).hexdigest()


def _pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def decode(raw: bytes) -> Any:
    def invalid(value: str) -> Any:
        raise ValueError(f"non-finite JSON value {value}")

    value = json.loads(raw, object_pairs_hook=_pairs, parse_constant=invalid)
    canonical(value)  # Also rejects exponent overflow (1e999).
    return value


def read_json(path: str | Path) -> Any:
    with Path(path).open("rb") as stream:
        raw = stream.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        raise ValueError("calibration input exceeds 64 MiB")
    return decode(raw)


def read_jsonl(path: str | Path) -> list[dict[str, Any]]:
    with Path(path).open("rb") as stream:
        raw = stream.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        raise ValueError("calibration input exceeds 64 MiB")
    rows = [decode(line) for line in raw.splitlines() if line.strip()]
    if not all(isinstance(row, dict) for row in rows):
        raise ValueError("calibration JSONL rows must be objects")
    return rows


def publish(path: str | Path, raw: bytes) -> None:
    """Publish privately and atomically; never replace an existing file or symlink."""
    if len(raw) > MAX_BYTES:
        raise ValueError("calibration artifact exceeds 64 MiB")
    destination = Path(path)
    temporary: Path | None = None
    try:
        with tempfile.NamedTemporaryFile(dir=destination.parent, delete=False) as stream:
            temporary = Path(stream.name)
            stream.write(raw)
            stream.flush()
            os.fsync(stream.fileno())
        os.link(temporary, destination)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def write_artifact(path: str | Path, document: dict[str, Any]) -> str:
    sha256 = digest(document)
    publish(path, canonical({"sha256": sha256, "document": document}) + b"\n")
    return sha256


def read_artifact(path: str | Path, *, expected: str = "") -> tuple[dict[str, Any], str]:
    envelope = read_json(path)
    if not isinstance(envelope, dict) or set(envelope) != {"sha256", "document"}:
        raise ValueError("expected a calibration artifact envelope")
    document = envelope["document"]
    if not isinstance(document, dict):
        raise ValueError("artifact document must be an object")
    sha256 = digest(document)
    if envelope["sha256"] != sha256 or (expected and expected != sha256):
        raise ValueError("calibration artifact SHA-256 mismatch")
    return document, sha256


def consensus(ratings: list[dict[str, Any]]) -> bool | None:
    votes = [rating["passed"] for rating in ratings if type(rating.get("passed")) is bool]
    if not votes or sum(votes) * 2 == len(votes):
        return None
    return bool(sum(votes) * 2 > len(votes))


def _input_key(record: dict[str, Any]) -> str | None:
    content = load_records([record])[0].input
    if content is None or not content.as_text().strip():
        return None
    # Exact canonical content, including message/tool structure. No semantic claim.
    return digest(content.to_dict())


def validate_set(document: dict[str, Any]) -> None:  # noqa: PLR0915 - validate the complete artifact
    if set(document) != {"schema", "name", "version", "question", "cases"}:
        raise ValueError("unknown or missing calibration-set fields")
    if document["schema"] != SET_SCHEMA:
        raise ValueError("unsupported calibration-set schema")
    for key in ("name", "version", "question"):
        if not isinstance(document[key], str) or not document[key].strip():
            raise ValueError(f"calibration set needs a non-empty {key}")
    cases = document["cases"]
    if not isinstance(cases, list) or not cases:
        raise ValueError("calibration set needs cases")
    groups: dict[str, str] = {}
    inputs: dict[str, tuple[str, str]] = {}
    for case in cases:
        if not isinstance(case, dict) or set(case) != {
            "record",
            "split",
            "group",
            "slices",
            "ratings",
            "label",
            "source",
        }:
            raise ValueError("unknown or missing calibration-case fields")
        if case["split"] not in ("tuning", "held-out"):
            raise ValueError("each calibration case needs split tuning or held-out")
        if not isinstance(case["group"], str) or not case["group"]:
            raise ValueError("each case needs a non-empty independent group")
        previous = groups.setdefault(case["group"], case["split"])
        if previous != case["split"]:
            raise ValueError("a group crosses tuning and held-out splits")
        if not isinstance(case["record"], dict) or not isinstance(case["record"].get("id"), str):
            raise ValueError("each record needs an explicit string id")
        if not case["record"]["id"]:
            raise ValueError("record ids cannot be empty")
        if not isinstance(case["slices"], dict) or not all(
            isinstance(k, str) and k and isinstance(v, str) and v for k, v in case["slices"].items()
        ):
            raise ValueError("slices must map non-empty strings to strings")
        if not isinstance(case["source"], dict):
            raise ValueError("case source must be an object")
        raters = set()
        if not isinstance(case["ratings"], list):
            raise ValueError("ratings must be a list")
        for rating in case["ratings"]:
            if not isinstance(rating, dict) or not isinstance(rating.get("annotator"), str):
                raise ValueError("each rating needs an annotator")
            if (
                set(rating) - {"annotator", "passed", "skipped", "comment", "created_at"}
                or "passed" not in rating
            ):
                raise ValueError("unknown or missing human rating fields")
            if not rating["annotator"] or rating["annotator"] in raters:
                raise ValueError("each annotator may rate a case only once")
            raters.add(rating["annotator"])
            if rating.get("passed") is not None and type(rating["passed"]) is not bool:
                raise ValueError("human passed labels must be booleans or null")
            if type(rating.get("skipped", False)) is not bool or (
                rating.get("skipped", False) and rating["passed"] is not None
            ):
                raise ValueError("skipped human ratings must be boolean abstentions")
        label = case["label"]
        if label is not None and type(label) is not bool:
            raise ValueError("consensus label must be boolean or null")
        if label != consensus(case["ratings"]):
            raise ValueError("stored label does not match strict-majority human ratings")
        input_key = _input_key(case["record"])
        if input_key is not None:
            prior = inputs.setdefault(input_key, (case["split"], case["group"]))
            if prior != (case["split"], case["group"]):
                raise ValueError("identical inputs must share a group and split")
    load_records([case["record"] for case in cases])  # Includes duplicate ID checks.
    if {case["split"] for case in cases} != {"tuning", "held-out"}:
        raise ValueError("a calibration set must contain tuning and held-out cases")


def freeze_set(
    rows: Iterable[Mapping[str, Any]], *, name: str, version: str, question: str
) -> dict[str, Any]:
    """Freeze explicit splits and all human votes; ties remain unlabelled.

    Each row contains record, split and ratings; group defaults to the record ID.
    Task references are preserved. Human labels never enter evaluator records.
    """
    cases = []
    for row in rows:
        unknown = set(row) - {"record", "split", "ratings", "group", "slices", "source"}
        if unknown:
            raise ValueError(f"unknown calibration row fields: {sorted(unknown)}")
        if not isinstance(row.get("record"), dict) or not row["record"].get("id"):
            raise ValueError("each row needs a record with an explicit id")
        ratings = row.get("ratings", [])
        if not isinstance(ratings, list) or not all(isinstance(r, dict) for r in ratings):
            raise ValueError("ratings must contain objects")
        cases.append(
            {
                "record": row["record"],
                "split": row.get("split"),
                "group": row.get("group", row["record"]["id"]),
                "slices": row.get("slices", {}),
                "ratings": ratings,
                "label": consensus(ratings),
                "source": row.get("source", {}),
            }
        )
    document = {
        "schema": SET_SCHEMA,
        "name": name,
        "version": version,
        "question": question,
        "cases": cases,
    }
    validate_set(document)
    return cast(dict[str, Any], decode(canonical(document)))  # Detach caller-owned rows.


def annotation_rows(
    annotations: Iterable[dict[str, Any]],
    *,
    question: str,
    assignments: Mapping[str, dict[str, Any]],
) -> list[dict[str, Any]]:
    """Import ``evalsi annotate export`` using explicit item-ID split assignments.

    Only pass/fail answers are accepted. Stored machine scores are never labels.
    Protobuf JSON conversion preserves messages, trajectory and camelCase usage.
    """
    from google.protobuf.json_format import ParseDict, ParseError

    from evalsi.convert import record_from_proto
    from evalsi.v1alpha1.record_pb2 import Record as ProtoRecord

    rows: dict[str, dict[str, Any]] = {}
    for annotation in annotations:
        item = annotation.get("item", {})
        item_id = annotation.get("itemId", "")
        if not item_id or item.get("id") != item_id or item_id not in assignments:
            raise ValueError("annotation needs its exported item and explicit split assignment")
        assignment = assignments[item_id]
        if not isinstance(assignment, dict):
            raise ValueError("annotation assignment must be an object")
        if set(assignment) - {"split", "group", "slices"}:
            raise ValueError("unknown annotation assignment fields")
        queue = annotation.get("queue", "")
        source = {"queue": queue, "item_id": item_id, "run": item.get("source", {})}
        try:
            record = record_from_proto(ParseDict(item.get("record", {}), ProtoRecord())).to_dict()
        except ParseError as exc:
            raise ValueError("invalid annotation record protobuf JSON") from exc
        if not record["id"]:
            raise ValueError("exported annotation record needs an id")
        row = rows.setdefault(
            item_id, {"record": record, **assignment, "ratings": [], "source": source}
        )
        if row["record"] != record or row["source"] != source:
            raise ValueError("conflicting revisions of an exported annotation item")
        answers = [a for a in annotation.get("answers", []) if a.get("question") == question]
        if len(answers) > 1:
            raise ValueError("duplicate answers to the calibration question")
        passed = None
        if answers and not annotation.get("skipped", False):
            if set(answers[0]) != {"question", "passed"} or type(answers[0]["passed"]) is not bool:
                raise ValueError("calibration question must have pass/fail human answers")
            passed = answers[0]["passed"]
        row["ratings"].append(
            {
                "annotator": annotation.get("annotator", ""),
                "passed": passed,
                "skipped": bool(annotation.get("skipped", False)),
                "comment": annotation.get("comment", ""),
                "created_at": annotation.get("createdAt", ""),
            }
        )
    if set(rows) != set(assignments):
        raise ValueError("assignments include items absent from the annotation export")
    return list(rows.values())
