from __future__ import annotations

import contextlib
import json
import os
import re
import stat
import sys
import threading
import time
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest

from evalsi.cli import EXIT_GATES_FAILED, main
from evalsi.training import (
    VLLM,
    CheckpointEvaluator,
    LoRA,
    RegressionGate,
    Served,
    ServingError,
    find,
)
from evalsi.training.callback import CheckpointRunner
from evalsi.training.checkpoints import localize
from evalsi.training.loop import StepResult
from evalsi.training.serving import Operator, resource_name

QUESTIONS = [(f"q{i}", f"What is {i} + {i}?", str(2 * i)) for i in range(12)]


class FakeModels:
    """An OpenAI-compatible server. Model names say how many questions they
    get wrong: 'run-base' none, 'run-<step>' as set in ``wrong``."""

    def __init__(self) -> None:
        self.wrong: dict[str, int] = {}
        self.adapters: list[tuple[str, str]] = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def _json(self, status: int, body: Any) -> None:
                data = json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self) -> None:
                self._json(200, {"data": []})

            def do_POST(self) -> None:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if self.path.endswith("/load_lora_adapter"):
                    outer.adapters.append(("load", body["lora_name"]))
                    if "missing" in body["lora_path"]:
                        return self._json(404, {"error": "no such adapter"})
                    return self._json(200, {})
                if self.path.endswith("/unload_lora_adapter"):
                    outer.adapters.append(("unload", body["lora_name"]))
                    return self._json(200, {})
                prompt = body["messages"][-1]["content"]
                index = next(i for i, (_, q, _) in enumerate(QUESTIONS) if q == prompt)
                wrong = outer.wrong.get(body["model"], 0)
                answer = QUESTIONS[index][2] if index >= wrong else "no idea"
                self._json(
                    200,
                    {
                        "model": body["model"],
                        "choices": [{"message": {"content": answer}, "finish_reason": "stop"}],
                        "usage": {"prompt_tokens": 5, "completion_tokens": 1},
                    },
                )

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.base_url = f"http://127.0.0.1:{self.server.server_address[1]}/v1"

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def models() -> Iterator[FakeModels]:
    m = FakeModels()
    yield m
    m.close()


class Named:
    """Serves every checkpoint as its own model name on the fake server."""

    def __init__(self, base_url: str) -> None:
        self.base_url = base_url
        self.served: list[str] = []

    @contextlib.contextmanager
    def serve(self, checkpoint: str, name: str) -> Iterator[Served]:
        self.served.append(checkpoint)
        yield Served(self.base_url, name)


def write_spec(tmp_path: Path) -> Path:
    spec = {
        "apiVersion": "evals.si/v1alpha1",
        "kind": "EvalRun",
        "metadata": {"name": "ckpt"},
        "spec": {
            "target": {"connector": "openai-compatible", "model": "x", "base_url": "http://unused"},
            "dataset": {
                "inline": {
                    "records": [
                        {"id": i, "input": {"text": q}, "reference": {"text": a}}
                        for i, q, a in QUESTIONS
                    ]
                }
            },
            "evaluators": [{"ref": "exact-match"}],
        },
    }
    path = tmp_path / "run.yaml"
    path.write_text(json.dumps(spec))
    return path


def test_learning_curve_and_regression(tmp_path: Path, models: FakeModels) -> None:
    models.wrong = {"run-100": 1, "run-200": 8}
    serving = Named(models.base_url)
    evaluator = CheckpointEvaluator(
        write_spec(tmp_path),
        training_run="run",
        serving=serving,
        log_dir=tmp_path / "log",
        regression=["exact-match:0.1"],
    )
    base = evaluator.evaluate_base("Qwen/base")
    assert base.summaries["exact-match"]["mean"] == 1.0
    small = evaluator.evaluate("/ckpt/checkpoint-100", 100)
    (c,) = small.comparisons
    assert c.diff == pytest.approx(-1 / 12)
    assert c.paired_n == 12
    assert not c.regressed  # one question of twelve is not a significant drop
    big = evaluator.evaluate("/ckpt/checkpoint-200", 200)
    (c,) = big.comparisons
    assert c.significant
    assert c.regressed
    assert big.regressed
    assert not big.passed
    assert serving.served == ["Qwen/base", "/ckpt/checkpoint-100", "/ckpt/checkpoint-200"]

    curve = evaluator.curve()
    assert [r.step for r in curve] == ["base", "100", "200"]
    # A fresh evaluator reads the same history from the log directory.
    again = CheckpointEvaluator(
        write_spec(tmp_path), training_run="run", serving=serving, log_dir=tmp_path / "log"
    )
    assert [r.step for r in again.history()] == ["base", "100", "200"]


def test_serving_failures_are_recorded_not_scored(tmp_path: Path) -> None:
    class Broken:
        @contextlib.contextmanager
        def serve(self, checkpoint: str, name: str) -> Iterator[Served]:
            raise ServingError("out of GPU memory")
            yield Served("", "")  # pragma: no cover

    evaluator = CheckpointEvaluator(
        write_spec(tmp_path), training_run="r", serving=Broken(), log_dir=tmp_path
    )
    result = evaluator.evaluate("/ckpt", 5)
    assert "out of GPU memory" in result.error
    assert not result.passed
    assert result.summaries == {}


def test_lora_serving(models: FakeModels) -> None:
    with LoRA(models.base_url).serve("/ckpt/checkpoint-7", "run-7") as served:
        assert served == Served(models.base_url, "run-7")
    assert models.adapters == [("load", "run-7"), ("unload", "run-7")]
    with (
        pytest.raises(ServingError, match="enable-lora"),
        LoRA(models.base_url).serve("/missing", "run-8"),
    ):
        pass


FAKE_VLLM = """\
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
args = sys.argv[1:]
if "broken" in args[1]:
    print("CUDA out of memory", flush=True)
    sys.exit(3)
port = int(args[args.index("--port") + 1])
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        data = json.dumps({"data": [{"id": args[args.index("--served-model-name") + 1]}]}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
HTTPServer(("127.0.0.1", port), H).serve_forever()
"""


def test_vllm_process(tmp_path: Path) -> None:
    script = tmp_path / "vllm"
    script.write_text(f"#!{sys.executable}\n{FAKE_VLLM}")
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    vllm = VLLM(command=[str(script), "serve"], startup_timeout_s=20, log_dir=str(tmp_path))
    import httpx

    with vllm.serve("/ckpt/checkpoint-3", "run-3") as served:
        assert served.model == "run-3"
        assert httpx.get(f"{served.base_url}/models").json()["data"][0]["id"] == "run-3"
    with pytest.raises(httpx.HTTPError):
        httpx.get(f"{served.base_url}/models", timeout=2)
    with pytest.raises(ServingError, match="CUDA out of memory"), vllm.serve("/broken", "x"):
        pass


def test_find_checkpoints(tmp_path: Path) -> None:
    for name, files in {
        "checkpoint-100": ["config.json", "model.safetensors"],
        "checkpoint-20": ["adapter_config.json"],
        "global_step_300": ["model.safetensors"],
        "checkpoint-400": ["optimizer.pt"],  # no model yet
        "runs": ["config.json"],
    }.items():
        (tmp_path / name).mkdir()
        for f in files:
            (tmp_path / name / f).write_text("{}")
    assert [c.step for c in find(tmp_path, settle_s=0)] == [20, 100, 300]
    # Just written: not settled yet.
    assert find(tmp_path, settle_s=3600) == []
    old = time.time() - 120
    for p in tmp_path.rglob("*"):
        os.utime(p, (old, old))
    assert len(find(tmp_path, settle_s=60)) == 3
    assert find(tmp_path / "missing") == []
    assert localize(find(tmp_path, settle_s=0)[0], tmp_path / "cache") == str(
        tmp_path / "checkpoint-20"
    )


def test_regression_gate_parse() -> None:
    assert RegressionGate.parse("exact-match") == RegressionGate("exact-match", 0.0)
    assert RegressionGate.parse("judge:0.05") == RegressionGate("judge", 0.05)
    with pytest.raises(ValueError, match="metric"):
        RegressionGate.parse(":0.1")


def test_runner_evaluates_in_order_in_the_background(tmp_path: Path, models: FakeModels) -> None:
    models.wrong = {"r-2": 12}
    evaluator = CheckpointEvaluator(
        write_spec(tmp_path),
        training_run="r",
        serving=Named(models.base_url),
        log_dir=tmp_path,
        regression=["exact-match"],
    )
    evaluator.evaluate_base("base")
    runner = CheckpointRunner(evaluator)
    runner.submit("/c1", 1)
    runner.submit("/c2", 2)
    runner.close()
    assert [r.step for r in runner.results] == ["1", "2"]
    assert runner.regressed


def test_cli_watch_and_curve(
    tmp_path: Path, models: FakeModels, capsys: pytest.CaptureFixture[str]
) -> None:
    out = tmp_path / "out"
    for step in (10, 20):
        (out / f"checkpoint-{step}").mkdir(parents=True)
        (out / f"checkpoint-{step}" / "config.json").write_text("{}")
    models.wrong = {"base-model": 0, "run-10": 0, "run-20": 12}
    spec = str(write_spec(tmp_path))
    log = str(tmp_path / "log")
    common = ["-f", spec, "--training-run", "run", "--log-dir", log, "--regression", "exact-match"]
    # Endpoint serving: every checkpoint is the same served model, named by --model.
    code = main(
        [
            "checkpoints",
            "watch",
            str(out),
            "--once",
            "--settle",
            "0",
            "--serve",
            "endpoint",
            "--base-url",
            models.base_url,
            "--model",
            "run-10",
            *common,
        ]
    )
    assert code == 0
    assert main(["checkpoints", "curve", *common]) == 0
    capsys.readouterr()
    # Re-evaluate step 20 against a model that gets everything wrong.
    code = main(
        [
            "checkpoints",
            "eval",
            str(out / "checkpoint-20"),
            "--step",
            "20",
            "--serve",
            "endpoint",
            "--base-url",
            models.base_url,
            "--model",
            "run-20",
            *common,
        ]
    )
    assert code == 0  # no base yet, so nothing to regress against
    code = main(
        [
            "checkpoints",
            "eval",
            "base-model",
            "--step",
            "base",
            "--serve",
            "endpoint",
            "--base-url",
            models.base_url,
            "--model",
            "base-model",
            *common,
        ]
    )
    assert code == 0
    assert main(["checkpoints", "curve", *common]) == EXIT_GATES_FAILED
    table = capsys.readouterr().out
    assert "regressed" in table
    assert "-1.000!" in table


class FakeMLflow:
    """The MLflow REST calls the registry watcher makes, serving two
    versions of a model through mlflow-artifacts."""

    def __init__(self) -> None:
        files = {
            "1/model/config.json": b'{"v": 1}',
            "2/model/config.json": b'{"v": 2}',
            "2/model/sub/weights.safetensors": b"w",
        }

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def _send(self, status: int, data: bytes) -> None:
                self.send_response(status)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self) -> None:
                from urllib.parse import parse_qs, urlparse

                url = urlparse(self.path)
                q = {k: v[0] for k, v in parse_qs(url.query).items()}
                if url.path.endswith("/model-versions/search"):
                    assert q["filter"] == "name='policy'"
                    versions = [
                        {
                            "version": "1",
                            "status": "READY",
                            "tags": [{"key": "step", "value": "100"}],
                        },
                        {"version": "2", "status": "READY", "tags": []},
                        {"version": "3", "status": "PENDING_REGISTRATION"},
                    ]
                    return self._send(200, json.dumps({"model_versions": versions}).encode())
                if url.path.endswith("/get-download-uri"):
                    uri = f"mlflow-artifacts:/{q['version']}/model"
                    return self._send(200, json.dumps({"artifact_uri": uri}).encode())
                prefix = "/api/2.0/mlflow-artifacts/artifacts"
                if url.path == prefix:
                    rel = q.get("path", "")
                    children: dict[str, bool] = {}
                    for name in files:
                        if name.startswith(rel + "/"):
                            head, _, rest = name[len(rel) + 1 :].partition("/")
                            children[head] = bool(rest)
                    listing = [{"path": k, "is_dir": d} for k, d in children.items()]
                    return self._send(200, json.dumps({"files": listing}).encode())
                if url.path.startswith(prefix + "/"):
                    name = url.path[len(prefix) + 1 :]
                    return self._send(200, files[name]) if name in files else self._send(404, b"")
                self._send(404, b"")

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"


def test_mlflow_registry_and_stop_file(
    tmp_path: Path, models: FakeModels, monkeypatch: pytest.MonkeyPatch
) -> None:
    from evalsi.training.checkpoints import find_mlflow, localize_mlflow

    mlflow = FakeMLflow()
    try:
        found = find_mlflow("policy", mlflow.url)
        assert [(c.step, c.location) for c in found] == [
            (2, "mlflow:2/model"),
            (100, "mlflow:1/model"),
        ]
        local = Path(localize_mlflow(found[0], mlflow.url, tmp_path / "cache"))
        assert (local / "config.json").read_text() == '{"v": 2}'
        assert (local / "sub" / "weights.safetensors").read_bytes() == b"w"

        monkeypatch.setenv("MLFLOW_TRACKING_URI", mlflow.url)
        models.wrong = {"base-model": 0, "run-x": 12}
        common = [
            "-f",
            str(write_spec(tmp_path)),
            "--training-run",
            "run",
            "--log-dir",
            str(tmp_path / "log"),
            "--regression",
            "exact-match",
        ]
        endpoint = ["--serve", "endpoint", "--base-url", models.base_url]
        assert (
            main(
                [
                    "checkpoints",
                    "eval",
                    "b",
                    "--step",
                    "base",
                    *endpoint,
                    "--model",
                    "base-model",
                    *common,
                ]
            )
            == 0
        )
        stop = tmp_path / "STOP"
        code = main(
            [
                "checkpoints",
                "watch",
                "mlflow:policy",
                "--once",
                "--stop-file",
                str(stop),
                *endpoint,
                "--model",
                "run-x",
                *common,
            ]
        )
        assert code == 0
        assert json.loads(stop.read_text()) == {"step": "100", "regressed": ["exact-match"]}
    finally:
        mlflow.server.shutdown()
        mlflow.server.server_close()


@pytest.mark.parametrize(
    ("status", "passed", "finished"),
    [
        ("RUN_STATUS_SUCCEEDED", True, True),
        ("RUN_STATUS_FAILED", False, True),
        ("RUN_STATUS_ERROR", False, True),
        ("RUN_STATUS_CANCELLED", False, True),
        ("RUN_STATUS_PENDING", False, False),
        ("RUN_STATUS_RUNNING", False, False),
        ("RUN_STATUS_UNSPECIFIED", False, False),
        ("RUN_STATUS_SOMETHING_NEW", False, False),
        (None, False, False),
    ],
)
def test_unfinished_server_runs_never_pass(
    status: str | None, passed: bool, finished: bool
) -> None:
    from evalsi.training import format_curve
    from evalsi.training.loop import _from_run

    run: dict[str, Any] = {"id": "r", "labels": {"training-step": "10"}}
    if status is not None:
        run["status"] = status
    result = _from_run(run, "")
    assert result.passed is passed
    assert result.finished is finished
    if not finished:
        assert "ok" not in format_curve([result]).splitlines()[1]
    # Saved results from before status was recorded are finished embedded runs.
    old = StepResult.from_dict({"training_run": "t", "step": "1", "checkpoint": ""})
    assert old.passed


FAKE_KUBECTL = """\
import json, os, sys
log = os.environ["KUBECTL_LOG"]
args = sys.argv[1:]
with open(log, "a") as f:
    f.write(json.dumps({"args": args, "stdin": sys.stdin.read() if "-" in args else ""}) + "\\n")
verb = args[2]
if verb == "wait" and os.environ.get("KUBECTL_FAIL") == "wait":
    sys.stderr.write("error: timed out waiting for the condition")
    sys.exit(1)
if verb == "delete":
    failures = int(os.environ.get("KUBECTL_DELETE_FAILURES", "0"))
    deletes = sum(1 for line in open(log) if '"delete"' in line)
    if deletes <= failures:
        sys.stderr.write("error: connection refused")
        sys.exit(1)
if verb == "get":
    status = {"endpoint": "http://evalsi-ckpt-x.ns.svc:8000/v1", "servedModelName": "run-3"}
    print(json.dumps({"status": status if os.environ.get("KUBECTL_FAIL") != "endpoint" else {}}))
"""


def _kubectl(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> tuple[str, Path]:
    script = tmp_path / "kubectl"
    script.write_text(f"#!{sys.executable}\n{FAKE_KUBECTL}")
    script.chmod(script.stat().st_mode | stat.S_IEXEC)
    log = tmp_path / "kubectl.log"
    monkeypatch.setenv("KUBECTL_LOG", str(log))
    return str(script), log


def _calls(log: Path) -> list[dict[str, Any]]:
    return [json.loads(line) for line in log.read_text().splitlines()]


def test_operator_serving_applies_waits_reads_and_deletes(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    kubectl, log = _kubectl(tmp_path, monkeypatch)
    operator = Operator(
        "team-a",
        "vllm/vllm-openai:v0.11.0",
        claim="ckpts",
        args=["--max-model-len", "4096"],
        resources={"nvidia.com/gpu": "1"},
        api_key_secret="vllm-key",
        kubectl=kubectl,
        timeout_s=120,
    )
    with operator.serve("run-1/checkpoint-200", "run-3") as served:
        assert (served.base_url, served.model) == ("http://evalsi-ckpt-x.ns.svc:8000/v1", "run-3")
        assert len(_calls(log)) == 3  # applied, waited, read; not yet deleted
    applied, waited, read, deleted = _calls(log)
    assert applied["args"][:4] == ["--namespace", "team-a", "apply", "-f"]
    manifest = json.loads(applied["stdin"])
    assert manifest["kind"] == "CheckpointServer"
    assert manifest["metadata"]["name"] == "run-3"
    assert manifest["spec"] == {
        "image": "vllm/vllm-openai:v0.11.0",
        "model": "/checkpoints/run-1/checkpoint-200",
        "servedModelName": "run-3",
        "checkpointClaim": "ckpts",
        "args": ["--max-model-len", "4096"],
        "resources": {"limits": {"nvidia.com/gpu": "1"}},
        "apiKeySecret": "vllm-key",
    }
    assert waited["args"][2:] == [
        "wait",
        "--for=condition=Ready",
        "checkpointservers.evals.si/run-3",
        "--timeout=120s",
    ]
    assert read["args"][2:4] == ["get", "checkpointservers.evals.si/run-3"]
    assert deleted["args"][2:] == [
        "delete",
        "checkpointservers.evals.si/run-3",
        "--ignore-not-found",
        "--wait=false",
    ]


def test_operator_serving_deletes_what_it_made_when_it_fails(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    kubectl, log = _kubectl(tmp_path, monkeypatch)
    operator = Operator("ns", "vllm:1", kubectl=kubectl, delete_backoff_s=0)
    monkeypatch.setenv("KUBECTL_FAIL", "wait")
    with (
        pytest.raises(ServingError, match="timed out waiting"),
        operator.serve("/c/step-1", "step-1"),
    ):
        pass
    assert [c["args"][2] for c in _calls(log)] == ["apply", "wait", "delete"]
    log.unlink()
    monkeypatch.setenv("KUBECTL_FAIL", "endpoint")
    with (
        pytest.raises(ServingError, match="reports no endpoint"),
        operator.serve("/c/step-1", "step-1"),
    ):
        pass
    assert _calls(log)[-1]["args"][2] == "delete"
    # An error in the evaluation itself still deletes it.
    log.unlink()
    monkeypatch.delenv("KUBECTL_FAIL")
    with pytest.raises(RuntimeError, match="boom"), operator.serve("/c/step-1", "step-1"):
        raise RuntimeError("boom")
    assert _calls(log)[-1]["args"][2] == "delete"
    with (
        pytest.raises(ServingError, match="no-such-kubectl"),
        Operator("ns", "vllm:1", kubectl="no-such-kubectl").serve("a", "a"),
    ):
        pass


def test_resource_names_are_dns_labels() -> None:
    assert resource_name("run-3") == "run-3"
    assert resource_name("checkpoint-200") == "checkpoint-200"
    odd = resource_name("Step_200/final")
    assert re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", odd)
    assert len(odd) <= 49
    assert odd != resource_name("step-200-final")  # the hash tells them apart
    assert resource_name("///").startswith("ckpt-")


def test_cli_serve_operator_flags() -> None:
    import argparse

    from evalsi.cli_training import _serving

    def args(**kw: Any) -> argparse.Namespace:
        base: dict[str, Any] = {
            "serve": "operator",
            "namespace": "ns",
            "image": "vllm:1",
            "checkpoint_claim": "pvc",
            "gpus": "2",
            "api_key_secret": "key",
            "vllm_arg": ["--dtype", "bfloat16"],
        }
        return argparse.Namespace(**{**base, **kw})

    op = _serving(args())
    assert isinstance(op, Operator)
    assert (op.namespace, op.image, op.claim) == ("ns", "vllm:1", "pvc")
    assert op.resources == {"nvidia.com/gpu": "2"}
    assert (op.api_key_secret, list(op.args)) == ("key", ["--dtype", "bfloat16"])
    assert _serving(args(gpus="")).resources == {}
    with pytest.raises(ValueError, match="--namespace and --image"):
        _serving(args(image=None))


def test_operator_serving_retries_the_delete_and_reports_a_server_left_behind(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    kubectl, log = _kubectl(tmp_path, monkeypatch)
    operator = Operator("ns", "vllm:1", kubectl=kubectl, delete_backoff_s=0)

    def deletes() -> int:
        return sum(1 for c in _calls(log) if c["args"][2] == "delete")

    # A transient failure is retried and the evaluation is unaffected.
    monkeypatch.setenv("KUBECTL_DELETE_FAILURES", "2")
    with operator.serve("/c/step-1", "step-1"):
        pass
    assert deletes() == 3
    log.unlink()

    # A delete that keeps failing is an error after a good evaluation: the
    # server is holding GPUs, and nobody else will delete it.
    monkeypatch.setenv("KUBECTL_DELETE_FAILURES", "99")
    with (
        pytest.raises(ServingError, match=r"could not delete .* by hand"),
        operator.serve("/c/step-1", "step-1"),
    ):
        pass
    assert deletes() == 3
    log.unlink()

    # After a failed evaluation the evaluation's error stays the error, with the leak noted.
    with pytest.raises(RuntimeError, match="boom") as caught, operator.serve("/c/step-1", "step-1"):
        raise RuntimeError("boom")
    assert any("left running" in note for note in caught.value.__notes__)
