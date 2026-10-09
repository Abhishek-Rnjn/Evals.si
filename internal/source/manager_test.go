package source

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

var epoch = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// fakeStore is a trace store: traces by ID, listed by start time.
type fakeStore struct {
	mu        sync.Mutex
	infos     []Info
	pageSize  int
	lists     int
	listErr   error
	written   []Score
	priorSeen []map[[2]string]store.SourceWrite
}

func (f *fakeStore) set(infos ...Info) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos = infos
}

func (f *fakeStore) List(_ context.Context, req ListRequest) (ListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		return ListResult{}, f.listErr
	}
	var all []Info
	for _, in := range f.infos {
		if !in.Started.Before(req.Since) {
			all = append(all, in)
		}
	}
	size := f.pageSize
	if size <= 0 {
		size = len(all) + 1
	}
	start := 0
	if req.Cursor != "" {
		fmt.Sscanf(req.Cursor, "%d", &start)
	}
	end := min(start+size, len(all))
	res := ListResult{Infos: all[start:end]}
	if end < len(all) {
		res.Next = fmt.Sprint(end)
	}
	return res, nil
}

func (f *fakeStore) Fetch(_ context.Context, infos []Info) ([]ingest.Trace, error) {
	var out []ingest.Trace
	for _, in := range infos {
		out = append(out, fakeTrace(in.ID, in.Started))
	}
	return out, nil
}

func (f *fakeStore) WriteBack(_ context.Context, scores []Score, prior map[[2]string]store.SourceWrite) ([]store.SourceWrite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.priorSeen = append(f.priorSeen, prior)
	var out []store.SourceWrite
	for _, s := range scores {
		f.written = append(f.written, s)
		id := "a-" + s.TraceID
		if w, ok := prior[[2]string{s.TraceID, s.Metric}]; ok {
			id = w.RemoteID
		}
		out = append(out, store.SourceWrite{TraceID: s.TraceID, Metric: s.Metric, RemoteID: id, Digest: s.Digest(), Written: epoch})
	}
	return out, nil
}

func fakeTrace(id string, started time.Time) ingest.Trace {
	raw := make([]byte, 16)
	copy(raw, id)
	sp := &tracepb.Span{
		TraceId: raw, SpanId: []byte{1, 2, 3, 4, 5, 6, 7, 8}, Name: "agent",
		StartTimeUnixNano: uint64(started.UnixNano()), EndTimeUnixNano: uint64(started.Add(time.Second).UnixNano()),
	}
	return ingest.Trace{TraceID: hex.EncodeToString(raw), Spans: []ingest.Span{{Span: sp, Resource: ingest.Attrs{AttrSourceTraceID: id}}}}
}

type call struct {
	ids      []string
	policies []string
	labels   map[string]string
	project  string
}

type fakeIngester struct {
	mu    sync.Mutex
	calls []call
	err   error
	block chan struct{} // when set, IngestBatchContext waits on it or ctx
	// Called after each successful call with the number of calls so far.
	onCall func(n int)
}

func (f *fakeIngester) IngestBatchContext(ctx context.Context, traces []ingest.Trace, policies []string) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	if f.err != nil {
		f.mu.Unlock()
		return f.err
	}
	c := call{policies: policies}
	for _, t := range traces {
		c.ids = append(c.ids, sourceID(t))
		c.project = t.Project
		c.labels = t.Spans[0].Labels
	}
	f.calls = append(f.calls, c)
	n, hook := len(f.calls), f.onCall
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return nil
}

func (f *fakeIngester) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.ids...)
	}
	return out
}

type harness struct {
	m   *Manager
	st  *store.Store
	fs  *fakeStore
	ing *fakeIngester
	now time.Time
	src *evalsiv1alpha1.TraceSource
	r   *runner
}

func newHarness(t *testing.T, mutate func(*evalsiv1alpha1.TraceSource)) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{st: st, fs: &fakeStore{}, ing: &fakeIngester{}, now: epoch}
	h.src = &evalsiv1alpha1.TraceSource{
		Name: "studio", Project: "p", Connector: "fake", Endpoint: "http://store",
		Policies: []string{"quality"}, TraceLabels: map[string]string{"team": "evals"},
		MaxTraceDuration: durationpb.New(30 * time.Minute), MaxInProgressAge: durationpb.New(time.Hour),
		Poll:      &evalsiv1alpha1.Poll{Interval: durationpb.New(time.Second)},
		UpdatedAt: timestamppb.New(epoch),
	}
	if mutate != nil {
		mutate(h.src)
	}
	h.m = New(st, h.ing, Options{
		Factories: map[string]Factory{"fake": func(*evalsiv1alpha1.TraceSource, string, *http.Client) (Connector, error) { return h.fs, nil }},
		Now:       func() time.Time { return h.now }, SyncEvery: 10 * time.Millisecond, PageSize: 10, MaxRate: 1_000_000,
	})
	if err := st.PutSource(context.Background(), h.src); err != nil {
		t.Fatal(err)
	}
	h.r = h.m.newRunner(h.src)
	return h
}

func (h *harness) cycle(t *testing.T) error {
	t.Helper()
	return h.r.cycle(context.Background(), h.fs)
}

func (h *harness) state(t *testing.T) store.SourceState {
	t.Helper()
	st, err := h.st.SourceState(context.Background(), "p", "studio")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func info(id string, started time.Time) Info { return Info{ID: id, Started: started, Digest: "d1"} }

func TestTailingIngestsOnceAndLabels(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.cycle(t); err != nil { // first cycle: no backfill, so only what follows
		t.Fatal(err)
	}
	if st := h.state(t); !st.Watermark.Equal(epoch) {
		t.Fatalf("a tail-only source starts at now; watermark %v", st.Watermark)
	}
	h.now = epoch.Add(time.Minute)
	h.fs.set(info("tr-a", epoch.Add(10*time.Second)), info("tr-b", epoch.Add(20*time.Second)))
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if got := h.ing.ids(); len(got) != 2 {
		t.Fatalf("ingested %v", got)
	}
	c := h.ing.calls[0]
	if c.project != "p" || c.labels[LabelSource] != "studio" || c.labels["team"] != "evals" || len(c.policies) != 1 || c.policies[0] != "quality" {
		t.Fatalf("not labelled as the source's: %+v", c)
	}
	// Reading the same traces again scores nothing twice...
	h.now = epoch.Add(2 * time.Minute)
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if got := h.ing.ids(); len(got) != 2 {
		t.Fatalf("re-ingested unchanged traces: %v", got)
	}
	// ...but a trace whose content changed is scored again.
	h.fs.set(info("tr-a", epoch.Add(10*time.Second)), Info{ID: "tr-b", Started: epoch.Add(20 * time.Second), Digest: "d2"})
	h.now = epoch.Add(3 * time.Minute)
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if got := h.ing.ids(); len(got) != 3 || got[2] != "tr-b" {
		t.Fatalf("a changed trace was not re-ingested: %v", got)
	}
	if st := h.state(t); st.Pulled != 3 {
		t.Fatalf("pulled %d", st.Pulled)
	}
}

// A trace is logged when it ends but filtered by when it started, so the
// watermark must stay maxTraceDuration behind the cycle.
func TestWatermarkStaysAHorizonBehind(t *testing.T) {
	h := newHarness(t, nil)
	_ = h.cycle(t)
	// A 20-minute run starts at +1m and is logged at +21m. Another trace that
	// started later was logged earlier and moved the watermark.
	h.now = epoch.Add(10 * time.Minute)
	h.fs.set(info("tr-short", epoch.Add(5*time.Minute)))
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	// The later trace was seen, but the watermark must not pass the horizon
	// (now - 30m): a 20-minute run that started earlier is still to be logged.
	if wm := h.state(t).Watermark; wm.After(epoch) {
		t.Fatalf("watermark %v moved past the horizon on a trace seen at +5m (now %v)", wm, h.now)
	}
	h.now = epoch.Add(21 * time.Minute)
	h.fs.set(info("tr-long", epoch.Add(time.Minute)), info("tr-short", epoch.Add(5*time.Minute)))
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if got := h.ing.ids(); len(got) != 2 || got[1] != "tr-long" {
		t.Fatalf("the long trace, logged after a later one, was missed: %v", got)
	}
	// Well past the horizon the watermark follows time.
	h.now = epoch.Add(2 * time.Hour)
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if wm := h.state(t).Watermark; !wm.Equal(h.now.Add(-30 * time.Minute)) {
		t.Fatalf("watermark %v, want now-30m", wm)
	}
}

func TestInProgressTracesAreDeferred(t *testing.T) {
	h := newHarness(t, nil)
	_ = h.cycle(t)
	h.now = epoch.Add(2 * time.Hour)
	running := epoch.Add(90 * time.Minute)
	h.fs.set(Info{ID: "tr-run", Started: running, InProgress: true, Digest: "d1"}, info("tr-done", epoch.Add(100*time.Minute)))
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if got := h.ing.ids(); len(got) != 1 || got[0] != "tr-done" {
		t.Fatalf("ingested %v; the running trace must wait", got)
	}
	st := h.state(t)
	if st.Deferred != 1 || st.Watermark.After(running) {
		t.Fatalf("deferred %d, watermark %v is past the running trace's start %v", st.Deferred, st.Watermark, running)
	}
	// It finishes.
	h.now = epoch.Add(125 * time.Minute)
	h.fs.set(info("tr-run", running), info("tr-done", epoch.Add(100*time.Minute)))
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if got := h.ing.ids(); len(got) != 2 || got[1] != "tr-run" {
		t.Fatalf("ingested %v", got)
	}
	// One that never finishes is scored as it is after maxInProgressAge.
	h.now = epoch.Add(5 * time.Hour)
	h.fs.set(Info{ID: "tr-stuck", Started: epoch.Add(3 * time.Hour), InProgress: true, Digest: "d1"})
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if got := h.ing.ids(); got[len(got)-1] != "tr-stuck" {
		t.Fatalf("a stuck trace froze the source: %v", got)
	}
}

func TestNoCommitUnlessIngestSucceeds(t *testing.T) {
	h := newHarness(t, nil)
	_ = h.cycle(t)
	h.now = epoch.Add(2 * time.Hour)
	h.fs.set(info("tr-a", epoch.Add(time.Hour)))
	before := h.state(t)
	h.ing.err = errors.New("store is down")
	if err := h.cycle(t); err == nil {
		t.Fatal("a failed ingest was reported as success")
	}
	after := h.state(t)
	if !after.Watermark.Equal(before.Watermark) || after.Pulled != 0 {
		t.Fatalf("state moved on a failed ingest: %+v", after)
	}
	if seen, _ := h.st.SeenTraces(context.Background(), "p", "studio", []string{"tr-a"}); len(seen) != 0 {
		t.Fatal("a trace that was not ingested was marked seen")
	}
	// It is retried and succeeds.
	h.ing.err = nil
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if got := h.ing.ids(); len(got) != 1 {
		t.Fatalf("ingested %v", got)
	}
}

// A leader that steps down while an ingest is blocked must not commit.
func TestStepDownMidCycleCommitsNothing(t *testing.T) {
	h := newHarness(t, nil)
	_ = h.cycle(t)
	h.now = epoch.Add(2 * time.Hour)
	h.fs.set(info("tr-a", epoch.Add(time.Hour)))
	h.ing.block = make(chan struct{})
	before := h.state(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.r.cycle(ctx, h.fs) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cycle ignored its context")
	}
	if after := h.state(t); !after.Watermark.Equal(before.Watermark) || after.Pulled != 0 {
		t.Fatalf("a cancelled cycle committed: %+v", after)
	}
}

func TestBackfillPagesAndCommitsProgress(t *testing.T) {
	h := newHarness(t, func(s *evalsiv1alpha1.TraceSource) {
		s.Backfill = &evalsiv1alpha1.Backfill{Since: durationpb.New(24 * time.Hour)}
	})
	h.now = epoch.Add(48 * time.Hour)
	var infos []Info
	for i := 0; i < 25; i++ {
		infos = append(infos, info(fmt.Sprintf("tr-%02d", i), h.now.Add(-23*time.Hour+time.Duration(i)*time.Minute)))
	}
	h.fs.set(infos...)
	h.fs.pageSize = 10
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	if got := h.ing.ids(); len(got) != 25 {
		t.Fatalf("backfilled %d of 25", len(got))
	}
	if len(h.ing.calls) != 3 {
		t.Fatalf("expected three pages, got %d ingest calls", len(h.ing.calls))
	}
	st := h.state(t)
	if !st.BackfillFrom.IsZero() {
		t.Fatal("a caught-up source is still backfilling")
	}
	if want := h.now.Add(-30 * time.Minute); !st.Watermark.Equal(want) {
		t.Fatalf("watermark %v, want %v", st.Watermark, want)
	}
	if ph := evalsiv1alpha1.SourcePhase(h.r.phase.Load()); ph != evalsiv1alpha1.SourcePhase_SOURCE_PHASE_TAILING {
		t.Fatalf("phase %v", ph)
	}
}

func TestFailedListIsRecordedAndBacksOff(t *testing.T) {
	h := newHarness(t, nil)
	h.fs.listErr = &RetryAfterError{After: 90 * time.Second, Err: errors.New("429")}
	err := h.cycle(t)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := backoff(time.Second, 1, err); got != 90*time.Second {
		t.Fatalf("backoff %v, want the store's Retry-After", got)
	}
	if got := backoff(time.Second, 4, errors.New("x")); got != 8*time.Second {
		t.Fatalf("backoff %v", got)
	}
	if got := backoff(30*time.Second, 20, errors.New("x")); got != 5*time.Minute {
		t.Fatalf("backoff %v, want the 5m cap", got)
	}
	h.r.fail(context.Background(), err)
	got, _ := h.m.Status(context.Background(), h.src)
	if got.GetStatus().GetPhase() != evalsiv1alpha1.SourcePhase_SOURCE_PHASE_ERROR || got.GetStatus().GetLastError() == "" {
		t.Fatalf("status %v", got.GetStatus())
	}
}

func TestWriteBackSendsChangedScoresOnce(t *testing.T) {
	h := newHarness(t, func(s *evalsiv1alpha1.TraceSource) { s.WriteBack = &evalsiv1alpha1.WriteBack{Enabled: true} })
	ctx := context.Background()
	h.r.start(ctx)
	h.m.mu.Lock()
	h.m.runners[key{"p", "studio"}] = h.r
	h.m.mu.Unlock()
	defer h.r.stop()

	result := func(v float64) []*evalsiv1alpha1.EvaluationResult {
		return []*evalsiv1alpha1.EvaluationResult{{
			Evaluator: "task-success", EvaluatorRef: "builtin/task-success@1", Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED,
			Scores: []*evalsiv1alpha1.Score{{Name: "task-success", Value: &evalsiv1alpha1.Score_Number{Number: v}, Explanation: "because"}},
		}}
	}
	tinfo := ingest.TraceInfo{Project: "p", Labels: map[string]string{LabelSource: "studio"}, Resource: ingest.Attrs{AttrSourceTraceID: "tr-a"}}
	wait := func(n int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			h.fs.mu.Lock()
			got := len(h.fs.written)
			h.fs.mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("scores written never reached %d", n)
	}
	h.m.OnResults("quality", nil, tinfo, result(1))
	wait(1)
	if s := h.fs.written[0]; s.TraceID != "tr-a" || s.Metric != "task-success" || s.Value != 1.0 || s.Policy != "quality" {
		t.Fatalf("written %+v", s)
	}
	// The same score again is not sent; a changed one is, with the ID the store gave.
	h.m.OnResults("quality", nil, tinfo, result(1))
	h.m.OnResults("quality", nil, tinfo, result(0))
	wait(2)
	time.Sleep(700 * time.Millisecond)
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if len(h.fs.written) != 2 {
		t.Fatalf("written %d scores, want 2 (the repeat must be skipped)", len(h.fs.written))
	}
	last := h.fs.priorSeen[len(h.fs.priorSeen)-1]
	if w := last[[2]string{"tr-a", "task-success"}]; w.RemoteID != "a-tr-a" {
		t.Fatalf("an update did not carry the stored remote ID: %+v", last)
	}
	// Traces that are not from a source, or from one with write-back off, are ignored.
	h.m.OnResults("quality", nil, ingest.TraceInfo{Project: "p"}, result(1))
	if st, _ := h.st.SourceState(ctx, "p", "studio"); st.Scored != 2 {
		t.Fatalf("scored %d", st.Scored)
	}
}

func TestManagerStartsStopsAndRestartsRunners(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	running := func() int {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return len(h.m.runners)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitFor("the runner to start", func() bool { return running() == 1 })
	waitFor("the first poll", func() bool { h.fs.mu.Lock(); defer h.fs.mu.Unlock(); return h.fs.lists > 0 })

	paused := proto.Clone(h.src).(*evalsiv1alpha1.TraceSource)
	paused.Paused = true
	if err := h.st.PutSource(ctx, paused); err != nil {
		t.Fatal(err)
	}
	waitFor("the runner to stop when paused", func() bool { return running() == 0 })
	if got, _ := h.m.Status(ctx, paused); got.GetStatus().GetPhase() != evalsiv1alpha1.SourcePhase_SOURCE_PHASE_PAUSED {
		t.Fatalf("phase %v", got.GetStatus().GetPhase())
	}
	if err := h.st.PutSource(ctx, h.src); err != nil {
		t.Fatal(err)
	}
	waitFor("the runner to restart", func() bool { return running() == 1 })
	if err := h.st.DeleteSource(ctx, "p", "studio"); err != nil {
		t.Fatal(err)
	}
	waitFor("the runner to stop when deleted", func() bool { return running() == 0 })

	// An unknown connector is an error on the source, not a crash.
	bad := proto.Clone(h.src).(*evalsiv1alpha1.TraceSource)
	bad.Name, bad.Connector = "bad", "nope"
	if err := h.st.PutSource(ctx, bad); err != nil {
		t.Fatal(err)
	}
	waitFor("the error to be recorded", func() bool {
		st, _ := h.st.SourceState(ctx, "p", "bad")
		return st.LastError != ""
	})
}
