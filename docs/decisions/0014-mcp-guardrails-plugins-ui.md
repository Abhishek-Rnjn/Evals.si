# 0014. MCP on both sides, guardrails on agentgateway's protocols, Wasm plugins on wazero, and a read-only UI

- **Status:** Accepted, 2026-10-07 (Phase 6)

## Context

Phase 6 opens Evals.si to tools around it:

- coding agents that should check their own work;
- gateways that want a verdict before traffic goes through;
- third parties who write evaluators;
- people who want to look at results without a terminal.

Each of these brings code or traffic we do not control into the server.

## Decision

1. **MCP is served twice, with the same tools.**
   - **Locally:** `evalsi mcp` over stdio needs no server. A coding agent runs a spec, and its answer includes the comparison with the previous run of the same name, so a regression is part of the reply rather than a second question.
   - **On evalsid:** `/mcp` serves the same tools over streamable HTTP, behind the MCP authorization specification. The tools call the API in process with the caller's own credential, so the gate, the audit log and quotas apply unchanged.
   - **Per-tool rules:** `mcp.tool.name` in CEL lets rules restrict tools.
2. **Guardrails speak agentgateway's protocols.**
   - **No plugin of our own:** evalsid implements the gateway's prompt-guard webhook (LLM traffic) and its ExtMcp processor service (MCP traffic). ext_proc is not used: it works on raw HTTP bytes, below the level evaluators understand.
   - **Fail closed by default:** a guardrail that cannot decide (an evaluator error, a timeout, a `block_when` that cannot be evaluated) blocks unless it is set to fail open.
   - **Errors are plain text:** the gateway reads any JSON object it does not recognise as a pass, so an error must never be JSON.
   - **Redact first:** redaction runs before evaluators, so a judge never sees what was redacted.
3. **Wasm plugins run on wazero, with our own JSON ABI.**
   - **Why wazero:** it is pure Go, so `evalsid` stays one static binary.
   - **The ABI:** JSON over stdio of a WASI command module. Any language with a WASI target can implement it in an afternoon; we preferred that to a framework-specific plugin kit.
   - **Isolation:** a module gets no filesystem, network or environment, a fixed clock and seeded randomness. That makes it deterministic and safe without a sandbox rung.
   - **Pinning:** the manifest pins the module by sha256, and the index pins the manifest.
   - **One implementation:** Python runs Wasm evaluators through `evalsid wasm serve`, so scores match everywhere.
4. **The plugin index is a reviewed YAML file in the repository.** Publishing is a pull request. Installation verifies every download against its pin before writing anything.
5. **Annotation leases are per annotator.** An item needing two answers can be held by two people at once. A claim reserves a slot until it is answered, skipped or expires.
6. **A minimal, read-only web UI** (revisiting [0003](0003-no-web-ui-yet.md)).
   - **Delivery:** static files embedded in evalsid. There is no build step and no framework.
   - **Data:** read from the public REST API with the viewer's own credential, so the UI can show nothing the API would not.
   - **Security:** values are rendered as text only, and a strict CSP forbids inline script and framing.
   - **Out of scope:** writing (creating runs, applying policies, annotating) stays in the CLI and the API.

## Consequences

- **What to keep compatible:** the MCP tools and the guardrail protocols are now surfaces to keep compatible. A change in agentgateway's webhook or ExtMcp protocol needs a matching change here, and the vendored `ext_mcp.proto` is kept byte for byte.
- **What Wasm evaluators cannot do:** use a judge or the sandbox. Those stay Python plugins, or images.
- **Signing in to the UI:** the UI has no browser OIDC login of its own. A viewer pastes an API key or the token from `evalsi auth token`. A login flow is follow-up work.
- **Annotation:** a browser front end for annotators is follow-up work.
