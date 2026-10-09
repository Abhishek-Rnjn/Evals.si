# Webhooks

Evals.si calls your HTTP endpoint when a run finishes or fails a gate, so a pipeline or an application does not have to poll. Deliveries are signed with HMAC, retried with growing delays, and survive a restart.

## Events

| Event | Sent when |
|---|---|
| `run.finished` | A run reaches a final status: `SUCCEEDED`, `FAILED`, `ERROR` or `CANCELLED`. |
| `run.gate_failed` | A run finishes with at least one failed gate (status `FAILED`). It follows `run.finished`, so a pipeline can stop on this event alone. |

`ping` is sent by `TestWebhook`. A webhook with no `events` receives every event.

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
| `Evalsi-Event` | `run.finished`, `run.gate_failed` or `ping` |
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

## Verify the signature

Check the signature against the **raw** body, before parsing it, and refuse an old timestamp (the server signs each attempt with a fresh one):

```python
from evalsi.webhooks import verify_signature, SignatureError

verify_signature(secret, request.headers["Evalsi-Signature"], request.body)   # raises SignatureError
```

In Go, `webhooks.Verify(secret, header, body, tolerance, now)` in `internal/webhooks` does the same. Any language works: compute the HMAC over `"<t>.<body>"` and compare in constant time.

## Delivery and retries

Any `2xx` answer is a success. Anything else, or no answer within the timeout, is retried after 5 seconds, 30 seconds, 3 minutes, 15 minutes and an hour, up to `max_attempts` (default 6). After that the delivery is marked `FAILED` and stays listed for the retention period. Redirects are not followed.

A finished run writes its deliveries to the database first, and one replica at a time (holding a lease) sends them, so a restart or a replica crash loses nothing. Delivery is at least once: dedupe on `Evalsi-Delivery`.

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

A webhook makes the server send run results to a URL, and the server dials it from where it runs, so only admins get these permissions by default. On Kubernetes that means a webhook can reach in-cluster services by name: restrict who holds `webhooks.write`, and use egress NetworkPolicy if the server must not reach some of them. Rules can match `resource.webhook.name` and `resource.labels`.
