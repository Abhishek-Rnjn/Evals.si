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
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7266657473690002)`); err != nil {
			return IdempotencyResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE (scope, key) IN
		(SELECT scope, key FROM idempotency WHERE completed = 1 AND expires_ns <= ? ORDER BY expires_ns LIMIT 1000)`, now.UnixNano()); err != nil {
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

func (s *Store) CompleteIdempotency(ctx context.Context, scope, key, fingerprint, responseType string, response []byte, code int, message string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if s.db.d == postgresDialect {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7266657473690002)`); err != nil {
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
