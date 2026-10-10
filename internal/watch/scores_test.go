package watch

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// scoresStore holds n scored traces in project p, newest last, labelled
// workflow=a or workflow=b in turn (the oldest also first=yes), and one
// unscored trace.
func scoresStore(t *testing.T, n int) (*store.Store, time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for i := range n {
		id := fmt.Sprintf("t%02d", i)
		labels := map[string]string{"workflow": map[int]string{0: "a", 1: "b"}[i%2]}
		if i == 0 {
			labels["first"] = "yes"
		}
		sum := &evalsiv1alpha1.TraceSummary{Project: "p", TraceId: id, Service: "studio", StartTime: timestamppb.New(base.Add(time.Duration(i) * time.Minute)),
			Labels: labels}
		if err := st.PutTrace(ctx, sum, &evalsiv1alpha1.Record{Id: id}); err != nil {
			t.Fatal(err)
		}
		if err := st.PutTraceResults(ctx, "p", id, "prod", []*evalsiv1alpha1.EvaluationResult{{Evaluator: "loop"}, {Evaluator: "cost"}}); err != nil {
			t.Fatal(err)
		}
	}
	unscored := &evalsiv1alpha1.TraceSummary{Project: "p", TraceId: "u", Service: "studio", StartTime: timestamppb.New(base.Add(time.Hour))}
	if err := st.PutTrace(ctx, unscored, &evalsiv1alpha1.Record{Id: "u"}); err != nil {
		t.Fatal(err)
	}
	return st, base
}

// listAll pages through ListScores and returns the trace ids in order.
func listAll(t *testing.T, ctx context.Context, tr Traces, req *evalsiv1alpha1.ListScoresRequest) (ids []string, pages int) {
	t.Helper()
	for {
		resp, err := tr.ListScores(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, ts := range resp.Msg.GetTraces() {
			ids = append(ids, ts.GetTrace().GetTraceId())
		}
		if req.PageToken = resp.Msg.GetNextPageToken(); req.PageToken == "" {
			return ids, pages
		}
		if pages > 100 {
			t.Fatal("paging does not end")
		}
	}
}

func TestListScores(t *testing.T) {
	st, base := scoresStore(t, 7)
	tr := Traces{Store: st}
	ctx := context.Background()

	ids, pages := listAll(t, ctx, tr, &evalsiv1alpha1.ListScoresRequest{PageSize: 3})
	if fmt.Sprint(ids) != "[t06 t05 t04 t03 t02 t01 t00]" || pages != 3 {
		t.Errorf("all scores: %v in %d pages", ids, pages)
	}
	// A page that ends on the last trace has no token.
	resp, err := tr.ListScores(ctx, connect.NewRequest(&evalsiv1alpha1.ListScoresRequest{PageSize: 7}))
	if err != nil || len(resp.Msg.GetTraces()) != 7 || resp.Msg.GetNextPageToken() != "" {
		t.Errorf("one full page: %d traces, token %q, %v", len(resp.Msg.GetTraces()), resp.Msg.GetNextPageToken(), err)
	}
	first := resp.Msg.GetTraces()[0]
	if first.GetTrace().GetResults() != 2 || len(first.GetPolicies()) != 1 || first.GetPolicies()[0].GetPolicy() != "prod" {
		t.Errorf("first = %v", first)
	}

	// Labels filter across pages; every page is full but the last.
	ids, _ = listAll(t, ctx, tr, &evalsiv1alpha1.ListScoresRequest{Labels: []string{"workflow=a"}, PageSize: 2})
	if fmt.Sprint(ids) != "[t06 t04 t02 t00]" {
		t.Errorf("workflow=a: %v", ids)
	}
	ids, _ = listAll(t, ctx, tr, &evalsiv1alpha1.ListScoresRequest{Labels: []string{"workflow=a", "workflow=b"}})
	if len(ids) != 0 {
		t.Errorf("two values for one key match no trace: %v", ids)
	}
	ids, _ = listAll(t, ctx, tr, &evalsiv1alpha1.ListScoresRequest{Labels: []string{"workflow=c"}})
	if len(ids) != 0 {
		t.Errorf("workflow=c: %v", ids)
	}

	// Since, trace id (any case) and evaluator.
	ids, _ = listAll(t, ctx, tr, &evalsiv1alpha1.ListScoresRequest{Since: timestamppb.New(base.Add(5 * time.Minute))})
	if fmt.Sprint(ids) != "[t06 t05]" {
		t.Errorf("since: %v", ids)
	}
	resp, err = tr.ListScores(ctx, connect.NewRequest(&evalsiv1alpha1.ListScoresRequest{TraceId: "T03", Evaluator: "cost"}))
	if err != nil || len(resp.Msg.GetTraces()) != 1 || len(resp.Msg.GetTraces()[0].GetPolicies()[0].GetResults()) != 1 {
		t.Errorf("trace t03, cost: %v %v", resp, err)
	}

	for _, bad := range []*evalsiv1alpha1.ListScoresRequest{{Labels: []string{"workflow"}}, {Labels: []string{"=a"}}, {PageToken: "not a token"}} {
		if _, err := tr.ListScores(ctx, connect.NewRequest(bad)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%v: %v", bad, err)
		}
	}
}

// A sparse filter does not read the whole store for one page: past the scan
// budget the page comes back short, with a token that continues the scan.
func TestListScoresScanBudget(t *testing.T) {
	// With one trace a page, each chunk reads at most two traces: the oldest,
	// the only match, lies past the first page's budget.
	st, _ := scoresStore(t, 2*scoreScanChunks+2)
	tr := Traces{Store: st}
	ctx := context.Background()
	req := &evalsiv1alpha1.ListScoresRequest{Labels: []string{"first=yes"}, PageSize: 1}
	resp, err := tr.ListScores(ctx, connect.NewRequest(req))
	if err != nil || len(resp.Msg.GetTraces()) != 0 || resp.Msg.GetNextPageToken() == "" {
		t.Fatalf("first page: %v %v", resp, err)
	}
	ids, pages := listAll(t, ctx, tr, req)
	if fmt.Sprint(ids) != "[t00]" || pages < 2 {
		t.Errorf("first=yes: %v in %d pages", ids, pages)
	}
}

// Scores follow trace-read access: a label-scoped grant sees only the traces
// it may read, and paging continues past the ones it may not.
func TestListScoresAccess(t *testing.T) {
	st, _ := scoresStore(t, 6)
	ctx := context.Background()
	e, err := authz.NewEngine(ctx, authz.Options{Enabled: true, RBAC: authz.RBACConfig{
		Roles:    []authz.Role{{Name: "workflow-b", Permissions: []string{"traces.read"}, Condition: `resource.labels.workflow == "b"`}},
		Projects: map[string]map[string][]string{"p": {}, "q": {}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	key := func(roles map[string][]string) context.Context {
		return authz.WithChecker(ctx, &authz.Checker{Engine: e, Principal: &auth.Principal{
			Kind: auth.KindAPIKey, Method: auth.MethodAPIKey, Subject: "k", KeyName: "k", KeyRoles: roles}})
	}
	tr := Traces{Store: st}
	ids, _ := listAll(t, key(map[string][]string{"p": {"workflow-b"}}), tr, &evalsiv1alpha1.ListScoresRequest{PageSize: 2})
	if fmt.Sprint(ids) != "[t05 t03 t01]" {
		t.Errorf("workflow-b grant: %v", ids)
	}
	ids, _ = listAll(t, key(map[string][]string{"q": {"viewer"}}), tr, &evalsiv1alpha1.ListScoresRequest{})
	if len(ids) != 0 {
		t.Errorf("grant in another project: %v", ids)
	}
	ids, _ = listAll(t, key(map[string][]string{"q": {"viewer"}}), tr, &evalsiv1alpha1.ListScoresRequest{Project: "p"})
	if len(ids) != 0 {
		t.Errorf("asking for a project without a grant: %v", ids)
	}
}
