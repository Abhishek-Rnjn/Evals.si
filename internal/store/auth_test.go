package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
)

// TestMigratesPhase1Traces opens a database written before traces had
// projects: its traces and results move to the default project.
func TestMigratesPhase1Traces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evalsi.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	sum := marshal(&evalsiv1alpha1.TraceSummary{TraceId: "abc", Service: "svc"})
	rec := marshal(&evalsiv1alpha1.Record{Id: "abc"})
	res := marshal(&evalsiv1alpha1.EvaluationResult{Evaluator: "e"})
	for _, stmt := range []string{
		`CREATE TABLE traces (trace_id TEXT PRIMARY KEY, service TEXT NOT NULL, start_ns INTEGER NOT NULL, summary BLOB NOT NULL, record BLOB NOT NULL)`,
		`CREATE INDEX traces_by_service ON traces (service, start_ns DESC)`,
		`CREATE INDEX traces_by_start ON traces (start_ns DESC)`,
		`CREATE TABLE trace_results (trace_id TEXT NOT NULL, policy TEXT NOT NULL, evaluator TEXT NOT NULL, result BLOB NOT NULL, PRIMARY KEY (trace_id, policy, evaluator))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO traces VALUES ('abc', 'svc', 1, ?, ?)`, sum, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO trace_results VALUES ('abc', 'p', 'e', ?)`, res); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	got, _, err := s.GetTrace(ctx, "default", "abc")
	if err != nil || got.GetProject() != "default" {
		t.Fatalf("migrated trace = %v, %v", got, err)
	}
	results, _ := s.TraceResults(ctx, "default", "abc")
	if len(results) != 1 {
		t.Errorf("migrated results = %v", results)
	}
	// The same id in another project is a separate trace.
	if err := s.PutTrace(ctx, &evalsiv1alpha1.TraceSummary{Project: "other", TraceId: "abc", Service: "evil"}, &evalsiv1alpha1.Record{Id: "abc"}); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := s.GetTrace(ctx, "default", "abc"); again.GetService() != "svc" {
		t.Error("another project overwrote the trace")
	}
	if ps, _ := s.TraceProjects(ctx, "abc"); len(ps) != 2 {
		t.Errorf("TraceProjects = %v", ps)
	}
	only, _, _ := s.ListTraces(ctx, TraceFilter{Projects: []string{"other"}}, 10, "")
	if len(only) != 1 || only[0].GetService() != "evil" {
		t.Errorf("filtered list = %v", only)
	}
	// Re-opening is a no-op.
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
}

func TestAuthTables(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.CreateProject(ctx, authz.Project{Name: "p", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, authz.Project{Name: "p"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate project: %v", err)
	}
	r := authz.Role{Name: "r", Project: "p", Permissions: []string{"runs.read"}, Condition: "true"}
	if err := s.PutRole(ctx, r, true); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRole(ctx, r, true); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate role: %v", err)
	}
	r.Permissions = []string{"runs.*"}
	if err := s.PutRole(ctx, r, false); err != nil {
		t.Fatal(err)
	}
	roles, _ := s.AuthRoles(ctx)
	if len(roles) != 1 || roles[0].Permissions[0] != "runs.*" {
		t.Errorf("roles = %v", roles)
	}
	b := authz.Binding{Project: "p", Role: "r", Subject: "user:corp/a"}
	if err := s.CreateBinding(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBinding(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBinding(ctx, b); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	k := authz.KeyRecord{Name: "ci", Hash: "h1", Prefix: "evk_x", Roles: map[string][]string{"p": {"r"}}, CreatedAt: time.Now()}
	if err := s.CreateAPIKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.KeyByHash(ctx, "h1"); got == nil || got.Name != "ci" || got.Roles["p"][0] != "r" {
		t.Errorf("KeyByHash = %+v", got)
	}
	if got, err := s.KeyByHash(ctx, "nope"); got != nil || err != nil {
		t.Errorf("unknown hash = %v %v", got, err)
	}
	_ = s.TouchKey(ctx, "ci", time.Now())
	_ = s.RevokeAPIKey(ctx, "ci")
	if got, _ := s.APIKey(ctx, "ci"); !got.Revoked || got.LastUsedAt.IsZero() {
		t.Errorf("key = %+v", got)
	}
	for i, p := range []string{"a", "b", "a"} {
		_, err := s.AppendAudit(ctx, &evalsiv1alpha1.AuditEvent{Time: timestamppb.New(time.Unix(int64(i), 0)), Project: p, Action: "runs.create", Allowed: i != 1})
		if err != nil {
			t.Fatal(err)
		}
	}
	page, next, _ := s.ListAudit(ctx, authz.AuditQuery{}, 2, "")
	rest, _, _ := s.ListAudit(ctx, authz.AuditQuery{}, 2, next)
	if len(page) != 2 || page[0].GetId() != 3 || len(rest) != 1 {
		t.Errorf("audit pages = %v / %v", page, rest)
	}
	if only, _, _ := s.ListAudit(ctx, authz.AuditQuery{Projects: []string{"a"}}, 10, ""); len(only) != 2 {
		t.Errorf("project filter = %v", only)
	}
	if denied, _, _ := s.ListAudit(ctx, authz.AuditQuery{DeniedOnly: true}, 10, ""); len(denied) != 1 || denied[0].GetProject() != "b" {
		t.Errorf("denied = %v", denied)
	}
	if n, _ := s.DeleteAuditBefore(ctx, time.Unix(2, 0)); n != 2 {
		t.Errorf("retention deleted %d", n)
	}
}
