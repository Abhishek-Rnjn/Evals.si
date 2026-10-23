"""A small ACP agent (protocol version 1) for tests: JSON-RPC over stdio.

On a prompt it streams a thought, a plan and some text, then runs a "tool call":
it asks permission, and when allowed reads ``notes.txt`` through the client,
writes ``out.txt`` through the client and runs ``echo from-terminal`` in a client
terminal. The prompt text ``refuse`` makes it answer ``refusal``; ``bad-version``
(an environment variable) makes ``initialize`` answer another protocol version.
"""

from __future__ import annotations

import json
import os
import sys
from typing import Any

NEXT = 1000
STATE = {"cwd": os.getcwd()}


def send(message: dict[str, Any]) -> None:
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", **message}) + "\n")
    sys.stdout.flush()


def update(session: str, **fields: Any) -> None:
    send({"method": "session/update", "params": {"sessionId": session, "update": fields}})


def ask(method: str, params: dict[str, Any]) -> Any:
    """A request to the client, waiting for its response (other lines are ignored)."""
    global NEXT  # noqa: PLW0603
    NEXT += 1
    send({"id": NEXT, "method": method, "params": params})
    for line in sys.stdin:
        message = json.loads(line)
        if message.get("id") == NEXT and "method" not in message:
            if "error" in message:
                raise RuntimeError(message["error"])
            return message.get("result")
    raise SystemExit("client went away")


def prompt(session: str, text: str, rid: int) -> None:
    update(
        session,
        sessionUpdate="agent_thought_chunk",
        content={"type": "text", "text": "thinking about it. "},
    )
    update(
        session,
        sessionUpdate="plan",
        entries=[{"content": "read notes", "priority": "high", "status": "pending"}],
    )
    update(
        session,
        sessionUpdate="agent_message_chunk",
        content={"type": "text", "text": "I will look at the notes. "},
    )
    if text.strip() == "refuse":
        send({"id": rid, "result": {"stopReason": "refusal"}})
        return
    call = {
        "toolCallId": "call_1",
        "title": "Run tests",
        "kind": "execute",
        "rawInput": {"command": "pytest -q"},
        "locations": [{"path": "/workspace/notes.txt"}],
    }
    update(session, sessionUpdate="tool_call", status="pending", **call)
    permission = ask(
        "session/request_permission",
        {
            "sessionId": session,
            "toolCall": call,
            "options": [
                {"optionId": "yes", "name": "Allow", "kind": "allow_once"},
                {"optionId": "no", "name": "Reject", "kind": "reject_once"},
            ],
        },
    )
    chosen = permission["outcome"].get("optionId")
    if chosen != "yes":
        update(
            session,
            sessionUpdate="tool_call_update",
            toolCallId="call_1",
            status="failed",
            content=[{"type": "content", "content": {"type": "text", "text": "not allowed"}}],
        )
        update(
            session,
            sessionUpdate="agent_message_chunk",
            content={"type": "text", "text": "I was not allowed to."},
        )
        send({"id": rid, "result": {"stopReason": "end_turn"}})
        return
    update(session, sessionUpdate="tool_call_update", toolCallId="call_1", status="in_progress")
    workdir = STATE["cwd"]
    notes = ask("fs/read_text_file", {"sessionId": session, "path": f"{workdir}/notes.txt"})
    ask(
        "fs/write_text_file",
        {"sessionId": session, "path": f"{workdir}/out.txt", "content": notes["content"].upper()},
    )
    try:
        ask("fs/read_text_file", {"sessionId": session, "path": f"{workdir}/missing.txt"})
        missing = "found"
    except RuntimeError as exc:
        missing = f"error {exc.args[0]['code']}"
    terminal = ask(
        "terminal/create",
        {"sessionId": session, "command": "echo", "args": ["from-terminal"]},
    )
    exit_status = ask(
        "terminal/wait_for_exit", {"sessionId": session, "terminalId": terminal["terminalId"]}
    )
    output = ask("terminal/output", {"sessionId": session, "terminalId": terminal["terminalId"]})
    ask("terminal/release", {"sessionId": session, "terminalId": terminal["terminalId"]})
    update(
        session,
        sessionUpdate="tool_call_update",
        toolCallId="call_1",
        status="completed",
        content=[
            {
                "type": "content",
                "content": {
                    "type": "text",
                    "text": f"read {notes['content'].strip()!r}; missing: {missing}; "
                    f"terminal said {output['output'].strip()!r} exit {exit_status['exitCode']}",
                },
            }
        ],
    )
    update(
        session,
        sessionUpdate="agent_message_chunk",
        content={"type": "text", "text": "Done: wrote out.txt. café ✓"},
    )
    send({"id": rid, "result": {"stopReason": "end_turn"}})


def main() -> None:
    for line in sys.stdin:
        message = json.loads(line)
        method, rid = message.get("method"), message.get("id")
        if method == "initialize":
            version = 99 if os.environ.get("FAKE_ACP_BAD_VERSION") else 1
            send(
                {
                    "id": rid,
                    "result": {
                        "protocolVersion": version,
                        "agentCapabilities": {"loadSession": False},
                        "agentInfo": {"name": "fake-acp", "version": "1"},
                        "authMethods": [],
                    },
                }
            )
        elif method == "session/new":
            STATE["cwd"] = message["params"]["cwd"]
            send({"id": rid, "result": {"sessionId": "s1"}})
        elif method == "session/prompt":
            text = "".join(p.get("text", "") for p in message["params"]["prompt"])
            prompt(message["params"]["sessionId"], text, rid)
        elif rid is not None:
            send({"id": rid, "error": {"code": -32601, "message": f"unknown {method}"}})


if __name__ == "__main__":
    print("a stray line that is not JSON-RPC", flush=True)
    main()
