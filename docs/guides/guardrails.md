# Inline guardrails

Online policies (`evalsi watch`) score traffic after it has happened. A
guardrail runs while a request is in flight and changes what gets through. It
sits between a client and a model, or between an agent and its tools. For
each piece of content it does three things:

1. **Redacts.** Built-in detectors and your own patterns replace e-mail
   addresses, card numbers, keys and the like. The redacted text is what
   continues on to the model or tool, and it is also what the evaluators
   (and any judge) see.
2. **Evaluates.** Record-scope evaluators from any installed pack score the
   content, using the same packs as offline and online evaluation.
3. **Decides.** The content passes, passes redacted (mask), or is blocked.

evalsid speaks agentgateway's two inline protocols, so no gateway code is
needed:

| Traffic | agentgateway feature | evalsid endpoint |
|---|---|---|
| LLM calls | `ai.promptGuard.request/response[].webhook` | `POST /guardrails/{project}/{guardrail}/request` and `/response` |
| MCP calls | `mcpGuardrails.processors[]` of kind `remote` (the ExtMcp gRPC protocol) | `agentgateway.dev.ext_mcp.ExtMcp` on the main port |

Anything else can call `GuardrailService.Check` directly, over gRPC, Connect
or REST.

## Write a guardrail

```yaml
# examples/guardrails/support.yaml
name: support-chat
project: support
redact:
  - builtin: email
  - builtin: us-ssn
  - builtin: credit-card
  - pattern: 'ACCT-\d{6,}'
    replacement: '<ACCOUNT>'
evaluators:
  - secret-leak
  - ref: refusal
block_when: >-
  scores["secret-leak"] < 1 ||
  (phase == "response" && labels[?"tier"].orValue("") == "free" && content.size() > 20000)
message: This request was blocked by the support-chat guardrail.
timeout: 1.5s
failure_mode: closed
mode: enforce
```

```bash
evalsi guardrails apply -f examples/guardrails/support.yaml --server $EVALSID
evalsi guardrails check support-chat --project support --text "my key is AKIAIOSFODNN7EXAMPLE" --server $EVALSID
# block: secret-leak: found aws-access-key x1     (exit code 3)
evalsi guardrails check -f draft.yaml --text "..." --server $EVALSID   # try a file without storing it
```

The fields:

- **`redact`.** Each rule is either a `builtin` or an RE2 `pattern`, with an
  optional `replacement`. The builtins are `email`, `phone`, `credit-card`
  (Luhn-checked), `us-ssn`, `ipv4`, `aws-access-key`, `github-token`,
  `api-key`, `private-key` and `jwt`. A builtin is replaced with `<TYPE>`,
  for example `<EMAIL>`, and a pattern with `[REDACTED]`. Redaction on its
  own never blocks anything.
- **`evaluators`.** Record-scope evaluators. They see the content under
  check as the record's `output`. For a prompt, that is the newest message,
  with the earlier messages (redacted) as the record's `input`; the earlier
  messages were themselves checked when they were new. For an answer it is
  the answer, and for an MCP call it is the string values of the call's
  arguments, or of its result.
- **`block_when`.** CEL over `scores`, `phase` (`"request"` or
  `"response"`), `source` (`"llm"`, `"mcp"` or `"api"`), `method`, `tool`,
  `content` and `labels`. If it is left out, any failed pass/fail score
  blocks.
- **`timeout`.** Defaults to 2s and can be at most 30s. Keep it under the
  gateway's own call timeout, which is 10s for both protocols.
- **`failure_mode`.** This decides what happens when an evaluator errors or
  times out, or when `block_when` cannot be evaluated (for example because
  it names a score that was skipped). `closed`, the default, blocks; `open`
  lets the content through. Either way the response carries
  `failed: true`, and the `evalsi_guardrail_failed_total` counter goes up.
- **`mode: audit`.** The guardrail decides and reports what it would do,
  but passes everything through unchanged. Use it to see what a new
  guardrail would block before you enforce it.
- **`phases`.** Limits the guardrail to `request` or `response`. By
  default it checks both.

Pick evaluators that fit the latency budget. Deterministic ones, such as
`pii-leak`, `secret-leak`, `canary-leak`, `refusal`, regex checks and the
`ml-classic` classifiers on features, take milliseconds. A judge-based
evaluator adds a model call to every request. Reserve those for routes
that can afford it, or run them online (`evalsi watch`) instead.

## Connect agentgateway

Give the gateway a credential that can do nothing but check content: an API
key bound to the built-in `guard` role in the project. Add `ingest` if the
same gateway also exports traces.

```yaml
# evalsi.yaml
auth:
  api_keys:
    keys:
      - name: gateway
        key: sha256:...            # from evalsid auth new-key
        roles: {support: [guard, ingest]}
```

The examples below send the key as `Authorization: Bearer` through
`backendAuth`, reading it from a file that the gateway watches for
rotation.

### LLM traffic: the prompt-guard webhook

```yaml
# agentgateway config
binds:
- port: 3000
  listeners:
  - routes:
    - backends:
      - ai:
          name: openai
          provider: {openAI: {model: gpt-4.1-mini}}
      policies:
        ai:
          promptGuard:
            request:
            - webhook:
                target:
                  host: evalsid.evalsi.svc:8080
                  policies:
                    backendAuth: {key: {value: {file: /etc/agentgateway/evalsi-key}}}
                headers:
                  ":path": '"/guardrails/support/support-chat/request"'
                  # Headers named X-Evalsi-Label-* become labels for block_when.
                  x-evalsi-label-tier: 'request.headers["x-tier"]'
                  x-evalsi-label-user: jwt.sub
                failureMode: failClosed
            response:
            - webhook:
                target:
                  host: evalsid.evalsi.svc:8080
                  policies:
                    backendAuth: {key: {value: {file: /etc/agentgateway/evalsi-key}}}
                headers:
                  ":path": '"/guardrails/support/support-chat/response"'
```

The webhook protocol works like this:

- **Requests.** The webhook receives every message of the prompt and
  answers with one of three actions:
  - **pass:** the prompt goes on unchanged;
  - **mask:** it returns every message, with the redacted content;
  - **reject:** the client gets `403` and the guardrail's `message`.
- **Masking cost.** When agentgateway applies a mask it rebuilds the
  messages as plain text: tool calls in the prompt are dropped, and
  multi-part content is flattened. evalsid therefore returns a mask only
  when something was actually redacted.
- **Streaming.** With `promptGuard.streaming: Enabled`, the gateway checks
  windows of the streamed answer. A block stops the stream, but text
  already sent cannot be recalled.
- **Errors.** evalsid answers errors (unknown guardrail, no permission,
  malformed body) in plain text, because agentgateway reads any JSON object
  it does not recognise as a pass. A plain-text error goes to the
  webhook's `failureMode` instead, which is `failClosed` unless you change
  it.

### MCP traffic: the ExtMcp processor

```yaml
# agentgateway config
binds:
- port: 3001
  listeners:
  - routes:
    - backends:
      - mcp:
          targets:
          - name: billing
            mcp: {host: http://billing-mcp:8080/mcp}
      policies:
        mcpGuardrails:
          processors:
          - kind: remote
            host: evalsid.evalsi.svc:8080
            policies:
              backendAuth: {key: {value: {file: /etc/agentgateway/evalsi-key}}}
            metadata:
              project: '"support"'
              guardrail: '"support-chat"'
              user: jwt.sub              # becomes labels["user"]
            methods:
              "tools/call": full          # arguments and results
              "tools/list": response      # tool descriptions (tool poisoning)
            failureMode: failClosed
```

The processor's `metadata` names the guardrail. Any other string metadata
becomes `labels`, and a single target's name becomes `labels["service"]`.
evalsid checks the following:

- **`tools/call` requests:** the string values of the `arguments`. The tool
  name is available as `tool` in `block_when`, and in the record's
  metadata as `mcp.tool`.
- **Results and other methods:** every string value in the params or the
  result, except structural ones (`type`, `mimeType`).

A mask comes back as `mutated` params or result, with the same JSON and
only the strings changed. A block comes back as `PERMISSION_DENIED`, which
the gateway turns into a JSON-RPC error carrying the guardrail's message.

ext_proc (Envoy external processing) is not supported. agentgateway runs it
on raw HTTP bytes, below the level of LLM messages and MCP calls, where
evaluators have nothing meaningful to score.

## Access

| Permission | Allows | Built-in roles |
|---|---|---|
| `guardrails.read` | read guardrails | viewer and up |
| `guardrails.check` | check content against a project's guardrails (the webhook, ExtMcp, `Check`) | runner and up, `guard` |
| `guardrails.write` | apply and delete guardrails (audited) | editor and up |

A `Check` with an inline guardrail is a dry run that scores content the way
`Evaluate` does, so it needs `evaluations.run`. Rules can match
`resource.guardrail.name` and `resource.labels`.
Checks are not written to the audit log, because a gateway makes one per
request. They are counted per guardrail, phase and verdict in `/metrics`:

- `evalsi_guardrail_checks_total`
- `evalsi_guardrail_blocked_total`
- `evalsi_guardrail_failed_total`
- `evalsi_guardrail_check_seconds_total`

## Notes

- **Several replicas.** A guardrail is stored once. Every replica
  recompiles it when its version changes, so `apply` takes effect on the
  next check everywhere, with no restart.
- **Volume and latency.** Every check is a database read plus the
  evaluators' time. Deterministic evaluators run in the Python worker over
  a local socket, a round trip of a few milliseconds. For very high
  volumes, scale evalsid replicas the way you would for ingest.
