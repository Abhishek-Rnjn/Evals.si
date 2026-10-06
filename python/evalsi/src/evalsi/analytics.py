"""Cross-run analytics in DuckDB.

Runs, from results files (``evalsi run --output``) or from a server through
its API, are loaded into three tables:

``runs``
    run_id, name, project, status, model, source, created_at, labels (JSON)
``scores``
    run_id, record_id, trial, evaluator, metric, value, passed, label,
    outcome, reason, explanation: one row per score, plus one per result
    that was skipped or errored (value NULL)
``records``
    run_id, record_id, input, output, reference, metadata (JSON)

so results can be sliced by record metadata across many runs, in SQL or
with :meth:`Warehouse.slice`. Going through the API, a server's results are
read the same way whatever its backend (SQLite, PostgreSQL, ClickHouse),
and only the runs the caller may read.

Needs ``duckdb`` (``pip install 'evalsi[analytics]'``).
"""

from __future__ import annotations

import json
import math
from collections.abc import Iterable, Mapping, Sequence
from pathlib import Path
from typing import Any

_SCHEMA = """
CREATE TABLE IF NOT EXISTS runs (
    run_id VARCHAR PRIMARY KEY, name VARCHAR, project VARCHAR, status VARCHAR,
    model VARCHAR, source VARCHAR, created_at TIMESTAMPTZ, labels JSON
);
CREATE TABLE IF NOT EXISTS scores (
    run_id VARCHAR, record_id VARCHAR, trial INTEGER, evaluator VARCHAR,
    metric VARCHAR, value DOUBLE, passed BOOLEAN, label VARCHAR,
    outcome VARCHAR, reason VARCHAR, explanation VARCHAR
);
CREATE TABLE IF NOT EXISTS records (
    run_id VARCHAR, record_id VARCHAR, input VARCHAR, output VARCHAR,
    reference VARCHAR, metadata JSON
);
"""


def _duckdb() -> Any:
    try:
        import duckdb
    except ImportError as exc:  # pragma: no cover - depends on the install
        raise RuntimeError(
            "cross-run analytics needs duckdb: pip install 'evalsi[analytics]'"
        ) from exc
    return duckdb


def _text(value: Any) -> str | None:
    if value is None:
        return None
    if isinstance(value, Mapping):
        if "text" in value:
            return str(value["text"])
        return json.dumps(value, ensure_ascii=False, default=str)
    return str(value)


def _metric(evaluator: str, score_name: str) -> str:
    return evaluator if not score_name or score_name == evaluator else f"{evaluator}.{score_name}"


def _score_rows(run_id: str, results: Iterable[Mapping[str, Any]]) -> list[tuple[Any, ...]]:
    rows: list[tuple[Any, ...]] = []
    for r in results:
        rid = str(r.get("record_id", r.get("recordId", "")))
        trial = int(r.get("trial", 0) or 0)
        evaluator = str(r.get("evaluator", ""))
        outcome = str(r.get("outcome", "")).lower().removeprefix("outcome_")
        reason = str(r.get("reason", "")) or None
        scores = r.get("scores") or []
        if outcome != "scored" or not scores:
            empty = (None, None, None, outcome, reason, None)
            rows.append((run_id, rid, trial, evaluator, evaluator, *empty))
            continue
        for s in scores:
            value: float | None = None
            passed = s.get("passed")
            if "number" in s and s["number"] is not None:
                value = float(s["number"])
            elif passed is not None:
                value = 1.0 if passed else 0.0
            label = s.get("label")
            rows.append(
                (
                    run_id,
                    rid,
                    trial,
                    evaluator,
                    _metric(evaluator, str(s.get("name", ""))),
                    value,
                    None if passed is None else bool(passed),
                    None if label is None else str(label),
                    outcome,
                    reason,
                    str(s.get("explanation", "")) or None,
                )
            )
    return rows


def _record_rows(run_id: str, records: Iterable[Mapping[str, Any]]) -> list[tuple[Any, ...]]:
    return [
        (
            run_id,
            str(r.get("id", "")),
            _text(r.get("input")),
            _text(r.get("output")),
            _text(r.get("reference")),
            json.dumps(r.get("metadata") or {}, ensure_ascii=False, default=str),
        )
        for r in records
    ]


class Warehouse:
    """Runs loaded into DuckDB, in memory or in a file (``path``)."""

    def __init__(self, path: str = ":memory:") -> None:
        self.db = _duckdb().connect(path)
        self.db.execute(_SCHEMA)

    def close(self) -> None:
        self.db.close()

    def __enter__(self) -> Warehouse:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def _replace(
        self,
        run: tuple[Any, ...],
        results: Iterable[Mapping[str, Any]],
        records: Iterable[Mapping[str, Any]],
    ) -> None:
        run_id = run[0]
        self.db.execute("BEGIN")
        try:
            for table in ("runs", "scores", "records"):
                self.db.execute(f"DELETE FROM {table} WHERE run_id = ?", [run_id])
            self.db.execute("INSERT INTO runs VALUES (?, ?, ?, ?, ?, ?, ?, ?)", list(run))
            scores = _score_rows(run_id, results)
            if scores:
                self.db.executemany(
                    "INSERT INTO scores VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", scores
                )
            recs = _record_rows(run_id, records)
            if recs:
                self.db.executemany("INSERT INTO records VALUES (?, ?, ?, ?, ?, ?)", recs)
            self.db.execute("COMMIT")
        except Exception:
            self.db.execute("ROLLBACK")
            raise

    def add_results(self, data: Mapping[str, Any], *, run_id: str = "") -> str:
        """A results file's content (``evalsi run --output``). The run is
        named by ``run_id``, else the manifest's run id or name and start."""
        manifest = data.get("manifest") or {}
        target = manifest.get("target") or {}
        rid = (
            run_id
            or str(manifest.get("run_id") or "")
            or f"{manifest.get('name') or 'run'}@{manifest.get('started_at') or ''}"
        )
        labels = (manifest.get("spec") or {}).get("labels") or manifest.get("labels") or {}
        run = (
            rid,
            str(manifest.get("name") or ""),
            str(manifest.get("project") or ""),
            "finished",
            str(target.get("model") or "") if isinstance(target, Mapping) else "",
            "file",
            manifest.get("started_at"),
            json.dumps(labels, default=str),
        )
        self._replace(run, data.get("results") or [], data.get("records") or [])
        return rid

    def add_results_file(self, path: str | Path) -> str:
        p = Path(path)
        return self.add_results(json.loads(p.read_text(encoding="utf-8")), run_id="")

    def add_server_run(self, client: Any, run: Mapping[str, Any]) -> str:
        """A server run (as ``GetRun``/``ListRuns`` return it), with its results."""
        run_id = str(run.get("id", ""))
        results: list[dict[str, Any]] = []
        records: list[dict[str, Any]] = []
        token = ""
        while True:
            page = client.list_run_results(run_id, page_token=token)
            results += page.get("results", [])
            records += page.get("records", [])
            token = page.get("nextPageToken", "")
            if not token:
                break
        target = (run.get("spec") or {}).get("target") or {}
        row = (
            run_id,
            str(run.get("name") or ""),
            str(run.get("project") or ""),
            str(run.get("status", "")).removeprefix("RUN_STATUS_").lower(),
            str(target.get("model") or ""),
            "server",
            run.get("createdAt"),
            json.dumps(run.get("labels") or {}),
        )
        self._replace(row, results, records)
        return run_id

    def add_server_runs(
        self,
        client: Any,
        *,
        project: str = "",
        labels: Mapping[str, str] | None = None,
        run_ids: Sequence[str] = (),
        limit: int = 50,
    ) -> list[str]:
        """The newest ``limit`` runs of a project (with these labels), or the given runs."""
        if run_ids:
            return [self.add_server_run(client, client.get_run(r)) for r in run_ids]
        loaded: list[str] = []
        token = ""
        while len(loaded) < limit:
            page = client.list_runs(project=project, page_token=token)
            for run in page.get("runs", []):
                have = run.get("labels") or {}
                if labels and any(have.get(k) != v for k, v in labels.items()):
                    continue
                loaded.append(self.add_server_run(client, run))
                if len(loaded) >= limit:
                    break
            token = page.get("nextPageToken", "")
            if not token:
                break
        return loaded

    def query(
        self, sql: str, params: Sequence[Any] = ()
    ) -> tuple[list[str], list[tuple[Any, ...]]]:
        cur = self.db.execute(sql, list(params))
        columns = [d[0] for d in cur.description or []]
        return columns, cur.fetchall()

    def slice(
        self, metric: str, by: str, *, level: float = 0.95
    ) -> tuple[list[str], list[tuple[Any, ...]]]:
        """``metric`` per run and per value of the record metadata field
        ``by`` (a dotted path), with a normal-approximation interval."""
        z = {0.9: 1.6449, 0.95: 1.96, 0.99: 2.5758}.get(level)
        if z is None:
            raise ValueError("level must be 0.9, 0.95 or 0.99")
        path = "$." + ".".join(json.dumps(p) for p in by.split("."))
        alias = by.replace('"', '""')
        _, rows = self.query(
            f"""
            SELECT s.run_id, any_value(r.name) AS run,
                   coalesce(json_extract_string(rec.metadata, ?), '(none)') AS "{alias}",
                   count(s.value) AS n, avg(s.value) AS mean, stddev_samp(s.value) AS sd
            FROM scores s
            JOIN runs r USING (run_id)
            LEFT JOIN records rec ON rec.run_id = s.run_id AND rec.record_id = s.record_id
            WHERE s.metric = ? AND s.value IS NOT NULL
            GROUP BY ALL
            ORDER BY 3, any_value(r.created_at), 1
            """,
            [path, metric],
        )
        out = []
        for run_id, name, key, n, mean, sd in rows:
            half = z * sd / math.sqrt(n) if n > 1 and sd is not None else None
            low = None if half is None else mean - half
            high = None if half is None else mean + half
            out.append((run_id, name, key, n, mean, low, high))
        return ["run_id", "run", by, "n", "mean", "low", "high"], out


def format_table(columns: Sequence[str], rows: Sequence[Sequence[Any]]) -> str:
    def cell(v: Any) -> str:
        if v is None:
            return ""
        if isinstance(v, float):
            return f"{v:.3f}"
        return str(v)

    body = [[cell(v) for v in row] for row in rows]
    widths = [max([len(c), *(len(r[i]) for r in body)]) for i, c in enumerate(columns)]
    lines = ["  ".join(c.ljust(w) for c, w in zip(columns, widths, strict=True)).rstrip()]
    for r in body:
        lines.append("  ".join(v.ljust(w) for v, w in zip(r, widths, strict=True)).rstrip())
    return "\n".join(lines)
