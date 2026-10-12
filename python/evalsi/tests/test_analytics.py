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


def test_slice_parity_fixture() -> None:
    """The same runs, metadata and scores as ``seedSlice`` in
    ``internal/runs/slice_test.go``, which expects these numbers from the
    server's ``SliceRuns``: the two implementations share one oracle."""
    em = "builtin/exact-match@1.0.0"

    def score(rid: str, trial: int, ok: bool) -> dict[str, Any]:
        return {
            "record_id": rid,
            "trial": trial,
            "evaluator": "exact-match",
            "evaluator_ref": em,
            "outcome": "scored",
            "scores": [{"name": "exact-match", "passed": ok}],
        }

    data = {
        "manifest": {"name": "first", "started_at": "2026-10-01T00:00:00+00:00"},
        "results": [
            score("r0", 0, True),
            score("r0", 1, False),
            score("r1", 0, False),
            score("r2", 0, True),
            score("r3", 0, True),
            score("r4", 0, True),
            score("r5", 0, False),
            # A dataset-scope result has no record.
            {**score("", 0, True), "record_id": ""},
            # An errored result counts for nothing, whatever it holds.
            {**score("r3", 0, True), "outcome": "error"},
        ],
        "records": [
            {"id": "r0", "metadata": {"topic": "billing", "tier": 1}},
            {"id": "r1", "metadata": {"topic": "billing", "tier": 1}},
            {"id": "r2", "metadata": {"topic": "billing", "tier": 2}},
            {"id": "r3", "metadata": {"topic": "refunds", "vip": True}},
            {"id": "r4", "metadata": {"nested": {"team": "core"}}},
            {"id": "r5", "metadata": {"topic": None}},
            # r0's output in trial 0 carries other metadata than its dataset record.
            {"id": "r0", "metadata": {"topic": "shipping"}, "provenance": {"run": {"trial": 0}}},
        ],
    }
    with Warehouse() as wh:
        wh.add_results(data)

        def groups(by: str) -> dict[str, tuple[int, float]]:
            _, rows = wh.slice("exact-match", by)
            return {r[2]: (r[3], r[4]) for r in rows}

        assert groups("topic") == {
            "shipping": (1, 1.0),
            "billing": (3, pytest.approx(1 / 3)),
            "refunds": (1, 1.0),
            "(none)": (3, pytest.approx(2 / 3)),
        }
        assert {k: v[0] for k, v in groups("tier").items()} == {"1": 2, "2": 1, "(none)": 5}
        assert {k: v[0] for k, v in groups("vip").items()} == {"true": 1, "(none)": 7}
        assert {k: v[0] for k, v in groups("nested.team").items()} == {"core": 1, "(none)": 7}


def test_slice_on_the_server(monkeypatch: Any, capsys: Any) -> None:
    calls: list[dict[str, Any]] = []

    class Server:
        def __enter__(self) -> "Server":
            return self

        def __exit__(self, *exc: object) -> None:
            return None

        def slice_runs(self, metric: str, by: str, **kw: Any) -> dict[str, Any]:
            calls.append({"metric": metric, "by": by, **kw})
            if metric == "nope":
                return {"rows": [], "runs": 1}
            return {
                "rows": [
                    {"runId": "r1", "runName": "a", "key": "easy", "n": "2", "mean": 0.5},
                    {
                        "runId": "r1",
                        "runName": "a",
                        "key": "hard",
                        "n": "3",
                        "mean": 0.25,
                        "low": 0.1,
                        "high": 0.4,
                    },
                ],
                "runs": 1,
            }

    monkeypatch.setattr("evalsi.cli.server_client", lambda args: Server())
    base = ["analyze", "slice", "exact-match", "--by", "task.kind", "--server", "http://s"]
    argv = [*base, "--on-server", "--project", "p", "--label", "env=ci", "--format", "json"]
    assert main(argv) == 0
    assert json.loads(capsys.readouterr().out)[1] == {
        "run_id": "r1",
        "run": "a",
        "task.kind": "hard",
        "n": 3,
        "mean": 0.25,
        "low": 0.1,
        "high": 0.4,
    }
    assert calls[0]["labels"] == {"env": "ci"}
    assert calls[0]["project"] == "p"
    assert calls[0]["limit"] == 0
    assert main([*base, "--on-server", "--limit", "7", "--level", "0.99"]) == 0
    assert (calls[1]["limit"], calls[1]["level"]) == (7, 0.99)
    capsys.readouterr()
    assert (
        main(["analyze", "slice", "nope", "--by", "x", "--server", "http://s", "--on-server"]) == 1
    )
    # The server computes it from its own runs: nothing local to mix in.
    assert main(["analyze", "slice", "m", "--by", "x", "--on-server"]) == 1
    assert "--on-server needs --server" in capsys.readouterr().err
    assert main([*base, "--on-server", "--results", "x.json"]) == 1
    assert "drop --results" in capsys.readouterr().err


def test_archived_server_runs_are_skipped_when_listing_and_refused_by_name() -> None:
    client = FakeClient()
    client.runs[1]["archivedAt"] = "2026-10-12T00:00:00Z"
    with Warehouse() as wh:
        # Newest first from the fake is run-0, run-1, ...; run-1 has no results left.
        assert wh.add_server_runs(client, limit=3) == ["run-0", "run-2", "run-3"]
        with pytest.raises(ValueError, match="run-1 was archived"):
            wh.add_server_runs(client, run_ids=["run-1"])
