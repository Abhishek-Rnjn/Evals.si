"""``evalsi gate``: one gate run per release, exit 0 on pass and 3 on a failed gate."""

from __future__ import annotations

from pathlib import Path
from typing import Any, ClassVar

import pytest

from evalsi.cli import gate_key, gated_spec, main
from evalsi.client import ServerError

SPEC = """\
apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata:
  name: support-bot-gate
  labels: {evals.si/project: studio, team: agents}
spec:
  target:
    agent:
      http: {url: "http://support-bot-candidate:8080/invoke", output_path: output}
  dataset: {path: tests.jsonl}
  evaluators: [{ref: exact-match}]
  gates: [{metric: exact-match, min: 0.8}]
"""


class FakeClient:
    created: ClassVar[list[dict[str, Any]]] = []
    status: ClassVar[str] = "RUN_STATUS_SUCCEEDED"
    conflict: ClassVar[bool] = False

    def __init__(self, *_: Any, **__: Any) -> None:
        pass

    def __enter__(self) -> FakeClient:
        return self

    def __exit__(self, *_: Any) -> None:
        pass

    def create_run(self, spec: dict[str, Any], **kwargs: Any) -> dict[str, Any]:
        if FakeClient.conflict:
            raise ServerError("already_exists", "idempotency key reused with another request")
        FakeClient.created.append({"spec": spec, **kwargs})
        return {"id": "run-1", "status": "RUN_STATUS_RUNNING"}

    def watch_run(self, run_id: str) -> Any:
        yield {"run": {"id": run_id, "status": FakeClient.status, "summaries": [], "gates": []}}


@pytest.fixture
def fake(monkeypatch: pytest.MonkeyPatch) -> type[FakeClient]:
    import evalsi.client

    FakeClient.created, FakeClient.status, FakeClient.conflict = [], "RUN_STATUS_SUCCEEDED", False
    monkeypatch.setattr(evalsi.client, "Client", FakeClient)
    return FakeClient


@pytest.fixture
def spec_file(tmp_path: Path) -> Path:
    (tmp_path / "tests.jsonl").write_text('{"id": "1", "input": "hi", "reference": "hello"}\n')
    path = tmp_path / "gate.yaml"
    path.write_text(SPEC)
    return path


def _gate(spec_file: Path, *extra: str) -> int:
    return main(["gate", "-f", str(spec_file), "--server", "http://evalsi.test", "--quiet", *extra])


def test_gates_one_release_against_its_candidate(spec_file: Path, fake: type[FakeClient]) -> None:
    code = _gate(spec_file, "--release", "v7", "--target-url", "http://bot-v7:8080/invoke")
    assert code == 0
    (created,) = fake.created
    assert created["spec"]["target"]["agent"]["http"]["url"] == "http://bot-v7:8080/invoke"
    assert created["labels"] == {"team": "agents", "release": "v7"}
    assert created["project"] == "studio"
    # A pipeline retry of the same release sends the same key, so the server
    # returns the run it already made.
    assert created["idempotency_key"] == gate_key("studio", "support-bot-gate", "v7")


def test_exit_codes(spec_file: Path, fake: type[FakeClient]) -> None:
    fake.status = "RUN_STATUS_FAILED"
    assert _gate(spec_file, "--release", "v8") == 3
    fake.status = "RUN_STATUS_ERROR"
    assert _gate(spec_file, "--release", "v8") == 1


def test_a_release_gated_with_another_spec_is_refused(
    spec_file: Path, fake: type[FakeClient], capsys: pytest.CaptureFixture[str]
) -> None:
    fake.conflict = True
    assert _gate(spec_file, "--release", "v7", "--set", "trials=1") == 1
    assert "release v7 was already gated with a different spec" in capsys.readouterr().err


def test_keys_differ_by_release_name_and_project() -> None:
    key = gate_key("studio", "support-bot-gate", "v7")
    assert key == gate_key("studio", "support-bot-gate", "v7")
    assert (
        len(
            {
                key,
                gate_key("studio", "support-bot-gate", "v8"),
                gate_key("other", "support-bot-gate", "v7"),
                gate_key("studio", "other-gate", "v7"),
            }
        )
        == 4
    )
    assert len(key) <= 128  # the server's limit


def test_overrides() -> None:
    spec: dict[str, Any] = {"target": {"http": {"url": "http://a"}}, "trials": 1}
    out = gated_spec(spec, target_url="http://b", sets=["trials=5", "budget.max_cost_usd=2.5"])
    assert out == {
        "target": {"http": {"url": "http://b"}},
        "trials": 5,
        "budget": {"max_cost_usd": 2.5},
    }
    assert spec["target"]["http"]["url"] == "http://a"  # a copy
    with pytest.raises(ValueError, match="HTTP target"):
        gated_spec({"target": {"model": {}}}, target_url="http://b", sets=[])
    with pytest.raises(ValueError, match="PATH=VALUE"):
        gated_spec(spec, target_url=None, sets=["trials"])
