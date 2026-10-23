package source

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

// partialListConnector drops infos from its first drops listings without an
// error: a short page that looks like the end of the window.
type partialListConnector struct {
	*fakeStore
	drops atomic.Int32 // listings still to cut short
}

func (c *partialListConnector) List(ctx context.Context, req ListRequest) (ListResult, error) {
	res, err := c.fakeStore.List(ctx, req)
	if c.drops.Add(-1) >= 0 {
		if len(res.Infos) > 1 {
			res.Infos = res.Infos[:len(res.Infos)-1]
		}
	}
	return res, err
}

func TestScheduledWindowRelistsAfterAShortPage(t *testing.T) {
	h := newHarness(t, func(source *evalsiv1alpha1.TraceSource) { source.Schedule = scheduleConfig() })
	runs := &scheduledRuns{store: h.st}
	h.m.opts.Runs = runs
	h.fs.set(Info{ID: "a", Started: epoch.Add(10 * time.Second)}, Info{ID: "b", Started: epoch.Add(20 * time.Second)}, Info{ID: "c", Started: epoch.Add(30 * time.Second)})
	h.now = epoch.Add(time.Minute)
	connector := &partialListConnector{fakeStore: h.fs}
	connector.drops.Store(1) // the first listing loses the last trace
	if err := h.r.scheduledCycle(t.Context(), connector); err != nil {
		t.Fatal(err)
	}
	status, err := h.st.SourceSchedule(t.Context(), "p", "studio")
	if err != nil || runs.calls != 1 || status.GetPulled() != 3 {
		t.Fatalf("the trace a short page lost was not found: pulled=%d calls=%d err=%v", status.GetPulled(), runs.calls, err)
	}
	run, _ := h.st.GetRun(t.Context(), status.GetLastRunId())
	if run.GetLabels()["listing_unstable"] != "" || run.GetLabels()["incomplete_traces"] != "" {
		t.Fatalf("a recovered window was marked incomplete: %v", run.GetLabels())
	}
}

// A store that keeps returning more on every listing is recorded as unstable.
type growingConnector struct {
	*fakeStore
	calls int
}

func (c *growingConnector) List(ctx context.Context, req ListRequest) (ListResult, error) {
	c.calls++
	id := string(rune('a' + c.calls))
	c.fakeStore.set(append(c.fakeStore.infos, Info{ID: id, Started: epoch.Add(time.Duration(c.calls) * time.Second)})...)
	return c.fakeStore.List(ctx, req)
}

func TestScheduledWindowThatKeepsGrowingIsMarkedUnstable(t *testing.T) {
	h := newHarness(t, func(source *evalsiv1alpha1.TraceSource) { source.Schedule = scheduleConfig() })
	h.m.opts.Runs = &scheduledRuns{store: h.st}
	h.now = epoch.Add(time.Minute)
	connector := &growingConnector{fakeStore: h.fs}
	if err := h.r.scheduledCycle(t.Context(), connector); err != nil {
		t.Fatal(err)
	}
	status, _ := h.st.SourceSchedule(t.Context(), "p", "studio")
	run, err := h.st.GetRun(t.Context(), status.GetLastRunId())
	if err != nil || run.GetLabels()["listing_unstable"] != "true" {
		t.Fatalf("unstable listing not recorded: %v %v", run.GetLabels(), err)
	}
	if connector.calls != verifyPasses {
		t.Fatalf("listed %d times, want %d", connector.calls, verifyPasses)
	}
}

// flakyFetchConnector leaves a trace out of the first Fetch that asks for it.
type flakyFetchConnector struct {
	*fakeStore
	left  atomic.Int32
	calls atomic.Int32
}

func (c *flakyFetchConnector) Fetch(ctx context.Context, infos []Info) ([]ingest.Trace, error) {
	c.calls.Add(1)
	if c.left.Add(-1) >= 0 && len(infos) > 1 {
		infos = infos[1:]
	}
	return c.fakeStore.Fetch(ctx, infos)
}

func TestScheduledWindowAsksAgainForTracesAFetchLeftOut(t *testing.T) {
	h := newHarness(t, func(source *evalsiv1alpha1.TraceSource) { source.Schedule = scheduleConfig() })
	runs := &scheduledRuns{store: h.st}
	h.m.opts.Runs = runs
	h.fs.set(Info{ID: "a", Started: epoch.Add(10 * time.Second)}, Info{ID: "b", Started: epoch.Add(20 * time.Second)})
	h.now = epoch.Add(time.Minute)
	connector := &flakyFetchConnector{fakeStore: h.fs}
	connector.left.Store(1)
	if err := h.r.scheduledCycle(t.Context(), connector); err != nil {
		t.Fatal(err)
	}
	status, _ := h.st.SourceSchedule(t.Context(), "p", "studio")
	run, _ := h.st.GetRun(t.Context(), status.GetLastRunId())
	if status.GetPulled() != 2 || run.GetLabels()["incomplete_traces"] != "" {
		t.Fatalf("partial fetch lost a trace: pulled=%d labels=%v", status.GetPulled(), run.GetLabels())
	}
}

// A trace that stays missing (deleted) is skipped as before, and the run says
// so instead of looking complete.
func TestScheduledWindowRecordsTracesThatStayMissing(t *testing.T) {
	h := newHarness(t, func(source *evalsiv1alpha1.TraceSource) { source.Schedule = scheduleConfig() })
	h.m.opts.Runs = &scheduledRuns{store: h.st}
	h.fs.set(Info{ID: "deleted", Started: epoch.Add(10 * time.Second)}, Info{ID: "remaining", Started: epoch.Add(20 * time.Second)})
	h.now = epoch.Add(time.Minute)
	if err := h.r.scheduledCycle(t.Context(), disappearingTraceConnector{h.fs}); err != nil {
		t.Fatal(err)
	}
	status, _ := h.st.SourceSchedule(t.Context(), "p", "studio")
	run, _ := h.st.GetRun(t.Context(), status.GetLastRunId())
	if run.GetLabels()["incomplete_traces"] != "1" {
		t.Fatalf("labels = %v", run.GetLabels())
	}
}
