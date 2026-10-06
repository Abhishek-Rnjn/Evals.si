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

## Checklist

- The ingest key has the `ingest` role and one project, and lives in a Secret.
- The gateway trusts evalsi's CA (or the mesh terminates TLS).
- NetworkPolicy lets the gateway reach evalsi's OTLP port.
- Policies sample: the judge stage costs tokens on every sampled trace (`sampling.rate`, `when`).
- Alerts have `min_samples`, so a quiet hour does not page anyone.
