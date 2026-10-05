"""The ``agent`` pack: grade how an agent worked, from its trajectory.

Trajectories come from OTel traces (online) or from harness runs (offline).
Expected tool use comes from the reference, as JSON::

    {"tool_calls": [{"name": "lookup_order", "arguments": {"id": 42}}, "send_email"]}

A bare list works too. A string entry names a tool; an object may also pin
arguments, which match when every given argument equals the actual one.
"""

from __future__ import annotations

import dataclasses
import json
from collections import Counter
from typing import Any

from evalsi.evaluator import EvalContext, MetricSpec, Requirements, ScoreType, SkipRecord, evaluator
from evalsi.packs.judge import rubric_score
from evalsi.registry import Pack
from evalsi.types import Record, Score, ToolUse, Trajectory

NUMBER, PASSED = ScoreType.NUMBER, ScoreType.PASSED
TRAJECTORY = Requirements(output=False, trajectory=True)
TRAJECTORY_AND_REFERENCE = Requirements(output=False, reference=True, trajectory=True)


def _trajectory(record: Record) -> Trajectory:
    assert record.trajectory is not None
    return record.trajectory


def _args(raw: str) -> Any:
    try:
        return json.loads(raw) if raw else {}
    except json.JSONDecodeError:
        return raw


def expected_calls(record: Record) -> list[tuple[str, dict[str, Any] | None]]:
    """(tool name, pinned arguments or None) from the reference."""
    assert record.reference is not None
    ref = record.reference
    data: Any = ref.json if ref.is_json else None
    if data is None:
        try:
            data = json.loads(ref.as_text())
        except json.JSONDecodeError as exc:
            raise SkipRecord("the reference is not JSON describing expected tool calls") from exc
    if isinstance(data, dict):
        data = data.get("tool_calls")
    if not isinstance(data, list):
        raise SkipRecord("the reference has no tool_calls list")
    out: list[tuple[str, dict[str, Any] | None]] = []
    for item in data:
        if isinstance(item, str):
            out.append((item, None))
        elif isinstance(item, dict) and isinstance(item.get("name"), str):
            args = item.get("arguments")
            out.append((item["name"], args if isinstance(args, dict) else None))
    return out


def _matches(expected: tuple[str, dict[str, Any] | None], actual: ToolUse) -> bool:
    name, pinned = expected
    if name != actual.name:
        return False
    if not pinned:
        return True
    got = _args(actual.arguments)
    return isinstance(got, dict) and all(got.get(k) == v for k, v in pinned.items())


@evaluator(
    name="builtin/tool-call-accuracy",
    version="1.0.0",
    description=(
        "Recall and precision of tool calls against the expected calls in the reference "
        "(order ignored; pinned arguments must match)."
    ),
    requires=TRAJECTORY_AND_REFERENCE,
    outputs=[
        MetricSpec("recall", NUMBER, min=0.0, max=1.0, higher_is_better=True),
        MetricSpec("precision", NUMBER, min=0.0, max=1.0, higher_is_better=True),
    ],
)
def tool_call_accuracy(record: Record) -> list[Score]:
    expected = expected_calls(record)
    actual = _trajectory(record).tool_uses()
    unused = list(actual)
    missing: list[str] = []
    for exp in expected:
        hit = next((a for a in unused if _matches(exp, a)), None)
        if hit is None:
            missing.append(exp[0])
        else:
            unused.remove(hit)
    matched = len(expected) - len(missing)
    recall = matched / len(expected) if expected else 1.0
    precision = matched / len(actual) if actual else (1.0 if not expected else 0.0)
    explanation = f"matched {matched} of {len(expected)} expected, {len(actual)} made"
    return [
        Score(number=recall, name="recall", explanation=explanation, metadata={"missing": missing}),
        Score(number=precision, name="precision", explanation=explanation),
    ]


@evaluator(
    name="builtin/trajectory-match",
    version="1.0.0",
    description=(
        "Tool-name sequence against the reference. mode: strict (same order and count), "
        "unordered (same multiset), subset (only expected tools, any subset), superset "
        "(every expected tool, extras allowed)."
    ),
    requires=TRAJECTORY_AND_REFERENCE,
    outputs=[MetricSpec("trajectory-match", PASSED, higher_is_better=True)],
)
def trajectory_match(record: Record, *, mode: str = "strict") -> Score:
    expected = [name for name, _ in expected_calls(record)]
    actual = [t.name for t in _trajectory(record).tool_uses()]
    want, got = Counter(expected), Counter(actual)
    if mode == "strict":
        passed = expected == actual
    elif mode == "unordered":
        passed = want == got
    elif mode == "subset":
        passed = not (got - want)
    elif mode == "superset":
        passed = not (want - got)
    else:
        raise ValueError(f"mode must be strict, unordered, subset or superset, not {mode!r}")
    return Score(passed=passed, explanation=f"expected {expected}, got {actual}")


@evaluator(
    name="builtin/tool-errors",
    version="1.0.0",
    description="Share of tool calls that failed.",
    requires=TRAJECTORY,
    outputs=[MetricSpec("tool-errors", NUMBER, min=0.0, max=1.0, higher_is_better=False)],
)
def tool_errors(record: Record) -> Score:
    tools = _trajectory(record).tool_uses()
    if not tools:
        raise SkipRecord("the trajectory has no tool calls")
    failed = [t.name for t in tools if t.error]
    return Score(number=len(failed) / len(tools), metadata={"failed": failed})


@evaluator(
    name="builtin/loop-detection",
    version="1.0.0",
    description=(
        "Passes unless the agent repeated an identical tool call (same tool, same "
        "arguments) more than max_repeats times in a row."
    ),
    requires=TRAJECTORY,
    outputs=[MetricSpec("loop-detection", PASSED, higher_is_better=True)],
)
def loop_detection(record: Record, *, max_repeats: int = 2) -> Score:
    longest, run, previous = 0, 0, None
    for tool in _trajectory(record).tool_uses():
        key = (tool.name, json.dumps(_args(tool.arguments), sort_keys=True))
        run = run + 1 if key == previous else 1
        previous = key
        longest = max(longest, run)
    passed = longest <= max_repeats
    return Score(
        passed=passed,
        explanation="" if passed else f"the same call repeated {longest} times in a row",
        metadata={"longest_repeat": longest},
    )


@evaluator(
    name="builtin/step-budget",
    version="1.0.0",
    description="Counts LLM and tool steps; with max_steps or max_tool_calls, checks the budget.",
    requires=TRAJECTORY,
    outputs=[
        MetricSpec("steps", NUMBER, min=0.0, higher_is_better=False),
        MetricSpec("tool_calls", NUMBER, min=0.0, higher_is_better=False),
        MetricSpec("within-budget", PASSED, higher_is_better=True),
    ],
)
def step_budget(
    record: Record, *, max_steps: int | None = None, max_tool_calls: int | None = None
) -> list[Score]:
    trajectory = _trajectory(record)
    steps = sum(1 for s in trajectory.steps if s.type in ("llm", "tool"))
    tools = len(trajectory.tool_uses())
    scores = [Score(number=steps, name="steps"), Score(number=tools, name="tool_calls")]
    if max_steps is not None or max_tool_calls is not None:
        within = (max_steps is None or steps <= max_steps) and (
            max_tool_calls is None or tools <= max_tool_calls
        )
        scores.append(Score(passed=within, name="within-budget"))
    return scores


def _transcript(trajectory: Trajectory, limit: int = 400) -> str:
    def clip(text: str) -> str:
        return text if len(text) <= limit else text[:limit] + " ..."

    lines = []
    for i, step in enumerate(trajectory.steps, start=1):
        if step.type == "tool":
            line = f"{i}. tool {step.name}({clip(step.input.as_text() if step.input else '')})"
            result = step.error or (step.output.as_text() if step.output else "")
            line += f" -> {'ERROR ' if step.error else ''}{clip(result)}"
        elif step.type == "llm":
            line = f"{i}. model call" + (f": {clip(step.output.as_text())}" if step.output else "")
        else:
            continue
        lines.append(line)
    return "\n".join(lines) or "(no model or tool steps)"


@evaluator(
    name="builtin/goal-completion",
    version="1.0.0",
    description="Judge rating of whether the agent achieved the user's goal, given its trajectory.",
    requires=Requirements(input=True, output=False, judge=True, trajectory=True),
    outputs=[MetricSpec("goal-completion", NUMBER, min=0.0, max=1.0, higher_is_better=True)],
)
async def goal_completion(record: Record, *, ctx: EvalContext) -> Score:
    rubric = (
        "Did the agent accomplish what the user asked? Use the trajectory to check that "
        "claimed actions actually happened and succeeded; an agent that says it did "
        "something its tools never did has not completed the goal."
    )
    transcript = f"<trajectory>\n{_transcript(_trajectory(record))}\n</trajectory>"
    if record.output is None:
        outputs = [s.output for s in _trajectory(record).steps if s.output is not None]
        if outputs:
            record = dataclasses.replace(record, output=outputs[-1])
    if record.output is None:
        raise SkipRecord("no final answer to judge")
    return await rubric_score(
        record, rubric, ctx, label="goal-completion", extra_sections=transcript
    )


PACK = Pack(
    name="agent",
    description=(
        "Agent trajectories: tool-call accuracy, trajectory match, tool errors, loops, "
        "step budgets, judge-rated goal completion."
    ),
    evaluators=[
        tool_call_accuracy,
        trajectory_match,
        tool_errors,
        loop_detection,
        step_budget,
        goal_completion,
    ],
)
