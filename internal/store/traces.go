package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// PutTrace stores (or replaces, when late spans re-assemble it) a trace.
func (s *Store) PutTrace(ctx context.Context, summary *evalsiv1alpha1.TraceSummary, record *evalsiv1alpha1.Record) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO traces (trace_id, service, start_ns, summary, record) VALUES (?, ?, ?, ?, ?)`,
		summary.GetTraceId(), summary.GetService(), summary.GetStartTime().AsTime().UnixNano(), marshal(summary), marshal(record))
	return err
}

// ListTraces returns trace summaries newest first, with result counts.
func (s *Store) ListTraces(ctx context.Context, service string, pageSize int, pageToken string) ([]*evalsiv1alpha1.TraceSummary, string, error) {
	offset := 0
	if pageToken != "" {
		var err error
		if offset, err = strconv.Atoi(pageToken); err != nil || offset < 0 {
			return nil, "", fmt.Errorf("store: bad page token")
		}
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.summary, (SELECT COUNT(*) FROM trace_results r WHERE r.trace_id = t.trace_id)
		FROM traces t WHERE (? = '' OR t.service = ?)
		ORDER BY t.start_ns DESC, t.trace_id LIMIT ? OFFSET ?`, service, service, pageSize+1, offset)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.TraceSummary
	for rows.Next() {
		var blob []byte
		var n int32
		if err := rows.Scan(&blob, &n); err != nil {
			return nil, "", err
		}
		sum := &evalsiv1alpha1.TraceSummary{}
		if err := proto.Unmarshal(blob, sum); err != nil {
			return nil, "", err
		}
		sum.Results = n
		out = append(out, sum)
	}
	next := ""
	if len(out) > pageSize {
		out, next = out[:pageSize], strconv.Itoa(offset+pageSize)
	}
	return out, next, rows.Err()
}

// GetTrace returns a stored trace's record.
func (s *Store) GetTrace(ctx context.Context, traceID string) (*evalsiv1alpha1.Record, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT record FROM traces WHERE trace_id = ?`, traceID).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rec := &evalsiv1alpha1.Record{}
	return rec, proto.Unmarshal(blob, rec)
}

// PutTraceResults stores a policy's results for a trace.
func (s *Store) PutTraceResults(ctx context.Context, traceID, policy string, results []*evalsiv1alpha1.EvaluationResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range results {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO trace_results (trace_id, policy, evaluator, result) VALUES (?, ?, ?, ?)`,
			traceID, policy, r.GetEvaluator(), marshal(r)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TraceResults returns results for a trace grouped by policy, policies sorted.
func (s *Store) TraceResults(ctx context.Context, traceID string) ([]*evalsiv1alpha1.PolicyResults, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT policy, result FROM trace_results WHERE trace_id = ? ORDER BY policy, rowid`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.PolicyResults
	for rows.Next() {
		var policy string
		var blob []byte
		if err := rows.Scan(&policy, &blob); err != nil {
			return nil, err
		}
		r := &evalsiv1alpha1.EvaluationResult{}
		if err := proto.Unmarshal(blob, r); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].GetPolicy() != policy {
			out = append(out, &evalsiv1alpha1.PolicyResults{Policy: policy})
		}
		out[len(out)-1].Results = append(out[len(out)-1].Results, r)
	}
	return out, rows.Err()
}

// DeleteTracesBefore removes traces (and their results) that started before t.
func (s *Store) DeleteTracesBefore(ctx context.Context, t time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM trace_results WHERE trace_id IN (SELECT trace_id FROM traces WHERE start_ns < ?)`, t.UnixNano()); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM traces WHERE start_ns < ?`, t.UnixNano())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// PutPolicy creates or replaces a policy.
func (s *Store) PutPolicy(ctx context.Context, p *evalsiv1alpha1.OnlineEvalPolicy) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO policies (name, project, policy) VALUES (?, ?, ?)`,
		p.GetName(), p.GetProject(), marshal(p))
	return err
}

// DeletePolicy removes a policy; ErrNotFound when it does not exist.
func (s *Store) DeletePolicy(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM policies WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Policies returns every policy, sorted by name.
func (s *Store) Policies(ctx context.Context) ([]*evalsiv1alpha1.OnlineEvalPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT policy FROM policies ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.OnlineEvalPolicy
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		p := &evalsiv1alpha1.OnlineEvalPolicy{}
		if err := proto.Unmarshal(blob, p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
