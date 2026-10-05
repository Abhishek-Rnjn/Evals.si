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

// PutTrace stores (or replaces, when late spans re-assemble it) a trace in
// its project (summary.Project).
func (s *Store) PutTrace(ctx context.Context, summary *evalsiv1alpha1.TraceSummary, record *evalsiv1alpha1.Record) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO traces (project, trace_id, service, start_ns, summary, record) VALUES (?, ?, ?, ?, ?, ?)`,
		summary.GetProject(), summary.GetTraceId(), summary.GetService(), summary.GetStartTime().AsTime().UnixNano(), marshal(summary), marshal(record))
	return err
}

// TraceFilter selects traces.
type TraceFilter struct {
	// nil: every project.
	Projects []string
	Service  string
}

// ListTraces returns trace summaries newest first, with result counts.
func (s *Store) ListTraces(ctx context.Context, f TraceFilter, pageSize int, pageToken string) ([]*evalsiv1alpha1.TraceSummary, string, error) {
	offset := 0
	if pageToken != "" {
		var err error
		if offset, err = strconv.Atoi(pageToken); err != nil || offset < 0 {
			return nil, "", fmt.Errorf("store: bad page token")
		}
	}
	where, args := projectFilter("t.project", f.Projects)
	args = append(args, f.Service, f.Service, pageSize+1, offset)
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.project, t.summary,
		       (SELECT COUNT(*) FROM trace_results r WHERE r.project = t.project AND r.trace_id = t.trace_id)
		FROM traces t WHERE `+where+` AND (? = '' OR t.service = ?)
		ORDER BY t.start_ns DESC, t.trace_id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.TraceSummary
	for rows.Next() {
		var project string
		var blob []byte
		var n int32
		if err := rows.Scan(&project, &blob, &n); err != nil {
			return nil, "", err
		}
		sum := &evalsiv1alpha1.TraceSummary{}
		if err := proto.Unmarshal(blob, sum); err != nil {
			return nil, "", err
		}
		sum.Project, sum.Results = project, n
		out = append(out, sum)
	}
	next := ""
	if len(out) > pageSize {
		out, next = out[:pageSize], strconv.Itoa(offset+pageSize)
	}
	return out, next, rows.Err()
}

// TraceProjects lists the projects holding a trace with this id.
func (s *Store) TraceProjects(ctx context.Context, traceID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project FROM traces WHERE trace_id = ? ORDER BY project`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetTrace returns a stored trace's summary and record.
func (s *Store) GetTrace(ctx context.Context, project, traceID string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, error) {
	var sumBlob, recBlob []byte
	err := s.db.QueryRowContext(ctx, `SELECT summary, record FROM traces WHERE project = ? AND trace_id = ?`, project, traceID).Scan(&sumBlob, &recBlob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	sum, rec := &evalsiv1alpha1.TraceSummary{}, &evalsiv1alpha1.Record{}
	if err := proto.Unmarshal(sumBlob, sum); err != nil {
		return nil, nil, err
	}
	sum.Project = project
	return sum, rec, proto.Unmarshal(recBlob, rec)
}

// PutTraceResults stores a policy's results for a trace.
func (s *Store) PutTraceResults(ctx context.Context, project, traceID, policy string, results []*evalsiv1alpha1.EvaluationResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range results {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO trace_results (project, trace_id, policy, evaluator, result) VALUES (?, ?, ?, ?, ?)`,
			project, traceID, policy, r.GetEvaluator(), marshal(r)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// TraceResults returns results for a trace grouped by policy, policies sorted.
func (s *Store) TraceResults(ctx context.Context, project, traceID string) ([]*evalsiv1alpha1.PolicyResults, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT policy, result FROM trace_results WHERE project = ? AND trace_id = ? ORDER BY policy, rowid`, project, traceID)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM trace_results WHERE (project, trace_id) IN (SELECT project, trace_id FROM traces WHERE start_ns < ?)`, t.UnixNano()); err != nil {
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

// TraceQuery selects stored traces of one project, newest first.
type TraceQuery struct {
	Project string
	Service string
	// Only traces the policy evaluated.
	Policy string
	// Only traces that started at or after this time; zero means any.
	Since time.Time
	Limit int
}

// StoredTrace is a trace with its record and, when a policy was named, that
// policy's results.
type StoredTrace struct {
	Summary *evalsiv1alpha1.TraceSummary
	Record  *evalsiv1alpha1.Record
	Results []*evalsiv1alpha1.EvaluationResult
}

// QueryTraces returns the traces a query selects.
func (s *Store) QueryTraces(ctx context.Context, q TraceQuery) ([]StoredTrace, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = -1
	}
	var since int64
	if !q.Since.IsZero() {
		since = q.Since.UnixNano()
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.trace_id, t.summary, t.record FROM traces t
		WHERE t.project = ? AND (? = '' OR t.service = ?) AND t.start_ns >= ?
		  AND (? = '' OR EXISTS (SELECT 1 FROM trace_results r WHERE r.project = t.project AND r.trace_id = t.trace_id AND r.policy = ?))
		ORDER BY t.start_ns DESC, t.trace_id LIMIT ?`,
		q.Project, q.Service, q.Service, since, q.Policy, q.Policy, limit)
	if err != nil {
		return nil, err
	}
	var out []StoredTrace
	for rows.Next() {
		var id string
		var sumBlob, recBlob []byte
		if err := rows.Scan(&id, &sumBlob, &recBlob); err != nil {
			rows.Close()
			return nil, err
		}
		st := StoredTrace{Summary: &evalsiv1alpha1.TraceSummary{}, Record: &evalsiv1alpha1.Record{}}
		if err := proto.Unmarshal(sumBlob, st.Summary); err != nil {
			rows.Close()
			return nil, err
		}
		if err := proto.Unmarshal(recBlob, st.Record); err != nil {
			rows.Close()
			return nil, err
		}
		st.Summary.Project = q.Project
		out = append(out, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if q.Policy == "" {
		return out, nil
	}
	for i := range out {
		groups, err := s.TraceResults(ctx, q.Project, out[i].Summary.GetTraceId())
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if g.GetPolicy() == q.Policy {
				out[i].Results = g.GetResults()
			}
		}
	}
	return out, nil
}
