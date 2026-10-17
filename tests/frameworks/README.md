# Framework trace recorders

These scripts record the fixtures in [`internal/ingest/testdata/frameworks`](../../internal/ingest/testdata/frameworks). The fixtures test the mapping profiles in `internal/ingest` (see [the guide](../../docs/guides/agent-frameworks.md)).

Each recorder:
- runs one agent with one instrumentation against the demo's [mock model](../../examples/demo/mock-model/server.py);
- captures the OTLP export locally;
- writes it as OTLP/JSON, with every export merged and the machine's resource attributes (host, process, OS) dropped.

Every framework does the same task: one research question, one `search_docs` call, then a fixed report.

Give each framework its own environment; their dependencies conflict. The versions used for the checked-in fixtures are pinned in `requirements-<framework>.txt`. CI does not install them: the fixtures are checked in, and only the Go test runs.

```bash
python3 ../../examples/demo/mock-model/server.py --port 8124 &

uv venv /tmp/fw-crewai && VIRTUAL_ENV=/tmp/fw-crewai uv pip install -r requirements-crewai.txt
/tmp/fw-crewai/bin/python record_crewai.py mlflow          # or openinference, openllmetry
```

For a live OpenAI-compatible model, set `MOCK_MODEL_URL` to its `/v1` endpoint
and `EVALSI_FRAMEWORK_MODEL` to its served model ID. The default remains the
mock's `gpt-4o-mini` alias. Use a disposable checkout: recorders overwrite their
named fixtures. A successful recording is not by itself a mapping-profile
verification; check the exported model/tool/usage fields and run the profile
tests against the recording before claiming compatibility.

For live recordings, build the ingest test binary for the runner's platform
and execute `TestLiveFrameworkProfiles` there with
`EVALSI_FRAMEWORK_RECORDINGS` pointing to the exports and
`EVALSI_FRAMEWORK_MODEL` naming the actual OpenAI-compatible model. Include
`EVALSI_FRAMEWORK_ANTHROPIC_MODEL` when the directory contains Claude exports.
The test uses each instrumentation's existing contract, including documented
missing answers, tool spans and usage, rather than treating them as present.

The Claude Agent SDK recorder also accepts `EVALSI_FRAMEWORK_MODEL`.
For live Anthropic access, explicitly set `EVALSI_FRAMEWORK_API_KEY_ENV` to
the name of the variable holding its key and set `MOCK_MODEL_URL` to the
gateway's `/v1` URL. No ambient credentials or Claude configuration are
inherited; the default remains the credential-free mock.

| Recorder | Modes |
|---|---|
| `record_crewai.py` | `mlflow`, `openinference`, `openllmetry` |
| `record_langgraph.py` | `openinference`, `langsmith` |
| `record_openai_agents.py` | `openinference`, `openllmetry` |
| `record_llamaindex.py` | `openinference` |
| `record_claude_agent_sdk.py` | `native` (Claude Code's own traces), `openinference` |

The Claude Agent SDK recorder starts the bundled Claude Code CLI with only `PATH`, `LANG`, `LC_ALL` and `TMPDIR` from its environment, and with an empty home. Nothing of the machine's own Claude setup reaches it. A `TRACEPARENT` that a surrounding session sets, marked as not sampled, would silence its spans.

After recording, check the fixture before committing it:

```bash
grep -l "/home\|/root\|/tmp" ../../internal/ingest/testdata/frameworks/*.json   # should print nothing
go test ./internal/ingest -run TestFrameworkProfiles
```

Never edit a fixture by hand to make the test pass. If a new framework version records something different, record it again and change the profile.
