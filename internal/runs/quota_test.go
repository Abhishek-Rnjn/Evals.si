package runs

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	var now atomic.Value
	now.Store(day)
	h := quotaHarness(t, config.Quotas{
		Default:  config.QuotaLimits{JudgeTokensPerDay: 250},
		Projects: map[string]config.QuotaLimits{"big": {JudgeTokensPerDay: 100000}},
	}, func() time.Time { return now.Load().(time.Time) })
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
	now.Store(day.Add(24 * time.Hour))
	if _, err := h.tryCreate("p", spec()); err != nil {
		t.Fatalf("a new day: %v", err)
	}
}

// Sandbox time counts the duration of sandbox-pool evaluators toward the
// project's day, stops the run that passes the quota, and refuses new runs
// until tomorrow.
func TestQuotaDailySandboxSecondsCountEvaluators(t *testing.T) {
	day := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var now atomic.Value
	now.Store(day)
	h := quotaHarness(t, config.Quotas{
		Default:  config.QuotaLimits{SandboxSecondsPerDay: 3},
		Projects: map[string]config.QuotaLimits{"big": {SandboxSecondsPerDay: 1000}},
	}, func() time.Time { return now.Load().(time.Time) })
	h.worker.sandboxedDuration = 2 * time.Second
	spec := func() *evalsiv1alpha1.RunSpec {
		return &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline("easy"), Evaluators: refs("test/sandboxed")}
	}
	// 2 seconds: under the quota of 3.
	first, err := h.tryCreate("p", spec())
	if err != nil {
		t.Fatal(err)
	}
	if run := h.wait(t, first.GetId()); run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("first run: %v %s", run.GetStatus(), run.GetError())
	}
	if used, _ := h.st.UsageOn(context.Background(), "p", "2026-10-06"); used.SandboxMillis != 2000 {
		t.Fatalf("counted %d ms, want 2000", used.SandboxMillis)
	}
	// 4 in all: the second run stops once the project is over.
	second, err := h.tryCreate("p", spec())
	if err != nil {
		t.Fatal(err)
	}
	run := h.wait(t, second.GetId())
	if run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR || !strings.Contains(run.GetError(), "sandbox_seconds_per_day 3") {
		t.Fatalf("second run: %v %q", run.GetStatus(), run.GetError())
	}
	if _, err := h.tryCreate("p", spec()); connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "sandbox_seconds_per_day") {
		t.Fatalf("want ResourceExhausted naming the quota, got %v", err)
	}
	// Another project has its own quota, and tomorrow is a new day.
	if _, err := h.tryCreate("big", spec()); err != nil {
		t.Fatal(err)
	}
	now.Store(day.Add(24 * time.Hour))
	if _, err := h.tryCreate("p", spec()); err != nil {
		t.Fatalf("a new day: %v", err)
	}
}

// An evaluator that does not run in the sandbox costs no sandbox time, however long it takes.
func TestQuotaSandboxSecondsIgnoreOtherEvaluators(t *testing.T) {
	h := quotaHarness(t, config.Quotas{Default: config.QuotaLimits{SandboxSecondsPerDay: 1}}, nil)
	h.worker.sandboxedDuration = 5 * time.Second // only test/sandboxed reports it; exact-match does not
	run := h.wait(t, mustCreate(t, h, &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline("easy", "easy"), Evaluators: refs("exact-match")}).GetId())
	if run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("%v %s", run.GetStatus(), run.GetError())
	}
	if used, _ := h.st.UsageOn(context.Background(), "p", h.m.today()); used.SandboxMillis != 0 {
		t.Fatalf("counted %d ms", used.SandboxMillis)
	}
}

func mustCreate(t *testing.T, h *harness, spec *evalsiv1alpha1.RunSpec) *evalsiv1alpha1.Run {
	t.Helper()
	run, err := h.tryCreate("p", spec)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// An agent task counts from its start to its end, and a run that passes the
// quota stops.
func TestQuotaDailySandboxSecondsCountAgentTasks(t *testing.T) {
	h := quotaHarness(t, config.Quotas{Default: config.QuotaLimits{SandboxSecondsPerDay: 2}}, nil)
	h.worker.taskDelay = 400 * time.Millisecond
	// Six tasks (three records, two trials) of 0.4s each is 2.4s of sandboxed work.
	run := h.wait(t, mustCreate(t, h, agentSpec("pass", "pass", "pass")).GetId())
	if run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR || !strings.Contains(run.GetError(), "sandbox_seconds_per_day 2") {
		t.Fatalf("%v %q", run.GetStatus(), run.GetError())
	}
	if used, _ := h.st.UsageOn(context.Background(), "p", h.m.today()); used.SandboxMillis < 2000 {
		t.Fatalf("counted %d ms, want at least 2000", used.SandboxMillis)
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

func TestArchivingAllowsAnotherRunAtQuota(t *testing.T) {
	h := quotaHarness(t, config.Quotas{Default: config.QuotaLimits{MaxStoredRuns: 1}}, nil)
	spec := &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline("easy"), Evaluators: refs("exact-match")}
	run := h.wait(t, mustCreate(t, h, spec).GetId())
	if _, err := h.tryCreate("p", spec); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("full quota: %v", err)
	}
	if _, err := archive(h, run.GetId()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tryCreate("p", spec); err != nil {
		t.Fatalf("archive did not release quota: %v", err)
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
	// The global limit is 2: one of p's runs and q's run start; p's other
	// run waits. Which of p's runs takes the slot is up to the scheduler.
	deadline := time.Now().Add(5 * time.Second)
	for h.worker.evaluateCalls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := h.worker.evaluateCalls.Load(); n != 2 {
		t.Fatalf("%d runs evaluating, want 2 (one per project)", n)
	}
	pending := 0
	for _, run := range []*evalsiv1alpha1.Run{a, b} {
		if got, _ := h.st.GetRun(context.Background(), run.GetId()); got.GetStatus() == evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("%d of p's runs are PENDING, want 1", pending)
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
	if l := (config.Quotas{Default: config.QuotaLimits{SandboxSecondsPerDay: 60}, Projects: map[string]config.QuotaLimits{"x": {SandboxSecondsPerDay: 5}}}); l.For("x").SandboxSecondsPerDay != 5 || l.For("y").SandboxSecondsPerDay != 60 {
		t.Errorf("sandbox seconds do not inherit: %+v", l)
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

// A shadow replay is two runs: the stored-run quota admits both or neither.
func TestQuotaStoredRunsCoverShadowReplays(t *testing.T) {
	h := quotaHarness(t, config.Quotas{Default: config.QuotaLimits{MaxStoredRuns: 2}}, nil)
	recorded := &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: []*evalsiv1alpha1.Record{
		{Id: "a", Input: text("easy"), Output: text("right"), Reference: text("right")},
	}}}}
	replay := func() error {
		_, err := h.m.CreateShadowReplay(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateShadowReplayRequest{
			Project: "p", Candidate: &evalsiv1alpha1.RunSpec{Target: target(), Dataset: recorded, Evaluators: refs("exact-match")},
		}))
		return err
	}
	count := func() int {
		n, err := h.st.CountRuns(context.Background(), "p")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Two free slots: the pair fits.
	if err := replay(); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("%d stored runs after a replay, want 2", n)
	}
	// None free, or one: refused, and nothing is stored.
	if err := replay(); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("a replay over the quota: %v", err)
	}
	h2 := quotaHarness(t, config.Quotas{Default: config.QuotaLimits{MaxStoredRuns: 1}}, nil)
	h = h2
	if err := replay(); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("a replay with one free slot: %v", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("a refused replay stored %d runs", n)
	}
}
