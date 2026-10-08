---
title: Agent platforms and studios
description: Connect visual agent builders, low-code studios and agent frameworks to Evals.si.
---

Agent studios and builders (visual workflow tools, low-code platforms, hosted agent services and frameworks) differ in how you build agents. For evaluation they need only one of a few capabilities, and Evals.si integrates through those rather than through a per-product plugin.

:::note
This page describes what Evals.si supports. It does not claim tested compatibility with any named product. Check each platform's own docs for how to enable its tracing export or expose its agent endpoint, then use the recipe below that matches.
:::

## Pick the recipe that matches your platform

| If the platform can... | Use | Gets you |
|---|---|---|
| Export OpenTelemetry traces (OTel GenAI, OpenInference, OpenLLMetry, MLflow) | **Recipe A** | Live scoring of production traffic |
| Expose a deployed agent as an A2A, MCP, OpenAI Responses or HTTP endpoint | **Recipe B** | Offline runs, benchmarks and CI gates against the real agent |
| Run behind a gateway that supports webhook or MCP guardrails | **Recipe C** | Inline redaction and blocking |
| Run as a command-line program | **Recipe B (CLI)** | Sandboxed runs on benchmarks like SWE-bench |

## Recipe A: score live traffic from traces

1. Create an ingest key that can only write traces to one project:
   ```bash
   evalsi auth keys create studio-ingest --role support=ingest --server https://evals.example.com
   ```
2. In the platform, point its OpenTelemetry exporter at the server's OTLP endpoint (gRPC 4317 or HTTP 4318) and send the key in the `authorization` header.
3. Look at a few stored traces first. Evals.si keeps every raw attribute, and attribute names differ between platforms and conventions.
4. Apply a policy with a selector written against what you saw:
   ```bash
   evalsi policy apply -f support-policy.yaml --server https://evals.example.com
   evalsi policy stats support-agent --server https://evals.example.com
   ```

A policy samples deterministically, runs cheap evaluators before a judge, alerts on windowed metrics and promotes bad traces into a dataset. See the [watch example](__BASE__/examples/watch/).

## Recipe B: evaluate the agent itself

Put the agent in a run spec's `target.agent`:

| Kind | Spec | How it is driven |
|---|---|---|
| A2A | `a2a: {url, headers_env}` | JSON-RPC `message/send`, following the task until it settles |
| MCP | `mcp: {url, tool, argument}` | The agent is a tool on an MCP server |
| OpenAI Responses | `responses: {base_url, model, api_key_env}` | `POST <base_url>/responses` with `previous_response_id` for later turns |
| HTTP | `http: {url, method, body_template, output_path, messages_path}` | A JSON body from a template; the answer and its steps are read by path |
| CLI | `cli: {command, install, env_from, allow_hosts, timeout}` | Runs inside the task's sandbox |

Credentials go in environment variables named in the spec, not in the spec itself. Add `trials` to get pass@k and pass^k, and gates so a run fails when quality drops. Turn production failures into the next test set with the promote step from Recipe A.

See [Agent runs](__BASE__/docs/guides/agent-runs/) and the [agent examples](__BASE__/examples/agents/).

## Recipe C: guard traffic inline

If the platform's calls go through a gateway such as [agentgateway](__BASE__/integrations/agentgateway/), configure the gateway's prompt-guard webhook or MCP guardrail processor to call Evals.si. The platform needs no changes. Any other caller can use `GuardrailService.Check` over gRPC, Connect or REST. Start in audit mode to see what would be blocked.

## From inside a studio

Studios that let you add a custom node, tool or evaluator step can call the HTTP API from it to run an evaluation or read a result. Coding agents can use Evals.si over [MCP](__BASE__/docs/guides/mcp/), locally with `evalsi mcp` or on the server at `/mcp`. Platforms with an extension model for graders can load your evaluators as [Wasm plugins](__BASE__/docs/guides/plugins/).

## Kubernetes-hosted platforms

If the studio runs in a cluster, combine this page with the [Kubernetes integration](__BASE__/integrations/kubernetes/): export traces to the in-cluster `evalsi` Service, use service-account identity, and gate rollouts on an `EvalRun`.
