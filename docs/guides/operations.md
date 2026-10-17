# Upgrades, migrations, backup and restore

## Upgrade policy (D7)

Use matching versions of the server, workers, operator and charts. Upgrade one
minor version at a time; N-1 means the immediately preceding minor release,
not any older binary. During a rolling upgrade N and N-1 must read the same
rows and messages. A release must test both versions against the migrated
database before claiming that compatibility. No published release pair has
yet passed that test here: zero-downtime upgrade compatibility is not certified.

Migrations run at server startup. PostgreSQL schema creation and additive
column changes run in one transaction under a database advisory lock; another
starting replica waits. There is no down-migration command. Release migrations
must expand before contracting: add nullable/defaulted columns or tables,
keep old names and representations readable for N-1, and postpone destructive
changes until a separately documented maintenance release. Do not rename/drop
columns or require a backfill during a rolling startup. Large indexes and
backfills need a separate resumable migration, not the startup transaction.

Before an upgrade, save the release values, back up all storage below, read the
release's migration notes, and verify N-1 compatibility evidence. Install the
CRD chart first, then the API/worker chart and sandboxd at the same target
version. Keep at least two API replicas on PostgreSQL; SQLite cannot support a
rolling multi-replica upgrade. Use `helm upgrade --wait --timeout 10m`; on Helm
4, see the Kubernetes guide's server-side-apply conflict guidance. Check health,
worker completion, source cursors, webhook retries and a known dataset run.

If readiness fails, stop the rollout and investigate. Helm rollback changes
workloads, not databases, CRDs, object storage or NATS. Roll back binaries only
when the previous version is proven compatible with the new schema. Otherwise
stop writers and restore the matched pre-upgrade backup into a fresh release.
Never run an older binary against a restored/newer schema by assumption.

## Consistent backup (D10)

This is a maintenance-window procedure. Backups contain private records and
authentication state: encrypt them, restrict access, and never commit them.
Choose and test an RPO/RTO for your installation. Managed PostgreSQL PITR,
versioned object storage and managed ClickHouse backups are preferred for
production; their restore procedures remain provider-specific.

1. Stop external API writes and OTLP producers; pause upstream collectors and
   training clients. Disable source polling and policy input. Wait for all runs
   to become terminal and work queues to drain. Do not acknowledge/drop pending
   messages to make a backup pass. Preserve pending webhook deliveries in SQL.
2. Save exact image/chart versions, Helm values, CRDs/custom resources and
   replica counts. Securely back up referenced Secrets, CA keys, bootstrap API
   keys, S3 credentials and authentication configuration with your secret-store
   procedure. They are needed to restore the same identities.
3. Stop the operator, all workers and every API replica. Do not stop PostgreSQL
   or object storage. For a disposable release named `evalsi`:

   ```sh
   export NS=evalsi RELEASE=evalsi BACKUP=/absolute/path/to/private-backup
   umask 077
   mkdir -p "$BACKUP/objects"
   helm get values evalsi -n "$NS" -a > "$BACKUP/values.yaml"
    kubectl get deployment -n "$NS" -o json | jq --arg release "$RELEASE" \
       '.items |= map(select(.metadata.annotations["meta.helm.sh/release-name"] == $release))' \
       > "$BACKUP/deployments.json"
    test "$(jq '.items | length' "$BACKUP/deployments.json")" -gt 0
    jq -r '.items[].metadata.name' "$BACKUP/deployments.json" | while read -r name; do
       kubectl scale deployment/"$name" -n "$NS" --replicas=0
       kubectl rollout status deployment/"$name" -n "$NS" --timeout=180s
    done
   ```

    Leave dependency StatefulSets running. Verify explicitly that no
    API/worker/operator process remains.
4. Dump PostgreSQL using a client of the same major version as the database.
   This example uses the chart's trial PostgreSQL; for a managed database use
   `pg_dump --format=custom --no-owner` with credentials supplied securely.

   ```sh
   kubectl exec -n "$NS" evalsi-dev-postgres-0 -- \
     pg_dump -U evalsi -d evalsi --format=custom --no-owner > "$BACKUP/postgres.dump"
   test -s "$BACKUP/postgres.dump"
   ```

5. Mirror the entire dataset/promotions bucket while writers are stopped.
   Configure an `mc` alias named `evalsi-backup` through your secret manager;
   do not put credentials in shell history. Do not use `--remove` when copying
   a backup. This copies current objects, not historical versions, delete
   markers, bucket policies, lifecycle rules or encryption keys; preserve those
   separately when restoring versioned/encrypted production storage.

   ```sh
   export BUCKET=evalsi
   mc mirror --overwrite "evalsi-backup/$BUCKET" "$BACKUP/objects/"
   (cd "$BACKUP" && find objects -type f -exec shasum -a 256 {} \; > objects.sha256)
   (cd "$BACKUP" && shasum -a 256 postgres.dump > postgres.sha256)
   ```

6. If ClickHouse holds traces, take a supported ClickHouse backup of its
   database with writers stopped. SQL plus S3 alone does not back up those
   traces. Snapshot NATS JetStream storage with its supported backup tooling
   when queues/streams must survive; a dataset dump is not a JetStream backup.
   Preserve the stream definitions and consumer state. Never mix SQL, S3,
   ClickHouse or JetStream snapshots from different write epochs.
7. Encrypt and move the backup off-cluster, record versions/time and checksums,
   and test a restore before declaring it usable. Resume the saved replicas
   and producers only after the backup is complete.

    ```sh
    jq -r '.items[] | [.metadata.name, (.spec.replicas // 1)] | @tsv' \
       "$BACKUP/deployments.json" | while read -r name replicas; do
          kubectl scale deployment/"$name" -n "$NS" --replicas="$replicas"
       done
    ```

## Restore into an empty installation

Do not apply this to a live database or bucket. Use an isolated namespace,
empty PostgreSQL database and empty object bucket, with no API, operator,
workers or producers running. Restore the same application version first;
an upgrade is a separate operation after restore verification.

```sh
export RESTORE_NS=evalsi-restore
(cd "$BACKUP" && shasum -a 256 -c postgres.sha256 && shasum -a 256 -c objects.sha256)
kubectl exec -i -n "$RESTORE_NS" evalsi-dev-postgres-0 -- \
  pg_restore -U evalsi -d evalsi --no-owner --exit-on-error < "$BACKUP/postgres.dump"
mc mb --ignore-existing "evalsi-restore/$BUCKET"
mc mirror --overwrite "$BACKUP/objects/" "evalsi-restore/$BUCKET"
mc mirror --overwrite "evalsi-restore/$BUCKET" "$BACKUP/restored-objects/"
diff -r "$BACKUP/objects" "$BACKUP/restored-objects"
```

Restore external traces, NATS snapshots, configuration, CA/identity Secrets and
bucket settings as appropriate. Clear only expired leases after they expire;
do not remove unresolved idempotency claims to force a retry. (An unresolved claim blocks its key until six days past its 24-hour expiry and is then pruned; an hourly background pass removes expired outcomes in every scope, and logs `idempotency pruning` when it does.) With the same
image versions, start API, workers and operator, then compare projects, run
counts, terminal results, source cursors, annotations and dataset checksums.
Read a known completed run through the authenticated API and run a restored
dataset. Check pending webhooks before enabling external delivery to avoid
surprises. Switch traffic only after these checks; resume producers last.

## WAL archiving and PITR for the bundled trial database

`devPostgres` is a single trial database. The chart does not configure a durable
WAL archive, continuous base backups, PITR retention or automatic recovery.
A `pg_dump` supports logical restore; it cannot recover to an arbitrary time.
For production, prefer a managed PostgreSQL service or a PostgreSQL operator
with tested base-backup/WAL management, and supply its DSN through
`storage.postgres.dsnSecret`.

A custom deployment needs `wal_level=replica`, `archive_mode=on`, and an
`archive_command` or supported archive library that durably copies every
completed WAL segment to independent storage. Do not treat a directory on the
same database PVC as an independent backup. Keep continuous WAL from each
retained physical base backup, monitor `pg_stat_archiver` failures and archive
age, and test restoring the selected base backup plus WAL before expiring it.
Preserve encryption keys and credentials outside the failed installation.

On an empty PostgreSQL data directory, restore a matching physical base backup,
configure `restore_command` to retrieve the archived segments, create
`recovery.signal`, select `recovery_target_time`, and choose whether recovery
pauses or promotes. Check both retained-before-target data and excluded
after-target commits before accepting traffic. PostgreSQL major versions,
timelines and backup consistency must match. Merely enabling `archive_mode`
does not establish a usable PITR chain.

A [disposable kind drill](../verification/LEFTOVERS-2026-10-17.md) now records
both a quiesced logical/database-and-bucket restore and real physical/WAL
recovery: rows 1 and 2 survived the selected target, committed row 3 did not,
and the recovery instance promoted. Its WAL archive was on the fixture PVC and
copied off-pod for recovery. This verifies mechanics, not independent storage
failure, managed-provider recovery, ClickHouse/JetStream backups or production
RPO/RTO. The script enables local archiving only on its synthetic restored
fixture; do not use that layout as the production archive destination.

## Reference cluster values (D11)

Clusters without a default StorageClass must select one explicitly for every
bundled persistent component, including `devPostgres.storageClassName`.
The trial PostgreSQL is still a single instance, not an HA/PITR deployment.

The chart includes `values-kind.yaml`, `values-eks.yaml`, `values-gke.yaml`,
`values-aks.yaml` and `values-openshift.yaml`. The cloud files are starting
points, not certified cluster installs: create the PostgreSQL DSN and object
credentials Secrets they name, select your storage classes, install CRDs and
review sandbox isolation on your actual CNI/runtime. They deliberately do not
claim NetworkPolicy enforcement, managed identity or a default judge model.
GKE and AKS examples require an S3-compatible gateway/store; native GCS and
Azure Blob storage are not configured by these files.
OpenShift requires a reviewed SCC admitting the chart's fixed non-root UIDs
and fsGroups; this reference does not grant privileged/anyuid permissions.

```sh
helm install evalsi-crds deploy/helm/evalsi-crds --set operator.namespace=evalsi
helm upgrade --install evalsi deploy/helm/evalsi -n evalsi --create-namespace \
  -f deploy/helm/evalsi/values-kind.yaml --wait --timeout 10m
```

Kind is a local trial with bundled PostgreSQL/MinIO and the pod sandbox rung;
it is not HA or a production backup service. Do not attempt Landlock on macOS.