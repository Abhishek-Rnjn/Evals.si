package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A migration is one ordered schema step. Steps are applied once, in
// version order, and recorded in schema_migrations, so a database knows which
// release last changed it.
//
// The rule for writing one (CONTRIBUTING, "Schema changes") is expand, then
// contract: a step that only adds tables, columns or indexes, or backfills
// them, leaves the database readable by the previous release, so N and N-1
// can run side by side during a rolling upgrade and N-1 can be rolled back
// to. A step that drops, renames or rewrites what N-1 reads is a contract
// step; it ships at least one release after the code stopped needing the old
// shape, and it raises the minimum reader to itself, so an older evalsid
// refuses the database instead of misreading it.
type migration struct {
	version int
	name    string
	// contract: the previous release can no longer read the database.
	contract bool
	// always: the step is idempotent DDL (CREATE ... IF NOT EXISTS, missing
	// columns) and runs on every open, so a table or column added to the
	// schema constants reaches existing databases without a new step.
	always bool
	apply  func(ctx context.Context, q querier, d dialect) error
}

// allSchemas is every table the store creates, for step 1.
const allSchemas = schema + authSchema + leaseSchema + quotaSchema + sandboxSchema + rewardCacheSchema +
	annotationSchema + failureSchema + datasetCatalogSchema + guardrailSchema + webhookSchema + sourceSchema +
	idempotencySchema + watchPendingSchema

// migrations are the store's schema steps, oldest first. Append; never
// reorder, renumber or edit a released step.
var migrations = []migration{
	{version: 1, name: "tables", always: true, apply: func(ctx context.Context, q querier, d dialect) error {
		_, err := q.ExecContext(ctx, d.ddl(allSchemas))
		return err
	}},
	{version: 2, name: "tenant and added columns", always: true, apply: addTenantColumns},
	{version: 3, name: "archived runs column", apply: migrateArchivedRuns},
	{version: 4, name: "watch pending streams", apply: migrateWatchPending},
	{version: 5, name: "audit hash chain", apply: addAuditChain},
}

// addAuditChain adds the audit log's sequence and hash columns (decision
// 0026). Existing entries keep sequence 0: the chain starts after them.
func addAuditChain(ctx context.Context, q querier, d dialect) error {
	for _, col := range []struct{ name, def string }{
		{"seq", "INTEGER NOT NULL DEFAULT 0"},
		{"hash", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addTypedColumn(ctx, q, d, "audit", col.name, col.def); err != nil {
			return err
		}
	}
	_, err := q.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS audit_by_seq ON audit (seq)`)
	return err
}

const migrationsSchema = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  applied_ns INTEGER NOT NULL,
  min_reader INTEGER NOT NULL
);`

// ErrSchemaTooNew is returned when the database was migrated by a release
// whose changes this evalsid cannot read.
var ErrSchemaTooNew = errors.New("store: the database schema is newer than this evalsid can read")

// SchemaVersion is the latest schema version this build knows.
func SchemaVersion() int { return migrations[len(migrations)-1].version }

// applyMigrations brings the database up to the latest step, inside q (a
// transaction the caller commits). It refuses a database whose minimum reader
// is newer than this build.
func applyMigrations(ctx context.Context, q querier, d dialect, steps []migration) error {
	if _, err := q.ExecContext(ctx, d.ddl(migrationsSchema)); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	minReader := 0
	rows, err := q.QueryContext(ctx, `SELECT version, min_reader FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v, r int
		if err := rows.Scan(&v, &r); err != nil {
			_ = rows.Close()
			return err
		}
		applied[v] = true
		minReader = max(minReader, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	latest := steps[len(steps)-1].version
	if minReader > latest {
		return fmt.Errorf("%w: it needs schema version %d or later and this build knows up to %d; run a newer evalsid, or restore a backup taken before the upgrade", ErrSchemaTooNew, minReader, latest)
	}
	for _, step := range steps {
		if applied[step.version] && !step.always {
			continue
		}
		if err := step.apply(ctx, q, d); err != nil {
			return fmt.Errorf("schema step %d (%s): %w", step.version, step.name, err)
		}
		if applied[step.version] {
			continue
		}
		if step.contract {
			minReader = step.version
		}
		if _, err := q.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_ns, min_reader) VALUES (?, ?, ?, ?)`,
			step.version, step.name, time.Now().UnixNano(), minReader); err != nil {
			return err
		}
	}
	return nil
}

// AppliedMigration is one recorded schema step.
type AppliedMigration struct {
	Version   int
	Name      string
	AppliedAt time.Time
	MinReader int
}

// Migrations lists the schema steps applied to this database, oldest first.
func (s *Store) Migrations(ctx context.Context) ([]AppliedMigration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version, name, applied_ns, min_reader FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppliedMigration
	for rows.Next() {
		var m AppliedMigration
		var ns int64
		if err := rows.Scan(&m.Version, &m.Name, &ns, &m.MinReader); err != nil {
			return nil, err
		}
		m.AppliedAt = time.Unix(0, ns).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}
