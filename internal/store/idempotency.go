package store

import (
	"context"
	"errors"
	"time"
)

const idempotencySchema = `
CREATE TABLE IF NOT EXISTS idempotency (
  scope TEXT NOT NULL, key TEXT NOT NULL, fingerprint TEXT NOT NULL,
  completed INTEGER NOT NULL DEFAULT 0, response_type TEXT NOT NULL DEFAULT '',
  response BLOB NOT NULL, code INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT '',
  expires_ns INTEGER NOT NULL, PRIMARY KEY (scope, key)
);
CREATE INDEX IF NOT EXISTS idempotency_expiry ON idempotency (completed, expires_ns);
`

var ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")
var ErrIdempotencyCapacity = errors.New("idempotency cache capacity is exhausted; reconcile unresolved requests or wait for completed entries to expire")

type IdempotencyResult struct {
	Claimed      bool
	Completed    bool
	ResponseType string
	Response     []byte
	Code         int
	Message      string
}

func (s *Store) BeginIdempotency(ctx context.Context, scope, key, fingerprint string, now time.Time) (IdempotencyResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IdempotencyResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if s.db.d == postgresDialect {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?, 7266657473690002))`, scope); err != nil {
			return IdempotencyResult{}, err
		}
	}
	// Completed outcomes expire at their TTL. A claim that never completed (a
	// crash between the side effect and its record) is kept much longer so it is
	// not silently re-executed, but it is reaped at last: otherwise such rows
	// count toward the per-scope cap forever.
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE (scope, key) IN
		(SELECT scope, key FROM idempotency
		 WHERE scope = ? AND ((completed = 1 AND expires_ns <= ?) OR (completed = 0 AND expires_ns <= ?))
		 ORDER BY expires_ns LIMIT 1000)`, scope, now.UnixNano(), now.Add(-UnresolvedClaimRetention).UnixNano()); err != nil {
		return IdempotencyResult{}, err
	}
	var entries, existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN key = ? THEN 1 ELSE 0 END), 0) FROM idempotency WHERE scope = ?`, key, scope).Scan(&entries, &existing); err != nil {
		return IdempotencyResult{}, err
	}
	if entries >= 1024 && existing == 0 {
		return IdempotencyResult{}, ErrIdempotencyCapacity
	}
	insert, err := tx.ExecContext(ctx, `INSERT INTO idempotency (scope, key, fingerprint, response, expires_ns)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (scope, key) DO NOTHING`, scope, key, fingerprint, []byte{}, now.Add(24*time.Hour).UnixNano())
	if err != nil {
		return IdempotencyResult{}, err
	}
	count, err := insert.RowsAffected()
	if err != nil {
		return IdempotencyResult{}, err
	}
	result := IdempotencyResult{Claimed: count == 1}
	if !result.Claimed {
		var stored string
		var completed int
		if err := tx.QueryRowContext(ctx, `SELECT fingerprint, completed, response_type, response, code, message FROM idempotency WHERE scope = ? AND key = ?`, scope, key).
			Scan(&stored, &completed, &result.ResponseType, &result.Response, &result.Code, &result.Message); err != nil {
			return IdempotencyResult{}, err
		}
		if stored != fingerprint {
			return IdempotencyResult{}, ErrIdempotencyConflict
		}
		result.Completed = completed == 1
	}
	return result, tx.Commit()
}

// UnresolvedClaimRetention is how long past its expiry a claim that never
// completed blocks its key before it is reaped.
const UnresolvedClaimRetention = 6 * 24 * time.Hour

// PruneIdempotency removes expired outcomes and long-unresolved claims from
// every scope, and returns how many it removed. BeginIdempotency only cleans
// the scope it is called for, so a scope that is never used again (a retired
// key or project) would otherwise keep its rows, and their cached responses,
// for good. Each scope is cleaned under the same per-scope lock as a request in
// that scope, so the two never contend for the same rows.
func (s *Store) PruneIdempotency(ctx context.Context, now time.Time) (int, error) {
	unresolvedBefore := now.Add(-UnresolvedClaimRetention).UnixNano()
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT scope FROM idempotency
		WHERE (completed = 1 AND expires_ns <= ?) OR (completed = 0 AND expires_ns <= ?) LIMIT 100`, now.UnixNano(), unresolvedBefore)
	if err != nil {
		return 0, err
	}
	var scopes []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			_ = rows.Close()
			return 0, err
		}
		scopes = append(scopes, scope)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	removed := 0
	for _, scope := range scopes {
		n, err := s.pruneIdempotencyScope(ctx, scope, now.UnixNano(), unresolvedBefore)
		if err != nil {
			return removed, err
		}
		removed += n
	}
	return removed, nil
}

func (s *Store) pruneIdempotencyScope(ctx context.Context, scope string, now, unresolvedBefore int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if s.db.d == postgresDialect {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?, 7266657473690002))`, scope); err != nil {
			return 0, err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE (scope, key) IN
		(SELECT scope, key FROM idempotency
		 WHERE scope = ? AND ((completed = 1 AND expires_ns <= ?) OR (completed = 0 AND expires_ns <= ?))
		 ORDER BY expires_ns LIMIT 1000)`, scope, now, unresolvedBefore)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

// ReleaseIdempotency drops a claim whose request failed, so a retry with the
// same key runs again instead of replaying the failure. Completed outcomes are
// left alone.
func (s *Store) ReleaseIdempotency(ctx context.Context, scope, key, fingerprint string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM idempotency WHERE scope = ? AND key = ? AND fingerprint = ? AND completed = 0`, scope, key, fingerprint)
	return err
}

func (s *Store) CompleteIdempotency(ctx context.Context, scope, key, fingerprint, responseType string, response []byte, code int, message string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if s.db.d == postgresDialect {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?, 7266657473690002))`, scope); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE idempotency SET completed = 1, response_type = ?, response = ?, code = ?, message = ?, expires_ns = ?
		WHERE scope = ? AND key = ? AND fingerprint = ? AND completed = 0
		AND (SELECT COALESCE(SUM(LENGTH(response)), 0) FROM idempotency WHERE scope = ?) + ? <= ?`, responseType, response, code, message, now.Add(24*time.Hour).UnixNano(), scope, key, fingerprint, scope, len(response), 64<<20)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrIdempotencyCapacity
	}
	return tx.Commit()
}
