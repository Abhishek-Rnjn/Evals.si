package source

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
	runmanager "github.com/abhishek-rnjn/evals.si/internal/runs"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

type scheduledRuns struct {
	store *store.Store
	calls int
}

func (runs *scheduledRuns) CreateScheduledRun(ctx context.Context, id string, request *evalsiv1alpha1.CreateRunRequest) (*evalsiv1alpha1.Run, error) {
	if run, err := runs.store.GetRun(ctx, id); err == nil {
		return run, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	runs.calls++
	run := &evalsiv1alpha1.Run{Id: id, Name: request.GetName(), Project: request.GetProject(), Status: evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING, CreatedAt: timestamppb.New(epoch), Spec: request.GetSpec()}
	return run, runs.store.CreateRun(ctx, run, request.GetSpec().GetDataset().GetInline().GetRecords())
}

func (runs *scheduledRuns) CreateRun(ctx context.Context, request *connect.Request[evalsiv1alpha1.CreateRunRequest]) (*connect.Response[evalsiv1alpha1.CreateRunResponse], error) {
	return connect.NewResponse(&evalsiv1alpha1.CreateRunResponse{}), nil
}

func scheduleConfig() *evalsiv1alpha1.PullSchedule {
	return &evalsiv1alpha1.PullSchedule{Cron: "* * * * *", Window: durationpb.New(time.Minute), StartingDeadline: durationpb.New(2 * time.Minute), RunSpec: &evalsiv1alpha1.RunSpec{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "builtin/latency"}}}}
}

func TestScheduledWindowRealWorkerReport(t *testing.T) {
	command := strings.Fields(os.Getenv("EVALSI_E2E_WORKER"))
	if len(command) == 0 {
		t.Skip("set EVALSI_E2E_WORKER for scheduled batch report integration")
	}
	worker, err := pluginhost.Start(t.Context(), pluginhost.Options{Command: command, NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)
	manifests, err := worker.Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(source *evalsiv1alpha1.TraceSource) { source.Schedule = scheduleConfig(); source.Policies = nil })
	engine := evaluation.New(worker, catalog.New(manifests), nil, "", config.Evaluate{BatchSize: 64, Parallelism: 2, MaxRecords: 10000})
	manager, err := runmanager.New(t.Context(), h.st, worker, engine, runmanager.Options{Evaluate: config.Evaluate{BatchSize: 64, Parallelism: 2, MaxRecords: 10000}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Shutdown)
	h.m.opts.Runs = manager
	h.fs.set(Info{ID: "scheduled-record", Started: epoch.Add(10 * time.Second)})
	h.now = epoch.Add(time.Minute)
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	manager.Wait()
	status, err := h.st.SourceSchedule(t.Context(), "p", "studio")
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.st.GetRun(t.Context(), status.GetLastRunId())
	if err != nil || run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED || len(run.GetSummaries()) == 0 {
		t.Fatalf("scheduled report: %v, %v", run, err)
	}
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	all, _, err := h.st.ListRuns(t.Context(), []string{"p"}, 100, "")
	if err != nil || len(all) != 1 {
		t.Fatal("scheduled retry created a second real-worker run")
	}
}

func TestScheduledWindowOverlapAndRestartReconciliation(t *testing.T) {
	h := newHarness(t, func(source *evalsiv1alpha1.TraceSource) { source.Schedule = scheduleConfig() })
	runs := &scheduledRuns{store: h.st}
	h.m.opts.Runs = runs
	h.fs.set(Info{ID: "inside", Started: epoch.Add(10 * time.Second)}, Info{ID: "boundary", Started: epoch.Add(time.Minute)})
	h.now = epoch.Add(time.Minute)
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	status, err := h.st.SourceSchedule(t.Context(), "p", "studio")
	if err != nil || status.GetLastRunId() == "" || runs.calls != 1 {
		t.Fatal("scheduled run was not created")
	}
	records, err := h.st.Records(t.Context(), status.GetLastRunId())
	if err != nil || len(records) != 1 {
		t.Fatal("window must be start-inclusive and end-exclusive")
	}
	if err := h.st.PutSourceSchedule(t.Context(), "p", "studio", &evalsiv1alpha1.SourceStatus{}); err != nil {
		t.Fatal(err)
	}
	if err := h.cycle(t); err != nil || runs.calls != 1 {
		t.Fatal("restart after run creation duplicated the run")
	}
	h.now = epoch.Add(2 * time.Minute)
	if err := h.cycle(t); err != nil || runs.calls != 1 {
		t.Fatal("pending scheduled runs overlapped")
	}
	run, err := h.st.GetRun(t.Context(), status.GetLastRunId())
	if err != nil {
		t.Fatal(err)
	}
	run.Status = evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED
	if err := h.st.UpdateRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := h.cycle(t); err != nil || runs.calls != 2 {
		t.Fatal("next eligible window did not run after completion")
	}
}

func TestScheduledMissedWindowsAndInProgress(t *testing.T) {
	h := newHarness(t, func(source *evalsiv1alpha1.TraceSource) { source.Schedule = scheduleConfig() })
	h.m.opts.Runs = &scheduledRuns{store: h.st}
	h.now = epoch.Add(10 * time.Minute)
	if err := h.cycle(t); err != nil {
		t.Fatal(err)
	}
	status, err := h.st.SourceSchedule(t.Context(), "p", "studio")
	if err != nil || status.GetMissedWindows() != 7 || status.GetLastScheduledAt().AsTime() != epoch.Add(8*time.Minute) {
		t.Fatalf("missed windows: %+v, %v", status, err)
	}
	h.fs.set(Info{ID: "running", Started: epoch.Add(8*time.Minute + 10*time.Second), InProgress: true})
	if err := h.cycle(t); err == nil {
		t.Fatal("in-progress trace was scored as a complete scheduled window")
	}
	status, err = h.st.SourceSchedule(t.Context(), "p", "studio")
	if err != nil || status.GetLastScheduledAt().AsTime() != epoch.Add(8*time.Minute) {
		t.Fatal("failed window advanced the cursor")
	}
}

func TestScheduleConfiguration(t *testing.T) {
	for _, expression := range []string{"@hourly", "* * * * * *", "CRON_TZ=Europe/London * * * * *", "not cron"} {
		config := scheduleConfig()
		config.Cron = expression
		if _, err := parseSchedule(config); err == nil {
			t.Fatal("unsupported cron expression accepted")
		}
	}
	config := scheduleConfig()
	config.RunSpec.Target = &evalsiv1alpha1.Target{Model: "unexpected-target"}
	if _, err := parseSchedule(config); err == nil {
		t.Fatal("scheduled scoring accepted a target")
	}
}

func TestScheduledWindowLeaseExcludesAnotherReplica(t *testing.T) {
	h := newHarness(t, func(source *evalsiv1alpha1.TraceSource) { source.Schedule = scheduleConfig() })
	runs := &scheduledRuns{store: h.st}
	h.m.opts.Runs = runs
	h.fs.set(Info{ID: "inside", Started: epoch.Add(10 * time.Second)})
	h.now = epoch.Add(time.Minute)
	if acquired, err := h.st.AcquireLease(t.Context(), "scheduled-source:p/studio", "another-replica", 2*time.Minute); err != nil || !acquired {
		t.Fatal(err)
	}
	if err := h.cycle(t); err != nil || runs.calls != 0 {
		t.Fatal("another replica's scheduled pull overlapped")
	}
	if err := h.st.ReleaseLease(t.Context(), "scheduled-source:p/studio", "another-replica"); err != nil {
		t.Fatal(err)
	}
	if err := h.cycle(t); err != nil || runs.calls != 1 {
		t.Fatal("released schedule lease did not permit the window")
	}
}
