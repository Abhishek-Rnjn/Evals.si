package runs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/harness/v1alpha1"
	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// fakeWorker: the target answers "right" for records whose input is "easy",
// alternates right/wrong per trial for "flaky", fails generation for "broken".
// exact-match compares against the reference "right"; "slow" waits until
// released or cancelled; test/count counts records.
type fakeWorker struct {
	generateCalls atomic.Int64
	taskCalls     atomic.Int64
	evaluateCalls atomic.Int64
	judgeTokens   int64
	mu            sync.Mutex
	flaky         map[string]int
	release       chan struct{}
}

func newFake() *fakeWorker {
	return &fakeWorker{flaky: map[string]int{}, release: make(chan struct{})}
}

func (f *fakeWorker) Describe(context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	return nil, nil
}

func text(s string) *evalsiv1alpha1.Content {
	return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: s}}
}

func (f *fakeWorker) Generate(_ context.Context, req *pluginv1alpha1.GenerateRequest) (*pluginv1alpha1.GenerateResponse, error) {
	f.generateCalls.Add(int64(len(req.GetRecords())))
	resp := &pluginv1alpha1.GenerateResponse{}
	for _, r := range req.GetRecords() {
		res := &pluginv1alpha1.GenerateResult{RecordId: r.GetId(), Usage: &evalsiv1alpha1.Usage{InputTokens: proto.Int64(10), OutputTokens: proto.Int64(5)}}
		switch r.GetInput().GetText() {
		case "broken":
			res.Error, res.Usage = "HTTP 500", nil
		case "flaky":
			f.mu.Lock()
			n := f.flaky[r.GetId()]
			f.flaky[r.GetId()]++
			f.mu.Unlock()
			res.Output = text(map[bool]string{true: "right", false: "wrong"}[n%2 == 0])
		default:
			res.Output = text("right")
		}
		resp.Results = append(resp.Results, res)
	}
	return resp, nil
}

// RunTask: "pass" tasks pass the checker, "fail" ones do not, "flaky" ones
// pass on even trials, and "crash" ones cannot run (an infrastructure error).
func (f *fakeWorker) RunTask(_ context.Context, req *pluginv1alpha1.RunTaskRequest, _ func(*harnessv1alpha1.TrajectoryEvent)) (*pluginv1alpha1.TaskResult, error) {
	f.taskCalls.Add(1)
	rec := proto.Clone(req.GetRecord()).(*evalsiv1alpha1.Record)
	in := rec.GetInput().GetText()
	if in == "crash" {
		return &pluginv1alpha1.TaskResult{Error: "environment setup failed"}, nil
	}
	passed := in == "pass" || in == "flaky" && req.GetTrial()%2 == 0
	rec.Output = text("done")
	rec.Usage = &evalsiv1alpha1.Usage{InputTokens: proto.Int64(100), OutputTokens: proto.Int64(10)}
	rec.Check = &evalsiv1alpha1.TaskCheck{Passed: passed}
	rec.Trajectory = &evalsiv1alpha1.Trajectory{Steps: []*evalsiv1alpha1.Step{{Type: evalsiv1alpha1.StepType_STEP_TYPE_TOOL, Name: "bash"}}}
	rec.Provenance = &evalsiv1alpha1.Provenance{Isolation: &evalsiv1alpha1.IsolationReport{Driver: "bwrap", Level: evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_NAMESPACED}}
	return &pluginv1alpha1.TaskResult{Record: rec}, nil
}

func (f *fakeWorker) LoadDataset(_ context.Context, req *pluginv1alpha1.LoadDatasetRequest) ([]*evalsiv1alpha1.Record, error) {
	path := req.GetSource().GetPath()
	if uri := req.GetSource().GetUri(); uri != "" {
		path = uri
	} else if !filepath.IsAbs(path) {
		return nil, errors.New("path must be absolute")
	}
	return []*evalsiv1alpha1.Record{{Input: text("easy"), Reference: text("right"), Metadata: map[string]*structpb.Value{"path": structpb.NewStringValue(path)}}}, nil
}

func (f *fakeWorker) Evaluate(ctx context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	f.evaluateCalls.Add(int64(len(req.GetRecords())))
	if req.GetEvaluator() == "test/slow" {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
	}
	resp := &pluginv1alpha1.EvaluateResponse{BatchId: req.GetBatchId()}
	for _, r := range req.GetRecords() {
		res := &evalsiv1alpha1.EvaluationResult{RecordId: r.GetId(), Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED}
		score := &evalsiv1alpha1.Score{Name: catalog.ShortName(req.GetEvaluator())}
		if req.GetEvaluator() == "builtin/task-success" {
			score.Value = &evalsiv1alpha1.Score_Passed{Passed: r.GetCheck().GetPassed()}
		} else if req.GetEvaluator() == "builtin/exact-match" {
			score.Value = &evalsiv1alpha1.Score_Passed{Passed: r.GetOutput().GetText() == r.GetReference().GetText()}
			score.Cost = &evalsiv1alpha1.Usage{InputTokens: proto.Int64(f.judgeTokens)}
		} else {
			score.Value = &evalsiv1alpha1.Score_Number{Number: 1}
		}
		res.Scores = []*evalsiv1alpha1.Score{score}
		resp.Results = append(resp.Results, res)
	}
	return resp, nil
}

func (f *fakeWorker) Reduce(_ context.Context, req *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	return &pluginv1alpha1.ReduceResponse{Scores: []*evalsiv1alpha1.Score{
		{Name: "count", Value: &evalsiv1alpha1.Score_Number{Number: float64(len(req.GetRecords()))}},
	}}, nil
}

func emptySchema() *structpb.Struct {
	s, _ := structpb.NewStruct(map[string]any{"type": "object", "properties": map[string]any{}})
	return s
}

func manifests() []*evalsiv1alpha1.EvaluatorManifest {
	return []*evalsiv1alpha1.EvaluatorManifest{
		{Name: "builtin/exact-match", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
			Outputs:      []*evalsiv1alpha1.MetricSpec{{Name: "exact-match", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED}},
			ParamsSchema: emptySchema()},
		{Name: "builtin/task-success", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
			Outputs:      []*evalsiv1alpha1.MetricSpec{{Name: "task-success", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED}},
			ParamsSchema: emptySchema()},
		{Name: "test/slow", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
			Outputs:      []*evalsiv1alpha1.MetricSpec{{Name: "slow", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER}},
			ParamsSchema: emptySchema()},
		{Name: "test/count", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_DATASET,
			Outputs:      []*evalsiv1alpha1.MetricSpec{{Name: "count", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER}},
			ParamsSchema: emptySchema()},
	}
}

type harness struct {
	m      *Manager
	st     *store.Store
	worker *fakeWorker
	dir    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db", "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return newHarnessWith(t, st, dir)
}

func newHarnessWith(t *testing.T, st *store.Store, dir string) *harness {
	t.Helper()
	w := newFake()
	opts := config.Evaluate{BatchSize: 2, Parallelism: 2, MaxRecords: 100}
	engine := evaluation.New(w, catalog.New(manifests()), nil, "", opts)
	datasets := filepath.Join(dir, "datasets")
	_ = os.MkdirAll(datasets, 0o700)
	m, err := New(context.Background(), st, w, engine, Options{DatasetsDir: datasets, MaxConcurrent: 2, Evaluate: opts})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return &harness{m: m, st: st, worker: w, dir: dir}
}

func inline(inputs ...string) *evalsiv1alpha1.DatasetSource {
	var recs []*evalsiv1alpha1.Record
	for i, in := range inputs {
		recs = append(recs, &evalsiv1alpha1.Record{Id: fmt.Sprintf("r%d", i), Input: text(in), Reference: text("right")})
	}
	return &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: recs}}}
}

func target() *evalsiv1alpha1.Target {
	return &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: "m", BaseUrl: "http://t/v1"}
}

func refs(names ...string) []*evalsiv1alpha1.EvaluatorRef {
	var out []*evalsiv1alpha1.EvaluatorRef
	for _, n := range names {
		out = append(out, &evalsiv1alpha1.EvaluatorRef{Ref: n})
	}
	return out
}

func (h *harness) create(t *testing.T, spec *evalsiv1alpha1.RunSpec) *evalsiv1alpha1.Run {
	t.Helper()
	resp, err := h.m.CreateRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Name: "n", Project: "p", Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetRun()
}

func (h *harness) wait(t *testing.T, id string) *evalsiv1alpha1.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.st.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		h.m.mu.Lock()
		_, active := h.m.active[id]
		h.m.mu.Unlock()
		if terminal(run.GetStatus()) && !active {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish", id)
	return nil
}

func summary(run *evalsiv1alpha1.Run, metric string) *evalsiv1alpha1.MetricSummary {
	for _, s := range run.GetSummaries() {
		if s.GetMetric() == metric {
			return s
		}
	}
	return nil
}

func TestRunWithTrialsGatesAndFailures(t *testing.T) {
	h := newHarness(t)
	run := h.create(t, &evalsiv1alpha1.RunSpec{
		Target:     target(),
		Dataset:    inline("easy", "flaky", "broken"),
		Evaluators: refs("exact-match", "test/count"),
		Trials:     2,
		Gates: []*evalsiv1alpha1.Gate{
			{Metric: "exact-match", Min: proto.Float64(0.5)},
			{Metric: "exact-match.pass^2", Min: proto.Float64(0.9)},
		},
	})
	if run.GetProgress().GetTotal() != 2*(3+3+1) {
		t.Errorf("total tasks = %d", run.GetProgress().GetTotal())
	}
	final := h.wait(t, run.GetId())
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED {
		t.Fatalf("status = %v (%s), want FAILED because pass^2 is 0.5", final.GetStatus(), final.GetError())
	}
	if final.GetProgress().GetDone() != final.GetProgress().GetTotal() {
		t.Errorf("progress = %v", final.GetProgress())
	}
	em := summary(final, "exact-match")
	// Scored: easy x2 right, flaky right then wrong; broken errored twice.
	if em.GetN() != 4 || em.GetMean() != 0.75 || em.GetErrors() != 2 || em.GetCi().GetMethod() != "clustered-t" {
		t.Errorf("exact-match = %v", em)
	}
	if s := summary(final, "exact-match.pass@2"); s.GetMean() != 1 {
		t.Errorf("pass@2 = %v", s)
	}
	if s := summary(final, "exact-match.pass^2"); s.GetMean() != 0.5 {
		t.Errorf("pass^2 = %v", s)
	}
	if s := summary(final, "count"); s.GetMean() != 2 {
		t.Errorf("count = %v", s)
	}
	if g := final.GetGates(); !g[0].GetPassed() || g[1].GetPassed() {
		t.Errorf("gates = %v", g)
	}
	if got := tokens(final.GetTargetUsage()); got != 4*15 {
		t.Errorf("target tokens = %d, want 60", got)
	}
	if h.worker.generateCalls.Load() != 6 {
		t.Errorf("generate calls = %d", h.worker.generateCalls.Load())
	}

	page, err := h.m.ListRunResults(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: run.GetId(), PageSize: 4}))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Msg.GetResults()) != 4 || page.Msg.GetNextPageToken() == "" || page.Msg.GetRecords()[0].GetOutput().GetText() != "right" {
		t.Errorf("results page = %v", page.Msg)
	}
	if !strings.HasPrefix(page.Msg.GetResults()[2].GetReason(), "target failed") {
		t.Errorf("broken record result = %v", page.Msg.GetResults()[2])
	}
}

func TestWatchStreamsUntilDone(t *testing.T) {
	h := newHarness(t)
	run := h.create(t, &evalsiv1alpha1.RunSpec{Dataset: inline("easy", "easy"), Evaluators: refs("test/slow")})
	ch := make(chan *evalsiv1alpha1.WatchRunResponse, 64)
	sub := make(chan struct{})
	go func() {
		h.m.mu.Lock()
		a := h.m.active[run.GetId()]
		a.subs[ch] = true
		h.m.mu.Unlock()
		close(sub)
	}()
	<-sub
	close(h.worker.release)
	final := h.wait(t, run.GetId())
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("status = %v", final.GetStatus())
	}
	var results, progress, terminalSeen int
	for ev := range ch {
		switch {
		case ev.GetResult() != nil:
			results++
		case ev.GetProgress() != nil:
			progress++
		case terminal(ev.GetRun().GetStatus()):
			terminalSeen++
		}
	}
	if results != 2 || progress == 0 || terminalSeen != 1 {
		t.Errorf("results=%d progress=%d terminal=%d", results, progress, terminalSeen)
	}
}

func TestCancelThenResumeSkipsFinishedWork(t *testing.T) {
	h := newHarness(t)
	run := h.create(t, &evalsiv1alpha1.RunSpec{
		Target: target(), Dataset: inline("easy", "easy", "easy"), Evaluators: refs("exact-match", "test/slow"),
	})
	// Wait until exact-match is done and test/slow is blocked.
	deadline := time.Now().Add(5 * time.Second)
	for {
		keys, _ := h.st.ResultKeys(context.Background(), run.GetId())
		if len(keys) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exact-match results never stored")
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := h.m.CancelRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CancelRunRequest{Id: run.GetId()}))
	if err != nil || resp.Msg.GetRun().GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED {
		t.Fatalf("cancel = %v, %v", resp, err)
	}
	gen, evals := h.worker.generateCalls.Load(), h.worker.evaluateCalls.Load()
	close(h.worker.release)
	if _, err := h.m.ResumeRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.ResumeRunRequest{Id: run.GetId()})); err != nil {
		t.Fatal(err)
	}
	final := h.wait(t, run.GetId())
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("status = %v (%s)", final.GetStatus(), final.GetError())
	}
	if h.worker.generateCalls.Load() != gen {
		t.Errorf("resume regenerated outputs: %d -> %d", gen, h.worker.generateCalls.Load())
	}
	if added := h.worker.evaluateCalls.Load() - evals; added != 3 {
		t.Errorf("resume evaluated %d records, want only the 3 slow ones", added)
	}
	if _, err := h.m.ResumeRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.ResumeRunRequest{Id: run.GetId()})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("resuming a finished run: %v", err)
	}
}

func TestBudgetStopsRun(t *testing.T) {
	h := newHarness(t)
	h.worker.judgeTokens = 100
	run := h.create(t, &evalsiv1alpha1.RunSpec{
		Dataset: inline("easy", "easy", "easy"), Evaluators: refs("exact-match"),
		Budget: &evalsiv1alpha1.Budget{MaxJudgeTokens: 150},
	})
	final := h.wait(t, run.GetId())
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR || !strings.Contains(final.GetError(), "max_judge_tokens") {
		t.Fatalf("status = %v, error = %q", final.GetStatus(), final.GetError())
	}
}

func TestRestartMarksUnfinishedRunsResumable(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stale := &evalsiv1alpha1.Run{Id: "run-stale", CreatedAt: timestamppb.Now(), Status: evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING,
		Spec: &evalsiv1alpha1.RunSpec{Dataset: inline("easy"), Evaluators: refs("exact-match")}}
	if err := st.CreateRun(context.Background(), stale, []*evalsiv1alpha1.Record{{Id: "r0", Input: text("easy"), Output: text("right"), Reference: text("right")}}); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, st, dir)
	got, _ := st.GetRun(context.Background(), "run-stale")
	if got.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR || !strings.Contains(got.GetError(), "resume") {
		t.Fatalf("stale run = %v", got)
	}
	if _, err := h.m.ResumeRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.ResumeRunRequest{Id: "run-stale"})); err != nil {
		t.Fatal(err)
	}
	if final := h.wait(t, "run-stale"); final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("resumed run = %v (%s)", final.GetStatus(), final.GetError())
	}
}

func TestDatasetPaths(t *testing.T) {
	h := newHarness(t)
	datasets := filepath.Join(h.dir, "datasets")
	if err := os.WriteFile(filepath.Join(datasets, "qa.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "secret.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(filepath.Join(h.dir, "secret.jsonl"), filepath.Join(datasets, "link.jsonl"))
	path := func(p string) *evalsiv1alpha1.DatasetSource {
		return &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Path{Path: p}}
	}
	run := h.create(t, &evalsiv1alpha1.RunSpec{Dataset: path("qa.jsonl"), Evaluators: refs("test/count")})
	records, _ := h.st.Records(context.Background(), run.GetId())
	if got := records[0].GetMetadata()["path"].GetStringValue(); got != filepath.Join(mustEval(t, datasets), "qa.jsonl") {
		t.Errorf("worker got path %q", got)
	}
	for p, want := range map[string]string{
		"../secret.jsonl": "outside",
		"link.jsonl":      "outside",
		"/etc/passwd":     "relative",
		"missing.jsonl":   "missing.jsonl",
	} {
		_, err := h.m.CreateRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{
			Spec: &evalsiv1alpha1.RunSpec{Dataset: path(p), Evaluators: refs("test/count")},
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
			t.Errorf("path %q: err = %v", p, err)
		}
	}
}

func TestDatasetURIs(t *testing.T) {
	h := newHarness(t)
	datasets := mustEval(t, filepath.Join(h.dir, "datasets"))
	for _, p := range []string{filepath.Join(datasets, "run.eval"), filepath.Join(h.dir, "secret.eval")} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	uri := func(u string) *evalsiv1alpha1.DatasetSource {
		return &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Uri{Uri: u}}
	}
	for in, want := range map[string]string{
		"inspect://run.eval?scores=false": "inspect://" + filepath.Join(datasets, "run.eval") + "?scores=false",
		"hf://openai/gsm8k?split=test":    "hf://openai/gsm8k?split=test",
	} {
		run := h.create(t, &evalsiv1alpha1.RunSpec{Dataset: uri(in), Evaluators: refs("test/count")})
		records, _ := h.st.Records(context.Background(), run.GetId())
		if got := records[0].GetMetadata()["path"].GetStringValue(); got != want {
			t.Errorf("uri %q reached the worker as %q, want %q", in, got, want)
		}
	}
	for u, want := range map[string]string{
		"inspect://../secret.eval": "outside",
		"inspect:///etc/passwd":    "relative",
		"/etc/passwd":              "scheme",
		"://run.eval":              "scheme",
	} {
		_, err := h.m.CreateRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{
			Spec: &evalsiv1alpha1.RunSpec{Dataset: uri(u), Evaluators: refs("test/count")},
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
			t.Errorf("uri %q: err = %v", u, err)
		}
	}
}

func mustEval(t *testing.T, p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateRunValidation(t *testing.T) {
	h := newHarness(t)
	cases := map[string]*evalsiv1alpha1.RunSpec{
		"spec is required":    nil,
		"unknown evaluator":   {Dataset: inline("easy"), Evaluators: refs("nope")},
		"needs a target":      {Dataset: inline("easy"), Evaluators: refs("exact-match"), Trials: 3},
		"gate needs":          {Dataset: inline("easy"), Evaluators: refs("exact-match"), Gates: []*evalsiv1alpha1.Gate{{Metric: "x"}}},
		"base_url":            {Target: &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: "m"}, Dataset: inline("easy"), Evaluators: refs("exact-match")},
		"no records":          {Dataset: inline(), Evaluators: refs("exact-match")},
		"needs one of inline": {Dataset: &evalsiv1alpha1.DatasetSource{}, Evaluators: refs("exact-match")},
	}
	for want, spec := range cases {
		_, err := h.m.CreateRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Spec: spec}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestListAndCompare(t *testing.T) {
	h := newHarness(t)
	spec := func(inputs ...string) *evalsiv1alpha1.RunSpec {
		return &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline(inputs...), Evaluators: refs("exact-match")}
	}
	base := h.create(t, spec("flaky", "flaky", "easy", "flaky"))
	h.wait(t, base.GetId())
	cand := h.create(t, spec("easy", "easy", "easy", "easy"))
	h.wait(t, cand.GetId())
	list, err := h.m.ListRuns(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{Project: "p"}))
	if err != nil || len(list.Msg.GetRuns()) != 2 || list.Msg.GetRuns()[0].GetSpec() != nil {
		t.Fatalf("ListRuns = %v, %v", list, err)
	}
	cmp, err := h.m.CompareRuns(context.Background(), connect.NewRequest(&evalsiv1alpha1.CompareRunsRequest{
		BaselineRunId: base.GetId(), CandidateRunId: cand.GetId(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := cmp.Msg.GetComparisons()[0]
	// Baseline: flaky records answer "right" on their first call, so all four pass.
	if c.GetMetric() != "exact-match" || c.GetPairedN() != 4 || c.GetCandidateMean() != 1 {
		t.Fatalf("comparison = %v", c)
	}
	if _, err := h.m.CompareRuns(context.Background(), connect.NewRequest(&evalsiv1alpha1.CompareRunsRequest{BaselineRunId: "x", CandidateRunId: cand.GetId()})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown run: %v", err)
	}
}

func agentSpec(inputs ...string) *evalsiv1alpha1.RunSpec {
	return &evalsiv1alpha1.RunSpec{
		Target:      target(),
		Harness:     &evalsiv1alpha1.Harness{Kind: &evalsiv1alpha1.Harness_Builtin{Builtin: &evalsiv1alpha1.BuiltinHarness{MaxSteps: 10}}},
		Environment: &evalsiv1alpha1.Environment{Image: "python:3.12-slim"},
		Dataset:     inline(inputs...),
		Evaluators:  refs("task-success"),
		Trials:      2,
		Gates:       []*evalsiv1alpha1.Gate{{Metric: "task-success.pass^2", Min: proto.Float64(0.3)}},
	}
}

func TestAgentRunsDriveTasksThroughTheWorker(t *testing.T) {
	h := newHarness(t)
	run := h.wait(t, h.create(t, agentSpec("pass", "fail", "flaky", "crash")).GetId())
	if run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("status %v: %s", run.GetStatus(), run.GetError())
	}
	if h.worker.taskCalls.Load() != 8 || h.worker.generateCalls.Load() != 0 {
		t.Errorf("tasks %d, generate %d", h.worker.taskCalls.Load(), h.worker.generateCalls.Load())
	}
	// pass and flaky trial 0 of 3 scorable tasks per trial; crash is an error, not a failure.
	if s := summary(run, "task-success"); s.GetMean() != 0.5 || s.GetN() != 6 {
		t.Errorf("task-success %v", s)
	}
	if s := summary(run, "task-success.pass^2"); s.GetMean() < 0.33 || s.GetMean() > 0.34 {
		t.Errorf("pass^2 %v", s)
	}
	if run.GetTargetUsage().GetInputTokens() != 600 {
		t.Errorf("usage %v", run.GetTargetUsage())
	}
	outputs, err := h.st.Outputs(context.Background(), run.GetId())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outputs {
		if o.Record == nil {
			if o.Error != "environment setup failed" {
				t.Errorf("crash output: %+v", o)
			}
			continue
		}
		p := o.Record.GetProvenance()
		if p.GetRun().GetRunId() != run.GetId() || p.GetIsolation().GetDriver() != "bwrap" || o.Record.GetCheck() == nil {
			t.Errorf("stored record lost its provenance or check: %v", o.Record)
		}
	}
	results, err := h.st.Results(context.Background(), run.GetId(), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	errored := 0
	for _, r := range results {
		if r.Result.GetOutcome() == evalsiv1alpha1.Outcome_OUTCOME_ERROR {
			errored++
		}
	}
	if errored != 2 {
		t.Errorf("errored results = %d, want the crash task twice", errored)
	}
	// Resuming re-runs nothing that finished.
	if _, err := h.m.ResumeRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.ResumeRunRequest{Id: run.GetId()})); err == nil {
		h.wait(t, run.GetId())
		if h.worker.taskCalls.Load() != 8 {
			t.Errorf("resume re-ran tasks: %d", h.worker.taskCalls.Load())
		}
	}
}

func TestAgentRunValidation(t *testing.T) {
	h := newHarness(t)
	h.m.opts.Agents = config.Agents{TrustedCommands: [][]string{{"npx", "mcp-crm"}}, TrustedPython: []string{"acme.check:parse"}}
	cases := map[string]func(*evalsiv1alpha1.RunSpec){
		"builtin needs a model": func(s *evalsiv1alpha1.RunSpec) { s.Target = nil },
		"cli needs an environment": func(s *evalsiv1alpha1.RunSpec) {
			s.Target.Agent = &evalsiv1alpha1.AgentTarget{Kind: &evalsiv1alpha1.AgentTarget_Cli{Cli: &evalsiv1alpha1.CLIAgent{Command: []string{"agent"}}}}
			s.Environment = nil
		},
		"untrusted mcp command": func(s *evalsiv1alpha1.RunSpec) {
			s.GetHarness().GetBuiltin().Tools = &evalsiv1alpha1.Tools{Mcp: []*evalsiv1alpha1.MCPServer{{Name: "x", Command: []string{"rm", "-rf", "/"}}}}
		},
		"untrusted python harness": func(s *evalsiv1alpha1.RunSpec) {
			s.Harness = &evalsiv1alpha1.Harness{Kind: &evalsiv1alpha1.Harness_External{External: &evalsiv1alpha1.ExternalHarness{Kind: &evalsiv1alpha1.ExternalHarness_Python{Python: "os:system"}}}}
		},
		"untrusted checker parser": func(s *evalsiv1alpha1.RunSpec) {
			s.Environment.Checker = &evalsiv1alpha1.Checker{Command: []string{"pytest"}, Parser: "os:system"}
		},
		"recording outside datasets_dir": func(s *evalsiv1alpha1.RunSpec) {
			s.GetHarness().GetBuiltin().Recording = &evalsiv1alpha1.Recording{Mode: "record", Dir: "../out"}
		},
		"bad network": func(s *evalsiv1alpha1.RunSpec) {
			s.Environment.Sandbox = &evalsiv1alpha1.SandboxPolicy{Network: "allowlist"}
		},
	}
	for name, mutate := range cases {
		spec := agentSpec("pass")
		mutate(spec)
		_, err := h.m.CreateRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Spec: spec}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	ok := map[string]func(*evalsiv1alpha1.RunSpec){
		"trusted mcp command": func(s *evalsiv1alpha1.RunSpec) {
			s.GetHarness().GetBuiltin().Tools = &evalsiv1alpha1.Tools{Mcp: []*evalsiv1alpha1.MCPServer{{Name: "crm", Command: []string{"npx", "mcp-crm"}}}}
		},
		"adapter parser": func(s *evalsiv1alpha1.RunSpec) {
			s.Environment.Checker = &evalsiv1alpha1.Checker{Command: []string{"pytest"}, Parser: "evalsi_swebench:parse"}
		},
		"trusted parser": func(s *evalsiv1alpha1.RunSpec) {
			s.Environment.Checker = &evalsiv1alpha1.Checker{Command: []string{"pytest"}, Parser: "acme.check:parse"}
		},
		"a2a agent": func(s *evalsiv1alpha1.RunSpec) {
			s.Target = &evalsiv1alpha1.Target{Agent: &evalsiv1alpha1.AgentTarget{Kind: &evalsiv1alpha1.AgentTarget_A2A{A2A: &evalsiv1alpha1.A2AAgent{Url: "http://agent"}}}}
		},
		"recording": func(s *evalsiv1alpha1.RunSpec) {
			s.GetHarness().GetBuiltin().Recording = &evalsiv1alpha1.Recording{Mode: "record", Dir: "tapes"}
		},
	}
	for name, mutate := range ok {
		spec := agentSpec("pass")
		mutate(spec)
		if _, err := h.m.CreateRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Spec: spec})); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	spec, err := h.m.workerSpec(func() *evalsiv1alpha1.RunSpec {
		s := agentSpec("pass")
		s.GetHarness().GetBuiltin().Recording = &evalsiv1alpha1.Recording{Mode: "replay", Dir: "tapes"}
		return s
	}())
	if err != nil || !filepath.IsAbs(spec.GetHarness().GetBuiltin().GetRecording().GetDir()) {
		t.Errorf("worker spec recording dir: %v %v", spec, err)
	}
}

func TestPromotionRescoringAndShadowReplay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	run := h.wait(t, h.create(t, &evalsiv1alpha1.RunSpec{
		Target: target(), Dataset: inline("easy", "flaky", "broken"), Evaluators: refs("exact-match"), Trials: 2,
	}).GetId())

	// Promote the records that failed in some trial (flaky in trial 1); broken errored.
	resp, err := h.m.PromoteResults(ctx, connect.NewRequest(&evalsiv1alpha1.PromoteResultsRequest{
		RunId: run.GetId(), Dataset: "regressions", When: `scores["exact-match"] < 1.0 || errored`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetPromoted() != 2 || resp.Msg.GetPath() != "promoted/p/regressions.jsonl" {
		t.Fatalf("promoted %v", resp.Msg)
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, "datasets", "promoted", "p", "regressions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"promoted_from"`) || strings.Contains(lines[0], `"output"`) {
		t.Fatalf("promoted rows:\n%s", raw)
	}
	for _, bad := range []*evalsiv1alpha1.PromoteResultsRequest{
		{RunId: run.GetId(), Dataset: "../x", When: "true"},
		{RunId: run.GetId(), Dataset: "x"},
		{RunId: run.GetId(), Dataset: "x", When: "scores"},
	} {
		if _, err := h.m.PromoteResults(ctx, connect.NewRequest(bad)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%v: %v", bad, err)
		}
	}

	// Re-score the first run's outputs with another evaluator, without the target.
	calls := h.worker.generateCalls.Load()
	rescored := h.wait(t, h.create(t, &evalsiv1alpha1.RunSpec{
		Dataset:    &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Run{Run: &evalsiv1alpha1.RunOutputs{RunId: run.GetId(), AllTrials: true}}},
		Evaluators: refs("exact-match"),
	}).GetId())
	if h.worker.generateCalls.Load() != calls || rescored.GetRecords() != 4 {
		t.Fatalf("re-scoring ran the target or lost records: %d records", rescored.GetRecords())
	}
	if s := summary(rescored, "exact-match"); s.GetMean() != 0.75 {
		t.Errorf("re-scored exact-match %v", s)
	}

	// Shadow replay: recorded production answers against the candidate.
	recorded := &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: []*evalsiv1alpha1.Record{
		{Id: "a", Input: text("easy"), Output: text("wrong"), Reference: text("right")},
		{Id: "b", Input: text("easy"), Output: text("right"), Reference: text("right")},
	}}}}
	shadow, err := h.m.CreateShadowReplay(ctx, connect.NewRequest(&evalsiv1alpha1.CreateShadowReplayRequest{
		Name: "candidate-v2", Project: "p",
		Candidate: &evalsiv1alpha1.RunSpec{Target: target(), Dataset: recorded, Evaluators: refs("exact-match"),
			Gates: []*evalsiv1alpha1.Gate{{Metric: "exact-match", Min: proto.Float64(0.9)}}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	base := h.wait(t, shadow.Msg.GetBaseline().GetId())
	cand := h.wait(t, shadow.Msg.GetCandidate().GetId())
	if base.GetSpec().GetTarget() != nil || len(base.GetGates()) != 0 || base.GetName() != "candidate-v2-baseline" {
		t.Errorf("baseline %v", base)
	}
	if summary(base, "exact-match").GetMean() != 0.5 || summary(cand, "exact-match").GetMean() != 1 {
		t.Errorf("baseline %v candidate %v", summary(base, "exact-match"), summary(cand, "exact-match"))
	}
	cmp, err := h.m.CompareRuns(ctx, connect.NewRequest(&evalsiv1alpha1.CompareRunsRequest{BaselineRunId: base.GetId(), CandidateRunId: cand.GetId()}))
	if err != nil || cmp.Msg.GetComparisons()[0].GetPairedN() != 2 || cmp.Msg.GetComparisons()[0].GetDiff() != 0.5 {
		t.Fatalf("compare %v %v", cmp, err)
	}
	if _, err := h.m.CreateShadowReplay(ctx, connect.NewRequest(&evalsiv1alpha1.CreateShadowReplayRequest{
		Candidate: &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline("easy"), Evaluators: refs("exact-match")},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a dataset without recorded outputs: %v", err)
	}
}

func TestTraceDatasets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now()
	for i, svc := range []string{"support", "support", "billing"} {
		id := fmt.Sprintf("trace-%d", i)
		sum := &evalsiv1alpha1.TraceSummary{TraceId: id, Service: svc, Name: "turn", Project: "p", Error: i == 1,
			StartTime: timestamppb.New(now.Add(-time.Duration(i) * time.Hour))}
		rec := &evalsiv1alpha1.Record{Id: id, Input: text("easy"), Output: text("recorded"), Reference: text("right")}
		if err := h.st.PutTrace(ctx, sum, rec); err != nil {
			t.Fatal(err)
		}
	}
	other := &evalsiv1alpha1.TraceSummary{TraceId: "elsewhere", Service: "support", Project: "q", StartTime: timestamppb.Now()}
	if err := h.st.PutTrace(ctx, other, &evalsiv1alpha1.Record{Id: "elsewhere", Input: text("easy")}); err != nil {
		t.Fatal(err)
	}
	src := func(q *evalsiv1alpha1.TraceQuery) *evalsiv1alpha1.DatasetSource {
		return &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Traces{Traces: q}}
	}
	cases := []struct {
		q    *evalsiv1alpha1.TraceQuery
		want int64
	}{
		{&evalsiv1alpha1.TraceQuery{}, 3},
		{&evalsiv1alpha1.TraceQuery{Service: "support"}, 2},
		{&evalsiv1alpha1.TraceQuery{Filter: `error`}, 1},
		{&evalsiv1alpha1.TraceQuery{Lookback: durationpb.New(90 * time.Minute)}, 2},
	}
	for _, c := range cases {
		run := h.wait(t, h.create(t, &evalsiv1alpha1.RunSpec{Dataset: src(c.q), Evaluators: refs("exact-match")}).GetId())
		if run.GetRecords() != c.want {
			t.Errorf("%v: %d records, want %d", c.q, run.GetRecords(), c.want)
		}
	}
	_, err := h.m.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "p", Spec: &evalsiv1alpha1.RunSpec{
		Dataset: src(&evalsiv1alpha1.TraceQuery{Filter: "nope("}), Evaluators: refs("exact-match")}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad filter: %v", err)
	}
	records, err := h.st.Records(ctx, h.wait(t, h.create(t, &evalsiv1alpha1.RunSpec{Dataset: src(&evalsiv1alpha1.TraceQuery{Service: "billing"}), Evaluators: refs("exact-match")}).GetId()).GetId())
	if err != nil || records[0].GetProvenance().GetTrace().GetTraceId() != "trace-2" || records[0].GetMetadata()["trace"] == nil {
		t.Errorf("trace provenance %v %v", records, err)
	}
}
