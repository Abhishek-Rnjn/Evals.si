from __future__ import annotations

import json
from pathlib import Path
from typing import Any, ClassVar

import pytest

from evalsi.cli import load_policy, main
from evalsi.cli_sources import load_source, source_from_doc

SOURCE = """\
apiVersion: evals.si/v1alpha1
kind: TraceSource
metadata:
  name: studio-mlflow
  labels: {evals.si/project: studio, team: agents}
spec:
  connector: mlflow
  endpoint: https://mlflow.example.com
  locations: ["1", "2"]
  credentials: {env: STUDIO_MLFLOW_TOKEN}
  backfill: {since: 720h}
  poll: {interval: 30s, maxRecordsPerSecond: 50}
  max_trace_duration: 45m
  policies: [studio-agents]
  writeBack: {enabled: true}
  traceLabels: {studio: standalone}
"""


def test_load_source(tmp_path: Path) -> None:
    path = tmp_path / "source.yaml"
    path.write_text(SOURCE)
    src = load_source(str(path))
    assert src["name"] == "studio-mlflow"
    assert src["project"] == "studio"
    # Prefixed labels stay behind, as for policies and runs.
    assert src["labels"] == {"team": "agents"}
    assert src["locations"] == ["1", "2"]
    assert src["credentials"] == {"env": "STUDIO_MLFLOW_TOKEN"}
    # Go-style durations become protobuf JSON seconds.
    assert src["backfill"]["since"] == "2592000s"
    assert src["poll"] == {"interval": "30s", "maxRecordsPerSecond": 50}
    assert src["maxTraceDuration"] == "2700s"
    assert src["writeBack"] == {"enabled": True}


def test_source_from_doc_rejects_mistakes() -> None:
    with pytest.raises(ValueError, match="kind must be TraceSource"):
        source_from_doc({"kind": "OnlineEvalPolicy", "spec": {}})
    with pytest.raises(ValueError, match="spec"):
        source_from_doc({"kind": "TraceSource"})
    with pytest.raises(ValueError, match="invalid source"):
        source_from_doc({"kind": "TraceSource", "spec": {"connectr": "mlflow"}})


def test_policy_durations(tmp_path: Path) -> None:
    # The integration guide's policy writes window: 1h.
    path = tmp_path / "policy.yaml"
    path.write_text(
        "kind: OnlineEvalPolicy\nmetadata: {name: p}\n"
        "spec: {stages: [{evaluators: [{ref: tool-errors}]}], window: 1h}\n"
    )
    assert load_policy(str(path))["window"] == "3600s"


class FakeClient:
    calls: ClassVar[list[tuple[str, str, dict[str, Any]]]] = []

    def __init__(self, *_: Any, **__: Any) -> None:
        pass

    def __enter__(self) -> FakeClient:
        return self

    def __exit__(self, *_: Any) -> None:
        pass

    def call(self, service: str, method: str, body: dict[str, Any]) -> dict[str, Any]:
        FakeClient.calls.append((service, method, body))
        source = {
            "name": "studio-mlflow",
            "project": "studio",
            "connector": "mlflow",
            "status": {
                "phase": "SOURCE_PHASE_TAILING",
                "lagSeconds": 12.4,
                "pulled": "7",
                "scored": "5",
                "watermark": "2026-10-01T00:00:00Z",
            },
        }
        if method == "ListSources":
            return {"sources": [source]}
        return {"source": source}


@pytest.fixture
def fake(monkeypatch: pytest.MonkeyPatch) -> type[FakeClient]:
    import evalsi.client

    FakeClient.calls = []
    monkeypatch.setattr(evalsi.client, "Client", FakeClient)
    return FakeClient


def test_cli_commands(
    tmp_path: Path, fake: type[FakeClient], capsys: pytest.CaptureFixture[str]
) -> None:
    path = tmp_path / "source.yaml"
    path.write_text(SOURCE)
    server = ["--server", "http://evalsid"]
    assert main(["source", "apply", "-f", str(path), "--dry-run", *server]) == 0
    _, method, body = fake.calls[-1]
    assert method == "ApplySource"
    assert body["validateOnly"] is True
    assert body["source"]["name"] == "studio-mlflow"
    assert "valid: source studio/studio-mlflow" in capsys.readouterr().out

    assert main(["source", "list", "--project", "studio", *server]) == 0
    out = capsys.readouterr().out
    for want in ("studio/studio-mlflow", "tailing", "12s"):
        assert want in out

    assert main(["source", "list", "--format", "json", *server]) == 0
    assert json.loads(capsys.readouterr().out)[0]["name"] == "studio-mlflow"

    for action, method in [
        ("get", "GetSource"),
        ("pause", "PauseSource"),
        ("resume", "ResumeSource"),
        ("delete", "DeleteSource"),
    ]:
        assert main(["source", action, "studio-mlflow", "--project", "studio", *server]) == 0
        assert fake.calls[-1][1:] == (method, {"project": "studio", "name": "studio-mlflow"})

    since = "2026-09-01T00:00:00Z"
    assert main(["source", "backfill", "studio-mlflow", "--since", since, *server]) == 0
    assert fake.calls[-1][2]["since"] == since
    assert main(["source", "backfill", "studio-mlflow", "--since", "2026-09-01", *server]) == 1
    assert "time zone" in capsys.readouterr().err


@pytest.mark.parametrize(
    ("given", "want"),
    [
        ("720h", "2592000s"),
        ("250ms", "0.25s"),
        ("1.5h", "5400s"),
        ("30s", "30s"),
        ("2160h", "7776000s"),
    ],
)
def test_duration_is_fixed_point(given: str, want: str) -> None:
    from evalsi.runspec import _duration

    assert _duration(given) == want
