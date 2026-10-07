"""tau2-bench (τ-bench) domains as Evals.si agent tasks.

The importer ``taubench://<domain>`` turns a domain's tasks into records, one
per task, each carrying the full tau2 task::

    dataset: {uri: "taubench://airline?tasks=0,1,2"}
    harness: {external: {python: "evalsi_taubench:TauBenchHarness"}}

``TauBenchHarness`` is the built-in harness (any chat model as the agent, the
same budgets, record/replay and trace export) with the tau2 domain around it:

- the agent's tools are the domain's tools, run against a live tau2
  environment with the task's initial state, and its system prompt is tau2's
  agent instruction with the domain policy;
- the user is the Evals.si user simulator, through the run's judge, with the
  task's persona and instructions as its persona and goal; it opens the
  conversation, as in tau2;
- grading is tau2's own evaluator (``evaluate_simulation``, evaluation type
  ``all``) on the conversation as tau2 messages: the final database is
  compared with a gold replay of the expected actions, and expected actions,
  environment assertions and information the agent must communicate are
  checked as the task's reward basis says.

The harness config takes the built-in harness's fields (``maxSteps``,
``budget``, ``userSimulator: {judge, maxTurns}``, ``recording`` ...).

Options: ``tasks`` (comma-separated ids), ``task_set`` (default: the domain),
``data_dir`` (tau2's data directory; default ``TAU2_DATA_DIR``).

In domains where the user has tools of their own (telecom: the user's
phone), the simulated user is tau2's: tau2's user prompt and guidelines,
through the run's judge, choosing at each turn between a message to the
agent and one of their tools, which runs against the same tau2 environment.
Their tool calls are part of the conversation tau2 grades, so assertions on
the device's end state hold.

Not supported: tasks graded by natural-language assertions (tau2 grades
those with its own LLM judge); the importer skips them.
"""

from __future__ import annotations

import json
import os
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any

from google.protobuf import json_format

from evalsi.types import TaskCheck
from evalsi.v1alpha1 import agent_pb2
from evalsi_harness import BuiltinHarness, FunctionTool, HarnessContext, Task, TaskError, ToolResult
from evalsi_harness.events import Event, StepEvent
from evalsi_harness.harness import LiveTask
from evalsi_harness.tools import Tool
from evalsi_harness.user_sim import UserSimulator

DEFAULT_MAX_STEPS = 100


def _data_dir(options: dict[str, str]) -> None:
    # tau2 reads its data directory once, at import.
    if options.get("data_dir"):
        os.environ["TAU2_DATA_DIR"] = options["data_dir"]


def _registry() -> Any:
    from tau2.registry import registry

    return registry


def load(domain: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``taubench://<domain>?tasks=...&task_set=...``."""
    _data_dir(options)
    from tau2.data_model.tasks import RewardType

    registry = _registry()
    domain = domain.strip("/")
    if domain not in registry.get_domains():
        raise ValueError(f"unknown tau2 domain {domain!r}; known: {registry.get_domains()}")
    tasks = registry.get_tasks_loader(options.get("task_set", domain))()
    wanted = [t for t in options.get("tasks", "").split(",") if t]
    if wanted:
        known = {t.id for t in tasks}
        missing = [t for t in wanted if t not in known]
        if missing:
            raise ValueError(f"{domain}: no tasks {missing}")
        tasks = [t for t in tasks if t.id in wanted]
    rows = []
    for task in tasks:
        criteria = task.evaluation_criteria
        if criteria is not None and RewardType.NL_ASSERTION in criteria.reward_basis:
            continue
        rows.append(
            {
                "id": f"{domain}/{task.id}",
                "input": str(task.user_scenario),
                "reference": criteria.model_dump(mode="json") if criteria else None,
                "metadata": {
                    "taubench": {"domain": domain, "task": task.model_dump(mode="json")},
                },
            }
        )
    return rows


@dataclass
class _TauState:
    domain: str
    source: Task
    task: Any
    env: Any
    tool_names: set[str]
    history: list[Any] = field(default_factory=list)
    events: list[Event] = field(default_factory=list)
    stop_reason: str = ""
    started: float = field(default_factory=time.time)
    # Dual-control domains: the user with tools of their own.
    user: ToolUser | None = None


def _history_to_chat(messages: list[Any]) -> list[dict[str, Any]]:
    """tau2 messages as the harness's chat messages."""
    out: list[dict[str, Any]] = []
    for m in messages:
        if m.role == "tool":
            out.append({"role": "tool", "tool_call_id": m.id, "content": m.content or ""})
            continue
        msg: dict[str, Any] = {"role": m.role, "content": m.content or ""}
        if m.tool_calls:
            msg["tool_calls"] = [
                {
                    "id": c.id,
                    "type": "function",
                    "function": {"name": c.name, "arguments": json.dumps(c.arguments)},
                }
                for c in m.tool_calls
            ]
        out.append(msg)
    return out


class TauBenchHarness(BuiltinHarness):
    """The built-in harness, with a tau2 domain's tools, policy, user and grading."""

    def __init__(self, ctx: HarnessContext, config: dict[str, Any] | None = None) -> None:
        builtin = json_format.ParseDict(config or {}, agent_pb2.BuiltinHarness())
        super().__init__(ctx, builtin)
        self._tau: dict[int, _TauState] = {}
        self._handles: dict[str, int] = {}

    def _state(self, live: LiveTask) -> _TauState:
        return self._tau[id(live)]

    async def setup(self, task: Task) -> str:
        meta = task.record.metadata.get("taubench")
        if not isinstance(meta, dict):
            raise TaskError(
                f"record {task.id} is not a tau-bench task (import it with taubench://)"
            )
        if task.spec.HasField("target") and task.spec.target.HasField("agent"):
            raise TaskError("tau-bench tasks run the built-in agent loop over a target model")
        from tau2.data_model.tasks import Task as TauTask

        tau_task = TauTask.model_validate(meta["task"])
        try:
            env = _registry().get_env_constructor(meta["domain"])()
        except Exception as exc:  # an unknown domain or missing data
            raise TaskError(f"tau2 domain {meta['domain']}: {exc}") from exc
        initial = tau_task.initial_state
        history = list(initial.message_history or []) if initial else []
        env.set_state(
            initialization_data=initial.initialization_data if initial else None,
            initialization_actions=initial.initialization_actions if initial else None,
            message_history=history,
        )
        handle = await super().setup(task)
        live = self._get(handle)
        self._tau[id(live)] = _TauState(
            domain=meta["domain"],
            source=task,
            task=tau_task,
            env=env,
            tool_names={t.name for t in env.get_tools()},
            history=history,
        )
        self._handles[handle] = id(live)
        return handle

    def describe(self) -> Any:
        manifest = super().describe()
        manifest.name = "evalsi-taubench"
        manifest.description = "tau2-bench domains: tools, policy, a simulated user and DB grading."
        return manifest

    def _system(self, live: LiveTask) -> str:
        from tau2.agent.llm_agent import AGENT_INSTRUCTION, SYSTEM_PROMPT

        prompt: str = SYSTEM_PROMPT.format(
            domain_policy=self._state(live).env.get_policy(), agent_instruction=AGENT_INSTRUCTION
        )
        if self.config.instructions:
            prompt += f"\n\n{self.config.instructions}"
        return prompt

    def _loop_config(self, live: LiveTask) -> Any:
        config = super()._loop_config(live)
        config.max_steps = self.config.max_steps or DEFAULT_MAX_STEPS
        # tau2 tools change shared state in order.
        config.parallel = False
        return config

    async def _tools(self, live: LiveTask) -> list[Tool]:
        from tau2.data_model.message import ToolCall

        state = self._state(live)
        tools = await super()._tools(live)

        def bind(name: str) -> Callable[..., ToolResult]:
            def call(**args: Any) -> ToolResult:
                resp = state.env.get_response(ToolCall(id="", name=name, arguments=args))
                return ToolResult(resp.content or "", is_error=bool(resp.error))

            return call

        for tool in state.env.get_tools():
            fn = tool.openai_schema["function"]
            tools.append(
                FunctionTool(
                    tool.name,
                    bind(tool.name),
                    description=fn.get("description", ""),
                    input_schema=fn.get("parameters") or {"type": "object", "properties": {}},
                )
            )
        return tools

    def _user_simulator(self, task: Task) -> UserSimulator | None:
        if self.ctx.judge is None:
            raise TaskError("the simulated user needs a judge model (spec.judge)")
        us = self.config.user_simulator
        try:
            judge = self.ctx.judge(us.judge)
        except ValueError as exc:
            raise TaskError(str(exc)) from exc
        state = next((st for st in self._tau.values() if st.source is task), None)
        if state is not None and state.env.user_tools is not None:
            state.user = ToolUser(
                judge,
                env=state.env,
                scenario=str(state.task.user_scenario),
                max_turns=us.max_turns or 30,
                seed=f"{task.id}#{task.trial}",
            )
            return state.user
        scenario = task.record.metadata["taubench"]["task"]["user_scenario"]
        instructions = scenario.get("instructions")
        goal = instructions if isinstance(instructions, str) else json.dumps(instructions)
        return UserSimulator(
            judge,
            persona=us.persona or scenario.get("persona") or "",
            goal=goal,
            max_turns=us.max_turns or 30,
            seed=f"{task.id}#{task.trial}",
        )

    async def _conversation(
        self, live: LiveTask, user: UserSimulator | None, emit: Callable[[Event], None]
    ) -> list[dict[str, Any]]:
        state = self._state(live)
        conversation = _history_to_chat(state.history)
        if conversation and conversation[-1]["role"] == "user":
            return conversation
        assert user is not None
        opening = await user.reply(conversation)
        if not opening.message:
            raise TaskError("the simulated user did not open the conversation")
        # The step reaches state.events through run(), which grading reads;
        # state.history stays the task's own initial history.
        emit(StepEvent(_user_step(opening.message, live)))
        conversation.append({"role": "user", "content": opening.message})
        return conversation

    async def run(self, handle: str) -> Any:
        live = self._get(handle)
        state = self._state(live)
        async for event in super().run(handle):
            if isinstance(event, StepEvent):
                state.events.append(event)
            elif hasattr(event, "stop_reason"):
                state.stop_reason = event.stop_reason
            yield event

    async def check(self, handle: str) -> TaskCheck | None:
        state = self._state(self._get(handle))
        from tau2.data_model.simulation import SimulationRun, TerminationReason
        from tau2.evaluator.evaluator import EvaluationType, evaluate_simulation

        messages = [*state.history, *_events_to_tau(state.events, state.tool_names, state.user)]
        reason = (
            TerminationReason.USER_STOP
            if state.stop_reason == "completed"
            else TerminationReason.MAX_STEPS
        )
        now = time.time()
        simulation = SimulationRun(
            id=handle,
            task_id=state.task.id,
            start_time=str(state.started),
            end_time=str(now),
            duration=now - state.started,
            termination_reason=reason,
            messages=messages,
        )
        try:
            info = evaluate_simulation(
                simulation, state.task, EvaluationType.ALL, solo_mode=False, domain=state.domain
            )
        except ValueError as exc:
            return TaskCheck(passed=False, error=f"tau2 could not grade the conversation: {exc}")
        return _to_check(info, state.stop_reason)

    async def teardown(self, handle: str) -> None:
        key = self._handles.pop(handle, None)
        if key is not None:
            self._tau.pop(key, None)
        await super().teardown(handle)


def _user_step(text: str, live: LiveTask) -> Any:
    from evalsi.types import Content, Step
    from evalsi_harness.events import span_id

    return Step(type="user", name="simulated-user", output=Content(text=text), span_id=span_id())


def _events_to_tau(
    events: list[Event], tool_names: set[str], user: ToolUser | None = None
) -> list[Any]:
    """The agent's conversation as tau2 messages. Tool calls that never
    reached the environment (an unknown tool, arguments that are not JSON)
    are left out, as tau2 replays every call it is given. With a tool-using
    user, their tool calls go before the message they led to."""
    from tau2.data_model.message import AssistantMessage, ToolCall, ToolMessage, UserMessage

    out: list[Any] = []
    replayable: set[str] = set()
    for event in events:
        if not isinstance(event, StepEvent):
            continue
        step = event.step
        if step.type == "llm" and step.output is not None and step.output.messages:
            msg = step.output.messages[0]
            calls = []
            for c in msg.tool_calls or []:
                try:
                    args = (
                        json.loads(c.arguments or "{}")
                        if isinstance(c.arguments, str)
                        else c.arguments
                    )
                except json.JSONDecodeError:
                    continue
                if c.name in tool_names and isinstance(args, dict):
                    calls.append(ToolCall(id=c.id, name=c.name, arguments=args))
                    replayable.add(c.id)
            out.append(
                AssistantMessage(
                    role="assistant",
                    content=msg.content if isinstance(msg.content, str) and msg.content else None,
                    tool_calls=calls or None,
                )
            )
        elif step.type == "tool":
            call_id = str(step.attributes.get("gen_ai.tool.call.id", ""))
            if call_id in replayable:
                out.append(
                    ToolMessage(
                        id=call_id,
                        role="tool",
                        content=step.output.as_text() if step.output else "",
                        requestor="assistant",
                        error=bool(step.error),
                    )
                )
        elif step.type == "user" and step.output is not None:
            if user is not None:
                out.extend(user.take())
            out.append(UserMessage(role="user", content=step.output.as_text()))
    if user is not None:
        out.extend(user.take_rest())
    return out


USER_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "message": {
            "type": "string",
            "description": "What you say to the agent; empty when you call a tool instead.",
        },
        "tool": {"type": "string", "description": "The tool to call, or empty."},
        "arguments": {
            "type": "string",
            "description": "The tool's arguments as a JSON object, or {}.",
        },
        "done": {"type": "boolean", "description": "True when the conversation should end."},
    },
    "required": ["message", "tool", "arguments", "done"],
    "additionalProperties": False,
}
_STOP_TOKENS = ("###STOP###", "###TRANSFER###", "###OUT-OF-SCOPE###")
# Tool calls a user may make before saying something.
MAX_USER_TOOL_CALLS = 10


class ToolUser(UserSimulator):
    """tau2's simulated user for dual-control domains, through the run's judge:
    each turn they either message the agent or call one of their own tools
    (run against the task's tau2 environment, with requestor "user")."""

    def __init__(self, judge: Any, *, env: Any, scenario: str, max_turns: int, seed: str) -> None:
        from tau2.user.user_simulator import SYSTEM_PROMPT, get_global_user_sim_guidelines

        super().__init__(judge, persona="", goal=scenario, max_turns=max_turns, seed=seed)
        self.env = env
        self.tools = {t.name: t for t in env.get_user_tools()}
        self.system = SYSTEM_PROMPT.format(
            global_user_sim_guidelines=get_global_user_sim_guidelines(use_tools=True),
            instructions=scenario,
        )
        self.transcript: list[str] = []
        self.seen = 0
        # tau2 messages of the user's tool calls, per message they led to.
        self.exchanges: list[list[Any]] = []
        self.pending: list[Any] = []
        self.calls = 0

    def _tool_list(self) -> str:
        lines = []
        for name, tool in self.tools.items():
            fn = tool.openai_schema["function"]
            params = json.dumps(fn.get("parameters") or {}, separators=(",", ":"))
            lines.append(f"- {name}: {fn.get('description', '')} Parameters: {params}")
        return "\n".join(lines)

    def _catch_up(self, conversation: list[dict[str, Any]]) -> None:
        for m in conversation[self.seen :]:
            content = (m.get("content") or "").strip()
            if m["role"] == "assistant" and content:
                self.transcript.append(f"AGENT: {content}")
        self.seen = len(conversation)

    async def reply(self, conversation: list[dict[str, Any]]) -> Any:
        from evalsi.types import Usage
        from evalsi_harness.user_sim import UserTurn

        self.turns += 1
        if self.turns > self.max_turns:
            return UserTurn("", True, Usage())
        self._catch_up(conversation)
        usage = Usage(input_tokens=0, output_tokens=0)
        for _ in range(MAX_USER_TOOL_CALLS + 1):
            prompt = (
                f"<your_tools>\n{self._tool_list()}\n</your_tools>\n"
                f'<conversation id="{self.seed}">\n'
                + "\n".join(self.transcript)
                + "\n</conversation>\n"
                "Either write your next message to the agent (tool empty), or call one of your "
                "tools (message empty), never both. Use the stop tokens as your guidelines say."
            )
            response = await self.judge.backend.complete_json(
                system=self.system, prompt=prompt, schema=USER_SCHEMA
            )
            usage.input_tokens = (usage.input_tokens or 0) + (response.input_tokens or 0)
            usage.output_tokens = (usage.output_tokens or 0) + (response.output_tokens or 0)
            data = response.data
            tool = str(data.get("tool") or "").strip()
            message = str(data.get("message") or "").strip()
            if tool and not message:
                self._call(tool, str(data.get("arguments") or "{}"))
                continue
            done = bool(data.get("done"))
            for token in _STOP_TOKENS:
                if token in message:
                    done = True
                    message = message.replace(token, "").strip()
            if message:
                self.transcript.append(f"YOU: {message}")
                self.exchanges.append(self.pending)
                self.pending = []
            return UserTurn(message, done, usage)
        return UserTurn("", True, usage)

    def _call(self, name: str, raw: str) -> None:
        from tau2.data_model.message import ToolCall, UserMessage

        self.calls += 1
        call_id = f"user-{self.calls}"
        try:
            args = json.loads(raw or "{}")
            if not isinstance(args, dict):
                raise ValueError("the arguments must be a JSON object")
            if name not in self.tools:
                raise ValueError(f"you have no tool {name}")
        except ValueError as exc:  # not replayed: tau2 would replay it
            self.transcript.append(f"YOUR TOOL CALL {name}({raw}) FAILED: {exc}")
            return
        call = ToolCall(id=call_id, name=name, arguments=args, requestor="user")
        result = self.env.get_response(call)
        self.pending += [UserMessage(role="user", content=None, tool_calls=[call]), result]
        self.transcript.append(f"YOUR TOOL CALL {name}({json.dumps(args)}) -> {result.content}")

    def take(self) -> list[Any]:
        """The tool calls behind the next user message, in order."""
        return self.exchanges.pop(0) if self.exchanges else []

    def take_rest(self) -> list[Any]:
        rest = [m for ex in self.exchanges for m in ex] + self.pending
        self.exchanges, self.pending = [], []
        return rest


def _to_check(info: Any, stop_reason: str) -> TaskCheck:
    tests: dict[str, str] = {}

    def verdict(ok: bool) -> str:
        return "passed" if ok else "failed"

    if info.db_check is not None:
        tests["database"] = verdict(info.db_check.db_match)
    for i, a in enumerate(info.action_checks or []):
        tests[f"action {i}: {a.action.name}"] = verdict(a.action_match)
    for i, e in enumerate(info.env_assertions or []):
        tests[f"assertion {i}: {e.env_assertion.func_name}"] = verdict(e.met)
    for c in info.communicate_checks or []:
        tests[f"communicate: {c.info}"] = verdict(c.met)
    basis = ", ".join(str(getattr(b, "value", b)) for b in info.reward_basis or [])
    details = f"reward {info.reward:g}" + (f" on {basis}" if basis else "")
    failed = [name for name, v in tests.items() if v == "failed"]
    if failed:
        details += "; failed: " + "; ".join(failed)
    if stop_reason not in ("completed", ""):
        details += f" (the conversation stopped: {stop_reason})"
    note = (info.info or {}).get("note") if isinstance(info.info, dict) else None
    if note:
        details += f"; {note}"
    return TaskCheck(
        passed=info.reward >= 1.0, score=float(info.reward), details=details, tests=tests
    )


__all__ = ["TauBenchHarness", "load"]
