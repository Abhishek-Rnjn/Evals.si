"""A Claude Agent SDK agent, traced by Claude Code's own OTel traces (beta) or
by OpenInference.

    python record_claude_agent_sdk.py native|openinference

The SDK runs its bundled Claude Code CLI, which calls the Anthropic Messages
API at ANTHROPIC_BASE_URL (the mock model). The CLI gets an empty home, so
nothing of the recording machine's own Claude configuration is read.
"""

from __future__ import annotations

import asyncio
import os
import sys
import tempfile
from typing import Any

from otlp_capture import MOCK_URL, QUESTION, Capture, search_docs, tracer_provider

MODEL = "claude-sonnet-4-5"


def main() -> None:
    mode = sys.argv[1]
    # The CLI inherits this process's environment. Keep only what it needs:
    # a session the recorder runs in must not leak into it (a TRACEPARENT
    # marked unsampled silences its spans; a proxy carries the export away).
    for k in list(os.environ):
        if k not in ("PATH", "LANG", "LC_ALL", "TMPDIR"):
            del os.environ[k]
    capture = Capture()
    home = tempfile.mkdtemp()
    env = {
        "HOME": home,
        "CLAUDE_CONFIG_DIR": home,
        "ANTHROPIC_BASE_URL": MOCK_URL.removesuffix("/v1"),
        "ANTHROPIC_API_KEY": "unused",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "DISABLE_AUTOUPDATER": "1",
    }
    provider = None
    if mode == "native":
        env.update(
            CLAUDE_CODE_ENABLE_TELEMETRY="1",
            CLAUDE_CODE_ENHANCED_TELEMETRY_BETA="1",
            OTEL_TRACES_EXPORTER="otlp",
            OTEL_METRICS_EXPORTER="none",
            OTEL_LOGS_EXPORTER="none",
            OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf",
            OTEL_EXPORTER_OTLP_ENDPOINT=capture.endpoint.removesuffix("/v1/traces"),
            OTEL_LOG_USER_PROMPTS="1",
            OTEL_LOG_TOOL_DETAILS="1",
            OTEL_LOG_TOOL_CONTENT="1",
            OTEL_LOG_ASSISTANT_RESPONSES="1",
            OTEL_SERVICE_NAME="evalsi-fixture-claude-agent-sdk",
        )
    elif mode == "openinference":
        from openinference.instrumentation.claude_agent_sdk import ClaudeAgentSDKInstrumentor

        provider = tracer_provider(capture, "evalsi-fixture-claude-agent-sdk")
        ClaudeAgentSDKInstrumentor().instrument(tracer_provider=provider)
    else:
        sys.exit(f"unknown mode {mode}")

    asyncio.run(_run(env))
    if provider is not None:
        provider.force_flush()
    capture.wait(spans=2, timeout=30)
    capture.write(f"claude-agent-sdk-{mode}")


async def _run(env: dict[str, str]) -> None:
    from claude_agent_sdk import (
        ClaudeAgentOptions,
        ClaudeSDKClient,
        create_sdk_mcp_server,
        tool,
    )

    @tool("search_docs", "Searches the documentation corpus.", {"query": str})
    async def search(args: dict[str, Any]) -> dict[str, Any]:
        return {"content": [{"type": "text", "text": search_docs(args["query"])}]}

    options = ClaudeAgentOptions(
        model=MODEL,
        system_prompt="Search the documentation corpus, then write a short report.",
        mcp_servers={"docs": create_sdk_mcp_server("docs", tools=[search])},
        allowed_tools=["mcp__docs__search_docs"],
        tools=[],
        max_turns=4,
        env=env,
        cwd=env["HOME"],
    )
    async with ClaudeSDKClient(options=options) as client:
        await client.query(QUESTION)
        async for _ in client.receive_response():
            pass
        # The CLI exports spans in batches and is stopped when the client
        # closes: give it a batch interval first.
        await asyncio.sleep(7)


if __name__ == "__main__":
    main()
