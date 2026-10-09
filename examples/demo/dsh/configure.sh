#!/bin/sh
# Points dsh at the model named by the environment, then runs the command.
#
#   DEMO_PROVIDER_API  openai-completions (default), openai-responses or anthropic-messages
#   DEMO_BASE_URL      the endpoint, e.g. http://mock-model:8000/v1 (required)
#   DEMO_MODEL         the model id (default: mock)
#   DEMO_API_KEY_ENV   the name of the variable holding the key (default: DEMO_API_KEY)
#
# The key itself stays in its variable: dsh reads it from there (apiKeyEnv), so
# nothing secret is written to disk. Every profile dsh is run with gets the
# same provider and default model.
set -eu

: "${DEMO_BASE_URL:?set DEMO_BASE_URL to the model endpoint}"
api="${DEMO_PROVIDER_API:-openai-completions}"
model="${DEMO_MODEL:-mock}"
keyenv="${DEMO_API_KEY_ENV:-DEMO_API_KEY}"
home="${DSH_HOME:-$HOME/.dsh}"

for profile in headless web tui; do
  dir="$home/profiles/$profile"
  mkdir -p "$dir"
  cat > "$dir/cordis.patch.yml" <<YAML
- id: llm-pi-ai
  config:
    providers:
      demo:
        apiKeyEnv: $keyenv
        api: $api
        baseURL: $DEMO_BASE_URL
        models:
          - id: $model
- id: agent-default-model
  config:
    provider: demo
    model: $model
YAML
done
exec "$@"
