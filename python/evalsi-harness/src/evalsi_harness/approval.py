"""``ApprovalGate``: a hook that asks before a tool call runs.

List it under ``harness.builtin.hooks`` with rules and a scripted approver::

    hooks:
      - python: evalsi_harness.approval:ApprovalGate
        config:
          rules:                                  # first match wins; none matching = allow
            - {tool: bash, command: "git push*", action: ask}
            - {tool: write_file, path: "/etc/*", action: deny, reason: "system files"}
          approver:
            script:                               # first entry that matches and has turns left
              - {match: {command: "git push*"}, decision: deny, reason: "not on a Friday"}
              - {match: {tool: bash}, decision: approve}
            default: deny                         # approve | deny | no-answer; deny when unset

A rule's ``tool``, ``path`` and ``command`` are globs (see ``call_matches``); its action is
``allow``, ``ask`` or ``deny``. A ``deny`` stops the call with its reason and asks nobody. An
``ask`` records a ``hitl_request`` step, the approver answers, and a ``hitl_resolved`` step
records the answer: ``approve`` runs the call, ``edit`` runs it with the entry's ``arguments``
merged over the model's (an edited call that matches a ``deny`` rule is stopped), and ``deny``
or ``no-answer`` stops it (the agent sees the reason; a ``no-answer``
reads as an approver that never replied). An entry with ``times: n`` answers at most n
requests.

The approver is a script, not a person or a model, so the same run asks the same questions
and gets the same answers, and a recording replays with no approver involved. The rules and
script are part of the hook's config, which a recording checks. It is a pattern gate: it
matches the tool, ``path`` and ``command`` arguments, so a ``bash`` command that is chained,
wrapped in ``bash -c`` or assembled from variables can get past a command rule. Put a rule on
``bash`` itself when that matters. Calls from sub-agents and code-mode scripts pass through it
too; the denied-call evaluators are ``builtin/denial-respected`` and
``builtin/approval-coverage``.
"""

from __future__ import annotations

import json
from typing import Any

from evalsi.packs.approval import call_matches
from evalsi_harness.hooks import Deny, HookContext, Rewrite
from evalsi_harness.models import ToolCallRequest
from evalsi_harness.task import TaskError

ACTIONS = ("allow", "ask", "deny")
DECISIONS = ("approve", "edit", "deny", "no-answer")
_FALLBACK = ("approve", "deny", "no-answer")
_PATTERN_KEYS = {"tool", "path", "command"}


def _pattern(raw: Any, where: str) -> dict[str, str]:
    if not isinstance(raw, dict):
        raise TaskError(f"approval gate: {where} must be an object")
    bad = set(raw) - _PATTERN_KEYS
    if bad:
        raise TaskError(f"approval gate: {where} has unknown keys {sorted(bad)}")
    return {k: str(v) for k, v in raw.items()}


class ApprovalGate:
    def __init__(self, config: dict[str, Any] | None = None) -> None:
        config = config or {}
        self.rules: list[tuple[dict[str, str], str, str]] = []
        for i, rule in enumerate(config.get("rules") or []):
            if not isinstance(rule, dict):
                raise TaskError(f"approval gate: rules[{i}] must be an object")
            action = rule.get("action", "ask")
            if action not in ACTIONS:
                raise TaskError(f"approval gate: rules[{i}].action must be one of {ACTIONS}")
            pattern = _pattern({k: v for k, v in rule.items() if k in _PATTERN_KEYS}, f"rules[{i}]")
            unknown = set(rule) - _PATTERN_KEYS - {"action", "reason"}
            if unknown:
                raise TaskError(f"approval gate: rules[{i}] has unknown keys {sorted(unknown)}")
            self.rules.append((pattern, action, str(rule.get("reason") or "")))
        approver = config.get("approver") or {}
        self.default = approver.get("default", "deny")
        if self.default not in _FALLBACK:
            raise TaskError(f"approval gate: approver.default must be one of {_FALLBACK}")
        self.script: list[dict[str, Any]] = []
        for i, entry in enumerate(approver.get("script") or []):
            if not isinstance(entry, dict):
                raise TaskError(f"approval gate: approver.script[{i}] must be an object")
            decision = entry.get("decision")
            if decision not in DECISIONS:
                raise TaskError(
                    f"approval gate: approver.script[{i}].decision must be one of {DECISIONS}"
                )
            if decision == "edit" and not isinstance(entry.get("arguments"), dict):
                raise TaskError(f"approval gate: approver.script[{i}] edit needs arguments")
            times = entry.get("times")
            # Spec config arrives as JSON numbers, so 1 may be 1.0.
            if times is not None and (
                isinstance(times, bool) or not isinstance(times, int | float) or times < 1
            ):
                raise TaskError(f"approval gate: approver.script[{i}].times must be at least 1")
            self.script.append(
                {
                    "match": _pattern(entry.get("match") or {}, f"approver.script[{i}].match"),
                    "decision": decision,
                    "reason": str(entry.get("reason") or ""),
                    "arguments": entry.get("arguments"),
                    "left": None if times is None else int(times),
                }
            )
        self.requests = 0

    def _rule(self, tool: str, args: dict[str, Any]) -> tuple[int, str, str]:
        for i, (pattern, action, reason) in enumerate(self.rules):
            if call_matches(tool, args, pattern):
                return i, action, reason
        return -1, "allow", ""

    def _answer(
        self, tool: str, args: dict[str, Any]
    ) -> tuple[str, str, int, dict[str, Any] | None]:
        for i, entry in enumerate(self.script):
            if entry["left"] == 0 or not call_matches(tool, args, entry["match"]):
                continue
            if entry["left"] is not None:
                entry["left"] -= 1
            return entry["decision"], entry["reason"], i, entry["arguments"]
        return self.default, "", -1, None

    def before_tool(self, ctx: HookContext, call: ToolCallRequest) -> Deny | Rewrite | None:
        try:
            args = json.loads(call.arguments or "{}")
        except json.JSONDecodeError:
            return None  # the tool reports the bad arguments itself
        if not isinstance(args, dict):
            return None
        rule, action, why = self._rule(call.name, args)
        if action == "allow":
            return None
        if action == "deny":
            return Deny(why or f"{call.name} is not allowed by the approval policy")
        self.requests += 1
        ctx.emit(
            "hitl_request",
            request=self.requests,
            call_id=call.id,
            tool=call.name,
            arguments=json.dumps(args, sort_keys=True, default=str),
            rule=rule,
            reason=why,
        )
        decision, reason, entry, edit = self._answer(call.name, args)
        final = {**args, **edit} if decision == "edit" and edit else args
        if decision == "edit":  # an edit cannot reach what a deny rule forbids
            _, edited_action, edited_why = self._rule(call.name, final)
            if edited_action == "deny":
                decision = "deny"
                reason = edited_why or f"the edited {call.name} call is not allowed by the policy"
                final = args
        ctx.emit(
            "hitl_resolved",
            request=self.requests,
            call_id=call.id,
            tool=call.name,
            arguments=json.dumps(final, sort_keys=True, default=str),
            decision=decision,
            reason=reason,
            script_entry=entry,
        )
        if decision == "approve":
            return None
        if decision == "edit":
            return Rewrite(final)
        if decision == "no-answer":
            return Deny(reason or "no approver replied to the request, so the call was not run")
        return Deny(reason or "an approver denied the request")
