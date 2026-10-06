package store

import (
	"context"
	"strings"
	"time"
)

// rewardCacheSchema shares Reward Service component scores between replicas.
const rewardCacheSchema = `
CREATE TABLE IF NOT EXISTS reward_cache (
  key TEXT PRIMARY KEY, result BLOB NOT NULL, created_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS reward_cache_by_age ON reward_cache (created_ns);
`

// GetRewardCache returns the cached results among keys.
func (s *Store) GetRewardCache(ctx context.Context, keys []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for start := 0; start < len(keys); start += 500 {
		chunk := keys[start:min(start+500, len(keys))]
		args := make([]any, len(chunk))
		for i, k := range chunk {
			args[i] = k
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT key, result FROM reward_cache WHERE key IN (?`+strings.Repeat(", ?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			var v []byte
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = v
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PutRewardCache stores results; existing keys keep their first value.
func (s *Store) PutRewardCache(ctx context.Context, entries map[string][]byte) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	for k, v := range entries {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reward_cache (key, result, created_ns) VALUES (?, ?, ?) ON CONFLICT (key) DO NOTHING`, k, v, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneRewardCache deletes entries older than before.
func (s *Store) PruneRewardCache(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM reward_cache WHERE created_ns < ?`, before.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
