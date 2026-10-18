package store

import "context"

// The policy queue lives in the metadata database even when traces live in
// ClickHouse. Keeping the redacted record here also recovers a failed trace write.
const watchPendingSchema = `
CREATE TABLE IF NOT EXISTS watch_pending (
  project TEXT NOT NULL, trace_id TEXT NOT NULL, stream TEXT NOT NULL DEFAULT '', payload BLOB NOT NULL, start_ns INTEGER NOT NULL,
  PRIMARY KEY (project, trace_id, stream)
);
CREATE TABLE IF NOT EXISTS watch_write_lock (
  id INTEGER PRIMARY KEY, version INTEGER NOT NULL
);
INSERT INTO watch_write_lock (id, version) VALUES (1, 0) ON CONFLICT (id) DO NOTHING;
`

type WatchPending struct {
	Project, TraceID, Stream string
	Payload                  []byte
	StartNS                  int64
}

func (s *Store) PutWatchPending(ctx context.Context, items []WatchPending) error {
	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO watch_pending (project, trace_id, stream, payload, start_ns) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (project, trace_id, stream) DO UPDATE SET payload = excluded.payload, start_ns = excluded.start_ns`, item.Project, item.TraceID, item.Stream, item.Payload, item.StartNS); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListWatchPending(ctx context.Context, limit int, after ...WatchPending) ([]WatchPending, error) {
	var cursor WatchPending
	if len(after) > 0 {
		cursor = after[0]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT project, trace_id, stream, payload, start_ns FROM watch_pending
		WHERE project > ? OR (project = ? AND (trace_id > ? OR (trace_id = ? AND stream > ?))) ORDER BY project, trace_id, stream LIMIT ?`, cursor.Project, cursor.Project, cursor.TraceID, cursor.TraceID, cursor.Stream, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []WatchPending
	for rows.Next() {
		var item WatchPending
		if err := rows.Scan(&item.Project, &item.TraceID, &item.Stream, &item.Payload, &item.StartNS); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) WatchPendingMatches(ctx context.Context, item WatchPending) (bool, error) {
	return watchPendingMatches(ctx, s.db, item)
}

func watchPendingMatches(ctx context.Context, q querier, item WatchPending) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM watch_pending WHERE project = ? AND trace_id = ? AND stream = ? AND payload = ?`, item.Project, item.TraceID, item.Stream, item.Payload).Scan(&n)
	return n == 1, err
}

// A newer revision of the same stream must survive an older evaluation's ack.
// A nonempty owner also fences against the policy-engine lease.
func (s *Store) AckWatchPending(ctx context.Context, owner string, item WatchPending) error {
	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkWatchLease(ctx, tx, owner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM watch_pending WHERE project = ? AND trace_id = ? AND stream = ? AND payload = ?`, item.Project, item.TraceID, item.Stream, item.Payload); err != nil {
		return err
	}
	return tx.Commit()
}

// Upgrade pending work written by earlier development builds without losing it.
// Startup holds the metadata schema lock and invokes this inside a transaction.
func migrateWatchPending(ctx context.Context, q querier, d dialect) error {
	var exists int
	query := `SELECT COUNT(*) FROM pragma_table_info('watch_pending') WHERE name = 'stream'`
	if d == postgresDialect {
		query = `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'watch_pending' AND column_name = 'stream'`
	}
	if err := q.QueryRowContext(ctx, query).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	for _, statement := range []string{
		`ALTER TABLE watch_pending RENAME TO watch_pending_legacy`,
		watchPendingSchema,
		`INSERT INTO watch_pending (project, trace_id, stream, payload, start_ns) SELECT project, trace_id, '', payload, start_ns FROM watch_pending_legacy`,
		`DROP TABLE watch_pending_legacy`,
	} {
		if _, err := q.ExecContext(ctx, d.ddl(statement)); err != nil {
			return err
		}
	}
	return nil
}
