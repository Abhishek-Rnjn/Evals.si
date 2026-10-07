package store

import (
	"context"
)

// quotaSchema counts what each project used per UTC day, for daily quotas.
const quotaSchema = `
CREATE TABLE IF NOT EXISTS quota_usage (
  project TEXT NOT NULL, day TEXT NOT NULL,
  judge_tokens INTEGER NOT NULL DEFAULT 0, target_tokens INTEGER NOT NULL DEFAULT 0,
  tenant_id TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (project, day)
);
`

// Usage is what a project used on one day.
type Usage struct {
	JudgeTokens, TargetTokens int64
}

// AddUsage adds to a project's usage for day (YYYY-MM-DD, UTC) and returns
// the new totals.
func (s *Store) AddUsage(ctx context.Context, project, day string, add Usage) (Usage, error) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO quota_usage (project, day, judge_tokens, target_tokens) VALUES (?, ?, ?, ?)
		 ON CONFLICT (project, day) DO UPDATE SET
		   judge_tokens = quota_usage.judge_tokens + excluded.judge_tokens,
		   target_tokens = quota_usage.target_tokens + excluded.target_tokens`,
		project, day, add.JudgeTokens, add.TargetTokens); err != nil {
		return Usage{}, err
	}
	return s.UsageOn(ctx, project, day)
}

// UsageOn is a project's usage for day; zero when nothing was recorded.
func (s *Store) UsageOn(ctx context.Context, project, day string) (Usage, error) {
	var u Usage
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(judge_tokens), 0), COALESCE(SUM(target_tokens), 0) FROM quota_usage WHERE project = ? AND day = ?`,
		project, day).Scan(&u.JudgeTokens, &u.TargetTokens)
	return u, err
}

// CountRuns is the number of stored runs in a project.
func (s *Store) CountRuns(ctx context.Context, project string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE project = ?`, project).Scan(&n)
	return n, err
}
