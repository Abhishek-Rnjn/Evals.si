"""Code mode: a script that calls tools, in a restricted interpreter."""

from __future__ import annotations

import asyncio
import json
import time
from pathlib import Path
from typing import Any

import pytest

from evalsi.types import Usage
from evalsi_harness.codemode import (
    MAX_OPERATIONS,
    MAX_TOOL_CALLS,
    CodeError,
    Interpreter,
    Limit,
    RunCodeTool,
)
from evalsi_harness.events import StepEvent
from evalsi_harness.hooks import Deny, Hook, Hooks
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelTurn, ToolCallRequest, ToolSpec
from evalsi_harness.tools import FunctionTool, ToolResult
from tests.harness_support import record, run, spec_of
from tests.test_harness_agent import CHECK_HELLO


def spec(name: str, *props: str, required: tuple[str, ...] = ()) -> ToolSpec:
    schema = {"type": "object", "properties": {p: {"type": "string"} for p in props}}
    return ToolSpec(name, f"The {name} tool.", {**schema, "required": list(required)})


def run_script(source: str, tools: dict[str, Any] | None = None) -> tuple[str, list[Any]]:
    """Runs a script with fake tools; returns its output and the calls made."""
    calls: list[Any] = []
    tools = tools or {"echo": lambda a: ToolResult(f"echo:{a.get('text')}")}
    specs = {name: spec(name, "text", "n") for name in tools}

    def wrap(name: str, fn: Any) -> Any:
        async def go(arguments: dict[str, Any]) -> ToolResult:
            calls.append((name, arguments))
            return fn(arguments)  # type: ignore[no-any-return]

        return go

    interp = Interpreter({n: wrap(n, f) for n, f in tools.items()}, specs)
    return asyncio.run(interp.run(source)), calls


def fails(source: str, match: str) -> None:
    with pytest.raises((CodeError, Limit), match=match):
        run_script(source)


# --- the language ------------------------------------------------------------------


def test_scripts_compute_with_ordinary_python() -> None:
    out, _ = run_script(
        """
total = 0
for i in range(5):
    if i % 2:
        continue
    total += i
print("total", total)
words = [w.upper() for w in "a b c".split() if w != "b"]
print(words, len(words))
d = {k: v * 2 for k, v in [("x", 1), ("y", 2)]}
print(sorted(d.items()), max(d.values()))
print(f"{total:03d} {3.14159:.2f} {'z'!r}")
x, (y, z) = 1, (2, 3)
print(x + y + z, "ok" if x < y < z else "no", not x, x and y, 0 or z)
s = 0
while True:
    s += 1
    if s > 3:
        break
print(s, [1, 2, 3][1:], "abc"[::-1], 2**10, 7 // 2, -7 % 3)
"""
    )
    assert out.splitlines() == [
        "total 6",
        "['A', 'C'] 2",
        "[('x', 2), ('y', 4)] 4",
        "006 3.14 'z'",
        "6 ok False 2 3",
        "4 [2, 3] cba 1024 3 2",
    ]


def test_functions_lambdas_and_keyed_sorting_work() -> None:
    out, _ = run_script(
        """
def fib(n, memo=None):
    if n < 2:
        return n
    return fib(n - 1) + fib(n - 2)
people = [("bo", 31), ("al", 25), ("cy", 40)]
print(fib(12))
print(sorted(people, key=lambda p: p[1]))
print(max(people, key=lambda p: p[1])[0], min(people, key=lambda p: p[1])[0])
people.sort(key=lambda p: p[0], reverse=True)
print(people[0])
sq = lambda v: v * v
print(sq(7))
"""
    )
    assert out.splitlines() == [
        "144",
        "[('al', 25), ('bo', 31), ('cy', 40)]",
        "cy al",
        "('cy', 40)",
        "49",
    ]


def test_a_key_function_sees_the_scripts_variables_and_max_takes_several_values() -> None:
    out, _ = run_script(
        """
weights = {"a": 3, "b": 1, "c": 2}
print(sorted(["a", "b", "c"], key=lambda k: weights[k]))
print(max("a", "b", "c", key=lambda k: weights[k]), min(3, 1, 2, key=lambda v: -v))
items = ["a", "b", "c"]
items.sort(key=lambda k: weights[k], reverse=True)
print(items)
def rank(k):
    return weights[k] * 10
print(max(["a", "b"], key=rank))
"""
    )
    assert out.splitlines() == ["['b', 'c', 'a']", "a 3", "['a', 'c', 'b']", "a"]


def test_try_except_catches_script_errors_but_not_limits() -> None:
    out, _ = run_script(
        """
try:
    {}["missing"]
except KeyError:
    print("no key")
try:
    1 / 0
except Exception as e:
    print("caught", type(e).__name__ if False else "error")
else:
    print("never")
finally:
    print("done")
"""
    )
    assert out.splitlines() == ["no key", "caught error", "done"]
    with pytest.raises(Limit, match="too much work"):
        run_script("try:\n    while True:\n        pass\nexcept Exception:\n    print('hidden')\n")
    # break and return are control flow, not errors a script can swallow.
    out, _ = run_script(
        "for i in range(3):\n    try:\n        break\n    except Exception:\n        pass\nprint(i)"
    )
    assert out == "0\n"


def test_json_is_available_and_nothing_else_is_imported() -> None:
    out, _ = run_script(
        """
import json
data = json.loads('{"a": [1, 2]}')
print(data["a"][1], json.dumps({"k": 1}))
"""
    )
    assert out == '2 {"k": 1}\n'
    fails("import os", "only json can be imported")
    fails("import re", "only json can be imported")
    fails("from os import path", "ImportFrom is not supported")
    # Regular expressions are not offered at all: a model-written pattern can backtrack for ever.
    with pytest.raises(CodeError, match="NameError"):
        run_script("re.search('(a+)+$', 'a' * 40 + 'b')")


# --- safety ------------------------------------------------------------------------


@pytest.mark.parametrize(
    "source",
    [
        "open('/etc/passwd')",
        "eval('1')",
        "exec('x = 1')",
        "__import__('os')",
        "getattr(1, 'real')",
        "type(1)",
        "print.__class__",
        "(1).__class__",
        "''.__class__.__mro__",
        "x = [].__len__",
        "'{0.__class__}'.format(1)",
        "compile('1', 'a', 'exec')",
        "globals()",
        "vars()",
        "input()",
        "class A:\n    pass",
        "with open('f') as f:\n    pass",
        "(lambda: (yield))()",
        "del x",
        "global x",
        "import subprocess",
    ],
)
def test_nothing_outside_the_subset_is_reachable(source: str) -> None:
    refused = (CodeError, NameError, AttributeError, TypeError, SyntaxError, Limit)
    try:
        out, _ = run_script(source)
    except refused:
        return
    raise AssertionError(f"{source!r} ran and printed {out!r}")


def test_attributes_with_underscores_and_unlisted_methods_are_refused() -> None:
    fails("x = 'a'\nx.__class__", "starting with _")
    with pytest.raises(CodeError, match="has no allowed attribute"):
        run_script("(1).real")
    with pytest.raises(CodeError, match="has no allowed attribute"):
        run_script("'a'.encode()")
    with pytest.raises(CodeError, match="names starting with __"):
        run_script("__builtins__")


def test_a_script_cannot_shadow_a_tool_or_builtin() -> None:
    fails("echo = 1", "is a tool or builtin name")
    fails("len = 1", "is a tool or builtin name")


def test_resource_limits_stop_runaway_scripts() -> None:
    with pytest.raises(Limit, match=str(MAX_OPERATIONS)):
        run_script("while True:\n    pass")
    with pytest.raises(Limit, match="range is limited"):
        run_script("for i in range(10**9):\n    pass")
    with pytest.raises(Limit, match="too large"):
        run_script("x = 'ab' * 10**9")
    with pytest.raises(Limit, match="exponent"):
        run_script("x = 9 ** 9 ** 9")
    with pytest.raises(Limit, match="printed too much"):
        run_script("for i in range(100000):\n    print('x' * 1000)")
    with pytest.raises(Limit, match="nested too deeply"):
        run_script("def f():\n    return f()\nf()")
    fails("x = 1" + " " * 25_000, "longer than")


@pytest.mark.parametrize(
    "source",
    [
        "x = [1]\nfor i in range(40):\n    x.extend(x)",
        "x = [1]\nfor i in range(40):\n    x = x + x",
        "x = 'ab'\nfor i in range(40):\n    x = x + x",
        "n = 2 ** 64\nfor i in range(30):\n    n = n * n",
        "n = 3\nfor i in range(30):\n    n = n ** 64",
        "x = 'a'.ljust(10**9)",
        "x = 'a'.rjust(10**9, 'b')",
        "x = '1'.zfill(10**9)",
        "s = 'a' * 1000\nfor i in range(40):\n    s = s.replace('a', 'aa' * 10)",
        "x = 'abc'.replace('', 'x' * 100000)",
        "x = {}\nfor i in range(30):\n    x.update({j: j for j in range(100000)})",
        "x = f\"{'a':>{10**9}}\"",
        "x = int('9' * 100000)",
        "x = [0] * 10**7",
        "x = 10 * ('ab',) * 10**7",
    ],
)
def test_runaway_growth_is_stopped_before_it_is_built(source: str) -> None:
    tool = RunCodeTool({}, {})
    started = time.monotonic()
    r = asyncio.run(tool.call({"code": source}))
    assert time.monotonic() - started < 2, source
    assert r.is_error
    assert r.content.startswith(("Stopped:", "Error:")), r.content


def test_tool_call_count_is_capped() -> None:
    with pytest.raises(Limit, match=f"more than {MAX_TOOL_CALLS} tool calls"):
        run_script("for i in range(1000):\n    echo(text='x')")


# --- tools in scripts --------------------------------------------------------------


def test_tools_are_functions_with_keyword_or_positional_arguments() -> None:
    out, calls = run_script(
        """
a = echo(text="one")
b = echo("two")
c = echo("three", "4")
results = [echo(text=w) for w in ["x", "y"]]
print(a, b, c, results)
"""
    )
    assert out == "echo:one echo:two echo:three ['echo:x', 'echo:y']\n"
    assert calls[2] == ("echo", {"text": "three", "n": "4"})
    fails("echo('a', 'b', 'c')", "takes at most 2 positional")
    fails("echo('a', text='b')", "twice")


def test_a_failed_tool_returns_its_error_as_a_string_the_script_can_test() -> None:
    out, _ = run_script(
        "r = boom(text='x')\nprint(r.startswith('ERROR:'), r)",
        {"boom": lambda a: ToolResult("it broke", is_error=True)},
    )
    assert out == "True ERROR: it broke\n"


def test_errors_say_the_line_and_keep_what_was_printed() -> None:
    tool = RunCodeTool({"echo": lambda a: _async(ToolResult("x"))}, {"echo": spec("echo", "text")})
    r = asyncio.run(tool.call({"code": "print('before')\nx = 1\ny = x / 0\n"}))
    assert r.is_error
    assert r.content == "before\n\nError: line 3: ZeroDivisionError: division by zero"
    r = asyncio.run(tool.call({"code": "def (:"}))
    assert r.content.startswith("Error: line 1: syntax error")
    r = asyncio.run(tool.call({"code": "print(undefined_name)"}))
    assert "NameError" in r.content
    assert asyncio.run(tool.call({"code": "x = 1"})).content == "(the script printed nothing)"
    assert asyncio.run(tool.call({})).is_error
    r = asyncio.run(tool.call({"code": "while True:\n    pass"}))
    assert r.is_error
    assert r.content.startswith("Stopped:")


async def _async(value: ToolResult) -> ToolResult:
    return value


def test_the_tool_description_lists_the_functions() -> None:
    tool = RunCodeTool(
        {},
        {
            "bash": spec("bash", "command", "timeout_s", required=("command",)),
            "glob": spec("glob", "pattern"),
        },
    )
    d = tool.spec.description
    assert "bash(command, timeout_s=None): The bash tool." in d
    assert "glob(pattern=None): The glob tool." in d
    assert "print()" in d


# --- in the loop ---------------------------------------------------------------------


def turn(text: str = "", calls: list[ToolCallRequest] | None = None) -> ModelTurn:
    return ModelTurn(
        text=text,
        tool_calls=calls or [],
        stop_reason="tool_use" if calls else "end_turn",
        usage=Usage(input_tokens=50, output_tokens=5),
    )


class Scripted:
    name = "scripted"

    def __init__(self, *turns: ModelTurn) -> None:
        self.turns = list(turns)
        self.requests: list[tuple[list[dict[str, Any]], list[str]]] = []

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        self.requests.append(([dict(m) for m in messages], [t.name for t in tools]))
        return self.turns.pop(0)

    async def aclose(self) -> None:
        pass


def code_call(code: str) -> ModelTurn:
    return turn(calls=[ToolCallRequest("rc1", "run_code", json.dumps({"code": code}))])


def make_loop(model: Any, hooks: Hooks | None = None) -> tuple[AgentLoop, list[Any]]:
    events: list[Any] = []
    store: dict[str, str] = {"a": "alpha", "b": "beta", "c": "gamma"}

    def read(key: str) -> str:
        return store[key]

    def write(key: str, value: str) -> str:
        store[key] = value
        return "ok"

    loop = AgentLoop(
        model,
        [
            FunctionTool("read", read, input_schema={"type": "object", "properties": {"key": {}}}),
            FunctionTool(
                "write",
                write,
                input_schema={"type": "object", "properties": {"key": {}, "value": {}}},
            ),
        ],
        LoopConfig(code_mode=True),
        events.append,
        hooks=hooks,
    )
    return loop, events


def test_the_model_sees_run_code_alone_and_gets_only_what_the_script_prints() -> None:
    model = Scripted(
        code_call("vals = [read(k) for k in ['a', 'b', 'c']]\nprint(','.join(vals))"),
        turn("done"),
    )
    loop, _ = make_loop(model)
    final = asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    assert final.stop_reason == "completed"
    assert model.requests[0][1] == ["run_code"]  # read and write are hidden
    result = next(m for m in model.requests[1][0] if m["role"] == "tool")
    assert result["content"] == "alpha,beta,gamma\n"
    # Three reads, one script: the model paid for one round trip.
    assert final.extra["steps"] == 2
    assert final.extra["tool_calls"] == 4  # three inner reads and the run_code call


def test_inner_tool_calls_are_steps_under_the_run_code_step() -> None:
    model = Scripted(code_call("write('a', 'new')\nprint(read('a'))"), turn("done"))
    loop, events = make_loop(model)
    asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    steps = [e.step for e in events if isinstance(e, StepEvent) and e.step.type == "tool"]
    outer = next(s for s in steps if s.name == "run_code")
    inner = [s for s in steps if s.parent_span_id == outer.span_id]
    assert [s.name for s in inner] == ["write", "read"]
    assert json.loads(inner[0].input.text or "")["value"] == "new"  # type: ignore[union-attr]
    assert inner[1].output.text == "new"  # type: ignore[union-attr]
    assert outer.attributes["evalsi.tool.tool_calls"] == 2
    assert outer.parent_span_id == loop.root


def test_calling_a_hidden_tool_directly_says_to_use_run_code() -> None:
    model = Scripted(turn(calls=[ToolCallRequest("d1", "read", '{"key": "a"}')]), turn("done"))
    loop, _ = make_loop(model)
    asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    reply = next(m for m in model.requests[1][0] if m["role"] == "tool")
    assert reply["is_error"] is True
    assert "read is only available inside a run_code script" in reply["content"]


def test_hooks_apply_to_calls_made_inside_a_script() -> None:
    class Gate:
        def before_tool(self, ctx: Any, call: ToolCallRequest) -> Any:
            if call.name == "write":
                return Deny("read-only today")
            return None

    model = Scripted(code_call("r = write('a', 'x')\nprint(r)\nprint(read('a'))"), turn("done"))
    loop, events = make_loop(model, Hooks([Hook("gate", Gate(), {})]))
    asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    result = next(m for m in model.requests[1][0] if m["role"] == "tool")
    assert "denied by a hook: read-only today" in result["content"]
    assert "alpha" in result["content"]  # the read still worked, and write did not change it
    denied = [
        e.step
        for e in events
        if isinstance(e, StepEvent) and e.step.attributes.get("evalsi.tool.hook_denied")
    ]
    assert [d.name for d in denied] == ["write"]


def test_without_code_mode_nothing_changes() -> None:
    loop = AgentLoop(Scripted(), [FunctionTool("read", lambda: "x")], LoopConfig(), lambda e: None)
    assert "run_code" not in loop.tools
    assert loop.hidden == set()


def test_a_run_in_code_mode_replays_exactly(model: Any, sandboxes: Any, tmp_path: Path) -> None:
    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        if not any(m["role"] == "tool" for m in messages):
            code = (
                "files = bash(command='echo hello > out.txt && ls')\n"
                "print(files.strip())\n"
                "print(read_file(path='out.txt').strip())"
            )
            return {"tool_calls": [("run_code", {"code": code})]}
        return {"text": "finished"}

    server = model(script)

    def make(mode: str) -> Any:
        return spec_of(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "m",
                    "base_url": server.base_url,
                },
                "harness": {
                    "builtin": {
                        "tools": {"builtin": ["code_mode"]},
                        "recording": {"mode": mode, "dir": "tapes"},
                    }
                },
                "environment": {"checker": CHECK_HELLO},
            }
        )

    first = run(make("record"), record(), sandboxes, base_dir=tmp_path).record
    assert first is not None
    assert first.check is not None
    assert first.check.passed  # the script's bash call changed the sandbox
    names = [t["function"]["name"] for t in server.requests[0]["tools"]]
    assert names == ["run_code"]
    assert "code_mode" not in server.requests[0]["messages"][0]["content"]
    assert "run_code" in server.requests[0]["messages"][0]["content"]
    reply = next(m for m in server.requests[1]["messages"] if m["role"] == "tool")["content"]
    assert reply.splitlines() == ["out.txt", "", "[exit code 0]", "hello"]  # bash adds its trailer
    assert first.trajectory is not None
    inner = [s.name for s in first.trajectory.steps if s.type == "tool"]
    assert inner == ["bash", "read_file", "run_code"]
    sent = len(server.requests)
    again = run(make("replay"), record(), sandboxes, base_dir=tmp_path).record
    assert len(server.requests) == sent
    assert again is not None
    assert again.check is not None
    assert again.check.passed  # the inner bash ran again on replay


def test_a_subagent_never_inherits_fresh_rounds() -> None:
    from evalsi_harness.context import ROUND_DONE, ContextConfig
    from evalsi_harness.subagent import SUBAGENT_SYSTEM

    class Router:
        name = "router"

        def __init__(self) -> None:
            self.sub_calls = 0
            self.sub_seen: list[list[dict[str, Any]]] = []

        async def complete(
            self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
        ) -> ModelTurn:
            if system == SUBAGENT_SYSTEM:
                self.sub_calls += 1
                self.sub_seen.append([dict(m) for m in messages])
                return turn("the report")  # no TASK_COMPLETE: it was never told about it
            if not any(m["role"] == "tool" for m in messages):
                call = ToolCallRequest("d1", "delegate", '{"task": "look into it"}')
                return turn(calls=[call])
            return turn(f"all done {ROUND_DONE}")

        async def aclose(self) -> None:
            pass

    model = Router()
    loop = AgentLoop(
        model,
        [FunctionTool("read", lambda: "x", input_schema={"type": "object"})],
        LoopConfig(
            delegate_steps=10,
            context=ContextConfig(strategy="fresh-rounds", rounds=4, round_steps=10),
        ),
        lambda e: None,
    )
    final = asyncio.run(loop.run([{"role": "user", "content": "go"}]))
    assert final.stop_reason == "completed"
    assert model.sub_calls == 1  # one round, one reply; not four restarts
    assert [m["content"] for m in model.sub_seen[0]] == ["look into it"]
