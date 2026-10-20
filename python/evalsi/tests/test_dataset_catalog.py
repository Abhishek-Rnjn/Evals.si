from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx
import pytest
from google.protobuf import json_format

from evalsi.cli import main
from evalsi.client import Client
from evalsi.convert import record_from_proto
from evalsi.dataset_snapshots import read_snapshot
from evalsi.datasets import DatasetError
from evalsi.run import load_dataset
from evalsi.runspec import parse_spec
from evalsi.v1alpha1 import record_pb2, run_pb2

PIN = "a" * 64
REF = {"project": "support", "name": "refunds", "version": "v1", "sha256": PIN}
COMMON = ["--server", "http://unused", "--project", "support"]


@pytest.mark.parametrize("selection", ["bad", "a" * 24 + "@0", "a" * 24 + "@-1"])
def test_publish_bad_failure_selection_makes_no_request(
    monkeypatch: pytest.MonkeyPatch, selection: str
) -> None:
    from evalsi import cli_dataset_catalog

    calls = []

    def send(request: httpx.Request) -> httpx.Response:
        calls.append(request)
        return httpx.Response(200, json={})

    monkeypatch.setattr(
        cli_dataset_catalog,
        "server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    assert (
        main(
            [
                "dataset",
                "publish",
                *COMMON,
                "--name",
                "refunds",
                "--version",
                "v1",
                "--owner",
                "support",
                "--failure",
                selection,
            ]
        )
        == 1
    )
    assert not calls


@pytest.mark.parametrize("rows", [[{"output": "unidentified"}], [{"id": 42}], {"id": "one"}])
def test_publication_requires_explicit_string_ids_before_network(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, rows: Any
) -> None:
    from evalsi import cli_dataset_catalog

    def send(_: httpx.Request) -> httpx.Response:
        pytest.fail("invalid rows reached the network")

    monkeypatch.setattr(
        cli_dataset_catalog,
        "server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    source = tmp_path / "input.json"
    source.write_text(json.dumps(rows))
    assert (
        main(
            [
                "dataset",
                "publish",
                *COMMON,
                "--name",
                "refunds",
                "--version",
                "v1",
                "--owner",
                "support",
                "--records",
                str(source),
            ]
        )
        == 1
    )


def test_cli_sdk_publication_review_history_and_diff_pins(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    from evalsi import cli_dataset_catalog

    calls: list[tuple[str, dict[str, Any]]] = []

    def send(request: httpx.Request) -> httpx.Response:
        method = request.url.path.rsplit("/", 1)[-1]
        calls.append((method, json.loads(request.content)))
        return httpx.Response(200, json={"beforeSha256": PIN, "afterSha256": "b" * 64})

    monkeypatch.setattr(
        cli_dataset_catalog,
        "server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    source = tmp_path / "records.json"
    source.write_text(
        json.dumps(
            [
                {
                    "id": "refund",
                    "input": {"z": 2, "a": 1},
                    "provenance": {"trace": {"traceId": "a" * 32}},
                }
            ]
        )
    )
    assert (
        main(
            [
                "dataset",
                "publish",
                *COMMON,
                "--name",
                "refunds",
                "--version",
                "v1",
                "--owner",
                "support",
                "--records",
                str(source),
                "--label",
                "classification=internal",
            ]
        )
        == 0
    )
    assert calls[-1][1]["content"]["records"][0]["id"] == "refund"
    assert calls[-1][1]["content"]["records"][0]["provenance"]["trace"]["traceId"] == "a" * 32
    assert calls[-1][1]["content"]["labels"] == {"classification": "internal"}
    assert (
        main(
            [
                "dataset",
                "review-version",
                "refunds",
                "v1",
                *COMMON,
                "--sha256",
                PIN,
                "--revision",
                "1",
                "--decision",
                "approved",
                "--note",
                "Checked",
            ]
        )
        == 0
    )
    assert calls[-1][1]["expectedRevision"] == "1"
    assert calls[-1][1]["sha256"] == PIN
    assert calls[-1][1]["decision"] == "DATASET_VERSION_STATE_APPROVED"
    assert main(["dataset", "version-history", "refunds", "v1", *COMMON, "--page-token", "2"]) == 0
    assert calls[-1][1]["pageToken"] == "2"
    diff = [
        "dataset",
        "version-diff",
        "refunds",
        "v1",
        "v2",
        *COMMON,
        "--before-sha256",
        PIN,
        "--after-sha256",
        "b" * 64,
    ]
    assert main(diff) == 0
    assert main([*diff, "--fail-on-change"]) == 3
    assert calls[-1][1]["beforeSha256"] == PIN
    assert calls[-1][1]["afterSha256"] == "b" * 64
    assert "beforeSha256" in capsys.readouterr().out


def test_download_keeps_catalog_and_source_identity_and_never_overwrites(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    from evalsi import cli_dataset_catalog

    def send(_: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "reference": REF,
                "dataset": {
                    "sha256": PIN,
                    "state": "DATASET_VERSION_STATE_APPROVED",
                    "reviewedBy": "reviewer",
                    "content": {
                        "name": "refunds",
                        "version": "v1",
                        "owner": "support",
                        "split": "held-out",
                        "records": [
                            {
                                "id": "one",
                                "reference": {"text": "safe"},
                                "provenance": {"trace": {"traceId": "f" * 32}},
                            }
                        ],
                    },
                },
            },
        )

    monkeypatch.setattr(
        cli_dataset_catalog,
        "server_client",
        lambda _: Client("http://unused", transport=httpx.MockTransport(send)),
    )
    destination = tmp_path / "portable.json"
    arguments = [
        "dataset",
        "fetch-version",
        "refunds",
        "v1",
        *COMMON,
        "-o",
        str(destination),
        "--sha256",
    ]
    assert main([*arguments, "b" * 64]) == 1
    assert not destination.exists()
    assert main([*arguments, PIN]) == 0
    snapshot = read_snapshot(destination)
    assert snapshot.sha256 != PIN  # Portable JSON and catalog protobuf pins are separate contracts.
    assert snapshot.records[0]["provenance"]["catalog"] == REF
    assert snapshot.records[0]["provenance"]["record_source"]["trace"]["traceId"] == "f" * 32
    original = destination.read_bytes()
    assert main([*arguments, PIN]) == 1
    assert destination.read_bytes() == original
    assert not list(tmp_path.glob(".evalsi-snapshot-*"))


def test_catalog_json_execution_order_is_stable_and_arrays_tool_arguments_preserved() -> None:
    wire = {
        "id": "structured",
        "metadata": {"evalsi.dataset-version": {"sha256": PIN}},
        "input": {"json": {"z": [{"y": 2, "a": 1}], "a": {"z": 3, "b": 4}}},
        "output": {
            "messages": {
                "messages": [
                    {
                        "role": "assistant",
                        "toolCalls": [{"name": "refund", "arguments": '{"z":2,"a":1}'}],
                    }
                ]
            }
        },
        "trajectory": {
            "steps": [{"input": {"json": {"z": 2, "a": 1}}, "output": {"json": [2, 1]}}]
        },
    }
    record = record_from_proto(json_format.ParseDict(wire, record_pb2.Record()))
    assert record.input is not None
    assert list(record.input.json) == ["a", "z"]
    assert list(record.input.json["z"][0]) == ["a", "y"]
    assert record.output is not None
    assert record.output.messages is not None
    assert record.output.messages[0].tool_calls[0].arguments == '{"z":2,"a":1}'
    assert record.trajectory is not None
    assert record.trajectory.steps[0].input is not None
    assert list(record.trajectory.steps[0].input.json) == ["a", "z"]
    assert record.trajectory.steps[0].output is not None
    assert record.trajectory.steps[0].output.json == [2, 1]


def test_catalog_spec_parses_but_embedded_loading_requires_server(tmp_path: Path) -> None:
    spec = parse_spec(
        {
            "apiVersion": "evals.si/v1alpha1",
            "kind": "EvalRun",
            "spec": {"dataset": {"catalog": REF}, "evaluators": [{"ref": "exact-match"}]},
        }
    )
    assert spec.spec.dataset.catalog == run_pb2.DatasetVersionRef(**REF)
    with pytest.raises(DatasetError, match="--server"):
        load_dataset(spec.spec.dataset, tmp_path)
