package store

import "context"

// The policy queue lives in the metadata database even when traces live in
// ClickHouse. Keeping the redacted record here also recovers a failed trace write.
const watchPendingSchema = `
CREATE TABLE IF NOT EXISTS watch_pending (
  project TEXT NOT NULL, trace_id TEXT NOT NULL, payload BLOB NOT NULL, start_ns INTEGER NOT NULL,
  PRIMARY KEY (project, trace_id)
);
`

type WatchPending struct {
	Project, TraceID string
	Payload          []byte
	StartNS          int64
}

func (s *Store) PutWatchPending(ctx context.Context, items []WatchPending) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `INSERT INTO watch_pending (project, trace_id, payload, start_ns) VALUES (?, ?, ?, ?)
			ON CONFLICT (project, trace_id) DO UPDATE SET payload = excluded.payload, start_ns = excluded.start_ns`, item.Project, item.TraceID, item.Payload, item.StartNS); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT project, trace_id, payload, start_ns FROM watch_pending
		WHERE project > ? OR (project = ? AND trace_id > ?) ORDER BY project, trace_id LIMIT ?`, cursor.Project, cursor.Project, cursor.TraceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []WatchPending
	for rows.Next() {
		var item WatchPending
		if err := rows.Scan(&item.Project, &item.TraceID, &item.Payload, &item.StartNS); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) WatchPendingMatches(ctx context.Context, item WatchPending) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM watch_pending WHERE project = ? AND trace_id = ? AND payload = ?`, item.Project, item.TraceID, item.Payload).Scan(&n)
	return n == 1, err
}

// A newer revision of the same trace must survive an older evaluation's ack.
func (s *Store) AckWatchPending(ctx context.Context, item WatchPending) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM watch_pending WHERE project = ? AND trace_id = ? AND payload = ?`, item.Project, item.TraceID, item.Payload)
	return err
}
