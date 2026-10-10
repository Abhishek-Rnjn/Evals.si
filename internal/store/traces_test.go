package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// traceBackends are the trace stores under test: the store's own SQL, and
// ClickHouse in a database of its own when EVALSI_TEST_CLICKHOUSE_URL is set.
func traceBackends(t *testing.T) map[string]TraceStore {
	t.Helper()
	out := map[string]TraceStore{"sql": open(t).traces}
	if u := os.Getenv("EVALSI_TEST_CLICKHOUSE_URL"); u != "" {
		db := fmt.Sprintf("evalsi_t%d", time.Now().UnixNano())
		ch, err := OpenClickHouse(context.Background(), ClickHouseConfig{URL: u, Database: db}, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ch.(*clickhouse).exec(context.Background(), "DROP DATABASE "+db, nil, nil) })
		out["clickhouse"] = ch
	}
	return out
}

func trace(project, id, service string, start time.Time) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record) {
	return &evalsiv1alpha1.TraceSummary{Project: project, TraceId: id, Service: service, StartTime: timestamppb.New(start), Name: id},
		&evalsiv1alpha1.Record{Id: id, Metadata: nil}
}

func TestTraceStores(t *testing.T) {
	for name, ts := range traceBackends(t) {
		t.Run(name, func(t *testing.T) { testTraceStore(t, ts) })
	}
}

func testTraceStore(t *testing.T, ts TraceStore) {
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i, tr := range []struct{ project, id, service string }{
		{"p", "t1", "chat"}, {"p", "t2", "search"}, {"p", "t3", "chat"}, {"q", "t1", "chat"},
	} {
		sum, rec := trace(tr.project, tr.id, tr.service, base.Add(time.Duration(i)*time.Minute))
		if err := ts.PutTrace(ctx, sum, rec); err != nil {
			t.Fatal(err)
		}
	}
	// Re-putting replaces.
	sum, rec := trace("p", "t2", "search", base.Add(time.Minute))
	sum.Name = "renamed"
	if err := ts.PutTrace(ctx, sum, rec); err != nil {
		t.Fatal(err)
	}
	got, gotRec, err := ts.GetTrace(ctx, "p", "t2")
	if err != nil || got.GetName() != "renamed" || got.GetProject() != "p" || gotRec.GetId() != "t2" {
		t.Fatalf("GetTrace = %v %v %v", got, gotRec, err)
	}
	if _, _, err := ts.GetTrace(ctx, "q", "t2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another project's trace: %v", err)
	}
	// A batch in one write; a trace repeated in it keeps its last version.
	later := base.Add(48 * time.Hour) // after the retention check below
	b1, r1 := trace("b", "x1", "batch", later)
	b2, r2 := trace("b", "x2", "batch", later)
	b2again, r2again := trace("b", "x2", "batch", later)
	b2again.Name = "last"
	if err := ts.PutTraces(ctx, []TraceWrite{{b1, r1}, {b2, r2}, {b2again, r2again}}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := ts.GetTrace(ctx, "b", "x2"); err != nil || got.GetName() != "last" {
		t.Errorf("batched x2 = %v %v", got, err)
	}
	if _, _, err := ts.GetTrace(ctx, "b", "x1"); err != nil {
		t.Errorf("batched x1: %v", err)
	}
	if err := ts.PutTraces(ctx, nil); err != nil {
		t.Errorf("an empty batch: %v", err)
	}
	projects, err := ts.TraceProjects(ctx, "t1")
	if err != nil || fmt.Sprint(projects) != "[p q]" {
		t.Errorf("TraceProjects = %v %v", projects, err)
	}

	r := func(evaluator string) *evalsiv1alpha1.EvaluationResult {
		return &evalsiv1alpha1.EvaluationResult{RecordId: "t1", Evaluator: evaluator}
	}
	if err := ts.PutTraceResults(ctx, "p", "t1", "pol", []*evalsiv1alpha1.EvaluationResult{r("a"), r("b")}); err != nil {
		t.Fatal(err)
	}
	if err := ts.PutTraceResults(ctx, "p", "t1", "pol", []*evalsiv1alpha1.EvaluationResult{r("a")}); err != nil {
		t.Fatal(err)
	}
	if err := ts.PutTraceResults(ctx, "p", "t3", "other", []*evalsiv1alpha1.EvaluationResult{r("c")}); err != nil {
		t.Fatal(err)
	}
	groups, err := ts.TraceResults(ctx, "p", "t1")
	if err != nil || len(groups) != 1 || groups[0].GetPolicy() != "pol" || len(groups[0].GetResults()) != 2 {
		t.Fatalf("TraceResults = %v %v", groups, err)
	}

	// Newest first, with result counts, filtered by project and service, paged.
	page, next, err := ts.ListTraces(ctx, TraceFilter{Projects: []string{"p"}}, 2, "")
	if err != nil || len(page) != 2 || page[0].GetTraceId() != "t3" || page[1].GetTraceId() != "t2" || next == "" {
		t.Fatalf("ListTraces page 1 = %v %q %v", page, next, err)
	}
	if page[0].GetResults() != 1 {
		t.Errorf("t3 results = %d", page[0].GetResults())
	}
	page, next, err = ts.ListTraces(ctx, TraceFilter{Projects: []string{"p"}}, 2, next)
	if err != nil || len(page) != 1 || page[0].GetTraceId() != "t1" || page[0].GetResults() != 2 || next != "" {
		t.Fatalf("ListTraces page 2 = %v %q %v", page, next, err)
	}
	all, _, err := ts.ListTraces(ctx, TraceFilter{Service: "chat"}, 10, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("every project's chat traces = %v %v", all, err)
	}
	none, _, err := ts.ListTraces(ctx, TraceFilter{Projects: []string{}}, 10, "")
	if err != nil || len(none) != 0 {
		t.Fatalf("no projects = %v %v", none, err)
	}

	q, err := ts.QueryTraces(ctx, TraceQuery{Project: "p", Policy: "pol"})
	if err != nil || len(q) != 1 || q[0].Summary.GetTraceId() != "t1" || len(q[0].Results) != 2 {
		t.Fatalf("QueryTraces by policy = %+v %v", q, err)
	}
	q, err = ts.QueryTraces(ctx, TraceQuery{Project: "p", Since: base.Add(time.Minute), Limit: 1})
	if err != nil || len(q) != 1 || q[0].Summary.GetTraceId() != "t3" {
		t.Fatalf("QueryTraces since, limit = %+v %v", q, err)
	}

	n, err := ts.DeleteTracesBefore(ctx, base.Add(90*time.Second))
	if err != nil || n != 2 {
		t.Fatalf("DeleteTracesBefore = %d %v", n, err)
	}
	if _, _, err := ts.GetTrace(ctx, "p", "t1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted trace: %v", err)
	}
	if groups, _ := ts.TraceResults(ctx, "p", "t1"); len(groups) != 0 {
		t.Errorf("deleted trace's results: %v", groups)
	}
	if groups, _ := ts.TraceResults(ctx, "p", "t3"); len(groups) != 1 {
		t.Errorf("kept trace's results: %v", groups)
	}
}

func TestScoredTraces(t *testing.T) {
	for name, ts := range traceBackends(t) {
		t.Run(name, func(t *testing.T) { testScoredTraces(t, ts) })
	}
}

func testScoredTraces(t *testing.T, ts TraceStore) {
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// s1 and s2 start together, in two projects with the same id; u is unscored.
	put := func(project, id, service string, start time.Time) {
		t.Helper()
		sum, rec := trace(project, id, service, start)
		if err := ts.PutTrace(ctx, sum, rec); err != nil {
			t.Fatal(err)
		}
	}
	put("p", "s0", "chat", base)
	put("p", "s1", "chat", base.Add(time.Minute))
	put("q", "s1", "chat", base.Add(time.Minute))
	put("p", "s2", "search", base.Add(time.Minute))
	put("p", "u", "chat", base.Add(2*time.Minute))
	res := func(names ...string) []*evalsiv1alpha1.EvaluationResult {
		var out []*evalsiv1alpha1.EvaluationResult
		for _, n := range names {
			out = append(out, &evalsiv1alpha1.EvaluationResult{Evaluator: n})
		}
		return out
	}
	for _, w := range []struct{ project, id, policy string }{{"p", "s0", "prod"}, {"p", "s1", "prod"}, {"q", "s1", "prod"}, {"p", "s2", "canary"}} {
		if err := ts.PutTraceResults(ctx, w.project, w.id, w.policy, res("loop", "cost")); err != nil {
			t.Fatal(err)
		}
	}
	if err := ts.PutTraceResults(ctx, "p", "s1", "canary", res("pii")); err != nil {
		t.Fatal(err)
	}
	keys := func(got []ScoredTrace) []string {
		var out []string
		for _, st := range got {
			out = append(out, st.Summary.GetProject()+"/"+st.Summary.GetTraceId())
		}
		return out
	}
	check := func(q ScoreQuery, want ...string) []ScoredTrace {
		t.Helper()
		got, err := ts.ScoredTraces(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(keys(got)) != fmt.Sprint(want) {
			t.Errorf("%+v: got %v, want %v", q, keys(got), want)
		}
		return got
	}
	// Newest first, ties by trace id then project; the unscored trace is left out.
	all := check(ScoreQuery{}, "p/s1", "q/s1", "p/s2", "p/s0")
	if st := all[0]; st.Summary.GetResults() != 3 || len(st.Policies) != 2 || st.Policies[0].GetPolicy() != "canary" || len(st.Policies[1].GetResults()) != 2 {
		t.Errorf("p/s1 = %d results, %v", st.Summary.GetResults(), st.Policies)
	}
	check(ScoreQuery{Projects: []string{"q"}}, "q/s1")
	check(ScoreQuery{Projects: []string{}})
	check(ScoreQuery{TraceID: "s1"}, "p/s1", "q/s1")
	check(ScoreQuery{Service: "search"}, "p/s2")
	check(ScoreQuery{Since: base.Add(time.Minute)}, "p/s1", "q/s1", "p/s2")
	// A policy or evaluator selects both the traces and their results; the
	// count still covers every result.
	got := check(ScoreQuery{Policy: "canary"}, "p/s1", "p/s2")
	if st := got[0]; st.Summary.GetResults() != 3 || len(st.Policies) != 1 || len(st.Policies[0].GetResults()) != 1 {
		t.Errorf("canary p/s1 = %d results, %v", st.Summary.GetResults(), st.Policies)
	}
	got = check(ScoreQuery{Evaluator: "cost", Projects: []string{"p"}}, "p/s1", "p/s2", "p/s0")
	for _, st := range got {
		for _, g := range st.Policies {
			if len(g.GetResults()) != 1 || g.GetResults()[0].GetEvaluator() != "cost" {
				t.Errorf("evaluator cost: %s %v", st.Summary.GetTraceId(), g)
			}
		}
	}
	check(ScoreQuery{Evaluator: "pii", Policy: "prod"})
	// Pages resume after a cursor, through ties on the start time.
	var paged []string
	q := ScoreQuery{Limit: 1}
	for {
		page, err := ts.ScoredTraces(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		paged = append(paged, keys(page)...)
		c := CursorOf(page[len(page)-1].Summary)
		q.After = &c
	}
	if fmt.Sprint(paged) != fmt.Sprint(keys(all)) {
		t.Errorf("paged %v, want %v", paged, keys(all))
	}
}
