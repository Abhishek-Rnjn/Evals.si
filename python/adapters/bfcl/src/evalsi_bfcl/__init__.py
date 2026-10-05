"""The Berkeley Function Calling Leaderboard (BFCL v4) as Evals.si tasks.

The importer ``bfcl://<category>`` loads a single-turn category with BFCL's
own loaders (prompts, function docs with BFCL's language hints, and possible
answers)::

    dataset: {uri: "bfcl://simple_python?ids=simple_python_0,simple_python_1"}
    harness: {external: {python: "evalsi_bfcl:BFCLHarness"}}

``BFCLHarness`` offers the entry's functions to the target model as tools
(converted as BFCL does for function-calling models: ``.`` becomes ``_`` in
names, BFCL types become JSON Schema types), takes one model turn and grades
the calls with BFCL's AST checker, exactly as BFCL scores function-calling
("FC") models. Irrelevance categories pass when the model calls nothing,
``live_relevance`` when it calls something. The verdict is the record's
check, so ``task-success`` reports BFCL accuracy; tool calls are never
executed.

Categories: simple_python, simple_java, simple_javascript, multiple,
parallel, parallel_multiple, irrelevance and their live_ counterparts, and
live_relevance. Multi-turn, memory, web-search and format-sensitivity
categories need BFCL's own executable environments and are not supported.

Options: ``ids`` (comma-separated entry ids), ``limit`` (first N entries).
"""

from __future__ import annotations

import json
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from google.protobuf import json_format

from evalsi.run import target_config
from evalsi.types import Content, Message, Step, TaskCheck, ToolCall
from evalsi.v1alpha1 import agent_pb2
from evalsi_harness import BuiltinHarness, FinalEvent, HarnessContext, Task, TaskError
from evalsi_harness.events import Event, StepEvent, span_id
from evalsi_harness.harness import LiveTask
from evalsi_harness.models import ToolSpec, create_model

SUPPORTED = (
    "simple_python",
    "simple_java",
    "simple_javascript",
    "multiple",
    "parallel",
    "parallel_multiple",
    "irrelevance",
    "live_simple",
    "live_multiple",
    "live_parallel",
    "live_parallel_multiple",
    "live_irrelevance",
    "live_relevance",
)
# A BFCL model config whose function names are compared as given: the
# harness maps tool names back to the original (dotted) names itself.
CHECKER_MODEL = "gorilla-openfunctions-v2"


def _relevance(category: str) -> bool:
    return "relevance" in category


def load(category: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``bfcl://<category>?ids=...&limit=...``."""
    from bfcl_eval.utils import load_dataset_entry, load_ground_truth_entry

    category = category.strip("/")
    if category not in SUPPORTED:
        raise ValueError(
            f"BFCL category {category!r} is not supported; supported: {', '.join(SUPPORTED)}"
        )
    entries = load_dataset_entry(category)
    answers: dict[str, Any] = {}
    if not _relevance(category):
        answers = {a["id"]: a["ground_truth"] for a in load_ground_truth_entry(category)}
    wanted = [i for i in options.get("ids", "").split(",") if i]
    if wanted:
        known = {e["id"] for e in entries}
        missing = [i for i in wanted if i not in known]
        if missing:
            raise ValueError(f"{category}: no entries {missing}")
        entries = [e for e in entries if e["id"] in wanted]
    if options.get("limit"):
        entries = entries[: int(options["limit"])]
    rows = []
    for entry in entries:
        if len(entry["question"]) != 1:
            raise ValueError(f"{entry['id']} is multi-turn; only single-turn categories run")
        turn = entry["question"][0]
        last_user = next((m["content"] for m in reversed(turn) if m["role"] == "user"), "")
        rows.append(
            {
                "id": entry["id"],
                "input": last_user,
                "reference": answers.get(entry["id"]),
                "metadata": {
                    "bfcl": {
                        "category": category,
                        "messages": turn,
                        "functions": entry["function"],
                        "ground_truth": answers.get(entry["id"]),
                    }
                },
            }
        )
    return rows


def _language(category: str) -> Any:
    from bfcl_eval.constants.enums import Language

    if category.endswith("java"):
        return Language.JAVA
    if category.endswith("javascript"):
        return Language.JAVASCRIPT
    return Language.PYTHON


def tools_for(functions: list[dict[str, Any]]) -> tuple[list[ToolSpec], dict[str, str]]:
    """The functions as tool specs, the way BFCL converts them for
    function-calling models, and a map from tool names back to BFCL names."""
    from bfcl_eval.constants.enums import ModelStyle
    from bfcl_eval.constants.type_mappings import GORILLA_TO_OPENAPI
    from bfcl_eval.model_handler.utils import convert_to_tool

    converted = convert_to_tool(functions, GORILLA_TO_OPENAPI, ModelStyle.OPENAI_COMPLETIONS)
    specs, names = [], {}
    for original, wrapped in zip(functions, converted, strict=True):
        tool = wrapped.get("function", wrapped)  # {"type": "function", "function": {...}}
        specs.append(
            ToolSpec(
                name=tool["name"],
                description=tool.get("description", ""),
                input_schema=tool["parameters"],
            )
        )
        names[tool["name"]] = original["name"]
    return specs, names


def grade(
    category: str,
    functions: list[dict[str, Any]],
    ground_truth: Any,
    calls: list[dict[str, Any]] | None,
) -> TaskCheck:
    """BFCL's verdict on the model's calls (None: they could not be decoded)."""
    if category == "live_relevance":
        ok = bool(calls)
        return TaskCheck(
            passed=ok, score=float(ok), details="called a function" if ok else "no call"
        )
    if _relevance(category):
        ok = not calls
        return TaskCheck(
            passed=ok,
            score=float(ok),
            details="no call, as expected" if ok else "called a function that cannot help",
        )
    if calls is None:
        return TaskCheck(passed=False, score=0.0, details="the tool calls could not be decoded")
    from bfcl_eval.eval_checker.ast_eval.ast_checker import ast_checker

    result = ast_checker(
        functions, calls, ground_truth, _language(category), category, CHECKER_MODEL
    )
    valid = bool(result.get("valid"))
    errors = "; ".join(str(e) for e in result.get("error") or [])
    details = "matches a possible answer" if valid else f"{result.get('error_type', '')}: {errors}"
    return TaskCheck(passed=valid, score=float(valid), details=details)


@dataclass
class _State:
    category: str
    functions: list[dict[str, Any]]
    ground_truth: Any
    calls: list[dict[str, Any]] | None = field(default_factory=list)


class BFCLHarness(BuiltinHarness):
    """One model turn over a BFCL entry's functions, graded by BFCL's AST checker."""

    def __init__(self, ctx: HarnessContext, config: dict[str, Any] | None = None) -> None:
        super().__init__(ctx, json_format.ParseDict(config or {}, agent_pb2.BuiltinHarness()))
        self._bfcl: dict[int, _State] = {}

    async def setup(self, task: Task) -> str:
        meta = task.record.metadata.get("bfcl")
        if not isinstance(meta, dict):
            raise TaskError(f"record {task.id} is not a BFCL entry (import it with bfcl://)")
        if task.spec.HasField("target") and task.spec.target.HasField("agent"):
            raise TaskError("BFCL entries run against a target model, not an agent")
        handle = await super().setup(task)
        self._bfcl[id(self._get(handle))] = _State(
            meta["category"], meta["functions"], meta.get("ground_truth")
        )
        return handle

    async def _run_loop(self, live: LiveTask, emit: Callable[[Event], None]) -> FinalEvent:
        task = live.task
        if not task.spec.HasField("target") or not task.spec.target.model:
            raise TaskError("BFCL needs a target model (spec.target.connector and model)")
        state = self._bfcl[id(live)]
        try:
            model = create_model(target_config(task.spec.target))
        except ValueError as exc:
            raise TaskError(f"target: {exc}") from exc
        live.closers.append(model.aclose)
        specs, names = tools_for(state.functions)
        messages = live.task.record.metadata["bfcl"]["messages"]
        system = "\n\n".join(m["content"] for m in messages if m["role"] == "system")
        chat = [
            {"role": m["role"], "content": m["content"]} for m in messages if m["role"] != "system"
        ]
        turn = await model.complete(chat, specs, system=system)
        emit(
            StepEvent(
                Step(
                    type="llm",
                    name=model.name,
                    output=Content(
                        messages=[
                            Message(
                                role="assistant",
                                content=turn.text,
                                tool_calls=[
                                    ToolCall(id=c.id, name=c.name, arguments=c.arguments)
                                    for c in turn.tool_calls
                                ],
                            )
                        ]
                    ),
                    span_id=span_id(),
                    usage=turn.usage,
                )
            )
        )
        calls: list[dict[str, Any]] | None = []
        for c in turn.tool_calls:
            try:
                args = json.loads(c.arguments or "{}")
            except json.JSONDecodeError:
                calls = None
                break
            assert calls is not None
            calls.append({names.get(c.name, c.name): args})
        state.calls = calls
        return FinalEvent(
            output=Content(json=calls, is_json=True)
            if calls is not None
            else Content(text=turn.text),
            usage=turn.usage,
            extra={"steps": 1, "tool_calls": len(turn.tool_calls)},
        )

    async def check(self, handle: str) -> TaskCheck | None:
        state = self._bfcl[id(self._get(handle))]
        return grade(state.category, state.functions, state.ground_truth, state.calls)

    async def teardown(self, handle: str) -> None:
        live = self._live.get(handle)
        if live is not None:
            self._bfcl.pop(id(live), None)
        await super().teardown(handle)


__all__ = ["SUPPORTED", "BFCLHarness", "grade", "load", "tools_for"]
