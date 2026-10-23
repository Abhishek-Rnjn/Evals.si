# Webhooks

Evals.si calls your HTTP endpoint when a run finishes or fails a gate, when an online policy scores a trace, and when a policy's alert fires or resolves, so a pipeline or an application does not have to poll. Deliveries are signed with HMAC, retried with growing delays, and survive a restart.

## Events

| Event | Sent when |
|---|---|
| `run.finished` | A run reaches a final status: `SUCCEEDED`, `FAILED`, `ERROR` or `CANCELLED`. |
| `run.gate_failed` | A run finishes with at least one failed gate (status `FAILED`). It follows `run.finished`, so a pipeline can stop on this event alone. |
| `trace.scored` | An online policy stored results for a trace, whether the trace was pushed over OTLP or pulled by a [trace source](trace-sources.md): one event per trace and policy. **Sent only to webhooks that list it.** |
| `alert.fired` | A policy's [alert](end-to-end.md) started firing: its metric's window mean crossed the threshold. |
| `alert.resolved` | A firing alert stopped firing. |

`ping` is sent by `TestWebhook`. A webhook with no `events` receives every event except `trace.scored`.

`trace.scored` follows live traffic, one delivery per scored trace and policy, so it is opt-in: list `WEBHOOK_EVENT_TRACE_SCORED` in `events`. Its volume is the policy's sampled traffic; lower the policy's sampling rate, or narrow its selector, to send fewer. A webhook applied or deleted on another replica takes effect for `trace.scored` within 5 seconds. To read scores in bulk instead, or to catch up after downtime, use [`GET /v1alpha1/scores`](integrate-an-agent-harness.md#4-read-the-scores).

A policy's own `alerts[].webhook` URL still receives its unsigned alert body as before; `alert.fired` and `alert.resolved` carry the same body, signed and retried like every other event.

## Create one

```bash
curl -s localhost:8080/v1alpha1/webhooks -H 'content-type: application/json' -d '{
  "name": "ci", "url": "https://ci.example.com/hooks/evalsi",
  "events": ["WEBHOOK_EVENT_RUN_GATE_FAILED"]
}'
```

The response carries the generated `secret` **once**. Store it where the receiver can read it. List and get calls never return it (`secretSet` says whether one exists). To choose the secret yourself, send `secret` (at least 16 characters); sending none when you apply an existing webhook again keeps the stored one.

From Python:

```python
from evalsi.client import Client

with Client("http://localhost:8080") as client:
    hook = client.apply_webhook({"name": "ci", "url": "https://ci.example.com/hooks/evalsi"})
    secret = hook["secret"]
    print(client.test_webhook("ci"))                 # sends a ping now
    print(client.list_webhook_deliveries("ci"))      # state, attempts, last status
```

The other calls: `GET /v1alpha1/webhooks`, `GET /v1alpha1/webhooks/{name}`, `DELETE /v1alpha1/webhooks/{name}`, `POST /v1alpha1/webhooks/{name}:test` and `GET /v1alpha1/webhooks/{name}/deliveries`. `disabled: true` keeps a webhook but sends nothing.

## What a delivery looks like

A `POST` with `Content-Type: application/json` and these headers:

| Header | Value |
|---|---|
| `Evalsi-Event` | `run.finished`, `run.gate_failed`, `trace.scored`, `alert.fired`, `alert.resolved` or `ping` |
| `Evalsi-Delivery` | The delivery's ID. It is the same on every retry, so a receiver can drop duplicates. |
| `Evalsi-Signature` | `t=<unix seconds>,v1=<hex>`, where the hex is `HMAC-SHA256(secret, "<t>.<raw body>")` |

```json
{
  "id": "whd_5f0c…",
  "type": "run.gate_failed",
  "created_at": "2026-10-09T15:04:05Z",
  "project": "default",
  "run": { "id": "run-…", "name": "nightly", "status": "RUN_STATUS_FAILED",
           "summaries": [ … ], "gates": [ { "gate": {"metric": "exact-match", "min": 0.9}, "passed": false, "value": 0.5 } ],
           "createdBy": "key:ci", "finishedAt": "…" }
}
```

`run` is the run as `GetRun` returns it, **without its `spec`** (the target's and dataset's settings). Fetch the run by its ID for the rest.

A `trace.scored` delivery carries the trace's summary (as `ListTraces` returns it, labels included), the policy, and that policy's results for the trace. The trace's full record (input, output, trajectory) is not sent; fetch it with `GET /v1alpha1/traces/{trace_id}`:

```json
{
  "id": "whd_…", "type": "trace.scored", "created_at": "…", "project": "studio",
  "policy": "studio-agents",
  "trace": { "traceId": "0af7…", "service": "support-bot", "name": "invoke_agent", "startTime": "…", "duration": "4.2s",
             "steps": 7, "project": "studio", "labels": { "workflow": "support-bot", "version": "3" } },
  "results": [ { "evaluator": "loop-detection", "outcome": "OUTCOME_SCORED", "scores": [ … ] } ]
}
```

An alert delivery carries the policy and the alert as the policy's own alert webhook receives it:

```json
{
  "id": "whd_…", "type": "alert.fired", "created_at": "…", "project": "studio", "policy": "studio-agents",
  "alert": { "policy": "studio-agents", "metric": "task-success", "firing": true, "mean": 0.62, "n": 40, "below": 0.8, "at": "…" }
}
```

`ListWebhookDeliveries` shows each delivery's `runId`, or its `traceId` and `policy`.

## Verify the signature

Check the signature against the **raw** body, before parsing it, and refuse an old timestamp (the server signs each attempt with a fresh one):

```python
from evalsi.webhooks import verify_signature, SignatureError

verify_signature(secret, request.headers["Evalsi-Signature"], request.body)   # raises SignatureError
```

In Go, `webhooks.Verify(secret, header, body, tolerance, now)` in `internal/webhooks` does the same. Any language works: compute the HMAC over `"<t>.<body>"` and compare in constant time.

## Delivery and retries

Any `2xx` answer is a success. Anything else, or no answer within the timeout, is retried after 5 seconds, 30 seconds, 3 minutes, 15 minutes and an hour, up to `max_attempts` (default 6). After that the delivery is marked `FAILED` and stays listed for the retention period. Redirects are not followed.

An event writes its deliveries to the database first, and one replica at a time (holding a lease) sends them, so a restart or a replica crash loses nothing. Delivery is at least once: dedupe on `Evalsi-Delivery`.

```yaml
webhooks:
  max_attempts: 6      # attempts per delivery
  timeout: 10s         # how long to wait for an answer
  retention: 168h      # how long finished deliveries stay listed
```

`/metrics` counts attempts by outcome as `evalsi_webhook_attempts_total{outcome="delivered|retry|failed"}`.

## Access

| Permission | Allows | Built-in roles |
|---|---|---|
| `webhooks.read` | read a project's webhooks (not secrets) and their deliveries | admin and up |
| `webhooks.write` | apply, delete and test webhooks (audited) | admin and up |

A webhook makes the server send run results, and with `trace.scored` the summaries and scores of live traces, to a URL, and the server dials it from where it runs, so only admins get these permissions by default. On Kubernetes that means a webhook can reach in-cluster services by name: restrict who holds `webhooks.write`, and use egress NetworkPolicy if the server must not reach some of them. Rules can match `resource.webhook.name` and `resource.labels`.

## Destination guard

Since v0.4 the server refuses a webhook URL unless it is HTTPS to a public
address: private, loopback, link-local and metadata destinations are refused,
including names that resolve to those addresses. Each delivery is checked, so
the guard also covers webhooks saved before it was on. DNS answers are checked
again at connection time and the transport dials a checked address. Redirects
remain refused. When an operator configures an HTTP proxy, that proxy remains
part of the trusted network boundary.

For trusted internal receivers, list exact hosts or `*.domain` patterns under
`webhooks.allow_hosts` (`*.studio.svc.cluster.local` matches any name ending
in `.studio.svc.cluster.local`, not the bare domain). An exception permits HTTP
and internal addresses for that host. `webhooks.ssrf_guard: false` turns the
guard off. The option applies to WebhookService subscriptions, not policy
alert URLs or third-party connector callbacks.

```yaml
webhooks:
  allow_hosts: ["receiver.monitoring.svc.cluster.local", "*.studio.svc.cluster.local"]
  # ssrf_guard: false   # the behaviour before v0.4
```

With the Helm chart, set `webhooks.allowHosts` (and `webhooks.ssrfGuard:
false` to turn the guard off); the hosts of `bootstrap.webhooks` are allowed
for you, since the chart's values are the operator's own.

### Upgrading from v0.3

The guard was opt-in before v0.4. After upgrading, a saved webhook whose URL is
plain HTTP or an internal address (an in-cluster service name, a private IP)
is refused: the server logs `webhook destination refused` for each at start,
and each of its deliveries fails at once (no retries) with an error naming
`webhooks.allow_hosts` and `webhooks.ssrf_guard: false`. Testing the webhook (`POST /v1alpha1/webhooks/{name}:test`)
shows the same error. To keep such a webhook, list its host under
`webhooks.allow_hosts` (or `webhooks.allowHosts` in the chart) and restart;
deliveries that failed meanwhile are not resent.
