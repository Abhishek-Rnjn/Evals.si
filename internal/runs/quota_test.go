package runs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func quotaHarness(t *testing.T, q config.Quotas, now func() time.Time) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db", "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return newHarnessWith(t, st, dir, func(o *Options) {
		o.Quotas = q
		o.Now = now
	})
}

func (h *harness) tryCreate(project string, spec *evalsiv1alpha1.RunSpec) (*evalsiv1alpha1.Run, error) {
	resp, err := h.m.CreateRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: project, Spec: spec}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRun(), nil
}

func TestQuotaDailyJudgeTokens(t *testing.T) {
	day := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	now := day
	h := quotaHarness(t, config.Quotas{
		Default:  config.QuotaLimits{JudgeTokensPerDay: 250},
		Projects: map[string]config.QuotaLimits{"big": {JudgeTokensPerDay: 100000}},
	}, func() time.Time { return now })
	h.worker.judgeTokens = 100
	spec := func() *evalsiv1alpha1.RunSpec {
		return &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline("easy", "easy"), Evaluators: refs("exact-match")}
	}
	// 200 tokens: under the quota.
	first, err := h.tryCreate("p", spec())
	if err != nil {
		t.Fatal(err)
	}
	if run := h.wait(t, first.GetId()); run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("first run: %v %s", run.GetStatus(), run.GetError())
	}
	// 400 in all: the second run stops once the project is over.
	second, err := h.tryCreate("p", spec())
	if err != nil {
		t.Fatal(err)
	}
	run := h.wait(t, second.GetId())
	if run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR || !strings.Contains(run.GetError(), "judge_tokens_per_day 250") {
		t.Fatalf("second run: %v %q", run.GetStatus(), run.GetError())
	}
	// Now over: new runs are refused today ...
	if _, err := h.tryCreate("p", spec()); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
	// ... but not in another project with its own quota, or tomorrow.
	if _, err := h.tryCreate("big", spec()); err != nil {
		t.Fatal(err)
	}
	now = day.Add(24 * time.Hour)
	if _, err := h.tryCreate("p", spec()); err != nil {
		t.Fatalf("a new day: %v", err)
	}
}

func TestQuotaStoredRuns(t *testing.T) {
	h := quotaHarness(t, config.Quotas{Default: config.QuotaLimits{MaxStoredRuns: 1}}, nil)
	spec := &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline("easy"), Evaluators: refs("exact-match")}
	if _, err := h.tryCreate("p", spec); err != nil {
		t.Fatal(err)
	}
	_, err := h.tryCreate("p", spec)
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "max_stored_runs") {
		t.Fatalf("got %v", err)
	}
	if _, err := h.tryCreate("other", spec); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaConcurrentRuns(t *testing.T) {
	h := quotaHarness(t, config.Quotas{Default: config.QuotaLimits{MaxConcurrentRuns: 1}}, nil)
	slow := func() *evalsiv1alpha1.RunSpec {
		return &evalsiv1alpha1.RunSpec{Dataset: inline("easy"), Evaluators: refs("test/slow")}
	}
	a, err := h.tryCreate("p", slow())
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.tryCreate("p", slow())
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.tryCreate("q", slow())
	if err != nil {
		t.Fatal(err)
	}
	// The global limit is 2: p's first run and q's run start; p's second waits.
	deadline := time.Now().Add(5 * time.Second)
	for h.worker.evaluateCalls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := h.worker.evaluateCalls.Load(); n != 2 {
		t.Fatalf("%d runs evaluating, want 2 (one per project)", n)
	}
	got, _ := h.st.GetRun(context.Background(), b.GetId())
	if got.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING {
		t.Fatalf("p's second run is %v, want PENDING", got.GetStatus())
	}
	close(h.worker.release)
	for _, id := range []string{a.GetId(), b.GetId(), other.GetId()} {
		if run := h.wait(t, id); run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
			t.Fatalf("%s: %v %s", id, run.GetStatus(), run.GetError())
		}
	}
}

func TestQuotaLimitsInheritTheDefault(t *testing.T) {
	q := config.Quotas{
		Default:  config.QuotaLimits{MaxConcurrentRuns: 2, JudgeTokensPerDay: 10},
		Projects: map[string]config.QuotaLimits{"x": {JudgeTokensPerDay: 99}},
	}
	if l := q.For("x"); l.MaxConcurrentRuns != 2 || l.JudgeTokensPerDay != 99 {
		t.Errorf("x: %+v", l)
	}
	if l := q.For("y"); l.JudgeTokensPerDay != 10 {
		t.Errorf("y: %+v", l)
	}
}

func TestRunMetrics(t *testing.T) {
	h := newHarness(t)
	run := h.create(t, &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline("easy"), Evaluators: refs("exact-match")})
	h.wait(t, run.GetId())
	var b strings.Builder
	h.m.WriteMetrics(&b)
	for _, want := range []string{"evalsi_runs_active 0", "evalsi_runs_executing 0", `evalsi_runs_finished_total{status="succeeded"} 1`, `evalsi_runs_finished_total{status="error"} 0`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in:\n%s", want, b.String())
		}
	}
}
