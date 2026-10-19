"""Portable dataset snapshots: no-overwrite publication and externally pinned verification."""

from __future__ import annotations

import copy
import hashlib
import json
import os
import re
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, NoReturn

from evalsi.datasets import DatasetError, load_records

SCHEMA = "evalsi.dataset/v1"
MAX_BYTES = 64 * 1024 * 1024
IDENTITY_KEY = "evalsi.dataset"
_LABEL = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}")
_DIGEST = re.compile(r"[0-9a-f]{64}")
_MANIFEST_FIELDS = {
    "schema",
    "name",
    "version",
    "owner",
    "split",
    "tags",
    "records",
    "source_sha256",
}


def _canonical(value: Any) -> bytes:
    try:
        return json.dumps(
            value, sort_keys=True, ensure_ascii=False, separators=(",", ":"), allow_nan=False
        ).encode("utf-8")
    except (ValueError, TypeError, UnicodeError, RecursionError) as exc:
        raise DatasetError("snapshot content must be finite, valid UTF-8 JSON") from exc


def _hash(value: Any) -> str:
    return hashlib.sha256(_canonical(value)).hexdigest()


def _object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    out: dict[str, Any] = {}
    for key, value in pairs:
        if key in out:
            raise DatasetError("duplicate JSON object key")
        out[key] = value
    return out


def _nonfinite(value: str) -> NoReturn:
    raise DatasetError("non-finite JSON values are not allowed in snapshots")


def _decode(raw: bytes) -> Any:
    try:
        return json.loads(raw.decode("utf-8"), object_pairs_hook=_object, parse_constant=_nonfinite)
    except (UnicodeError, json.JSONDecodeError, RecursionError) as exc:
        raise DatasetError("invalid UTF-8 JSON in dataset snapshot/source") from exc


def _read(path: Path) -> bytes:
    try:
        with path.open("rb") as f:
            raw = f.read(MAX_BYTES + 1)
    except OSError as exc:
        raise DatasetError(f"cannot read dataset file: {exc}") from exc
    if len(raw) > MAX_BYTES:
        raise DatasetError("dataset snapshot/source exceeds 64 MiB")
    return raw


def _validate_records(rows: Any) -> None:
    if not isinstance(rows, list) or not rows or any(not isinstance(r, dict) for r in rows):
        raise DatasetError("snapshot records must be a nonempty list of objects")
    for row in rows:
        if not isinstance(row.get("id"), str) or not row["id"].strip():
            raise DatasetError("snapshots require an explicit nonempty string id for every record")
        metadata = row.get("metadata", {})
        if not isinstance(metadata, dict) or IDENTITY_KEY in metadata:
            raise DatasetError(f"record metadata must be an object without reserved {IDENTITY_KEY}")
        if "provenance" in row and not isinstance(row["provenance"], dict):
            raise DatasetError("record provenance must be an object")
    # Reuse the actual loader's ID uniqueness and record conversion checks.
    try:
        load_records(rows)
    except DatasetError:
        raise
    except (TypeError, ValueError, AttributeError, OverflowError) as exc:
        raise DatasetError("snapshot records do not match the evaluation record format") from exc


def _validate_manifest(manifest: Any, count: int) -> None:
    if not isinstance(manifest, dict) or set(manifest) != _MANIFEST_FIELDS:
        raise DatasetError("invalid snapshot manifest fields")
    if manifest["schema"] != SCHEMA:
        raise DatasetError("unsupported snapshot schema")
    for field in ("name", "version"):
        value = manifest[field]
        if not isinstance(value, str) or not _LABEL.fullmatch(value):
            raise DatasetError(f"snapshot {field} must be a safe label of 1-128 characters")
    owner = manifest["owner"]
    if (
        not isinstance(owner, str)
        or not owner.strip()
        or len(owner) > 256
        or any(ord(c) < 32 for c in owner)
    ):
        raise DatasetError("snapshot owner must be nonempty text of at most 256 characters")
    if manifest["split"] not in ("held-out", "tuning"):
        raise DatasetError("snapshot split must be held-out or tuning")
    tags = manifest["tags"]
    if (
        not isinstance(tags, list)
        or any(not isinstance(t, str) or not _LABEL.fullmatch(t) for t in tags)
        or len(tags) > 32
        or tags != sorted(set(tags))
    ):
        raise DatasetError("snapshot tags must be at most 32 unique sorted safe labels")
    if type(manifest["records"]) is not int or manifest["records"] != count:
        raise DatasetError("snapshot record count does not match its contents")
    if not isinstance(manifest["source_sha256"], str) or not _DIGEST.fullmatch(
        manifest["source_sha256"]
    ):
        raise DatasetError("invalid source SHA-256")


@dataclass(frozen=True)
class Snapshot:
    manifest: dict[str, Any]
    records: list[dict[str, Any]]
    sha256: str

    def info(self) -> dict[str, Any]:
        return {**self.manifest, "sha256": self.sha256}


def read_snapshot(path: str | Path, *, expected: str | None = None) -> Snapshot:
    if expected is not None and not _DIGEST.fullmatch(expected):
        raise DatasetError("expected SHA-256 must contain 64 lowercase hex characters")
    document = _decode(_read(Path(path)))
    if not isinstance(document, dict) or set(document) != {"manifest", "records", "sha256"}:
        raise DatasetError("invalid snapshot envelope")
    _validate_records(document["records"])
    _validate_manifest(document["manifest"], len(document["records"]))
    canonical = _canonical({"manifest": document["manifest"], "records": document["records"]})
    digest = hashlib.sha256(canonical).hexdigest()
    if document["sha256"] != digest or (expected is not None and digest != expected):
        raise DatasetError("dataset snapshot SHA-256 mismatch; restore the approved snapshot")
    # Execution must use the same object-key order covered by the canonical digest.
    # Content and tool-call conversion can serialize nested objects into strings.
    normalized = _decode(canonical)
    return Snapshot(normalized["manifest"], normalized["records"], digest)


def create_snapshot(
    source: str | Path,
    destination: str | Path,
    *,
    name: str,
    version: str,
    owner: str,
    split: str = "held-out",
    tags: list[str] | None = None,
) -> Snapshot:
    source, destination = Path(source), Path(destination).absolute()
    raw = _read(source)
    if source.suffix == ".json":
        rows = _decode(raw)
    elif source.suffix == ".jsonl":
        rows = [_decode(line) for line in raw.splitlines() if line.strip()]
    else:
        raise DatasetError("snapshot source must be a local .json or .jsonl file")
    _validate_records(rows)
    rows = _decode(_canonical(rows))
    manifest = {
        "schema": SCHEMA,
        "name": name,
        "version": version,
        "owner": owner,
        "split": split,
        "tags": sorted(set(tags or [])),
        "records": len(rows),
        "source_sha256": hashlib.sha256(raw).hexdigest(),
    }
    _validate_manifest(manifest, len(rows))
    payload = {"manifest": manifest, "records": rows}
    snapshot = Snapshot(manifest, rows, _hash(payload))
    encoded = _canonical({**payload, "sha256": snapshot.sha256}) + b"\n"
    if len(encoded) > MAX_BYTES:
        raise DatasetError("encoded snapshot exceeds 64 MiB")
    # Readers never see partial snapshots, and a racing creator cannot replace a version.
    if any(parent.is_symlink() for parent in destination.parents):
        raise DatasetError("snapshot destination must not have symlinked ancestors")
    temporary: Path | None = None
    try:
        fd, tmp = tempfile.mkstemp(prefix=".evalsi-snapshot-", dir=destination.parent)
        temporary = Path(tmp)
        with os.fdopen(fd, "wb") as f:
            f.write(encoded)
            f.flush()
            os.fsync(f.fileno())
        os.link(temporary, destination)
    except OSError as exc:
        raise DatasetError(
            f"cannot publish snapshot (existing paths are never replaced): {exc}"
        ) from exc
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    return snapshot


def import_snapshot(path: str, *, sha256: str = "", **options: str) -> list[dict[str, Any]]:
    if not sha256 or options:
        raise DatasetError(
            "snapshot:// requires sha256=<approved digest> and accepts no other options"
        )
    snapshot = read_snapshot(path, expected=sha256)
    rows = copy.deepcopy(snapshot.records)
    identity = snapshot.info()
    for row in rows:
        # The normal loader consumes top-level provenance; retain it in durable metadata.
        row.setdefault("metadata", {})[IDENTITY_KEY] = {
            **identity,
            "provenance": row.get("provenance", {}),
        }
    return rows


def diff_snapshots(before: Snapshot, after: Snapshot) -> dict[str, Any]:
    a, b = {r["id"]: r for r in before.records}, {r["id"]: r for r in after.records}
    changed = []
    for rid in sorted(a.keys() & b.keys()):
        if _canonical(a[rid]) != _canonical(b[rid]):
            fields = sorted(
                k
                for k in a[rid].keys() | b[rid].keys()
                if k not in a[rid]
                or k not in b[rid]
                or _canonical(a[rid][k]) != _canonical(b[rid][k])
            )
            changed.append(
                {
                    "id": rid,
                    "fields": fields,
                    "before_sha256": _hash(a[rid]),
                    "after_sha256": _hash(b[rid]),
                }
            )
    return {
        "before_sha256": before.sha256,
        "after_sha256": after.sha256,
        "different": before.sha256 != after.sha256,
        "metadata_changed": sorted(
            k for k in before.manifest if before.manifest[k] != after.manifest[k]
        ),
        "added": sorted(b.keys() - a.keys()),
        "removed": sorted(a.keys() - b.keys()),
        "changed": changed,
        "unchanged": len(a.keys() & b.keys()) - len(changed),
        "order_changed": list(a) != list(b),
    }
