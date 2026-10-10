import json
from typing import Any

import pytest

pytest.importorskip("duckdb")

from evalsi.analytics import Warehouse
from evalsi.cli import main


def results_file(name: str, started: str, outcomes: dict[str, bool]) -> dict[str, Any]:
    meta = {"a": {"task": {"kind": "easy"}}, "b": {"task": {"kind": "hard"}}, "c": {}}
    return {
        "manifest": {"name": name, "started_at": started, "target": {"model": "m-" + name}},
        "results": [
            {
                "record_id": rid,
                "evaluator": "exact-match",
                "outcome": "scored",
                "scores": [{"name": "exact-match", "passed": ok}],
            }
            for rid, ok in outcomes.items()
        ]
        + [{"record_id": "c", "evaluator": "judge", "outcome": "error", "reason": "timeout"}],
        "records": [{"id": rid, "output": {"text": rid}, "metadata": meta[rid]} for rid in meta],
    }


class FakeClient:
    """ListRuns and ListRunResults in the server's JSON form, paginated."""

    def __init__(self) -> None:
        self.runs = [
            {
                "id": f"run-{i}",
                "name": f"nightly-{i}",
                "project": "p",
                "status": "RUN_STATUS_SUCCEEDED",
                "labels": {"suite": "nightly" if i < 3 else "adhoc"},
                "createdAt": f"2026-10-0{i + 1}T00:00:00Z",
                "spec": {"target": {"model": f"model-{i}"}},
            }
            for i in range(4)
        ]

    def list_runs(self, *, project: str = "", page_token: str = "", **_: Any) -> dict[str, Any]:
        start = int(page_token or 0)
        # As the server does: ListRuns leaves the spec out.
        runs = [{k: v for k, v in r.items() if k != "spec"} for r in self.runs[start : start + 2]]
        page: dict[str, Any] = {"runs": runs}
        if start + 2 < len(self.runs):
            page["nextPageToken"] = str(start + 2)
        return page

    def get_run(self, run_id: str) -> dict[str, Any]:
        return next(r for r in self.runs if r["id"] == run_id)

    def list_run_results(self, run_id: str, *, page_token: str = "", **_: Any) -> dict[str, Any]:
        i = int(run_id.split("-")[1])
        rows = [
            {
                "recordId": f"r{j}",
                "trial": t,
                "evaluator": "quality",
                "outcome": "OUTCOME_SCORED",
                "scores": [{"name": "quality", "number": (i + j) / 10}],
            }
            for j in range(2)
            for t in range(2)
        ]
        recs = [{"id": f"r{j}", "metadata": {"lang": "en" if j == 0 else "de"}} for j in range(2)]
        if not page_token:
            return {"results": rows[:2], "records": recs, "nextPageToken": "2"}
        return {"results": rows[2:], "records": []}


def test_results_files_sliced_by_nested_metadata() -> None:
    with Warehouse() as wh:
        wh.add_results(results_file("base", "2026-10-01T00:00:00+00:00", {"a": True, "b": False}))
        wh.add_results(results_file("cand", "2026-10-02T00:00:00+00:00", {"a": True, "b": True}))
        columns, rows = wh.slice("exact-match", "task.kind")
        assert columns == ["run_id", "run", "task.kind", "n", "mean", "low", "high"]
        got = {(r[1], r[2]): r[4] for r in rows}
        assert got == {
            ("base", "easy"): 1.0,
            ("cand", "easy"): 1.0,
            ("base", "hard"): 0.0,
            ("cand", "hard"): 1.0,
        }
        # Errors are kept, without a value, for counting.
        _, errs = wh.query("select run_id, reason from scores where outcome = 'error'")
        assert len(errs) == 2
        assert errs[0][1] == "timeout"
        _, models = wh.query("select model from runs order by model")
        assert [m for (m,) in models] == ["m-base", "m-cand"]


def test_reloading_a_run_replaces_it() -> None:
    data = results_file("base", "2026-10-01T00:00:00+00:00", {"a": True, "b": False})
    with Warehouse() as wh:
        wh.add_results(data)
        wh.add_results(data)
        assert wh.query("select count(*) from scores")[1] == [(3,)]


def test_server_runs_with_label_filter_and_pages() -> None:
    with Warehouse() as wh:
        loaded = wh.add_server_runs(FakeClient(), labels={"suite": "nightly"}, limit=10)
        assert loaded == ["run-0", "run-1", "run-2"]
        _, rows = wh.query("select count(*), count(distinct trial) from scores")
        assert rows == [(12, 2)]
        _, rows = wh.query("select status, labels->>'suite' from runs limit 1")
        assert rows == [("succeeded", "nightly")]
        _, sliced = wh.slice("quality", "lang")
        de = [r for r in sliced if r[2] == "de"]
        assert [round(r[4], 3) for r in de] == [0.1, 0.2, 0.3]
        assert all(r[3] == 2 and r[5] == r[4] == r[6] for r in de)  # two equal trials: zero width


def test_list_loading_reads_each_runs_model() -> None:
    with Warehouse() as wh:
        wh.add_server_runs(FakeClient(), limit=2)
        _, models = wh.query("select model from runs order by run_id")
        assert [m for (m,) in models] == ["model-0", "model-1"]


class AliasedClient(FakeClient):
    """One record, two aliased evaluators, one result per page: the record
    comes back on both pages, as the server sends it."""

    def list_run_results(self, run_id: str, *, page_token: str = "", **_: Any) -> dict[str, Any]:
        results = [
            {
                "recordId": "r0",
                "evaluator": "quality",
                "evaluatorRef": "builtin/exact-match@1.0.0",
                "outcome": "OUTCOME_SCORED",
                "scores": [{"name": "exact-match", "passed": True}],
            },
            {
                "recordId": "r0",
                "evaluator": "format",
                "evaluatorRef": "builtin/format-check@1.0.0",
                "outcome": "OUTCOME_SCORED",
                "scores": [{"name": "format-check", "passed": True}],
            },
        ]
        record = {"id": "r0", "metadata": {"lang": "en"}}
        i = int(page_token or 0)
        page: dict[str, Any] = {"results": [results[i]], "records": [record]}
        if i == 0:
            page["nextPageToken"] = "1"
        return page


def test_aliased_metrics_keep_their_alias_and_pages_count_once() -> None:
    with Warehouse() as wh:
        wh.add_server_runs(AliasedClient(), run_ids=["run-0"])
        _, metrics = wh.query("select distinct metric from scores order by metric")
        assert [m for (m,) in metrics] == ["format", "quality"]
        assert wh.query("select count(*) from records")[1] == [(1,)]
        _, sliced = wh.slice("quality", "lang")
        assert [(r[2], r[3]) for r in sliced] == [("en", 1)]


def test_slices_pair_each_trial_with_its_own_output() -> None:
    data = {
        "manifest": {"name": "t", "started_at": "2026-10-01T00:00:00+00:00"},
        "results": [
            {
                "record_id": "r",
                "trial": t,
                "evaluator": "exact-match",
                "outcome": "scored",
                "scores": [{"name": "exact-match", "passed": t == 1}],
            }
            for t in range(2)
        ],
        "records": [
            {"id": "r", "metadata": {"tier": tier}, "provenance": {"run": {"trial": t}}}
            for t, tier in enumerate(["free", "paid"])
        ],
    }
    with Warehouse() as wh:
        wh.add_results(data)
        _, sliced = wh.slice("exact-match", "tier")
        assert sorted((r[2], r[3], r[4]) for r in sliced) == [("free", 1, 0.0), ("paid", 1, 1.0)]


def test_limit_and_run_ids() -> None:
    with Warehouse() as wh:
        assert wh.add_server_runs(FakeClient(), limit=1) == ["run-0"]
        assert wh.add_server_runs(FakeClient(), run_ids=["run-3"]) == ["run-3"]


def test_cli_with_a_database_file(tmp_path: Any, capsys: Any) -> None:
    for i, outcome in enumerate([False, True]):
        data = results_file(f"r{i}", f"2026-10-0{i + 1}T00:00:00+00:00", {"a": True, "b": outcome})
        (tmp_path / f"r{i}.json").write_text(json.dumps(data))
    db = str(tmp_path / "an.duckdb")
    pattern = str(tmp_path / "r*.json")
    args = ["analyze", "slice", "exact-match", "--by", "task.kind", "--results", pattern]
    assert main([*args, "--db", db]) == 0
    assert "hard" in capsys.readouterr().out
    # Later queries read the same file without loading again.
    sql = "select count(distinct run_id) as runs from scores"
    assert main(["analyze", "query", sql, "--db", db, "--format", "json"]) == 0
    assert json.loads(capsys.readouterr().out) == [{"runs": 2}]
    assert main(["analyze", "slice", "nope", "--by", "x", "--db", db]) == 1
