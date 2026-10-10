package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// sourceSchema holds trace sources and what the connector manager needs to
// resume them: the watermark and counters (source_state), the traces already
// handed to the policy engine (source_seen, which makes pulling at least
// once and scoring once), and the scores already written back
// (source_writes, which keeps a retry from duplicating an assessment).
const sourceSchema = `
CREATE TABLE IF NOT EXISTS sources (
  project TEXT NOT NULL, name TEXT NOT NULL, body BLOB NOT NULL, updated_ns INTEGER NOT NULL,
  PRIMARY KEY (project, name)
);
CREATE TABLE IF NOT EXISTS source_state (
  project TEXT NOT NULL, source TEXT NOT NULL,
  watermark_ns INTEGER NOT NULL, backfill_from_ns INTEGER NOT NULL,
  pulled INTEGER NOT NULL, scored INTEGER NOT NULL, deferred INTEGER NOT NULL,
  last_error TEXT NOT NULL, last_error_ns INTEGER NOT NULL,
  last_pull_ns INTEGER NOT NULL, last_write_ns INTEGER NOT NULL,
  PRIMARY KEY (project, source)
);
CREATE TABLE IF NOT EXISTS source_seen (
  project TEXT NOT NULL, source TEXT NOT NULL, trace_id TEXT NOT NULL,
  digest TEXT NOT NULL, started_ns INTEGER NOT NULL,
  PRIMARY KEY (project, source, trace_id)
);
CREATE INDEX IF NOT EXISTS source_seen_by_start ON source_seen (project, source, started_ns);
CREATE TABLE IF NOT EXISTS source_writes (
  project TEXT NOT NULL, source TEXT NOT NULL, trace_id TEXT NOT NULL, metric TEXT NOT NULL,
  remote_id TEXT NOT NULL, digest TEXT NOT NULL, written_ns INTEGER NOT NULL,
  PRIMARY KEY (project, source, trace_id, metric)
);
`

// PutSource creates or replaces a source. Its status is not stored here: it
// is assembled from SourceState when read.
func (s *Store) PutSource(ctx context.Context, src *evalsiv1alpha1.TraceSource) error {
	clone := proto.Clone(src).(*evalsiv1alpha1.TraceSource)
	clone.Status = nil
	body, err := proto.Marshal(clone)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO sources (project, name, body, updated_ns) VALUES (?, ?, ?, ?)
		 ON CONFLICT (project, name) DO UPDATE SET body = excluded.body, updated_ns = excluded.updated_ns`,
		src.GetProject(), src.GetName(), body, src.GetUpdatedAt().AsTime().UnixNano())
	return err
}

// GetSource returns a source, without status.
func (s *Store) GetSource(ctx context.Context, project, name string) (*evalsiv1alpha1.TraceSource, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM sources WHERE project = ? AND name = ?`, project, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	src := &evalsiv1alpha1.TraceSource{}
	return src, proto.Unmarshal(body, src)
}

// ListSources returns a project's sources by name ("" lists every project's).
func (s *Store) ListSources(ctx context.Context, project string) ([]*evalsiv1alpha1.TraceSource, error) {
	q, args := `SELECT body FROM sources ORDER BY project, name`, []any{}
	if project != "" {
		q, args = `SELECT body FROM sources WHERE project = ? ORDER BY name`, []any{project}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.TraceSource
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		src := &evalsiv1alpha1.TraceSource{}
		if err := proto.Unmarshal(body, src); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// DeleteSource removes a source with its state, seen traces and write records.
func (s *Store) DeleteSource(ctx context.Context, project, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE project = ? AND name = ?`, project, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, table := range []string{"source_state", "source_seen", "source_writes"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE project = ? AND source = ?`, project, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SourceState is what the manager resumes a source from, and what its status
// reports.
type SourceState struct {
	// Every trace that started before Watermark has been stored and queued.
	Watermark time.Time
	// Non-zero while a backfill is running: the time it reads from.
	BackfillFrom time.Time
	Pulled       int64
	Scored       int64
	Deferred     int
	LastError    string
	LastErrorAt  time.Time
	LastPullAt   time.Time
	LastWriteAt  time.Time
}

// SourceState returns a source's state; a source never pulled has the zero
// value.
func (s *Store) SourceState(ctx context.Context, project, name string) (SourceState, error) {
	var st SourceState
	var wm, bf, errNS, pullNS, writeNS int64
	err := s.db.QueryRowContext(ctx,
		`SELECT watermark_ns, backfill_from_ns, pulled, scored, deferred, last_error, last_error_ns, last_pull_ns, last_write_ns
		 FROM source_state WHERE project = ? AND source = ?`, project, name).
		Scan(&wm, &bf, &st.Pulled, &st.Scored, &st.Deferred, &st.LastError, &errNS, &pullNS, &writeNS)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Watermark, st.BackfillFrom = fromNS(wm).UTC(), fromNS(bf).UTC()
	st.LastErrorAt, st.LastPullAt, st.LastWriteAt = fromNS(errNS).UTC(), fromNS(pullNS).UTC(), fromNS(writeNS).UTC()
	return st, nil
}

// PutSourceState stores a source's state.
func (s *Store) PutSourceState(ctx context.Context, project, name string, st SourceState) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO source_state (project, source, watermark_ns, backfill_from_ns, pulled, scored, deferred, last_error, last_error_ns, last_pull_ns, last_write_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (project, source) DO UPDATE SET watermark_ns = excluded.watermark_ns, backfill_from_ns = excluded.backfill_from_ns,
		   pulled = excluded.pulled, scored = excluded.scored, deferred = excluded.deferred, last_error = excluded.last_error,
		   last_error_ns = excluded.last_error_ns, last_pull_ns = excluded.last_pull_ns, last_write_ns = excluded.last_write_ns`,
		project, name, ns(st.Watermark), ns(st.BackfillFrom), st.Pulled, st.Scored, st.Deferred, st.LastError,
		ns(st.LastErrorAt), ns(st.LastPullAt), ns(st.LastWriteAt))
	return err
}

// SeenTrace is a trace already handed to the policy engine.
type SeenTrace struct {
	TraceID string
	// Changes when the trace's content does, so a trace that gained spans is
	// scored again and an unchanged one is not.
	Digest  string
	Started time.Time
}

// SeenTraces returns which of ids the source has already handled, by trace ID.
func (s *Store) SeenTraces(ctx context.Context, project, source string, ids []string) (map[string]SeenTrace, error) {
	out := make(map[string]SeenTrace, len(ids))
	// Chunked so a large page stays under the drivers' parameter limits.
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		q := `SELECT trace_id, digest, started_ns FROM source_seen WHERE project = ? AND source = ? AND trace_id IN (?` + repeat(",?", len(part)-1) + `)`
		args := []any{project, source}
		for _, id := range part {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t SeenTrace
			var started int64
			if err := rows.Scan(&t.TraceID, &t.Digest, &started); err != nil {
				_ = rows.Close()
				return nil, err
			}
			t.Started = fromNS(started)
			out[t.TraceID] = t
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// MarkSeen records traces handed to the policy engine.
func (s *Store) MarkSeen(ctx context.Context, project, source string, seen []SeenTrace) error {
	if len(seen) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range seen {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO source_seen (project, source, trace_id, digest, started_ns) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (project, source, trace_id) DO UPDATE SET digest = excluded.digest, started_ns = excluded.started_ns`,
			project, source, t.TraceID, t.Digest, ns(t.Started)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneSeen forgets traces that started before t: they are older than any
// window the manager re-reads, so they can no longer come back.
func (s *Store) PruneSeen(ctx context.Context, project, source string, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM source_seen WHERE project = ? AND source = ? AND started_ns < ?`, project, source, ns(t))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SourceWrite is a score written back to a source's trace.
type SourceWrite struct {
	TraceID string
	// The policy that produced the score: two policies with the same
	// metric write separate scores.
	Policy string
	Metric string
	// The store's ID for what was written (an MLflow assessment ID), so a
	// changed score updates it instead of adding another.
	RemoteID string
	// Of the value written, so an unchanged score is not sent again.
	Digest  string
	Written time.Time
}

// WriteKey identifies one written-back score.
type WriteKey struct{ TraceID, Policy, Metric string }

// Key is the write's identity.
func (w SourceWrite) Key() WriteKey { return WriteKey{w.TraceID, w.Policy, w.Metric} }

// writeSep joins policy and metric in the metric column, so scores from
// different policies keep separate rows. Rows written before policies were
// recorded have no separator and read back with no policy.
const writeSep = "\x1f"

func writeColumn(policy, metric string) string {
	if policy == "" {
		return metric
	}
	return policy + writeSep + metric
}

func splitWriteColumn(col string) (policy, metric string) {
	if p, m, ok := strings.Cut(col, writeSep); ok {
		return p, m
	}
	return "", col
}

// SourceWrites returns what was written back for the given traces.
func (s *Store) SourceWrites(ctx context.Context, project, source string, ids []string) (map[WriteKey]SourceWrite, error) {
	out := map[WriteKey]SourceWrite{}
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		q := `SELECT trace_id, metric, remote_id, digest, written_ns FROM source_writes WHERE project = ? AND source = ? AND trace_id IN (?` + repeat(",?", len(part)-1) + `)`
		args := []any{project, source}
		for _, id := range part {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var w SourceWrite
			var written int64
			if err := rows.Scan(&w.TraceID, &w.Metric, &w.RemoteID, &w.Digest, &written); err != nil {
				_ = rows.Close()
				return nil, err
			}
			w.Written = fromNS(written)
			w.Policy, w.Metric = splitWriteColumn(w.Metric)
			out[w.Key()] = w
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return out, nil
}

// PutSourceWrite records a score written back.
func (s *Store) PutSourceWrite(ctx context.Context, project, source string, w SourceWrite) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO source_writes (project, source, trace_id, metric, remote_id, digest, written_ns) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (project, source, trace_id, metric) DO UPDATE SET remote_id = excluded.remote_id, digest = excluded.digest, written_ns = excluded.written_ns`,
		project, source, w.TraceID, writeColumn(w.Policy, w.Metric), w.RemoteID, w.Digest, ns(w.Written))
	return err
}
