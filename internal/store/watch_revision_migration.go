package store

import "context"

func migrateWatchState(ctx context.Context, q querier, d dialect) error {
	dirty, err := hasWatchColumn(ctx, q, d, "watch_score_references", "dirty")
	if err != nil {
		return err
	}
	if !dirty {
		if _, err := q.ExecContext(ctx, d.ddl(`ALTER TABLE watch_score_references ADD COLUMN dirty INTEGER NOT NULL DEFAULT 1`)); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS watch_score_dirty ON watch_score_references(dirty,project,trace_id,record_hash)`,
		`CREATE INDEX IF NOT EXISTS watch_head_expiry ON watch_trace_heads(deleted,start_ns)`,
		`CREATE INDEX IF NOT EXISTS watch_publication_expiry ON watch_publications(start_ns)`,
		`CREATE INDEX IF NOT EXISTS watch_score_expiry ON watch_score_references(start_ns)`,
	} {
		if _, err := q.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, col := range []struct{ table, column string }{{"leases", "epoch"}, {"watch_pending", "revision"}, {"watch_pending", "score_revision"}, {"watch_trace_heads", "score_revision"}} {
		exists, err := hasWatchColumn(ctx, q, d, col.table, col.column)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := q.ExecContext(ctx, d.ddl(`ALTER TABLE `+col.table+` ADD COLUMN `+col.column+` INTEGER NOT NULL DEFAULT 0`)); err != nil {
				return err
			}
		}
	}
	for _, table := range []string{"watch_pending", "traces"} {
		if err := addColumn(ctx, q, d, table, "record_hash"); err != nil {
			return err
		}
	}
	for _, table := range []string{"watch_pending", "watch_trace_heads"} {
		if err := addColumn(ctx, q, d, table, "snapshot_hash"); err != nil {
			return err
		}
	}
	if err := addColumn(ctx, q, d, "watch_trace_heads", "projection_hash"); err != nil {
		return err
	}
	exists, err := hasWatchColumn(ctx, q, d, "trace_results", "record_hash")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	for _, statement := range []string{
		`ALTER TABLE trace_results RENAME TO trace_results_legacy`,
		`CREATE TABLE trace_results (project TEXT NOT NULL, trace_id TEXT NOT NULL, policy TEXT NOT NULL, evaluator TEXT NOT NULL, record_hash TEXT NOT NULL DEFAULT '', result BLOB NOT NULL, tenant_id TEXT NOT NULL DEFAULT '', PRIMARY KEY(project,trace_id,policy,evaluator,record_hash))`,
		`INSERT INTO trace_results(project,trace_id,policy,evaluator,result,tenant_id) SELECT project,trace_id,policy,evaluator,result,tenant_id FROM trace_results_legacy`,
		`DROP TABLE trace_results_legacy`,
	} {
		if _, err := q.ExecContext(ctx, d.ddl(statement)); err != nil {
			return err
		}
	}
	return nil
}

func hasWatchColumn(ctx context.Context, q querier, d dialect, table, column string) (bool, error) {
	query := `SELECT COUNT(*) FROM pragma_table_info('` + table + `') WHERE name = '` + column + `'`
	if d == postgresDialect {
		query = `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='` + table + `' AND column_name='` + column + `'`
	}
	var n int
	err := q.QueryRowContext(ctx, query).Scan(&n)
	return n > 0, err
}
