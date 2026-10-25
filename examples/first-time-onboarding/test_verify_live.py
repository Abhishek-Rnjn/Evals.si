from __future__ import annotations

from pathlib import Path

import pytest
import yaml
from verify_live import ROOT, build_spec, check_result, configuration

from evalsi.runspec import load_spec


def test_live_configuration_requires_explicit_model(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", raising=False)
    with pytest.raises(ValueError, match="no model is billed by default"):
        configuration()


def test_live_configuration_names_credentials_without_copying_them(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("ANTHROPIC_DEFAULT_HAIKU_MODEL", "haiku-test")
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", "test-secret")
    monkeypatch.setenv("ANTHROPIC_BASE_URL", "http://localhost:1234/")
    assert configuration() == ("haiku-test", "ANTHROPIC_AUTH_TOKEN", "http://localhost:1234")


@pytest.mark.parametrize("database", [False, True])
def test_live_spec_needs_no_sandbox_and_preserves_reference_calls(
    database: bool, tmp_path: Path
) -> None:
    document = build_spec(
        "haiku-test", "TEST_KEY", "http://localhost:1234", stream=True, database=database
    )
    path = tmp_path / "run.yaml"
    path.write_text(yaml.safe_dump(document))
    load_spec(path)
    spec = document["spec"]
    assert spec["target"]["stream"] is True
    assert spec["harness"]["builtin"]["tools"]["sandbox"] is False
    assert len(spec["dataset"]["inline"]["records"]) == 3
    assert all(r["reference"]["json"]["tool_calls"] for r in spec["dataset"]["inline"]["records"])
    assert ("environment" in spec) is database
    assert "test-secret" not in repr(spec)


def test_live_result_rejects_success_shaped_missing_metrics() -> None:
    with pytest.raises(AssertionError, match=r"tool-call-accuracy\.recall"):
        check_result({"records": [{}, {}, {}], "summaries": []})


def test_live_result_rejects_evaluator_errors() -> None:
    with pytest.raises(AssertionError, match="Evaluator errors"):
        check_result({"records": [{}, {}, {}], "summaries": [{"errors": 1}]})


@pytest.mark.parametrize("name", ["recorded", "agent", "support"])
def test_kind_run_examples_parse(name: str) -> None:
    load_spec(ROOT / f"kind-{name}.yaml")
