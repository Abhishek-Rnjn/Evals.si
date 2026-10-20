package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// TraceStore keeps traces and their online scores: high-volume, append-mostly
// data that may live apart from the rest (ClickHouse at scale).
type TraceStore interface {
	PutTrace(ctx context.Context, summary *evalsiv1alpha1.TraceSummary, record *evalsiv1alpha1.Record) error
	// PutTraces stores several traces in one write (a transaction, or one
	// insert), which is what keeps ingest fast.
	PutTraces(ctx context.Context, traces []TraceWrite) error
	ListTraces(ctx context.Context, f TraceFilter, pageSize int, pageToken string) ([]*evalsiv1alpha1.TraceSummary, string, error)
	TraceProjects(ctx context.Context, traceID string) ([]string, error)
	GetTrace(ctx context.Context, project, traceID string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, error)
	PutTraceResults(ctx context.Context, project, traceID, policy string, results []*evalsiv1alpha1.EvaluationResult) error
	TraceResults(ctx context.Context, project, traceID string) ([]*evalsiv1alpha1.PolicyResults, error)
	DeleteTracesBefore(ctx context.Context, t time.Time) (int64, error)
	QueryTraces(ctx context.Context, q TraceQuery) ([]StoredTrace, error)
	ScoredTraces(ctx context.Context, q ScoreQuery) ([]ScoredTrace, error)
	Close() error
}

// sqlTraces keeps traces in the store's own SQL database.
type sqlTraces struct{ db *conn }

func (s *sqlTraces) Close() error { return nil }

// OpenPostgres opens a PostgreSQL database by DSN (a URL or key=value
// string), creating the schema if needed. Several evalsid replicas may share
// one database.
func OpenPostgres(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(16)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: connecting to postgres: %w", err)
	}
	c := &conn{db: db, d: postgresDialect}
	// Replicas starting together would race on CREATE TABLE IF NOT EXISTS:
	// an advisory lock serializes schema creation.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7266657473690001)`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := applyMigrations(ctx, &txn{tx: tx, d: postgresDialect}, postgresDialect, migrations); err != nil {
		_ = db.Close()
		if errors.Is(err, ErrSchemaTooNew) {
			return nil, err
		}
		return nil, fmt.Errorf("store: migrating: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return newStore(c), nil
}

func newStore(c *conn) *Store {
	return &Store{db: c, traces: &sqlTraces{db: c}}
}

// UseTraces moves traces and their scores to another backend.
func (s *Store) UseTraces(t TraceStore) { s.traces = t }

// Dialect names the SQL database ("sqlite" or "postgres").
func (s *Store) Dialect() string {
	if s.db.d == postgresDialect {
		return "postgres"
	}
	return "sqlite"
}

func (s *Store) PutTrace(ctx context.Context, summary *evalsiv1alpha1.TraceSummary, record *evalsiv1alpha1.Record) error {
	return s.traces.PutTrace(ctx, summary, record)
}

// TraceWrite is one trace to store.
type TraceWrite struct {
	Summary *evalsiv1alpha1.TraceSummary
	Record  *evalsiv1alpha1.Record
}

// PutTraces stores several traces in one write.
func (s *Store) PutTraces(ctx context.Context, traces []TraceWrite) error {
	return s.traces.PutTraces(ctx, traces)
}

// ListTraces returns trace summaries newest first, with result counts.
func (s *Store) ListTraces(ctx context.Context, f TraceFilter, pageSize int, pageToken string) ([]*evalsiv1alpha1.TraceSummary, string, error) {
	return s.traces.ListTraces(ctx, f, pageSize, pageToken)
}

// TraceProjects lists the projects holding a trace with this id.
func (s *Store) TraceProjects(ctx context.Context, traceID string) ([]string, error) {
	return s.traces.TraceProjects(ctx, traceID)
}

// GetTrace returns one trace of a project.
func (s *Store) GetTrace(ctx context.Context, project, traceID string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, error) {
	return s.traces.GetTrace(ctx, project, traceID)
}

type revisionTraceReader interface {
	getTraceRevision(context.Context, string, string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, string, error)
	traceResultsAt(context.Context, string, string, string) ([]*evalsiv1alpha1.PolicyResults, error)
}

// GetTraceWithResults pins results to the record returned by the first read,
// even if publication advances while assembling the response.
func (s *Store) GetTraceWithResults(ctx context.Context, project, id string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, []*evalsiv1alpha1.PolicyResults, error) {
	reader, ok := s.traces.(revisionTraceReader)
	if !ok {
		return nil, nil, nil, fmt.Errorf("trace backend does not support revision-bound reads")
	}
	summary, record, binding, err := reader.getTraceRevision(ctx, project, id)
	if err != nil {
		return nil, nil, nil, err
	}
	results, err := reader.traceResultsAt(ctx, project, id, binding)
	return summary, record, results, err
}

// PutTraceResults stores a policy's results for a trace.
func (s *Store) PutTraceResults(ctx context.Context, project, traceID, policy string, results []*evalsiv1alpha1.EvaluationResult) error {
	return s.traces.PutTraceResults(ctx, project, traceID, policy, results)
}

// TraceResults returns a trace's results, grouped by policy.
func (s *Store) TraceResults(ctx context.Context, project, traceID string) ([]*evalsiv1alpha1.PolicyResults, error) {
	return s.traces.TraceResults(ctx, project, traceID)
}

// DeleteTracesBefore removes traces (and their results) that started before t.
func (s *Store) DeleteTracesBefore(ctx context.Context, t time.Time) (int64, error) {
	if ch, ok := s.traces.(*clickhouse); ok {
		return s.deleteClickHouseTracesBefore(ctx, t, ch)
	}
	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := expireWatchMetadata(ctx, tx, t); err != nil {
		return 0, err
	}
	var n int64
	if _, ok := s.traces.(*sqlTraces); ok {
		n, err = deleteSQLTracesBefore(ctx, tx, t)
	} else {
		n, err = s.traces.DeleteTracesBefore(ctx, t)
	}
	if err != nil {
		return n, err
	}
	if err := finishWatchRetention(ctx, tx, t); err != nil {
		return n, err
	}
	return n, tx.Commit()
}

// QueryTraces returns the traces a query selects.
func (s *Store) QueryTraces(ctx context.Context, q TraceQuery) ([]StoredTrace, error) {
	return s.traces.QueryTraces(ctx, q)
}

// ScoredTraces returns the traces a score query selects, with their results.
func (s *Store) ScoredTraces(ctx context.Context, q ScoreQuery) ([]ScoredTrace, error) {
	return s.traces.ScoredTraces(ctx, q)
}
