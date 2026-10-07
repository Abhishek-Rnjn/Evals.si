package store

import (
	"context"
	"database/sql"
	"errors"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// guardrailSchema holds inline guardrails. updated_ns lets every replica
// notice a change without a broadcast: a check compares it with the
// version it compiled.
const guardrailSchema = `
CREATE TABLE IF NOT EXISTS guardrails (
  project TEXT NOT NULL, name TEXT NOT NULL, body BLOB NOT NULL, updated_ns INTEGER NOT NULL,
  PRIMARY KEY (project, name)
);
`

// PutGuardrail creates or replaces a guardrail.
func (s *Store) PutGuardrail(ctx context.Context, g *evalsiv1alpha1.Guardrail) error {
	body, err := proto.Marshal(g)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO guardrails (project, name, body, updated_ns) VALUES (?, ?, ?, ?)
		 ON CONFLICT (project, name) DO UPDATE SET body = excluded.body, updated_ns = excluded.updated_ns`,
		g.GetProject(), g.GetName(), body, g.GetUpdatedAt().AsTime().UnixNano())
	return err
}

// GetGuardrail returns a guardrail.
func (s *Store) GetGuardrail(ctx context.Context, project, name string) (*evalsiv1alpha1.Guardrail, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM guardrails WHERE project = ? AND name = ?`, project, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g := &evalsiv1alpha1.Guardrail{}
	return g, proto.Unmarshal(body, g)
}

// ListGuardrails returns a project's guardrails by name ("" lists every project's).
func (s *Store) ListGuardrails(ctx context.Context, project string) ([]*evalsiv1alpha1.Guardrail, error) {
	q, args := `SELECT body FROM guardrails ORDER BY project, name`, []any{}
	if project != "" {
		q, args = `SELECT body FROM guardrails WHERE project = ? ORDER BY name`, []any{project}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.Guardrail
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		g := &evalsiv1alpha1.Guardrail{}
		if err := proto.Unmarshal(body, g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteGuardrail removes a guardrail.
func (s *Store) DeleteGuardrail(ctx context.Context, project, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM guardrails WHERE project = ? AND name = ?`, project, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
