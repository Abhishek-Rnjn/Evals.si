from __future__ import annotations

import asyncio
import json
import math
from pathlib import Path
from typing import Any

import pytest

from evalsi import EvaluatorConfigError, Pack, Registry, Score, evaluator, rewards
from evalsi.registry import discover_packs
from evalsi.rewards import ComponentResult, RewardError, RewardSpecError
from evalsi.types import Record

VECTORS = Path(__file__).parents[3] / "testdata" / "reward_vectors.json"


@pytest.mark.parametrize(
    "case", json.loads(VECTORS.read_text())["cases"], ids=lambda c: str(c["name"])
)
def test_compose_matches_the_shared_vectors(case: dict[str, Any]) -> None:
    spec = rewards.RewardSpec.from_dict(case["spec"])
    got = {k: ComponentResult(**v) for k, v in case["components"].items()}
    if case["want"].get("error"):
        with pytest.raises(RewardError):
            rewards.compose(spec, got)
        return
    result = rewards.compose(spec, got)
    if case["want"]["total"] is None:
        assert result.total is None
    else:
        assert result.total == pytest.approx(case["want"]["total"])
    assert result.gated is case["want"]["gated"]


CALLS: list[str] = []


@evaluator(name="test/count", version="1.0.0")
def count(record: Record) -> Score:
    CALLS.append(record.id)
    return Score(number=len(record.output.as_text()) if record.output else 0)


@evaluator(name="test/flaky", version="1.0.0")
def flaky(record: Record) -> Score:
    raise RuntimeError("sandbox unavailable")


def registry() -> Registry:
    packs = discover_packs()
    packs.append(Pack(name="test", description="", evaluators=[count, flaky]))
    return Registry(packs)


SPEC = """\
apiVersion: evals.si/v1alpha1
kind: RewardSpec
metadata: {name: math-grpo}
spec:
  components:
    - {ref: format-check, weight: 0.2, gate: true, params: {pattern: '.*<answer>.*</answer>\\s*'}}
    - {ref: math-equiv, weight: 0.8}
"""


def test_trl_signature(tmp_path: Path) -> None:
    path = tmp_path / "reward.yaml"
    path.write_text(SPEC)
    reward = rewards.load(path)
    assert reward.__name__ == "math-grpo"
    totals = reward(
        prompts=["q1", "q2", "q3"],
        completions=["<answer>\\boxed{4}</answer>", "<answer>5</answer>", "4"],
        answer=["4", "4", "4"],
        trainer_state=object(),  # not a per-sample column; ignored
    )
    assert totals == pytest.approx([1.0, 0.2, 0.0])
    assert reward.last_results[2].gated is True
    assert reward.last_results[1].breakdown() == {"format-check": 1.0, "math-equiv": 0.0}


def test_trl_conversational_completions() -> None:
    reward = rewards.load({"components": [{"ref": "math-equiv"}]})
    completions = [[{"role": "assistant", "content": "\\boxed{7}"}]]
    prompts = [[{"role": "user", "content": "3+4?"}]]
    assert reward(prompts=prompts, completions=completions, solution=["7"]) == [1.0]


def test_verl_compute_score() -> None:
    import yaml

    reward = rewards.load(yaml.safe_load(SPEC))
    out = reward.compute_score("gsm8k", "<answer>\\boxed{72}</answer>", "72")
    assert out == {"score": pytest.approx(1.0), "format-check": 1.0, "math-equiv": 1.0}
    batch = reward.compute_score_batch(
        ["gsm8k", "gsm8k"], ["<answer>1</answer>", "<answer>2</answer>"], ["1", "1"], None
    )
    assert [b["score"] for b in batch] == pytest.approx([1.0, 0.2])


def test_cache_skips_repeated_work() -> None:
    CALLS.clear()
    reward = rewards.load({"components": [{"ref": "test/count"}]}, registry=registry())
    rollouts = [{"id": "a", "completion": "xx"}, {"id": "b", "completion": "xx"}]
    assert [r.total for r in reward.score(rollouts)] == [2.0, 2.0]
    assert [r.total for r in reward.score(rollouts)] == [2.0, 2.0]
    # Same content in both rollouts and both batches: the record id does not matter.
    assert len(CALLS) <= 2
    assert reward.last_results[0].components["count"].cached is True
    off = rewards.load({"cache": False, "components": [{"ref": "test/count"}]}, registry=registry())
    CALLS.clear()
    off.score(rollouts)
    off.score(rollouts)
    assert len(CALLS) == 4


def test_errors_follow_on_error() -> None:
    spec: dict[str, Any] = {"components": [{"ref": "test/count"}, {"ref": "test/flaky"}]}
    with pytest.raises(RewardError, match="sandbox unavailable"):
        rewards.load(spec, registry=registry()).score([{"completion": "x"}])
    spec["onError"] = "zero"
    (result,) = rewards.load(spec, registry=registry()).score([{"completion": "x"}])
    assert result.total == 1.0
    assert result.components["flaky"].status == "error"
    spec["onError"] = "none"
    reward = rewards.load(spec, registry=registry())
    assert reward(completions=["x"]) == [None]
    verl = reward.compute_score("d", "x", "")
    assert math.isnan(verl["score"])


def test_spec_validation() -> None:
    with pytest.raises(RewardSpecError, match="at least one component"):
        rewards.load({"components": []})
    with pytest.raises(RewardSpecError, match="appear twice"):
        rewards.load({"components": ["math-equiv", "math-equiv"]})
    with pytest.raises(RewardSpecError, match="unknown RewardSpec fields"):
        rewards.load({"components": ["math-equiv"], "weights": [1]})
    with pytest.raises(RewardSpecError, match="kind"):
        rewards.load({"kind": "EvalRun", "spec": {}})
    with pytest.raises(RewardSpecError, match="does not run code"):
        rewards.load({"components": [{"ref": "math-equiv", "sandbox": {"minIsolation": "vm"}}]})
    with pytest.raises(RewardSpecError, match="no metric"):
        rewards.load({"components": [{"ref": "math-equiv", "metric": "nope"}]})
    with pytest.raises(EvaluatorConfigError, match="unknown evaluator"):
        rewards.load({"components": ["no-such-evaluator"]})


def test_sandbox_settings_become_params() -> None:
    reward = rewards.load(
        {"components": [{"ref": "code-exec-tests", "sandbox": {"minIsolation": "namespaced"}}]}
    )
    assert isinstance(reward, rewards.LocalReward)
    assert reward._instances[0].params["min_isolation"] == "namespaced"


def test_spec_round_trips() -> None:
    import yaml

    spec = rewards.load_spec(yaml.safe_load(SPEC))
    again = rewards.RewardSpec.from_dict(spec.to_dict())
    assert again == spec
    assert again.fingerprint() == spec.fingerprint()


@pytest.mark.usefixtures("fake_sandbox")
def test_code_reward_end_to_end() -> None:
    reward = rewards.load(
        {
            "components": [
                {"ref": "python-syntax", "gate": True, "weight": 0},
                {"ref": "code-exec-tests", "weight": 1},
            ]
        }
    )
    tests = [{"input": "2", "output": "4"}, {"input": "3", "output": "6"}]
    completions = [
        "```python\nprint(int(input()) * 2)\n```",
        "```python\nprint(int(input()) + 2)\n```",
        "```python\ndef (\n```",
    ]
    totals = reward(completions=completions, tests=[tests] * 3)
    assert totals == pytest.approx([1.0, 0.5, 0.0])


def test_ascore_runs_inside_an_event_loop() -> None:
    reward = rewards.load({"components": ["math-equiv"]})

    async def main() -> list[float | None]:
        results = await reward.ascore([{"completion": "1", "answer": "1"}])
        # The sync form works from inside a running loop too (notebooks).
        sync = reward(completions=["2"], answer=["1"])
        return [results[0].total, *sync]

    assert asyncio.run(main()) == [1.0, 0.0]


def test_server_spec_and_results() -> None:
    import yaml

    from evalsi.rewards import _result_from_json, spec_to_proto_json

    spec = rewards.load_spec(yaml.safe_load(SPEC))
    body = spec_to_proto_json(spec)
    assert body["onError"] == "REWARD_ON_ERROR_RAISE"
    assert body["components"][0] == {
        "ref": "format-check",
        "name": "format-check",
        "weight": 0.2,
        "gate": True,
        "threshold": 1.0,
        "params": {"pattern": ".*<answer>.*</answer>\\s*"},
    }
    result = _result_from_json(
        {
            "rolloutId": "0",
            "gated": True,
            "total": 0,
            "components": {
                "format-check": {"status": "REWARD_COMPONENT_STATUS_SCORED", "cached": True},
                "math-equiv": {"status": "REWARD_COMPONENT_STATUS_ERROR", "reason": "x"},
            },
        }
    )
    assert result.total == 0.0
    assert result.gated is True
    assert result.components["format-check"].cached is True
    assert result.components["math-equiv"].status == "error"
    with pytest.raises(RewardSpecError, match="judge"):
        rewards.load(
            {"judge": {"provider": "anthropic", "model": "m"}, "components": ["math-equiv"]},
            server="http://localhost:1",
        )
    with pytest.raises(EvaluatorConfigError, match="server judge"):
        rewards.load({"judge": "local", "components": ["llm-judge"]})
