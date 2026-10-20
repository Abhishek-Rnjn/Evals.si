# Enterprise deployment: topology, dependencies, audit and data lifecycle

This guide is for the platform and security owners of a private Evals.si
installation. It collects in one place:

- the supported topologies;
- every connection the installation makes, and who operates each end;
- the audit settings an enterprise pilot should turn on;
- what happens to each kind of stored data, including how to erase it.

It covers the [product roadmap](../PRODUCT-ROADMAP.md)'s ENT-01 and ENT-06 items. What is built is described here. What has been verified, and on what, is recorded in [LEFTOVERS](../LEFTOVERS.md) and [verification](../verification/README.md); treat anything not listed there as unverified.

## Topologies

| Topology | Use | Storage | Replicas | Status |
|---|---|---|---|---|
| Single process (`evalsid serve`) | A developer or a CI job | SQLite in `data_dir` | One | Supported; SQLite cannot run more than one API replica |
| Kubernetes, one namespace (`--namespace-only`) | A team without cluster-admin | PostgreSQL, optional S3 and ClickHouse | Two or more API replicas; workers scale with KEDA | Installed and run on kind, AKS and an Istio ambient cluster ([verification](../verification/README.md)); not certified for production load |
| Kubernetes with the operator and sandbox pools | A platform team offering evaluation to several applications | PostgreSQL, NATS (three brokers), S3, optional ClickHouse | Two or more API replicas, workers and sandbox pools per release | Bounded EKS checks passed; M4 load, full upgrade and mesh enforcement are not certified ([LEFTOVERS](../LEFTOVERS.md#milestones)) |
| Air-gapped Kubernetes | No egress from the cluster | As above, from your registry | As above | The installer and bundle are checked in CI on kind; disconnected operation at a customer site is not yet exercised |

For every multi-replica install: use PostgreSQL, keep at least two API replicas, and follow [operations](operations.md) for upgrades, backups and restores. Each replica serves the API and OTLP ingest. Leases pick the one replica that runs the scheduler and the policy engine ([Kubernetes guide](kubernetes.md#high-availability)).

## Outbound and inbound connections

Everything below is off unless configured. In an air-gapped install, either point each one at a service inside your network or leave it unconfigured.

| Connection | Direction | Configured by | Who operates the other end | Required |
|---|---|---|---|---|
| PostgreSQL | evalsid → database | `storage.postgres` | You | For more than one replica |
| ClickHouse (traces and online scores) | evalsid → database | `storage.clickhouse` | You | No; traces go to the SQL store by default |
| S3-compatible object storage (datasets, promotions, artifacts) | evalsid → bucket | `storage.s3` | You | No; local files otherwise |
| NATS (work queues between API and workers) | evalsid and workers → brokers | `cluster` | You (the chart can run it) | For a distributed install |
| Model endpoints for targets and agents | workers → provider | run specs, `credentials.grants` | The model provider, or you for self-hosted models | Only for runs that call models |
| Judge models | workers → provider | `judges` | The model provider, or you | Only for judge-scored evaluators; there is no default judge |
| OIDC/JWKS issuers | evalsid → issuer | `auth.jwt.providers` | Your identity provider | When JWT login is used |
| External authorization | evalsid → your service | `authorization.ext_authz` | You | No |
| Trace sources (MLflow, Databricks, SageMaker, Langfuse, Phoenix, LangSmith, ClickHouse) | evalsid → store, and write-back of scores | `TraceSource` resources, `sources` | You, or the SaaS vendor | No |
| OTLP ingest | your agents and collectors → evalsid | `otlp` | You | No |
| Sinks (OTel logs, metrics and audit; MLflow) | evalsid → collector or store | `sinks` | You | No |
| Webhooks | evalsid → your URL | `webhooks`, applied webhooks | You | No |
| Sandbox images and agent installs | sandbox → registry, package indexes | task environments, `sandbox` | Your registry and mirrors | For code and agent runs |
| Agents' own egress | sandbox → per-host allowlist | `environment.sandbox` (`network: allowlist`, `allow_hosts`), `cli.allow_hosts` | Whoever runs those hosts | For agents that need it |

Credential grants (`credentials.grants`) decide which worker secrets a project may send, and to which hosts ([identity guide](identity.md#8-credentials-which-worker-secrets-a-project-may-use)). Trace sources refuse private, loopback and link-local hosts unless `sources.allow_hosts` lists them. Webhook URLs are not restricted by default ([LEFTOVERS](../LEFTOVERS.md#deferred-features)).

## Audit for an enterprise pilot

```yaml
audit:
  mode: strict              # the default when auth is on: a change that cannot be audited is refused
  reads: [traces, runs, datasets, failures]   # record who read what (never the data)
  retention: 8760h          # one year; set it from your regime
sinks:
  - otel: {endpoint: https://collector.internal:4318, audit: true}   # ship entries to your SIEM
```

After the first week, run `evalsi auth audit verify` as an owner, and store the printed last hash outside the installation (in a ticket, a WORM bucket or your SIEM). Run it again on a schedule. A break, or a last hash that no longer appears in the log, means entries were edited, removed or truncated. The [identity guide](identity.md#7-audit-metrics-external-authorization) describes each field and what is not built yet. `evalsi_audit_write_failures_total` on `/metrics` should stay at zero; alert when it rises.

## Data lifecycle and erasure

| Data | Where | Kept until | How to remove it | What remains after removal |
|---|---|---|---|---|
| Runs: specs, dataset snapshots, outputs, results | SQL store | `runs.retention` / `project_retention` (default: forever); `runs.archive_after` keeps only the summary | `evalsi delete-run` (authorized, audited, finished runs only), `evalsi archive-run`, or retention | Backups; exports through sinks, webhooks and MLflow write-back; downloaded files |
| Traces and online scores | SQL store or ClickHouse | `traces.retention` (default 168h) | Retention | Backups; the source system's own copy; scores written back to it |
| Failure cases and their history | SQL store | Until deleted | `evalsi inbox delete CASE_ID --revision N` ([failure inbox](failure-inbox.md)) | Source traces, dataset versions and runs made from the case |
| Dataset catalog versions | SQL store and object storage | Until retired; there is no erasure API | Retire the version; erasing content means a database and bucket operation by an operator | Runs that used the version, downloaded copies |
| Uploaded and promoted datasets | Object storage | Until deleted from the bucket | Operator deletion in the bucket | Bucket versions, if versioning is on |
| Annotations | SQL store | Until their queue is deleted | Delete the queue | Exports |
| Audit entries | SQL store | `audit.retention` (default 90 days) | Retention only; it records an `audit.retention` entry | Entries already exported to sinks |
| Backups | Your backup system | Your policy | Your backup system | Nothing Evals.si controls |

Deletes are immediate and hard: there is no grace period and no undo. Restore from a backup to recover something deleted by mistake. Trace redaction ([decision 0019](../decisions/0019-trace-redaction.md)) masks configured patterns at ingest, before storage.

To erase one person's data in response to a request:

1. Find their records. Search traces and failure cases by the identifying attributes your agents record. Search audit entries by principal.
2. Delete the failure cases and the runs that contain the data. Wait for traces to age out, or shorten `traces.retention` for the window.
3. Retire any dataset versions that contain it, then remove their content from the database and bucket as an operator task. Record that you did.
4. Remove the data from systems Evals.si exported to: the trace source, sink destinations, webhook receivers and MLflow.
5. Apply your backup policy. Evals.si cannot reach into backups or exported copies.

Not built yet: per-label retention rules, a delete grace period, an erasure API that covers every store, and legal holds. These are tracked in [REMAINING-WORK](../REMAINING-WORK.md) under ENT-06.
