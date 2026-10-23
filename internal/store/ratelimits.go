package store

import (
	"context"
	"slices"
)

// rateWindowSchema counts calls per rate-limit bucket (class and caller) in
// fixed windows, for limits shared by every replica (decision 0029). Rows
// are only ever a few windows old: PruneRateCounts drops the rest.
const rateWindowSchema = `
CREATE TABLE IF NOT EXISTS rate_windows (
  bucket TEXT NOT NULL, start_ns INTEGER NOT NULL,
  calls INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (bucket, start_ns)
);
`

// AddRateCounts adds each bucket's calls to the window starting at startNs
// and returns every listed bucket's total in it, all replicas together. A
// zero adds nothing and only reads. Buckets are written in a fixed order, so
// replicas writing the same buckets at once do not deadlock.
func (s *Store) AddRateCounts(ctx context.Context, startNs int64, add map[string]int64) (map[string]int64, error) {
	keys := make([]string, 0, len(add))
	for k := range add {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out := make(map[string]int64, len(keys))
	for _, k := range keys {
		var n int64
		if add[k] == 0 {
			err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(calls), 0) FROM rate_windows WHERE bucket = ? AND start_ns = ?`, k, startNs).Scan(&n)
		} else {
			err = tx.QueryRowContext(ctx,
				`INSERT INTO rate_windows (bucket, start_ns, calls) VALUES (?, ?, ?)
				 ON CONFLICT (bucket, start_ns) DO UPDATE SET calls = rate_windows.calls + excluded.calls
				 RETURNING calls`, k, startNs, add[k]).Scan(&n)
		}
		if err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, tx.Commit()
}

// PruneRateCounts drops the windows that started before beforeNs.
func (s *Store) PruneRateCounts(ctx context.Context, beforeNs int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM rate_windows WHERE start_ns < ?`, beforeNs)
	return err
}
