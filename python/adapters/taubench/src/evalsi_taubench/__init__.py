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

Option ``user=tau2`` replaces that simulator with tau2's own: its
``UserSimulator`` (the same prompts, tool calling and stop tokens) through
tau2's LiteLLM wrapper, so the user is a model LiteLLM names, with its own
credentials (for example ``OPENAI_API_KEY``), not the run's judge. ``user_llm``
picks the model (default tau2's ``gpt-4.1``) and ``user_llm_args`` is a JSON
object of LiteLLM arguments (default ``{"temperature": 0}``). The conversation
starts as in tau2: the user answers the agent's greeting. ``user=evalsi`` (the
default) keeps the simulator described above. Recording and replay work with
either; a recorded run replays without calling the user model.

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

from evalsi.packs.judge import fence
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
    user = _user_option(options)
    rows = []
    for task in tasks:
        criteria = task.evaluation_criteria
        if criteria is not None and RewardType.NL_ASSERTION in criteria.reward_basis:
            continue
        meta: dict[str, Any] = {"domain": domain, "task": task.model_dump(mode="json")}
        if user is not None:
            meta["user"] = user
        rows.append(
            {
                "id": f"{domain}/{task.id}",
                "input": str(task.user_scenario),
                "reference": criteria.model_dump(mode="json") if criteria else None,
                "metadata": {"taubench": meta},
            }
        )
    return rows


def _user_option(options: dict[str, str]) -> dict[str, Any] | None:
    """The ``user`` options as record metadata; None for the default simulator."""
    kind = options.get("user", "evalsi")
    if kind not in ("evalsi", "tau2"):
        raise ValueError(f"user must be evalsi or tau2, got {kind!r}")
    if kind == "evalsi":
        if options.get("user_llm") or options.get("user_llm_args"):
            raise ValueError("user_llm and user_llm_args apply to user=tau2")
        return None
    from tau2.config import DEFAULT_LLM_ARGS_USER, DEFAULT_LLM_USER

    args: Any = dict(DEFAULT_LLM_ARGS_USER)
    if options.get("user_llm_args"):
        try:
            args = json.loads(options["user_llm_args"])
        except json.JSONDecodeError as exc:
            raise ValueError(f"user_llm_args is not JSON: {exc}") from exc
        if not isinstance(args, dict):
            raise ValueError("user_llm_args must be a JSON object")
    return {"kind": "tau2", "llm": options.get("user_llm") or DEFAULT_LLM_USER, "llm_args": args}


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

    def _user_simulator_identity(self, task: Task) -> dict[str, Any]:
        return {
            "adapter": "taubench",
            "version": 1,
            "config": json_format.MessageToDict(self.config.user_simulator),
            "scenario": task.record.metadata["taubench"],
        }

    def _reexecute_tool(self, live: LiveTask, tool: Tool) -> bool:
        return tool.spec.name in self._state(live).tool_names or super()._reexecute_tool(live, tool)

    def _user_simulator(self, task: Task) -> UserSimulator | None:
        us = self.config.user_simulator

        def judge() -> Any:
            return self._user_judge(us.judge)

        state = next((st for st in self._tau.values() if st.source is task), None)
        wanted = task.record.metadata["taubench"].get("user")
        if state is not None and isinstance(wanted, dict) and wanted.get("kind") == "tau2":
            state.user = Tau2User(
                env=state.env,
                task=state.task,
                history=state.history,
                llm=str(wanted["llm"]),
                llm_args=dict(wanted.get("llm_args") or {}),
                max_turns=us.max_turns or 30,
                seed=f"{task.id}#{task.trial}",
            )
            return state.user
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
        self._replay_calls: list[list[str]] = []

    def replay_state(self) -> dict[str, Any]:
        state = {
            "actions": self._replay_calls,
            "transcript": list(self.transcript),
            "seen": self.seen,
            "calls": self.calls,
            "exchanges": [[m.model_dump(mode="json") for m in ex] for ex in self.exchanges],
            "pending": [m.model_dump(mode="json") for m in self.pending],
        }
        self._replay_calls = []
        return state

    def restore_replay_state(self, state: dict[str, Any]) -> None:
        from tau2.data_model.message import ToolMessage, UserMessage

        # Apply this turn's actions at the same point relative to agent calls.
        for name, raw in state["actions"]:
            self._call(name, raw)
        self._replay_calls = []
        self.transcript = list(state["transcript"])
        self.seen = state["seen"]
        self.calls = state["calls"]

        def message(data: dict[str, Any]) -> Any:
            cls = ToolMessage if data["role"] == "tool" else UserMessage
            return cls.model_validate(data)

        self.exchanges = [[message(m) for m in ex] for ex in state["exchanges"]]
        self.pending = [message(m) for m in state["pending"]]

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
                f"<your_tools>\n{fence(self._tool_list())}\n</your_tools>\n"
                f'<conversation id="{self.seed}">\n'
                + fence("\n".join(self.transcript))
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

        self._replay_calls.append([name, raw])
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


class Tau2User(ToolUser):
    """tau2's own user simulator: tau2's ``UserSimulator`` over its LiteLLM
    wrapper. The conversation, its tool calls (in domains where the user has
    tools) and the replay state are ToolUser's, so grading and recording work
    as they do for the Evals.si simulator; only the user's turns come from tau2."""

    def __init__(
        self,
        *,
        env: Any,
        task: Any,
        history: list[Any],
        llm: str,
        llm_args: dict[str, Any],
        max_turns: int,
        seed: str,
    ) -> None:
        from tau2.user.user_simulator import UserSimulator as Tau2UserSimulator

        # No judge: the user model is LiteLLM's. ToolUser's own __init__ would
        # build the Evals.si prompt, which this user does not use.
        def no_judge() -> Any:
            raise TaskError("tau2's user does not use the run's judge")

        UserSimulator.__init__(self, no_judge, persona="", goal="", max_turns=max_turns, seed=seed)
        self.env = env
        user_tools = env.get_user_tools() if env.user_tools is not None else None
        self.tools = {t.name: t for t in user_tools or []}
        self.sim = Tau2UserSimulator(
            tools=user_tools,
            instructions=task.user_scenario.instructions,
            llm=llm,
            llm_args=llm_args,
        )
        self.state = self.sim.get_init_state(message_history=_user_view(history))
        self.transcript = []
        self.seen = 0
        self.exchanges = []
        self.pending = []
        self.calls = 0
        self._replay_calls = []

    async def reply(self, conversation: list[dict[str, Any]]) -> Any:
        import asyncio

        from tau2.data_model.message import AssistantMessage, MultiToolMessage, ToolMessage
        from tau2.orchestrator.orchestrator import DEFAULT_FIRST_AGENT_MESSAGE

        from evalsi.types import Usage
        from evalsi_harness.user_sim import UserTurn

        self.turns += 1
        if self.turns > self.max_turns:
            return UserTurn("", True, Usage())
        fresh = [
            (m.get("content") or "").strip()
            for m in conversation[self.seen :]
            if m["role"] == "assistant" and (m.get("content") or "").strip()
        ]
        self.seen = len(conversation)
        for text in fresh:
            self.transcript.append(f"AGENT: {text}")
        # As in tau2, the first thing the user answers is the agent's greeting.
        message: Any = (
            AssistantMessage(role="assistant", content="\n".join(fresh))
            if fresh
            else DEFAULT_FIRST_AGENT_MESSAGE
        )
        usage = Usage(input_tokens=0, output_tokens=0)
        for _ in range(MAX_USER_TOOL_CALLS + 1):
            reply, self.state = await asyncio.to_thread(
                self.sim.generate_next_message, message, self.state
            )
            used = reply.usage or {}
            usage.input_tokens = (usage.input_tokens or 0) + int(used.get("prompt_tokens", 0))
            usage.output_tokens = (usage.output_tokens or 0) + int(used.get("completion_tokens", 0))
            if reply.is_tool_call():
                results: list[ToolMessage] = []
                for call in reply.tool_calls or []:
                    results.append(self._tau2_call(call))
                message = (
                    results[0]
                    if len(results) == 1
                    else MultiToolMessage(role="tool", tool_messages=results)
                )
                continue
            text = (reply.content or "").strip()
            done = self.sim.is_stop(reply)
            for token in _STOP_TOKENS:
                text = text.replace(token, "").strip()
            if text:
                self.transcript.append(f"YOU: {text}")
                self.exchanges.append(self.pending)
                self.pending = []
            return UserTurn(text, done, usage)
        return UserTurn("", True, usage)

    def _tau2_call(self, call: Any) -> Any:
        """Runs a tool call tau2's user made, for the user and the grader."""
        from tau2.data_model.message import UserMessage

        self._replay_calls.append([call.name, json.dumps(call.arguments)])
        self.calls += 1
        result = self.env.get_response(call)
        self.pending += [UserMessage(role="user", content=None, tool_calls=[call]), result]
        self.transcript.append(
            f"YOUR TOOL CALL {call.name}({json.dumps(call.arguments)}) -> {result.content}"
        )
        return result


def _user_view(history: list[Any]) -> list[Any]:
    """The task's initial messages as tau2's user simulator accepts them: what
    was said, not the agent's tool traffic."""
    return [
        m
        for m in history
        if m.role in ("user", "assistant") and m.content and not getattr(m, "tool_calls", None)
    ]


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
