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

Agentic categories run the same tool loop on one question: ``web_search_base``
and ``web_search_no_snippet`` give the model BFCL's WebSearchAPI (a SerpAPI
search and a page fetch; this needs ``SERPAPI_API_KEY`` and network access,
and ``no_snippet`` hides result snippets), and ``memory_kv``, ``memory_vector``
and ``memory_rec_sum`` give it BFCL's memory API for a scenario (student,
customer, finance, healthcare, notetaker). A memory question first replays
the scenario's prerequisite conversations, in which the target model writes
what it will later need into memory; that happens once per target model,
backend and scenario in a harness process (a chain that fails fails every
question that depends on it, where BFCL would start with empty memory), and
costs dozens of model calls. The model's last message without tool calls is
checked by BFCL's agentic checker (an expected answer, ignoring case and
punctuation); the ``memory_vector`` backend loads a sentence-transformers
model, which downloads on first use.

Format-sensitivity categories are not supported.

Options: ``ids`` (comma-separated entry ids), ``limit`` (first N entries).
"""

from __future__ import annotations

import asyncio
import copy
import functools
import json
import shutil
import tempfile
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
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
WEB_SEARCH = ("web_search_base", "web_search_no_snippet")
MEMORY = ("memory_kv", "memory_vector", "memory_rec_sum")
AGENTIC = WEB_SEARCH + MEMORY
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
    if category not in SUPPORTED + MULTI_TURN + AGENTIC:
        raise ValueError(
            f"BFCL category {category!r} is not supported; "
            f"supported: {', '.join(SUPPORTED + MULTI_TURN + AGENTIC)}"
        )
    entries = load_dataset_entry(category)
    answers: dict[str, Any] = {}
    if category in AGENTIC:
        # Ground-truth ids name the family ("memory_0-customer-0"), entry ids the category.
        family = "memory" if category in MEMORY else "web_search"
        answers = {
            a["id"].replace(family, category, 1): a["ground_truth"]
            for a in load_ground_truth_entry(category)
        }
        entries = [e for e in entries if "prereq" not in e["id"]]
    elif not _relevance(category):
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
    if category in AGENTIC:
        return [_agentic_row(category, e, answers.get(e["id"])) for e in entries]
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


def _agentic_row(category: str, entry: dict[str, Any], ground_truth: Any) -> dict[str, Any]:
    if len(entry["question"]) != 1:
        raise ValueError(f"{entry['id']}: an agentic entry is one question")
    messages = entry["question"][0]  # BFCL puts the answer-format system prompt first
    meta: dict[str, Any] = {
        "category": category,
        "messages": messages,
        "functions": entry["function"],
        "involved_classes": entry["involved_classes"],
        "ground_truth": ground_truth,
    }
    if category in MEMORY:
        meta["scenario"] = entry["scenario"]
    else:
        meta["initial_config"] = {
            entry["involved_classes"][0]: {"show_snippet": category != "web_search_no_snippet"}
        }
    return {
        "id": entry["id"],
        "input": next((m["content"] for m in reversed(messages) if m["role"] == "user"), ""),
        "reference": ground_truth,
        "metadata": {"bfcl": meta},
    }


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


def _runner(
    meta: dict[str, Any], entry_id: str, namespace: _Namespace
) -> Callable[[list[str]], list[str]]:
    """Runs call strings against the task's BFCL instances."""

    def run_calls(calls: list[str]) -> list[str]:
        return execute(meta, entry_id, calls, namespace)

    return run_calls


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


def grade_agentic(ground_truth: Any, final_text: str | None) -> TaskCheck:
    """BFCL's agentic verdict: an expected answer appears in the model's last
    message without tool calls (None: it never gave one)."""
    if final_text is None:
        return TaskCheck(
            passed=False, score=0.0, details="agentic:no_last_message: no final answer was given"
        )
    from bfcl_eval.eval_checker.agentic_eval.agentic_checker import agentic_checker

    result = agentic_checker(final_text, [str(a) for a in ground_truth or []])
    valid = bool(result.get("valid"))
    details = (
        "an expected answer is in the response"
        if valid
        else f"{result.get('error_type', '')}: {result.get('error_message', '')}"
    )
    return TaskCheck(passed=valid, score=float(valid), details=details)


@dataclass
class _Conversation:
    """What a tool loop did: the calls of every step of every turn, the model's
    last message without tool calls, and whether it was cut off."""

    turns: list[list[list[str]]] = field(default_factory=list)
    final_text: str | None = None
    forced: bool = False
    usage: Usage = field(default_factory=Usage)
    steps: int = 0
    calls_made: int = 0


async def _converse(
    model: Any,
    turns: list[list[dict[str, Any]]],
    *,
    system: str,
    functions: list[dict[str, Any]],
    run_calls: Callable[[list[str]], list[str]],
    emit: Callable[[Event], None] | None,
) -> _Conversation:
    """BFCL's tool loop over user turns: the model calls tools until it answers
    without one, at most MAX_STEPS_PER_TURN steps a turn, and past that the
    conversation is force-quit (no final answer)."""
    out = _Conversation()
    specs, names = tools_for(functions)
    chat: list[dict[str, Any]] = []
    for turn_index, messages in enumerate(turns):
        chat += [
            {"role": m["role"], "content": m["content"]} for m in messages if m["role"] != "system"
        ]
        turn_calls: list[list[str]] = []
        for _ in range(MAX_STEPS_PER_TURN + 1):
            turn = await model.complete(chat, specs, system=system)
            out.usage.input_tokens = (out.usage.input_tokens or 0) + (turn.usage.input_tokens or 0)
            out.usage.output_tokens = (out.usage.output_tokens or 0) + (
                turn.usage.output_tokens or 0
            )
            out.steps += 1
            chat.append(dict(turn.as_message()))
            if not turn.tool_calls:
                out.final_text = turn.text or ""
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
            outputs = iter(run_calls(executable))
            for c in turn.tool_calls:
                if results[c.id] == "":
                    results[c.id] = next(outputs)
                chat.append({"role": "tool", "tool_call_id": c.id, "content": results[c.id]})
            out.calls_made += len(executable)
            turn_calls.append(executable)
            if emit is not None:
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
            out.forced, out.final_text = True, None
            out.turns.append(turn_calls)
            return out
        out.turns.append(turn_calls)
    return out


def _memory_config(
    category: str, meta: dict[str, Any], entry_id: str, root: Path
) -> dict[str, Any]:
    """BFCL's initial_config for a memory entry: where snapshots live."""
    return {
        meta["involved_classes"][0]: {
            "model_result_dir": root,
            "scenario": meta["scenario"],
            "test_id": entry_id,
            "test_category": category,
        }
    }


@functools.cache
def _prerequisites(category: str) -> tuple[dict[str, Any], ...]:
    from bfcl_eval.utils import load_dataset_entry

    return tuple(e for e in load_dataset_entry(category) if "prereq" in e["id"])


async def _run_prerequisites(model: Any, category: str, scenario: str, root: Path) -> None:
    """Replays the scenario's prerequisite conversations, in order, with the
    memory backend of the category; each leaves a snapshot for the next and for
    the questions (BFCL's own order and snapshots)."""
    from bfcl_eval.eval_checker.multi_turn_eval.multi_turn_utils import (
        execute_multi_turn_func_call,
    )
    from bfcl_eval.model_handler.utils import add_memory_instruction_system_prompt

    for original in _prerequisites(category):
        if original["id"].split("-")[1] != scenario:
            continue
        entry = copy.deepcopy(original)
        namespace = _Namespace()
        try:
            meta: dict[str, Any] = {
                "category": category,
                "involved_classes": entry["involved_classes"],
                "scenario": scenario,
            }
            meta["initial_config"] = _memory_config(category, meta, entry["id"], root)
            _, instances = execute_multi_turn_func_call(
                [], meta["initial_config"], entry["involved_classes"], namespace.name, entry["id"]
            )
            instance = next(iter(instances.values()))
            turns = add_memory_instruction_system_prompt(
                entry["question"], category, scenario, instance
            )
            system = "\n\n".join(m["content"] for m in turns[0] if m["role"] == "system")
            convo = await _converse(
                model,
                turns,
                system=system,
                functions=entry["function"],
                run_calls=_runner(meta, entry["id"], namespace),
                emit=None,
            )
            if convo.forced:
                raise RuntimeError(f"{entry['id']} was force-quit after {MAX_STEPS_PER_TURN} steps")
            instance._flush_memory_to_local_file()
        finally:
            namespace.close()


@dataclass
class _Chain:
    """One scenario's prerequisite conversations for one target model: run
    once, under the lock, into a snapshot directory the questions load."""

    root: Path
    lock: asyncio.Lock = field(default_factory=asyncio.Lock)
    done: bool = False
    error: str | None = None


@dataclass
class _State:
    category: str
    functions: list[dict[str, Any]]
    ground_truth: Any
    final_text: str | None = None
    calls: list[dict[str, Any]] | None = field(default_factory=list)
    # Multi-turn: call strings per step per turn, and the instances' namespace.
    turns: list[list[list[str]]] = field(default_factory=list)
    namespace: _Namespace | None = None


class BFCLHarness(BuiltinHarness):
    """One model turn over a BFCL entry's functions, graded by BFCL's AST checker."""

    def __init__(self, ctx: HarnessContext, config: dict[str, Any] | None = None) -> None:
        super().__init__(ctx, json_format.ParseDict(config or {}, agent_pb2.BuiltinHarness()))
        self._bfcl: dict[int, _State] = {}
        # Memory prerequisite chains by target model, category and scenario.
        self._chains: dict[tuple[str, str, str], _Chain] = {}

    async def aclose(self) -> None:
        await super().aclose()
        for chain in self._chains.values():
            shutil.rmtree(chain.root, ignore_errors=True)
        self._chains.clear()

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
        if state.category in AGENTIC:
            return await self._agentic(live, state, model, emit)
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

    async def _agentic(
        self, live: LiveTask, state: _State, model: Any, emit: Callable[[Event], None]
    ) -> FinalEvent:
        from bfcl_eval.eval_checker.multi_turn_eval.multi_turn_utils import (
            execute_multi_turn_func_call,
        )
        from bfcl_eval.model_handler.utils import add_memory_instruction_system_prompt

        meta = live.task.record.metadata["bfcl"]
        entry_id = live.task.record.id
        category = state.category
        messages = [dict(m) for m in meta["messages"]]
        if category in MEMORY:
            chain = await self._chain(live, model, category, meta["scenario"])
            initial = _memory_config(category, meta, entry_id, chain.root)
        else:
            initial = meta["initial_config"]
        namespace = state.namespace = _Namespace()
        exec_meta = {
            "initial_config": initial,
            "involved_classes": meta["involved_classes"],
            "category": category,
        }
        if category in MEMORY:
            # The instance loads the scenario's snapshot, whose core memory the system prompt shows.
            _, instances = execute_multi_turn_func_call(
                [], initial, meta["involved_classes"], namespace.name, entry_id
            )
            messages = add_memory_instruction_system_prompt(
                [messages], category, meta["scenario"], next(iter(instances.values()))
            )[0]
        system = "\n\n".join(m["content"] for m in messages if m["role"] == "system")
        convo = await _converse(
            model,
            [messages],
            system=system,
            functions=state.functions,
            run_calls=_runner(exec_meta, entry_id, namespace),
            emit=emit,
        )
        state.final_text = convo.final_text
        return FinalEvent(
            output=Content(text=convo.final_text or ""),
            usage=convo.usage,
            extra={
                "steps": convo.steps,
                "tool_calls": convo.calls_made,
                "force_quit": convo.forced,
            },
        )

    async def _chain(self, live: LiveTask, model: Any, category: str, scenario: str) -> _Chain:
        """The scenario's memory, after its prerequisite conversations ran once
        for this target model."""
        target = live.task.spec.target
        key = (f"{target.connector}|{target.model}|{target.base_url}", category, scenario)
        chain = self._chains.get(key)
        if chain is None:
            root = Path(tempfile.mkdtemp(prefix="evalsi-bfcl-memory-"))
            chain = self._chains[key] = _Chain(root=root)
        async with chain.lock:
            if chain.error:
                raise TaskError(f"the {category} {scenario} prerequisites failed: {chain.error}")
            if not chain.done:
                try:
                    await _run_prerequisites(model, category, scenario, chain.root)
                except Exception as exc:
                    chain.error = f"{type(exc).__name__}: {exc}"
                    raise TaskError(
                        f"the {category} {scenario} prerequisites failed: {chain.error}"
                    ) from exc
                chain.done = True
        return chain

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
        if state.category in AGENTIC:
            return grade_agentic(state.ground_truth, state.final_text)
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
    "AGENTIC",
    "MEMORY",
    "MULTI_TURN",
    "SUPPORTED",
    "WEB_SEARCH",
    "BFCLHarness",
    "call_string",
    "grade",
    "grade_agentic",
    "grade_multi_turn",
    "load",
    "tools_for",
]
