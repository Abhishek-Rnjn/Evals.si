"""A simulated user for multi-turn tasks, in the style of tau-bench.

After each agent reply the simulator, an LLM speaking through the run's
judge, answers as the user with a persona and a goal, or ends the
conversation when the goal is met (or cannot be). It sees only what a user
would see: the conversation, never the agent's tool calls.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from evalsi.judges import JudgeClient
from evalsi.types import Usage

SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "message": {"type": "string", "description": "What the user says next."},
        "done": {
            "type": "boolean",
            "description": "True when the goal is met, or the user gives up.",
        },
    },
    "required": ["message", "done"],
    "additionalProperties": False,
}

SYSTEM = (
    "You are simulating a user talking to an AI agent, to test the agent. Stay in "
    "character: speak only as the user, in the first person, briefly, as a real user "
    "would. Reveal information only when the agent asks for it or when it is needed. Do "
    "not solve the task yourself and never act as the agent. When the agent has done what "
    "you wanted, or it clearly cannot, set done to true."
)


@dataclass
class UserTurn:
    message: str
    done: bool
    usage: Usage


class UserSimulator:
    def __init__(
        self, judge: JudgeClient, *, persona: str, goal: str, max_turns: int = 10, seed: str = ""
    ) -> None:
        self.judge = judge
        self.persona = persona
        self.goal = goal
        self.max_turns = max_turns or 10
        # Part of the prompt, so cached replies differ between trials.
        self.seed = seed
        self.turns = 0

    async def reply(self, conversation: list[dict[str, Any]]) -> UserTurn:
        """The user's next message, given the conversation so far (user and
        assistant messages only)."""
        self.turns += 1
        if self.turns > self.max_turns:
            return UserTurn("", True, Usage())
        transcript = "\n".join(
            f"{'USER' if m['role'] == 'user' else 'AGENT'}: {m.get('content') or ''}"
            for m in conversation
            if m["role"] in ("user", "assistant") and (m.get("content") or "").strip()
        )
        prompt = (
            f"<persona>\n{self.persona or 'An ordinary customer.'}\n</persona>\n"
            f"<goal>\n{self.goal}\n</goal>\n"
            f'<conversation id="{self.seed}">\n{transcript}\n</conversation>\n'
            "Write the user's next message."
        )
        response = await self.judge.backend.complete_json(
            system=SYSTEM, prompt=prompt, schema=SCHEMA
        )
        data = response.data
        return UserTurn(
            message=str(data.get("message", "")).strip(),
            done=bool(data.get("done")),
            usage=Usage(input_tokens=response.input_tokens, output_tokens=response.output_tokens),
        )
