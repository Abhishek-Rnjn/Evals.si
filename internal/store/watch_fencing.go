package store

import (
	"context"
	"errors"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// ErrWatchStale means this leadership term or accepted trace revision ended.
var ErrWatchStale = errors.New("online work is no longer current")

// beginWatchWrite serializes short persistence operations across replicas and
// with lease changes, ingestion and retention. No evaluator runs under this lock.
// SQL trace writes share the transaction; an external trace backend is called
// while the metadata lock is held, never through another metadata connection.
func (s *Store) beginWatchWrite(ctx context.Context) (*txn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE watch_write_lock SET version = version WHERE id = 1`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func checkWatchLease(ctx context.Context, q querier, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if owner == "" {
		return nil
	} // Standalone engine.
	var held string
	var expires int64
	err := q.QueryRowContext(ctx, `SELECT owner, expires_ns FROM leases WHERE name = ?`, PolicyEngineLease).Scan(&held, &expires)
	if isNoRows(err) {
		return ErrWatchStale
	}
	if err != nil {
		return err
	}
	if held != owner || expires < time.Now().UnixNano() {
		return ErrWatchStale
	}
	return nil
}

// Reserve external-backend versions in a separate committed statement. Even if
// an INSERT times out after reaching ClickHouse and its metadata transaction
// rolls back, a later writer gets a strictly greater version. Local clocks on
// different replicas cannot reverse the order. Timestamp scale preserves the
// ordering of rows written before the durable queue existed.
func (s *Store) watchVersionContext(ctx context.Context, count int) (context.Context, error) {
	if _, ok := s.traces.(*clickhouse); !ok || count == 0 {
		return ctx, nil
	}
	var end int64
	now := time.Now().UnixNano()
	err := s.db.QueryRowContext(ctx, `UPDATE watch_write_lock SET version = CASE WHEN version > ? THEN version ELSE ? END + ? WHERE id = 1 RETURNING version`, now, now, count).Scan(&end)
	return context.WithValue(ctx, watchVersionKey{}, end-int64(count)+1), err
}

type watchVersionKey struct{}

func traceWriteVersion(ctx context.Context) int64 {
	if version, ok := ctx.Value(watchVersionKey{}).(int64); ok {
		return version
	}
	return time.Now().UnixNano()
}

type WatchTraceWrite struct {
	Pending WatchPending
	Trace   TraceWrite
}

// PutWatchTraces publishes accepted redacted traces regardless of evaluator or
// source-runner readiness. A true slot means that exact pending revision was
// current when it was persisted. Obsolete queue entries cannot overwrite it.
func (s *Store) PutWatchTraces(ctx context.Context, owner string, items []WatchTraceWrite) ([]bool, error) {
	ctx, err := s.watchVersionContext(ctx, len(items))
	if err != nil {
		return nil, err
	}
	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkWatchLease(ctx, tx, owner); err != nil {
		return nil, err
	}
	stored := make([]bool, len(items))
	var traces []TraceWrite
	for i, item := range items {
		matches, err := watchPendingMatches(ctx, tx, item.Pending)
		if err != nil {
			return nil, err
		}
		if !matches {
			continue
		}
		stored[i] = true
		traces = append(traces, item.Trace)
	}
	if len(traces) > 0 {
		if _, ok := s.traces.(*sqlTraces); ok {
			err = putSQLTraces(ctx, tx, traces)
		} else {
			err = s.traces.PutTraces(ctx, traces)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return stored, nil
}

// PutWatchResults checks leadership and the pending revision atomically with
// the write. Checking only when an evaluation starts leaves a takeover race.
func (s *Store) PutWatchResults(ctx context.Context, owner string, item WatchPending, policy string, results []*evalsiv1alpha1.EvaluationResult) error {
	ctx, err := s.watchVersionContext(ctx, 1)
	if err != nil {
		return err
	}
	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkWatchLease(ctx, tx, owner); err != nil {
		return err
	}
	matches, err := watchPendingMatches(ctx, tx, item)
	if err != nil {
		return err
	}
	if !matches {
		return ErrWatchStale
	}
	if _, ok := s.traces.(*sqlTraces); ok {
		err = putSQLTraceResults(ctx, tx, item.Project, item.TraceID, policy, results)
	} else {
		err = s.traces.PutTraceResults(ctx, item.Project, item.TraceID, policy, results)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
