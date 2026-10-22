# agentgateway and Evals.si on Kubernetes

[agentgateway](https://agentgateway.dev) sits in front of your agents' LLM, MCP and A2A traffic; Evals.si evaluates that traffic. Together, every call through the gateway can be scored online with no change to the agents, and the failures it finds become regression datasets. Both run the same way on a laptop and on Kubernetes, use the same OIDC providers and tokens, and select traffic with CEL.

```text
 agents ──► agentgateway ──► LLM providers, MCP servers, A2A agents
                 │  OTLP traces (an ingest key)
                 ▼
           evalsi (namespace evalsi) ──► OnlineEvalPolicy: sample, score, alert, promote
```

This guide assumes Evals.si is installed with the charts ([Kubernetes](kubernetes.md)) and agentgateway with its own Helm chart or the Gateway API. agentgateway's configuration keys change between versions; the snippets below show the shape, so check them against the docs of the version you run.

## 1. An ingest key for the gateway

The gateway authenticates its trace export with a key that may only write traces, to one project:

```bash
evalsi auth keys create gateway-ingest --role support=ingest \
  --server https://evals.example.com            # prints the key once
kubectl create secret generic evalsi-ingest -n agentgateway-system \
  --from-literal=authorization="Bearer <the key>"
```

Traces sent with this key land in project `support`. A resource attribute `evalsi.project` is honored only when the key may write there.

## 2. Export the gateway's traces to evalsi

Point agentgateway's OpenTelemetry tracing at the `evalsi` Service's OTLP port (gRPC 4317, or HTTP 4318), sending the key as the `authorization` header:

```yaml
# agentgateway configuration (shape; see your version's tracing docs)
config:
  tracing:
    otlpEndpoint: https://evalsi.evalsi.svc:4317
    headers:
      authorization: {secretKeyRef: {name: evalsi-ingest, key: authorization}}
    randomSampling: true          # sample here, or let the policy sample
```

The chart's API certificate comes from the internal CA in the Secret `evalsi-server-tls` (`ca.crt`); give the gateway that CA, or terminate TLS for evalsi in your mesh. Already running an OpenTelemetry Collector? Fan out from it instead: add an `otlp` exporter for evalsi with the same header.

If the namespaces enforce NetworkPolicy, allow the gateway's pods to reach `evalsi` on 4317 or 4318.

## 3. Evaluate the traffic

An `OnlineEvalPolicy` selects traces with CEL over the trace's service, root span name, attributes and resource, so it can pick traffic by gateway route, backend, MCP tool or A2A agent:

```yaml
apiVersion: evals.si/v1alpha1
kind: OnlineEvalPolicy
metadata:
  name: support-gateway
  namespace: evalsi
  labels: {evals.si/project: support}
spec:
  selector: 'service == "agentgateway" && attributes["gen_ai.request.model"] != ""'
  sampling:
    rate: 0.05
    always: ["error", "duration_ms > 20000"]
  stages:
    - evaluators: [{ref: builtin/latency}, {ref: builtin/json-valid}]
    - when: 'scores["json-valid"] == 1.0'
      evaluators: [{ref: builtin/llm-judge, params: {rubric: helpfulness}}]
  window: 10m
  alerts:
    - {metric: llm-judge, below: 0.7, min_samples: 50, webhook: https://hooks.example.com/evalsi}
  promote:
    when: 'scores["llm-judge"] < 0.5'
    dataset: support-gateway-regressions
```

```bash
kubectl apply -f support-gateway.yaml
kubectl get onlineevalpolicies -n evalsi      # SYNCED, SEEN, EVALUATED
```

Which attributes the gateway puts on its spans depends on its version and on the route type (LLM, MCP, A2A). Look at a few stored traces first (`evalsi` keeps every raw attribute) and write the selector against what is there.

Promoted traces become dataset records; an `EvalRun` over that dataset is the regression suite for the next change:

```yaml
spec:
  dataset: {path: promoted/support-gateway-regressions.jsonl}
```

## 4. One identity for both

Configure the same OIDC issuers on both: agentgateway validates the user's JWT and forwards it (`preserveToken`), and `evalsid` validates the same token again against the same issuer, so access rules see the same user on both sides. In the cluster, the gateway and your agents can use their service accounts instead (see [identity](identity.md#on-kubernetes-service-account-tokens)). Trusting identity headers from the gateway (`auth.trusted_proxy`) works only from configured source CIDRs, and is discouraged.

## 5. Quality per route, for routing decisions

The online scores can come back out as figures per route (or backend, or service), in two forms. What Evals.si builds is the figures; what acts on them is yours ([decision 0038](../decisions/0038-quality-scores-for-routing.md)).

**Get the route onto the trace.** Figures are grouped by `service`, the root span's `name`, or a trace label (`label:<key>`). Labels come from `evalsi.label.<key>` resource attributes and from the ingest key's labels, so put the route there. With an OpenTelemetry Collector between the gateway and evalsi, copy it from the span. Which span attribute names the route or backend depends on the gateway version: read a stored trace first, then write the transform against what is there (the attribute below is a placeholder):

```yaml
processors:
  transform/route:
    trace_statements:
      - context: span
        statements:
          - set(resource.attributes["evalsi.label.route"], attributes["<the gateway's route attribute>"]) where attributes["<the gateway's route attribute>"] != nil
```

A resource attribute belongs to the whole export request, so this works only when each export carries one route's spans. The simpler way is one ingest key per route (or per backend), each with its own label, in the key's `labels`.

**JSON, on demand.** `GET /v1alpha1/quality` aggregates the stored results of one policy over a window. It needs `traces.read` and counts only the traces the caller may read:

```bash
curl -s -H "Authorization: Bearer $EVALSI_KEY" \
  "https://evalsi.evalsi.svc:8080/v1alpha1/quality?policy=support-gateway&metric=llm-judge&group_by=label:route&window=30m&min_traces=30&threshold=0.7"
```

```json
{"policy":"support-gateway","metric":"llm-judge","group_by":"label:route","window_seconds":1800,"min_traces":30,"threshold":0.7,
 "generated_at":"2026-10-22T12:00:00Z",
 "groups":[
  {"group":"billing","traces":412,"error_rate":0.004,"metrics":{"llm-judge":{"traces":410,"mean":0.86}},"state":"ok"},
  {"group":"refunds","traces":96,"error_rate":0.02,"metrics":{"llm-judge":{"traces":95,"mean":0.61}},"state":"degraded"},
  {"group":"search","traces":8,"error_rate":0,"metrics":{"llm-judge":{"traces":8,"mean":0.4}},"state":"insufficient_data"}]}
```

`state` is `insufficient_data` below `min_traces` (default 20), `degraded` when the mean is under `threshold`, otherwise `ok`. Groups are the busiest first, at most `max_groups` (default 50, the rest counted in `omitted_groups`). The report reads at most 20000 traces, newest first (`truncated`). Without `metric`, every metric is reported and there is no state.

**Prometheus gauges.** For tooling that reads metrics (Grafana, Alertmanager, KEDA, Flagger), have the server publish them on `/metrics`:

```yaml
quality_exports:
  - name: gateway-routes
    project: support
    policy: support-gateway
    metric: llm-judge          # default: every metric
    group_by: label:route
    window: 30m                # defaults: 1h window, 20 traces, 50 groups, 1m refresh
    min_traces: 30
```

This gives `evalsi_quality_score{export,project,policy,metric,group}` (the mean; omitted for a group under `min_traces`), `evalsi_quality_traces`, `evalsi_quality_error_rate`, `evalsi_quality_report_age_seconds` and `evalsi_quality_export_failures_total`. A failed refresh keeps the last report, so alert on the age.

**One way to act on it: shift weights.** agentgateway implements the Kubernetes Gateway API, whose `HTTPRoute` splits traffic by `backendRef` weight. A small controller (a CronJob is enough) can read the JSON and move weight away from a degraded backend. This sketch is the shape, not a tested controller; it assumes the group is the backend's name and that the route has two backends:

```bash
#!/usr/bin/env bash
# Every 5 minutes: if the primary backend is degraded, send most traffic to the fallback.
set -euo pipefail
state=$(curl -sf -H "Authorization: Bearer $EVALSI_KEY" \
  "$EVALSI/v1alpha1/quality?policy=support-gateway&metric=llm-judge&group_by=label:backend&window=30m&min_traces=30&threshold=0.7" |
  jq -r '.groups[] | select(.group=="primary") | .state')
if [ "$state" = "degraded" ]; then primary=10 fallback=90; else primary=90 fallback=10; fi
kubectl patch httproute support -n evalsi --type=json -p "[
  {\"op\":\"replace\",\"path\":\"/spec/rules/0/backendRefs/0/weight\",\"value\":$primary},
  {\"op\":\"replace\",\"path\":\"/spec/rules/0/backendRefs/1/weight\",\"value\":$fallback}]"
```

Build in hysteresis (two bad reports in a row, a minimum time between changes), keep the fallback's weight above zero so its own score stays measurable, and never route on `insufficient_data`.

**What depends on the gateway.** Evals.si produces the figures and nothing in the gateway consumes them natively: a quality-aware load-balancing policy, a score-fed circuit breaker or auto-disabling an MCP tool needs an extension point in agentgateway that this guide does not assume exists. The weight-shifting loop above uses only the standard Gateway API and was not run against a real gateway here. The span attributes that carry the route, backend and tool depend on the gateway's version and route type.

## Checklist

- The ingest key has the `ingest` role and one project, and lives in a Secret.
- The gateway trusts evalsi's CA (or the mesh terminates TLS).
- NetworkPolicy lets the gateway reach evalsi's OTLP port.
- Policies sample: the judge stage costs tokens on every sampled trace (`sampling.rate`, `when`).
- Alerts have `min_samples`, so a quiet hour does not page anyone.
