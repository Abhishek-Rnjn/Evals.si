from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx
import pytest

from evalsi.cli import main
from evalsi.client import Client


@pytest.mark.parametrize(
    "selection",
    ["bad", "A" * 24 + "@1", "a" * 24 + "@0", "a" * 24 + "@-1", "a" * 24 + "@9223372036854775808"],
)
def test_invalid_export_selection_never_writes_an_artifact(
    tmp_path: Path,
    selection: str,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls = []

    def send(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(200, json={})

    monkeypatch.setattr(
        "evalsi.cli.server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    # cli_inbox imports the helper directly.
    monkeypatch.setattr(
        "evalsi.cli_inbox.server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    destination = tmp_path / "out.json"
    assert (
        main(
            [
                "inbox",
                "export",
                "--server",
                "http://unused",
                "--case",
                selection,
                "-o",
                str(destination),
                "--name",
                "test",
                "--version",
                "v1",
                "--owner",
                "test",
            ]
        )
        == 1
    )
    assert not destination.exists()
    assert not calls


def test_export_rejects_duplicate_ids_and_server_errors_without_publication(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from evalsi import cli_inbox

    case = "a" * 24 + "@3"
    calls: list[dict[str, Any]] = []

    def send(request: httpx.Request) -> httpx.Response:
        calls.append(json.loads(request.content))
        return httpx.Response(409, json={"code": "aborted", "message": "revision changed"})

    monkeypatch.setattr(
        cli_inbox,
        "server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    destination = tmp_path / "out.json"
    arguments = [
        "inbox",
        "export",
        "--server",
        "http://unused",
        "--case",
        case,
        "-o",
        str(destination),
        "--name",
        "test",
        "--version",
        "v1",
        "--owner",
        "test",
    ]
    assert main([*arguments, "--case", case]) == 1
    assert not calls
    assert main(arguments) == 1
    assert calls[0]["selections"] == [{"id": "a" * 24, "revision": "3"}]
    assert not destination.exists()


def test_exported_snapshot_never_replaces_existing_files(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    from evalsi import cli_inbox

    destination = tmp_path / "keep.json"
    destination.write_text("keep")

    def send(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "records": [
                    {
                        "id": "failure/" + "a" * 24,
                        "input": {"text": "case"},
                        "reference": {"text": "reviewed"},
                        "metadata": {"evalsi.failure": {"revision": "3"}},
                    }
                ]
            },
        )

    monkeypatch.setattr(
        cli_inbox,
        "server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    assert (
        main(
            [
                "inbox",
                "export",
                "--server",
                "http://unused",
                "--case",
                "a" * 24 + "@3",
                "-o",
                str(destination),
                "--name",
                "test",
                "--version",
                "v1",
                "--owner",
                "test",
            ]
        )
        == 1
    )
    assert destination.read_text() == "keep"
    assert not list(tmp_path.glob(".evalsi-snapshot-*"))


def test_cli_capture_edit_review_and_history_preserve_explicit_revisions(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    from evalsi import cli_inbox

    calls: list[tuple[str, dict[str, Any]]] = []
    case_id = "a" * 24

    def send(request: httpx.Request) -> httpx.Response:
        method = request.url.path.rsplit("/", 1)[-1]
        body = json.loads(request.content)
        calls.append((method, body))
        if method == "ListFailureEvents":
            return httpx.Response(200, json={"events": [], "nextPageToken": "2"})
        return httpx.Response(200, json={"failure": {"id": case_id, "revision": "2"}})

    monkeypatch.setattr(
        cli_inbox,
        "server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    common = ["--server", "http://unused", "--project", "support"]
    assert (
        main(
            [
                "inbox",
                "capture",
                "b" * 32,
                "--title",
                "Incident",
                "--reason",
                "Wrong amount",
                *common,
            ]
        )
        == 0
    )
    case_file = tmp_path / "edited.json"
    case_file.write_text(
        json.dumps(
            {
                "record": {"reference": {"text": "approval required"}},
                "title": "Incident",
                "reason": "Wrong amount",
                "status": "FAILURE_STATUS_IN_REVIEW",
            }
        )
    )
    assert main(["inbox", "edit", case_id, "--revision", "1", "-f", str(case_file), *common]) == 0
    assert (
        main(
            [
                "inbox",
                "review",
                case_id,
                "--revision",
                "2",
                "--decision",
                "approved",
                "--note",
                "Checked",
                *common,
            ]
        )
        == 0
    )
    assert calls[1][1]["expectedRevision"] == "1"
    assert calls[2][1]["expectedRevision"] == "2"
    assert calls[2][1]["decision"] == "FAILURE_STATUS_APPROVED"
    capsys.readouterr()
    assert main(["inbox", "history", case_id, "--page-token", "3", *common]) == 0
    assert json.loads(capsys.readouterr().out)["nextPageToken"] == "2"
    assert calls[-1][1]["pageToken"] == "3"
