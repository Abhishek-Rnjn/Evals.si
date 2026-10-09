#!/bin/sh
# Runs Deep Agents Code (dcode) headless on one task: dcode-demo "<instruction>".
#
#   DEMO_MODEL         "provider:model" (default openai:mock)
#   DEMO_MODEL_PARAMS  extra model arguments as JSON; for an OpenAI-compatible
#                      server that speaks chat completions, not the Responses
#                      API, the default switches the Responses API off
#
# Shell is allowed (-S all) because the sandbox, not dcode, is the boundary;
# MCP servers, the JS interpreter, tracing to LangSmith, update checks and the
# LangGraph CLI's analytics are off.
set -eu
: "${1:?usage: dcode-demo INSTRUCTION}"
params="${DEMO_MODEL_PARAMS:-}"
if [ -z "$params" ]; then
  params='{"use_responses_api": false}'
fi
export DEEPAGENTS_CODE_NO_UPDATE_CHECK=1 DEEPAGENTS_CODE_AUTO_UPDATE=0 LANGGRAPH_CLI_NO_ANALYTICS=1
# The same switches in its config file, which some of its startup paths read instead.
mkdir -p "${HOME:?HOME must be set}/.deepagents"
if [ ! -e "$HOME/.deepagents/config.toml" ]; then
  cat > "$HOME/.deepagents/config.toml" <<'TOML'
[update]
check = false
auto_update = false

[warnings]
suppress = ["tavily"]
TOML
fi
exec dcode -n "$1" -S all -q --no-mcp --no-tracing --no-interpreter -M "${DEMO_MODEL:-openai:mock}" --model-params "$params"
