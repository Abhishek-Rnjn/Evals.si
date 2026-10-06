package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// TraceStore keeps traces and their online scores: high-volume, append-mostly
// data that may live apart from the rest (ClickHouse at scale).
type TraceStore interface {
	PutTrace(ctx context.Context, summary *evalsiv1alpha1.TraceSummary, record *evalsiv1alpha1.Record) error
	ListTraces(ctx context.Context, f TraceFilter, pageSize int, pageToken string) ([]*evalsiv1alpha1.TraceSummary, string, error)
	TraceProjects(ctx context.Context, traceID string) ([]string, error)
	GetTrace(ctx context.Context, project, traceID string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, error)
	PutTraceResults(ctx context.Context, project, traceID, policy string, results []*evalsiv1alpha1.EvaluationResult) error
	TraceResults(ctx context.Context, project, traceID string) ([]*evalsiv1alpha1.PolicyResults, error)
	DeleteTracesBefore(ctx context.Context, t time.Time) (int64, error)
	QueryTraces(ctx context.Context, q TraceQuery) ([]StoredTrace, error)
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
	if _, err := tx.ExecContext(ctx, c.d.ddl(schema+authSchema)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: creating schema: %w", err)
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
	return s.traces.DeleteTracesBefore(ctx, t)
}

// QueryTraces returns the traces a query selects.
func (s *Store) QueryTraces(ctx context.Context, q TraceQuery) ([]StoredTrace, error) {
	return s.traces.QueryTraces(ctx, q)
}
