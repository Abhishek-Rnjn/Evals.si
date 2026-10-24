# 2. TinyLM, a fake model that calls tools

To evaluate an agent you need a model behind it. A real one costs money, needs a
key, and answers differently every time, which is exactly what you do not want
while you are learning what the evaluators do. So these tutorials use a fake.

TinyLM ([`examples/tinylm/tinylm.py`](../../examples/tinylm/tinylm.py)) is about
600 lines of standard-library Python. It speaks the same API as OpenAI, vLLM,
Ollama and most gateways, so Evals.si drives it exactly as it would drive any of
those. Swap its URL for a real one and nothing else in your spec changes.

## What "fake autoregressive model" means here

A language model produces its reply one token at a time, each conditioned on the
conversation so far. TinyLM keeps the shape of that and replaces the network:

1. **A policy reads the conversation.** It finds the task (the first user
   message), the tool calls made so far and their results, and decides what to do
   next: look around, read a file, write a fix, test it, or answer.
2. **The decision is decoded token by token.** Text and tool-call arguments are
   split into tokens, and when the caller asks for a stream every token is its own
   server-sent event, the way a provider's stream arrives.
3. **Usage is counted.** `prompt_tokens` and `completion_tokens` come back on
   every call, so budgets, cost and efficiency metrics have real numbers to work on.

Because the policy is a function of the conversation, the same conversation
always gets the same reply, which is what makes the tutorials reproducible.

## The personas

The model name picks a behaviour. Each one is a failure mode real agents have:

| Model | Behaviour | Which evaluator catches it |
|---|---|---|
| `tinylm-good` | lists files, reads the module, fixes every bug, self-tests, answers | (it passes) |
| `tinylm-lazy` | answers "Done!" without doing anything | `task-success` |
| `tinylm-partial` | fixes only the first bug | `task-success` |
| `tinylm-looper` | reads the same file forever | `loop-detection`, `run-completed` |
| `tinylm-hallucinator` | calls `run_tests`, which does not exist, then gives up | `tool-errors`, `task-success` |
| `tinylm-clumsy` | sends broken JSON arguments once, then recovers | `tool-errors` (and it still passes) |
| `tinylm-sneaky` | tries the network and asks for an escalation first | `policy-violations` |
| `tinylm-flaky` | botches one fix on every third attempt at a task | `pass^k` over trials |
| `tinylm-gullible` | obeys instructions planted in tool output | `injection-followed` |
| `tinylm-pusher` | commits and tries to `git push`; stops when told no | `denial-respected` (passes) |
| `tinylm-stubborn` | tries the push another way after a denial | `denial-respected` |
| `tinylm-support` | a refund agent: looks the order up, refunds only if paid | `tool-call-accuracy` (passes) |
| `tinylm-reckless` | refunds without looking anything up | `tool-call-accuracy` |

The coding personas know arithmetic "by name": a function called `mul` should
multiply, `mod` should take a remainder. That is enough to fix the bug-fix tasks
the next tutorials use without the fix being hard-coded.

## Start it and talk to it

```bash
cd examples/tinylm
python3 tinylm.py --port 8765 -v &
```

```text
TinyLM listening on http://127.0.0.1:8765/v1 (models: tinylm-good, tinylm-lazy, tinylm-partial, tinylm-looper, tinylm-hallucinator, tinylm-clumsy, tinylm-sneaky, tinylm-flaky, tinylm-pusher, tinylm-stubborn, tinylm-gullible, tinylm-support, tinylm-reckless)
```

`-v` logs every decision to standard error, which is the fastest way to see what
the agent is doing while a run is in progress. `--token-delay-ms 20` slows the
stream down if you want to watch it.

List the models:

```bash
curl -s localhost:8765/v1/models
```

```json
{"object": "list", "data": [{"id": "tinylm-good", "object": "model", "owned_by": "tinylm"}, {"id": "tinylm-lazy", ...
```

Ask for one turn, the way an agent harness would on its first call: a user task
and a list of tools.

```bash
curl -s localhost:8765/v1/chat/completions -d '{
  "model": "tinylm-good",
  "messages": [{"role": "user", "content": "Fix calc.py"}],
  "tools": [{"type": "function", "function": {"name": "bash", "parameters": {"type": "object"}}}]
}'
```

```json
{
    "id": "chatcmpl-18f65a5ec4ad",
    "object": "chat.completion",
    "model": "tinylm-good",
    "system_fingerprint": "tinylm-1",
    "choices": [
        {
            "index": 0,
            "message": {
                "role": "assistant",
                "content": "Let me look at the files in the working directory.",
                "tool_calls": [
                    {
                        "id": "call_c38a53256c7b_0",
                        "type": "function",
                        "function": {"name": "bash", "arguments": "{\"command\": \"ls -la\"}"}
                    }
                ]
            },
            "finish_reason": "tool_calls"
        }
    ],
    "usage": {"prompt_tokens": 53, "completion_tokens": 35, "total_tokens": 88}
}
```

This is the whole contract. The model returns text, zero or more tool calls with
JSON-encoded `arguments`, and a `finish_reason`: `tool_calls` means "run these and
call me again with the results", `stop` means "I am done, the text is my answer".

## The stream

With `"stream": true` the same reply arrives as 38 events. The text comes first,
a token per event:

```text
data: {"choices": [{"delta": {"role": "assistant", "content": ""}, "finish_reason": null}]}
data: {"choices": [{"delta": {"content": "Let"}, "finish_reason": null}]}
data: {"choices": [{"delta": {"content": " "}, "finish_reason": null}]}
data: {"choices": [{"delta": {"content": "me"}, "finish_reason": null}]}
...
```

Then the tool call: one event that names it, and its JSON arguments in pieces. The
client has to stitch them back together, and only knows the arguments are complete
when the finish reason arrives:

```text
data: {"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "command"}}]}, "finish_reason": null}]}
data: {"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "\""}}]}, "finish_reason": null}]}
...
data: {"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "la"}}]}, "finish_reason": null}]}
data: {"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "\""}}]}, "finish_reason": null}]}
data: {"choices": [{"delta": {"tool_calls": [{"index": 0, "function": {"arguments": "}"}}]}, "finish_reason": null}]}
data: {"choices": [{"delta": {}, "finish_reason": "tool_calls"}]}
data: {"choices": [], "usage": {"prompt_tokens": 9, "completion_tokens": 35, "total_tokens": 44}}
data: [DONE]
```

The built-in harness's stream reader checks exactly this: a stream that ends with
no finish reason is a broken response, not an answer, and a tool call cut off
mid-arguments is never run (see `stream_resume_attempts` in
[agent runs](../guides/agent-runs.md#model-options)).

## Using it from a run spec

The built-in agent is selected when `target` names a model. For TinyLM:

```yaml
target:
  connector: openai-compatible
  model: tinylm-good                    # the persona
  base_url: http://127.0.0.1:8765/v1
  stream: true                          # optional: token by token
```

That is the same block you would write for vLLM or a gateway, with your own
`model` and `base_url` and an `api_key_env` naming the variable that holds the key.
TinyLM ignores keys, so there is none here.

## Writing your own fake

If you want a stand-in for your own agent's model, copy `tinylm.py` and change
`decide()`. It receives the persona, the messages in chat-completions form and the
names of the tools offered, and returns a `Turn(text, calls)`. Everything about
HTTP, streaming and token counting is already handled.

The harness's own tests use a similar helper, `evalsi_harness.testing.ScriptedModelServer`,
which also speaks the Anthropic Messages API. It is a test utility, not a public
API, so prefer a copy of TinyLM for anything you keep.

Next: [3. Your first agent run with the built-in harness](03-built-in-harness.md).
