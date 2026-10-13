#!/usr/bin/env bash
# Phase 6's exit test with a real coding agent (docs/LEFTOVERS.md): Claude
# Code, headless, is given the evalsi MCP server and asked to run a spec and
# report its score. The check does not trust the agent's words alone: the run
# must have been made through the MCP `run` tool (the stream shows the call),
# its results file must exist with the known score (2 of 3), and the agent's
# answer must state that score and that the gate failed.
#
#   ANTHROPIC_API_KEY=... tests/mcp-agent/run.sh
#
# Needs claude (npm i -g @anthropic-ai/claude-code) and the Python workspace
# synced (cd python && uv sync --all-packages). CLAUDE overrides the binary.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
: "${ANTHROPIC_API_KEY:?set ANTHROPIC_API_KEY}"
claude="${CLAUDE:-claude}"
ws="$(mktemp -d)"
trap 'rm -rf "$ws"' EXIT
cp "$root/tests/mcp-agent/workspace/"* "$ws/"
cat > "$ws/mcp.json" <<JSON
{"mcpServers": {"evalsi": {"command": "uv", "args": ["run", "--no-sync", "--project", "$root/python", "evalsi", "mcp", "--root", "$ws"]}}}
JSON

cd "$ws"
"$claude" -p "Use the evalsi MCP tools to run the run spec answers.yaml in this directory. \
Then reply with one line: the exact-match mean as a decimal with two places, and whether the gate passed or failed." \
  --mcp-config "$ws/mcp.json" --strict-mcp-config \
  --allowedTools mcp__evalsi__run mcp__evalsi__list_evaluators \
  --output-format stream-json --verbose --max-turns 8 > "$ws/stream.jsonl"

python3 - "$ws" <<'PY'
import glob, json, sys
ws = sys.argv[1]
events = [json.loads(line) for line in open(f"{ws}/stream.jsonl") if line.strip()]
calls = [
    block for e in events if e.get("type") == "assistant"
    for block in e.get("message", {}).get("content", []) if block.get("type") == "tool_use"
]
names = [c["name"] for c in calls]
print("tool calls:", names)
assert "mcp__evalsi__run" in names, "the agent did not call the evalsi run tool"
saved = glob.glob(f"{ws}/.evalsi/results/answers-*.json")
assert len(saved) == 1, f"want one results file, got {saved}"
summaries = {s["metric"]: s for s in json.load(open(saved[0]))["summaries"]}
mean = summaries["exact-match"]["mean"]
assert abs(mean - 2 / 3) < 1e-9, f"exact-match mean {mean}, want 2/3"
final = next(e for e in reversed(events) if e.get("type") == "result")
answer = final.get("result", "")
print("answer:", answer)
assert final.get("subtype") == "success", final
assert "0.67" in answer, "the agent did not report the score"
assert "fail" in answer.lower(), "the agent did not report the failed gate"
print("passed")
PY
