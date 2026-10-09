from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx
import pytest

from evalsi.cli import EXIT_GATES_FAILED, main
from evalsi.cli_guardrails import guardrail_from_doc
from evalsi.client import Client

EXAMPLE = Path(__file__).parents[3] / "examples" / "guardrails" / "support.yaml"


def test_guardrail_from_doc() -> None:
    import yaml

    g = guardrail_from_doc(yaml.safe_load(EXAMPLE.read_text()))
    assert g["name"] == "support-chat"
    assert g["evaluators"] == [{"ref": "secret-leak"}, {"ref": "refusal"}]
    assert g["failureMode"] == "GUARDRAIL_FAILURE_MODE_CLOSED"
    assert g["mode"] == "GUARDRAIL_MODE_ENFORCE"
    assert g["timeout"] == "1.500s"
    assert g["redact"][3] == {"pattern": r"ACCT-\d{6,}", "replacement": "<ACCOUNT>"}
    doc = {"name": "x", "phases": ["response"], "mode": "audit", "redact": [{"builtin": "email"}]}
    assert guardrail_from_doc(doc)["phases"] == ["GUARDRAIL_PHASE_RESPONSE"]
    with pytest.raises(Exception, match="nope"):
        guardrail_from_doc({"name": "x", "nope": 1})


@pytest.fixture
def fake(monkeypatch: pytest.MonkeyPatch) -> list[tuple[str, dict[str, Any]]]:
    calls: list[tuple[str, dict[str, Any]]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        method = request.url.path.rsplit("/", 1)[-1]
        body = json.loads(request.content)
        calls.append((method, body))
        if method == "ApplyGuardrail":
            return httpx.Response(200, json=body)
        if method == "Check":
            blocked = "AKIA" in body["content"]
            return httpx.Response(
                200,
                json={
                    "decision": "GUARDRAIL_DECISION_BLOCK"
                    if blocked
                    else "GUARDRAIL_DECISION_MASK",
                    "reason": "secret-leak: found aws-access-key x1"
                    if blocked
                    else "redacted email x1",
                    "content": "hi <EMAIL>",
                    "scores": {"secret-leak": 0.0 if blocked else 1.0},
                },
            )
        return httpx.Response(404, json={"code": "not_found", "message": method})

    real = Client.__init__

    def init(self: Client, base_url: str, **kw: Any) -> None:
        kw["transport"] = httpx.MockTransport(handler)
        real(self, base_url, **kw)

    monkeypatch.setattr(Client, "__init__", init)
    monkeypatch.setenv("EVALSI_API_KEY", "evk_test")
    return calls


def test_cli_apply_and_check(
    fake: list[tuple[str, dict[str, Any]]], capsys: pytest.CaptureFixture[str]
) -> None:
    server = ["--server", "http://s"]
    assert main(["guardrails", "apply", "-f", str(EXAMPLE), *server]) == 0
    assert fake[-1][1]["guardrail"]["blockWhen"].startswith('scores["secret-leak"] < 1')

    args = ["guardrails", "check", "support-chat", "--project", "support", *server]
    assert main([*args, "--text", "hi a@b.co", "--label", "tier=free"]) == 0
    method, body = fake[-1]
    assert method == "Check"
    assert body["guardrail"] == "support-chat"
    assert body["phase"] == "GUARDRAIL_PHASE_REQUEST"
    assert body["labels"] == {"tier": "free"}
    out = capsys.readouterr().out
    assert "\nmask: redacted email x1\n" in out
    assert "hi <EMAIL>" in out

    assert main([*args, "--text", "AKIAABCDEFGHIJKLMNOP", "--phase", "response"]) == (
        EXIT_GATES_FAILED
    )
    assert "block: secret-leak" in capsys.readouterr().out

    # A file is checked inline, without storing it.
    assert main(["guardrails", "check", "-f", str(EXAMPLE), "--text", "x", *server]) == 0
    assert fake[-1][1]["inline"]["name"] == "support-chat"
    assert "guardrail" not in fake[-1][1]


def test_cli_credentials_list(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.url.path.endswith(".CatalogService/ListCredentials")
        assert json.loads(request.content) == {"project": "support"}
        return httpx.Response(
            200,
            json={
                "enforced": True,
                "grants": [
                    {"env": "OPENAI_API_KEY", "hosts": ["api.openai.com"], "allProjects": True},
                    {"env": "AGENT_TOKEN"},
                ],
                "judges": ["claude"],
            },
        )

    real = Client.__init__

    def init(self: Client, base_url: str, **kw: Any) -> None:
        kw["transport"] = httpx.MockTransport(handler)
        real(self, base_url, **kw)

    monkeypatch.setattr(Client, "__init__", init)
    monkeypatch.setenv("EVALSI_API_KEY", "evk_test")
    code = main(["credentials", "list", "--server", "http://evalsid", "--project", "support"])
    out = capsys.readouterr().out
    assert code == 0
    assert "OPENAI_API_KEY\tapi.openai.com\t(every project)" in out
    assert "AGENT_TOKEN\tany host" in out
    assert "judges: claude" in out
