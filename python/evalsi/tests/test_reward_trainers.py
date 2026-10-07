from __future__ import annotations

import json
import threading
import urllib.request
from pathlib import Path
from typing import Any

import pytest

from evalsi import rewards
from evalsi.cli import main
from evalsi.rewards import openrlhf, trl, verl

SPEC = {
    "name": "math",
    "components": [
        {
            "ref": "format-check",
            "name": "format",
            "weight": 0.2,
            "gate": True,
            "params": {"pattern": "(?s).*<answer>.*</answer>\\s*"},
        },
        {"ref": "math-equiv", "name": "correct", "weight": 0.8},
    ],
}


def test_trl_reward_funcs_log_components() -> None:
    funcs, weights = trl.reward_funcs(SPEC)
    assert weights == [1.0, 0.0, 0.0]
    assert [f.__name__ for f in funcs] == ["math", "math/format", "math/correct"]
    batch: dict[str, Any] = {
        "prompts": ["1+1?", "2+2?"],
        "completions": ["<answer>2</answer>", "<answer>5</answer>"],
        "answer": ["2", "4"],
    }
    total, fmt, correct = (f(**batch) for f in funcs)
    assert total == pytest.approx([1.0, 0.2])
    assert fmt == [1.0, 1.0]
    assert correct == [1.0, 0.0]
    # A component function on a batch the total has not seen scores it itself.
    other = {**batch, "completions": ["2", "<answer>4</answer>"]}
    assert funcs[2](**other) == [1.0, 1.0]
    assert funcs[1](**other) == [0.0, 1.0]
    funcs, weights = trl.reward_funcs(SPEC, breakdown=False)
    assert len(funcs) == 1
    with pytest.raises(KeyError):
        trl.component_func(rewards.load(SPEC), "nope")


def test_trl_skipped_components_are_none() -> None:
    funcs, _ = trl.reward_funcs(SPEC)
    # No reference column: math-equiv is skipped, which adds nothing to the total.
    assert funcs[0](completions=["<answer>2</answer>"]) == [0.2]
    assert funcs[2](completions=["<answer>2</answer>"]) == [None]


def test_verl_module(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    spec = tmp_path / "reward.json"
    spec.write_text(json.dumps(SPEC))
    out = verl.compute_score("gsm8k", "<answer>\\boxed{3}</answer>", "3", spec=str(spec))
    assert out == {"score": pytest.approx(1.0), "format": 1.0, "correct": 1.0}
    monkeypatch.setenv("EVALSI_REWARD_SPEC", str(spec))
    batch = verl.compute_score_batch(["a", "b"], ["<answer>1</answer>", "1"], ["1", "1"], None)
    assert [b["score"] for b in batch] == pytest.approx([1.0, 0.0])
    # Loaded once per process.
    assert verl.get_reward() is verl.get_reward(str(spec))
    monkeypatch.delenv("EVALSI_REWARD_SPEC")
    with pytest.raises(ValueError, match="no reward spec"):
        verl.get_reward()


def test_openrlhf_server() -> None:
    server = openrlhf.make_server(rewards.load(SPEC), port=0)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        url = f"http://127.0.0.1:{server.server_address[1]}/get_reward"
        body = {
            "query": ["Q: 1+1? A: <answer>2</answer>", "Q: 2+2? A: <answer>5</answer>"],
            "prompts": ["Q: 1+1? A: ", "Q: 2+2? A: "],
            "labels": ["2", "4"],
        }
        request = urllib.request.Request(
            url, json.dumps(body).encode(), {"Content-Type": "application/json"}
        )
        with urllib.request.urlopen(request) as response:
            out = json.load(response)
        assert out["rewards"] == pytest.approx([1.0, 0.2])
        assert out["scores"] == out["rewards"]
        assert out["extra_logs"] == {"format": [1.0, 1.0], "correct": [1.0, 0.0]}
        bad = urllib.request.Request(url, b"{}", {"Content-Type": "application/json"})
        with pytest.raises(urllib.error.HTTPError) as err:
            urllib.request.urlopen(bad)
        assert err.value.code == 500
    finally:
        server.shutdown()
        server.server_close()


def test_openrlhf_completion_split() -> None:
    assert openrlhf.completion_of("prompt answer", "prompt ") == "answer"
    assert openrlhf.completion_of("other", "prompt") == "other"


def test_cli_rewards_score(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    spec = tmp_path / "reward.yaml"
    spec.write_text(json.dumps(SPEC))
    data = tmp_path / "rollouts.jsonl"
    data.write_text(
        '{"id": "a", "completion": "<answer>2</answer>", "answer": "2"}\n'
        '{"id": "b", "completion": "2", "answer": "2"}\n'
    )
    assert main(["rewards", "score", "--spec", str(spec), "--data", str(data)]) == 0
    captured = capsys.readouterr()
    lines = [json.loads(line) for line in captured.out.splitlines()]
    assert [line["id"] for line in lines] == ["a", "b"]
    assert [line["total"] for line in lines] == pytest.approx([1.0, 0.0])
    assert lines[1]["gated"] is True
    assert "2 rollouts, mean reward 0.5000, 1 gated" in captured.err
