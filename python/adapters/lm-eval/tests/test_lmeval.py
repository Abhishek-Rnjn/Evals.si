"""Contract tests: real lm-eval on a local task, scripted target, no network."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from lm_eval.api.instance import Instance
from lm_eval.tasks import TaskManager

from evalsi.datasets import load_records
from evalsi.targets import Generation
from evalsi.types import Content, Record, Usage
from evalsi_lmeval import EvalsiLM, evaluate, read_samples

CAPITALS = {"France": "Paris", "Japan": "Tokyo", "Kenya": "Nairobi"}


class ScriptedTarget:
    """Answers capital questions, getting Kenya wrong, and runs on past a newline."""

    def __init__(self) -> None:
        self.prompts: list[Record] = []
        self.closed = 0

    async def generate(self, record: Record) -> Generation:
        self.prompts.append(record)
        assert record.input is not None
        text = record.input.as_text()
        country = next(c for c in CAPITALS if f"capital of {c}" in text)
        answer = "Mombasa" if country == "Kenya" else CAPITALS[country]
        return Generation(output=Content(text=f" {answer}\nQ: and more"), usage=Usage())

    async def aclose(self) -> None:
        self.closed += 1


@pytest.fixture
def task_dir(tmp_path: Path) -> Path:
    data = tmp_path / "capitals.jsonl"
    data.write_text(
        "".join(json.dumps({"country": c, "capital": k}) + "\n" for c, k in CAPITALS.items())
    )
    (tmp_path / "capitals.yaml").write_text(
        f"""
task: evalsi_capitals
dataset_path: json
dataset_kwargs:
  data_files:
    test: {data}
test_split: test
output_type: generate_until
doc_to_text: "Q: What is the capital of {{{{country}}}}?\\nA:"
doc_to_target: "{{{{capital}}}}"
generation_kwargs:
  until: ["\\n"]
  max_gen_toks: 16
filter_list:
  - name: strip
    filter:
      - function: remove_whitespace
      - function: take_first
metric_list:
  - metric: exact_match
    aggregation: mean
    higher_is_better: true
"""
    )
    return tmp_path


def test_evaluate_runs_lm_eval_tasks_through_a_target(task_dir: Path) -> None:
    target = ScriptedTarget()
    records, results = evaluate(
        ["evalsi_capitals"], target, task_manager=TaskManager(include_path=str(task_dir))
    )
    assert results["evalsi_capitals"]["exact_match,strip"] == pytest.approx(2 / 3)
    by_id = {r.id: r for r in records}
    assert set(by_id) == {"evalsi_capitals/0", "evalsi_capitals/1", "evalsi_capitals/2"}
    kenya = next(r for r in records if r.reference and r.reference.as_text() == "Nairobi")
    assert kenya.output is not None
    assert kenya.output.as_text() == "Mombasa"  # cut at the stop sequence, then filtered
    assert kenya.metadata["lm_eval_metrics"] == {"exact_match": 0.0}
    assert "capital of Kenya" in (kenya.input.as_text() if kenya.input else "")
    assert target.prompts


def test_stop_sequences_and_chat_prompts() -> None:
    target = ScriptedTarget()
    lm = EvalsiLM(target)
    chat = lm.apply_chat_template([{"role": "user", "content": "What is the capital of Japan?"}])
    requests = [
        Instance(
            "generate_until", {}, ("Q: What is the capital of France?\nA:", {"until": ["\n"]}), 0
        ),
        Instance("generate_until", {}, (chat, {"until": "\n", "max_gen_toks": 8}), 1),
    ]
    assert lm.generate_until(requests) == [" Paris", " Tokyo"]
    assert target.prompts[-1].input is not None
    assert target.prompts[-1].input.messages is not None
    with pytest.raises(NotImplementedError, match="log-probabilities"):
        lm.loglikelihood(requests)


def test_logged_samples_import(tmp_path: Path) -> None:
    sample = {
        "doc_id": 7,
        "doc": {"question": "2+2?"},
        "target": "4",
        "arguments": {"gen_args_0": {"arg_0": "Q: 2+2?\nA:", "arg_1": {"until": ["\n"]}}},
        "resps": [["4"]],
        "filtered_resps": ["4"],
        "filter": "strict-match",
        "metrics": ["exact_match"],
        "exact_match": 1.0,
    }
    path = tmp_path / "samples_gsm8k_2026-10-05T14-20-44.123456.jsonl"
    path.write_text(json.dumps(sample) + "\n")
    (record,) = load_records(f"lm-eval://{path}?doc=true")
    assert record.id == "gsm8k/7"
    assert record.output is not None
    assert record.output.as_text() == "4"
    assert record.reference is not None
    assert record.reference.as_text() == "4"
    assert record.metadata["lm_eval_metrics"] == {"exact_match": 1.0}
    assert record.metadata["lm_eval_doc"] == {"question": "2+2?"}
    assert read_samples(str(path), task="other")[0].id == "other/7"
