"""lm-evaluation-harness through Evals.si.

- ``EvalsiLM`` is an lm-eval model (registered as ``evalsi``) that generates
  through an Evals.si target connector (OpenAI-compatible or Anthropic), so
  benchmark runs share the connectors, keys and limits of every other run::

      lm_eval --model evalsi --tasks gsm8k \\
          --model_args connector=openai-compatible,model=qwen3,base_url=http://vllm:8000/v1

  Chat APIs do not return log-probabilities of arbitrary text, so only
  generative tasks (``generate_until``) are supported; multiple-choice tasks
  that rank continuations by log-likelihood need a local model.
- ``evaluate(tasks, target)`` runs tasks and returns the samples as records,
  with lm-eval's per-sample metrics in metadata.
- ``lm-eval://samples_<task>_<date>.jsonl`` (or ``read_samples``) imports the
  samples lm-eval logs with ``--log_samples``, so any Evals.si evaluator can
  re-score them.
"""

from __future__ import annotations

import asyncio
import dataclasses
import json
import re
from collections.abc import Iterable, Mapping
from pathlib import Path
from typing import Any

from lm_eval.api.model import LM
from lm_eval.api.registry import register_model

from evalsi.targets import Target, TargetConfig, create_target, generate_all
from evalsi.types import Content, Message, Record

__version__ = "0.1.0"
UPSTREAM = "lm-eval==0.4.13"


def _messages_or_text(context: str) -> Content:
    """lm-eval passes chat-templated prompts as the JSON our ``apply_chat_template`` made."""
    if context.startswith("["):
        try:
            chat = json.loads(context)
        except json.JSONDecodeError:
            chat = None
        if isinstance(chat, list) and all(isinstance(m, dict) and "role" in m for m in chat):
            return Content(messages=[Message(role=m["role"], content=m["content"]) for m in chat])
    return Content(text=context)


def _cut(text: str, until: Iterable[str]) -> str:
    """Truncate at the first stop sequence, as lm-eval's own API models do."""
    for stop in until:
        if stop:
            text = text.split(stop, 1)[0]
    return text


@register_model("evalsi")
class EvalsiLM(LM):  # type: ignore[misc]
    """An lm-eval model that generates through an Evals.si target."""

    def __init__(
        self,
        target: Target | None = None,
        *,
        concurrency: int = 8,
        **config: Any,
    ) -> None:
        super().__init__()
        self.config = TargetConfig(**config) if target is None else None
        self._fixed = target
        self.concurrency = int(concurrency)

    @property
    def tokenizer_name(self) -> str:
        return "evalsi-chat"

    def apply_chat_template(
        self, chat_history: list[dict[str, str]], add_generation_prompt: bool = True
    ) -> str:
        return json.dumps(chat_history, ensure_ascii=False)

    def chat_template(self, chat_template: bool | str = False) -> str:
        return ""

    def _target(self, max_tokens: int | None) -> Target:
        if self._fixed is not None:
            return self._fixed
        assert self.config is not None
        config = self.config
        if max_tokens:
            config = dataclasses.replace(config, max_tokens=int(max_tokens))
        return create_target(config)

    def generate_until(self, requests: list[Any], disable_tqdm: bool = False) -> list[str]:
        # Requests sharing generation settings go out together, concurrently.
        groups: dict[int | None, list[int]] = {}
        for i, request in enumerate(requests):
            gen_kwargs = request.args[1] if len(request.args) > 1 else {}
            groups.setdefault(gen_kwargs.get("max_gen_toks"), []).append(i)
        out = [""] * len(requests)
        for max_tokens, indexes in groups.items():
            records = [
                Record(id=str(i), input=_messages_or_text(requests[i].args[0])) for i in indexes
            ]
            target = self._target(max_tokens)
            generations = asyncio.run(_generate(target, records, self.concurrency))
            for i, generation in zip(indexes, generations, strict=True):
                if generation.error and generation.output is None:
                    raise RuntimeError(f"target failed on request {i}: {generation.error}")
                text = generation.output.as_text() if generation.output else ""
                gen_kwargs = requests[i].args[1] if len(requests[i].args) > 1 else {}
                out[i] = _cut(text, _stops(gen_kwargs.get("until")))
        return out

    def loglikelihood(self, requests: list[Any], disable_tqdm: bool = False) -> Any:
        raise NotImplementedError(
            "Evals.si targets are chat APIs without prompt log-probabilities; "
            "use generative tasks (generate_until), or run this task on a local model"
        )

    def loglikelihood_rolling(self, requests: list[Any], disable_tqdm: bool = False) -> Any:
        raise NotImplementedError(
            "Evals.si targets are chat APIs without prompt log-probabilities; "
            "perplexity tasks need a local model"
        )


def _stops(until: Any) -> list[str]:
    if until is None:
        return []
    return [until] if isinstance(until, str) else [str(u) for u in until]


async def _generate(target: Target, records: list[Record], concurrency: int) -> list[Any]:
    try:
        return await generate_all(target, records, concurrency=concurrency)
    finally:
        if hasattr(target, "aclose"):
            await target.aclose()


# --- samples as records ---

_SAMPLES_FILE = re.compile(r"^samples_(?P<task>.+)_\d{4}-\d{2}-\d{2}T[\d.-]+\.jsonl$")


def _prompt(arguments: Any) -> str:
    """The prompt from either the in-memory form ([(ctx, kwargs)]) or the
    logged form ({"gen_args_0": {"arg_0": ctx, ...}})."""
    if isinstance(arguments, Mapping):
        first = arguments.get("gen_args_0") or {}
        return str(first.get("arg_0", ""))
    if isinstance(arguments, list | tuple) and arguments:
        first = arguments[0]
        return str(first[0] if isinstance(first, list | tuple) else first)
    return ""


def _response(filtered: Any) -> Content | None:
    if isinstance(filtered, list | tuple) and len(filtered) == 1:
        filtered = filtered[0]
    if filtered is None or filtered == "":
        return None
    if isinstance(filtered, str):
        return Content(text=filtered)
    return Content.from_value(filtered)


def sample_to_record(sample: Mapping[str, Any], task: str, *, doc: bool = False) -> Record:
    metrics = {m: sample[m] for m in sample.get("metrics", []) if m in sample}
    metadata: dict[str, Any] = {
        "lm_eval_task": task,
        "lm_eval_doc_id": sample.get("doc_id"),
        "lm_eval_filter": sample.get("filter"),
        "lm_eval_metrics": metrics,
    }
    if doc:
        metadata["lm_eval_doc"] = sample.get("doc")
    target = sample.get("target")
    prompt = _prompt(sample.get("arguments"))
    return Record(
        id=f"{task}/{sample.get('doc_id')}",
        input=_messages_or_text(prompt) if prompt else None,
        output=_response(sample.get("filtered_resps")),
        reference=None if target in (None, "") else Content(text=str(target)),
        metadata=metadata,
    )


def read_samples(path: str, *, task: str = "", doc: str | bool = False) -> list[Record]:
    """Records from a ``--log_samples`` file; the task name comes from the file
    name unless given. ``doc=true`` keeps each source document in metadata."""
    file = Path(path)
    if not task:
        match = _SAMPLES_FILE.match(file.name)
        task = match.group("task") if match else file.stem
    keep_doc = doc if isinstance(doc, bool) else doc.lower() in ("1", "true", "yes")
    records = []
    with file.open(encoding="utf-8") as handle:
        for line in handle:
            if line.strip():
                records.append(sample_to_record(json.loads(line), task, doc=keep_doc))
    return records


def evaluate(
    tasks: list[str],
    target: TargetConfig | Target,
    *,
    limit: int | float | None = None,
    num_fewshot: int | None = None,
    apply_chat_template: bool = False,
    concurrency: int = 8,
    **kwargs: Any,
) -> tuple[list[Record], dict[str, Any]]:
    """Run lm-eval tasks against ``target``; returns (records, lm-eval's results).

    Each record carries lm-eval's per-sample metrics in metadata, and the
    results dict holds its aggregate scores.
    """
    import lm_eval

    if isinstance(target, TargetConfig):
        lm = EvalsiLM(concurrency=concurrency, **dataclasses.asdict(target))
    else:
        lm = EvalsiLM(target, concurrency=concurrency)
    results = lm_eval.simple_evaluate(
        model=lm,
        tasks=tasks,
        limit=limit,
        num_fewshot=num_fewshot,
        apply_chat_template=apply_chat_template,
        log_samples=True,
        **kwargs,
    )
    if results is None:  # only on non-zero ranks in distributed runs
        return [], {}
    records = [
        sample_to_record(sample, task)
        for task, samples in results.get("samples", {}).items()
        for sample in samples
    ]
    return records, results["results"]


__all__ = ["EvalsiLM", "evaluate", "read_samples", "sample_to_record"]
