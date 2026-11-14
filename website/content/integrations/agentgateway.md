---
title: agentgateway
description: Evaluate and guard the traffic that flows through agentgateway.
---

[agentgateway](https://agentgateway.dev) is a separate project that proxies your agents' LLM, MCP and A2A traffic. Evalsi works alongside it but does not require it.

```text
 agents ──► agentgateway ──► LLM providers, MCP servers, A2A agents
                 │  ▲
   OTLP traces   │  │  inline guardrail calls
   (ingest key)  ▼  │  (promptGuard webhook, ExtMcp)
              Evalsi
```

## Two integration points

**After the fact: online evaluation.** The gateway exports OpenTelemetry traces to Evalsi with an `ingest` key that can only write traces to one project. An `OnlineEvalPolicy` selects traffic with CEL (by route, backend, MCP tool or A2A agent), samples it, runs cheap checks first and a judge only after they pass, alerts on windowed metrics and promotes bad traces into a dataset.

**In flight: inline guardrails.** Evalsi speaks the gateway's two inline protocols directly:

| Traffic | agentgateway feature | Evalsi endpoint |
|---|---|---|
| LLM calls | `ai.promptGuard.request/response[].webhook` | `POST /guardrails/{project}/{guardrail}/request` and `/response` |
| MCP calls | `mcpGuardrails.processors[]` of kind `remote` | `agentgateway.dev.ext_mcp.ExtMcp` on the main port |

Each guardrail redacts, evaluates with any installed pack and then passes, masks or blocks. Audit mode reports what it would block before you enforce it.

## One identity, one query language

Both sides can trust the same OIDC issuers and tokens, and both select traffic with CEL. Evalsi access rules use agentgateway's `allow`, `deny` and `require` form.

## Go deeper

- [Inline guardrails guide](__BASE__/docs/guides/guardrails/)
- [agentgateway and Evalsi on Kubernetes](__BASE__/docs/guides/agentgateway-kubernetes/): ingest keys, CA trust, NetworkPolicy and policy examples.
- [Identity and access](__BASE__/docs/guides/identity/)

agentgateway's configuration keys change between versions. Treat the snippets in these guides as the shape of the setup and check them against the docs of the version you run.
