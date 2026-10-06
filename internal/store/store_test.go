package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// open returns an empty store: SQLite, or PostgreSQL in a schema of its own
// when EVALSI_TEST_POSTGRES_DSN is set (CI runs the package both ways).
func open(t *testing.T) *Store {
	t.Helper()
	if dsn := os.Getenv("EVALSI_TEST_POSTGRES_DSN"); dsn != "" {
		return openPostgres(t, dsn)
	}
	s, err := Open(filepath.Join(t.TempDir(), "sub", "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openPostgres(t *testing.T, dsn string) *Store {
	t.Helper()
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := OpenPostgres(ctx, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Dialect() != "postgres" {
		t.Fatal(s.Dialect())
	}
	return s
}

func run(id, project string, nanos int64) *evalsiv1alpha1.Run {
	return &evalsiv1alpha1.Run{Id: id, Project: project, CreatedAt: timestamppb.New(time.Unix(0, nanos)), Status: evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING}
}

func TestRunsLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	records := []*evalsiv1alpha1.Record{{Id: "a"}, {Id: "b"}}
	for i, id := range []string{"r1", "r2", "r3"} {
		r := run(id, map[bool]string{true: "p", false: "q"}[i < 2], int64(i))
		if err := s.CreateRun(ctx, r, records); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetRun(ctx, "r1")
	if err != nil || got.GetProject() != "p" {
		t.Fatalf("GetRun = %v, %v", got, err)
	}
	if _, err := s.GetRun(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	got.Status = evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING
	if err := s.UpdateRun(ctx, got); err != nil {
		t.Fatal(err)
	}
	running, _ := s.RunsWithStatus(ctx, evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING)
	if len(running) != 1 || running[0].GetId() != "r1" {
		t.Fatalf("RunsWithStatus = %v", running)
	}
	page, next, err := s.ListRuns(ctx, []string{"p"}, 1, "")
	if err != nil || len(page) != 1 || next == "" {
		t.Fatalf("ListRuns page 1 = %v %q %v", page, next, err)
	}
	page2, next2, _ := s.ListRuns(ctx, []string{"p"}, 1, next)
	if len(page2) != 1 || next2 != "" || page2[0].GetId() == page[0].GetId() {
		t.Fatalf("ListRuns page 2 = %v %q", page2, next2)
	}
	all, _, _ := s.ListRuns(ctx, nil, 10, "")
	if len(all) != 3 {
		t.Fatalf("ListRuns all = %d", len(all))
	}
	snap, _ := s.Records(ctx, "r1")
	if len(snap) != 2 || snap[1].GetId() != "b" {
		t.Fatalf("Records = %v", snap)
	}
}

func TestOutputsAndResultsAreIdempotent(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.CreateRun(ctx, run("r", "", 0), nil); err != nil {
		t.Fatal(err)
	}
	out := []Output{{RecordIdx: 0, Trial: 1, Record: &evalsiv1alpha1.Record{Id: "a"}}, {RecordIdx: 1, Trial: 1, Error: "boom"}}
	if err := s.PutOutputs(ctx, "r", out); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOutputs(ctx, "r", out[:1]); err != nil {
		t.Fatal(err)
	}
	outputs, _ := s.Outputs(ctx, "r")
	if len(outputs) != 2 || outputs[[2]int{1, 1}].Error != "boom" || outputs[[2]int{0, 1}].Record.GetId() != "a" {
		t.Fatalf("Outputs = %v", outputs)
	}
	res := func(rec, trial, ev int, name string) Result {
		return Result{RecordIdx: rec, Trial: trial, EvalIdx: ev, Result: &evalsiv1alpha1.EvaluationResult{Evaluator: name}}
	}
	batch := []Result{res(1, 0, 0, "em"), res(0, 0, 1, "j"), res(0, 0, 0, "em"), res(DatasetIndex, 0, 2, "count"), res(0, 1, 0, "em")}
	if err := s.PutResults(ctx, "r", batch); err != nil {
		t.Fatal(err)
	}
	if err := s.PutResults(ctx, "r", batch[:2]); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.ResultKeys(ctx, "r")
	if len(keys) != 5 || !keys[Key{DatasetIndex, 0, 2}] {
		t.Fatalf("ResultKeys = %v", keys)
	}
	ordered, _ := s.Results(ctx, "r", "", 0, 0)
	var order []string
	for _, r := range ordered {
		order = append(order, r.Result.GetEvaluator())
	}
	if want := "em j em count em"; joined(order) != want {
		t.Fatalf("order = %q, want %q", joined(order), want)
	}
	page, _ := s.Results(ctx, "r", "em", 2, 1)
	if len(page) != 2 || page[0].RecordIdx != 1 || page[1].Trial != 1 {
		t.Fatalf("filtered page = %+v", page)
	}
}

func joined(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
