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
live_relevance.

Multi-turn categories (multi_turn_base, multi_turn_miss_func,
multi_turn_miss_param, multi_turn_long_context) run BFCL's own loop: each
user turn, the model calls tools until it answers without one (at most 20
steps a turn), the calls run against BFCL's stateful API classes (file
system, trading, travel...), and functions held out by miss_func entries
are offered at their turn. BFCL's multi-turn checker then compares the end
state and results of every turn with the ground truth's. Calls are
executed only when they name a function offered at that step, with JSON
arguments rendered as Python literals, since BFCL evaluates them as code.

Memory, web-search and format-sensitivity categories are not supported.

Options: ``ids`` (comma-separated entry ids), ``limit`` (first N entries).
"""

from __future__ import annotations

import json
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from google.protobuf import json_format

from evalsi.run import target_config
from evalsi.types import Content, Message, Step, TaskCheck, ToolCall, Usage
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
MULTI_TURN = (
    "multi_turn_base",
    "multi_turn_miss_func",
    "multi_turn_miss_param",
    "multi_turn_long_context",
)
# BFCL's limit on model steps per turn; past it the entry fails.
MAX_STEPS_PER_TURN = 20
# A BFCL model config whose function names are compared as given: the
# harness maps tool names back to the original (dotted) names itself.
CHECKER_MODEL = "gorilla-openfunctions-v2"


def _relevance(category: str) -> bool:
    return "relevance" in category


def load(category: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``bfcl://<category>?ids=...&limit=...``."""
    from bfcl_eval.utils import load_dataset_entry, load_ground_truth_entry

    category = category.strip("/")
    if category not in SUPPORTED + MULTI_TURN:
        raise ValueError(
            f"BFCL category {category!r} is not supported; "
            f"supported: {', '.join(SUPPORTED + MULTI_TURN)}"
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
    if category in MULTI_TURN:
        return [_multi_turn_row(category, e, answers.get(e["id"])) for e in entries]
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


def _multi_turn_row(category: str, entry: dict[str, Any], ground_truth: Any) -> dict[str, Any]:
    first = next((m["content"] for m in entry["question"][0] if m["role"] == "user"), "")
    return {
        "id": entry["id"],
        "input": first,
        "reference": ground_truth,
        "metadata": {
            "bfcl": {
                "category": category,
                "turns": entry["question"],
                "functions": entry["function"],
                "missed_function": entry.get("missed_function") or {},
                "initial_config": entry.get("initial_config") or {},
                "involved_classes": entry["involved_classes"],
                "ground_truth": ground_truth,
            }
        },
    }


def call_string(name: str, arguments: dict[str, Any]) -> str:
    """A tool call as the Python call BFCL executes: keyword arguments as
    literals (the repr of JSON values), so evaluating it runs nothing else."""
    if not name.isidentifier():
        raise ValueError(f"function name {name!r}")
    parts = []
    for key, value in arguments.items():
        if not key.isidentifier():
            raise ValueError(f"argument name {key!r}")
        parts.append(f"{key}={value!r}")
    return f"{name}({', '.join(parts)})"


class _Namespace:
    """BFCL keeps the API instances of a run in module globals, named after
    the model and the entry; each task gets its own name, removed after."""

    def __init__(self) -> None:
        import uuid

        self.name = f"evalsi_{uuid.uuid4().hex}"

    def close(self) -> None:
        from bfcl_eval.eval_checker.multi_turn_eval import multi_turn_utils

        for key in [k for k in vars(multi_turn_utils) if self.name in k]:
            del vars(multi_turn_utils)[key]


def execute(
    meta: dict[str, Any], entry_id: str, calls: list[str], namespace: _Namespace
) -> list[str]:
    """Runs calls against the task's BFCL instances; one result per call."""
    from bfcl_eval.eval_checker.multi_turn_eval.multi_turn_utils import (
        execute_multi_turn_func_call,
    )

    results, _ = execute_multi_turn_func_call(
        calls,
        meta["initial_config"],
        meta["involved_classes"],
        namespace.name,
        entry_id,
        long_context="long_context" in meta["category"],
        is_evaL_run=False,
    )
    return [str(r) for r in results]


def grade_multi_turn(
    meta: dict[str, Any], entry_id: str, turns: list[list[list[str]]], namespace: _Namespace
) -> TaskCheck:
    """BFCL's multi-turn verdict on the calls of every step of every turn."""
    from bfcl_eval.eval_checker.multi_turn_eval.multi_turn_checker import multi_turn_checker

    truth = meta["ground_truth"] or []
    if len(turns) != len(truth):
        return TaskCheck(
            passed=False,
            score=0.0,
            details=f"force-terminated: {len(turns)} of {len(truth)} turns completed",
        )
    entry = {
        "id": entry_id,
        "initial_config": meta["initial_config"],
        "involved_classes": meta["involved_classes"],
    }
    try:
        result = multi_turn_checker(
            [[step for step in turn if step] for turn in turns],
            truth,
            entry,
            meta["category"],
            namespace.name + "_check",
        )
    except AssertionError as exc:
        return TaskCheck(passed=False, score=0.0, details=f"multi_turn:state_mismatch: {exc}")
    valid = bool(result.get("valid"))
    details = (
        "every turn matches the ground truth"
        if valid
        else f"{result.get('error_type', '')}: {result.get('error_message', '')}"
    )
    return TaskCheck(passed=valid, score=float(valid), details=details)


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
    # Multi-turn: call strings per step per turn, and the instances' namespace.
    turns: list[list[list[str]]] = field(default_factory=list)
    namespace: _Namespace | None = None


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
        if state.category in MULTI_TURN:
            return await self._multi_turn(live, state, model, emit)
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

    async def _multi_turn(
        self, live: LiveTask, state: _State, model: Any, emit: Callable[[Event], None]
    ) -> FinalEvent:
        from bfcl_eval.constants.default_prompts import (
            DEFAULT_USER_PROMPT_FOR_ADDITIONAL_FUNCTION_FC,
        )

        meta = live.task.record.metadata["bfcl"]
        entry_id = live.task.record.id
        state.namespace = _Namespace()
        functions = list(state.functions)
        held_out: dict[str, list[dict[str, Any]]] = meta.get("missed_function") or {}
        system_parts: list[str] = []
        chat: list[dict[str, Any]] = []
        usage = Usage()
        steps = calls_made = 0
        for turn_index, given in enumerate(meta["turns"]):
            messages = given
            if str(turn_index) in held_out:
                functions += held_out[str(turn_index)]
                messages = [
                    {"role": "user", "content": DEFAULT_USER_PROMPT_FOR_ADDITIONAL_FUNCTION_FC}
                ]
            system_parts += [m["content"] for m in messages if m["role"] == "system"]
            chat += [
                {"role": m["role"], "content": m["content"]}
                for m in messages
                if m["role"] != "system"
            ]
            specs, names = tools_for(functions)
            turn_calls: list[list[str]] = []
            for _ in range(MAX_STEPS_PER_TURN + 1):
                turn = await model.complete(chat, specs, system="\n\n".join(system_parts))
                usage.input_tokens = (usage.input_tokens or 0) + (turn.usage.input_tokens or 0)
                usage.output_tokens = (usage.output_tokens or 0) + (turn.usage.output_tokens or 0)
                steps += 1
                chat.append(dict(turn.as_message()))
                if not turn.tool_calls:
                    break
                executable: list[str] = []
                results: dict[str, str] = {}
                for c in turn.tool_calls:
                    try:
                        if c.name not in names:
                            raise ValueError(f"no function {c.name} is offered")
                        args = json.loads(c.arguments or "{}")
                        if not isinstance(args, dict):
                            raise ValueError("arguments are not an object")
                        executable.append(call_string(names[c.name], args))
                        results[c.id] = ""
                    except ValueError as exc:  # json.JSONDecodeError is a ValueError
                        results[c.id] = f"Error during execution: {exc}"
                outputs = iter(execute(meta, entry_id, executable, state.namespace))
                for c in turn.tool_calls:
                    if results[c.id] == "":
                        results[c.id] = next(outputs)
                    chat.append({"role": "tool", "tool_call_id": c.id, "content": results[c.id]})
                calls_made += len(executable)
                turn_calls.append(executable)
                emit(
                    StepEvent(
                        Step(
                            type="tool",
                            name=f"turn {turn_index}",
                            input=Content(text="\n".join(executable)),
                            output=Content(text="\n".join(results[c.id] for c in turn.tool_calls)),
                            span_id=span_id(),
                            usage=turn.usage,
                        )
                    )
                )
            else:
                # BFCL force-quits a turn that never ends; the entry fails.
                state.turns.append(turn_calls)
                break
            state.turns.append(turn_calls)
        last = next((m.get("content") for m in reversed(chat) if m["role"] == "assistant"), "")
        return FinalEvent(
            output=Content(text=str(last or "")),
            usage=usage,
            extra={"steps": steps, "tool_calls": calls_made, "turns": len(state.turns)},
        )

    async def check(self, handle: str) -> TaskCheck | None:
        state = self._bfcl[id(self._get(handle))]
        if state.category in MULTI_TURN:
            if state.namespace is None:
                return None
            live = self._get(handle)
            return grade_multi_turn(
                live.task.record.metadata["bfcl"], live.task.record.id, state.turns, state.namespace
            )
        return grade(state.category, state.functions, state.ground_truth, state.calls)

    async def teardown(self, handle: str) -> None:
        live = self._live.get(handle)
        if live is not None:
            state = self._bfcl.pop(id(live), None)
            if state is not None and state.namespace is not None:
                state.namespace.close()
        await super().teardown(handle)


__all__ = [
    "MULTI_TURN",
    "SUPPORTED",
    "BFCLHarness",
    "call_string",
    "grade",
    "grade_multi_turn",
    "load",
    "tools_for",
]
