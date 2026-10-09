---
title: Integrations
description: How Evals.si connects to gateways, Kubernetes and agent platforms.
---

Evals.si does not replace your agent stack. It evaluates it. Every integration uses one of four doors, so a platform that supports any of them works without custom code.

| Door | Direction | Use it to |
|---|---|---|
| **OTLP traces** | your platform → Evals.si | Score live traffic with an [online policy](__BASE__/docs/get-started/) and promote failures into regression datasets |
| **Agent targets** | Evals.si → your agent | Run your agent on sandboxed tasks and benchmarks over A2A, MCP, an OpenAI Responses API, plain HTTP or a CLI |
| **Inline guardrails** | your gateway → Evals.si | Redact, evaluate and block content while a request is in flight |
| **MCP and the API** | your tools → Evals.si | Let coding agents, CI and dashboards start runs and read results over gRPC, HTTP or MCP |

## Guides

- [agentgateway](__BASE__/integrations/agentgateway/): trace export and inline guardrails for LLM, MCP and A2A traffic.
- [Kubernetes](__BASE__/integrations/kubernetes/): install with Helm, drive runs and policies with CRDs, and wire in-cluster agents.
- [Agent platforms and studios](__BASE__/integrations/agent-platforms/): connect a visual or low-code agent builder, or any agent framework.

For the plugin side, see [Plugins and Wasm evaluators](__BASE__/docs/guides/plugins/).
