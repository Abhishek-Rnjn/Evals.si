package source

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/robfig/cron/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

type ScheduledRuns interface {
	CreateScheduledRun(context.Context, string, *evalsiv1alpha1.CreateRunRequest) (*evalsiv1alpha1.Run, error)
	CreateRun(context.Context, *connect.Request[evalsiv1alpha1.CreateRunRequest]) (*connect.Response[evalsiv1alpha1.CreateRunResponse], error)
}

func parseSchedule(config *evalsiv1alpha1.PullSchedule) (cron.Schedule, error) {
	if len(strings.Fields(config.GetCron())) != 5 {
		return nil, invalid("schedule.cron must be a five-field UTC cron expression")
	}
	schedule, err := cron.ParseStandard(config.GetCron())
	if err != nil {
		return nil, invalid("invalid schedule.cron")
	}
	if config.GetWindow() == nil || !config.GetWindow().IsValid() || config.GetWindow().AsDuration() < time.Minute || config.GetWindow().AsDuration() > 7*24*time.Hour {
		return nil, invalid("schedule.window must be between 1m and 168h")
	}
	if deadline := config.GetStartingDeadline(); deadline != nil && (!deadline.IsValid() || deadline.AsDuration() < time.Minute || deadline.AsDuration() > 24*time.Hour) {
		return nil, invalid("schedule.starting_deadline must be between 1m and 24h")
	}
	if config.GetMaxRecords() < 0 || config.GetMaxRecords() > 10000 {
		return nil, invalid("schedule.max_records must be between 1 and 10000, or zero for 10000")
	}
	spec := config.GetRunSpec()
	if spec == nil || len(spec.GetEvaluators()) == 0 || spec.GetDataset() != nil || spec.GetTarget() != nil || spec.GetHarness() != nil || spec.GetEnvironment() != nil || spec.GetTrials() > 1 {
		return nil, invalid("schedule.run_spec needs evaluators and must not set a dataset, target, harness, environment or multiple trials")
	}
	return schedule, nil
}

func (r *runner) scheduledCycle(ctx context.Context, connector Connector) (err error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	lease := "scheduled-source:" + r.src.GetProject() + "/" + r.src.GetName()
	owner := hex.EncodeToString(nonce[:])
	acquired, err := r.m.st.AcquireLease(ctx, lease, owner, 2*time.Minute)
	if err != nil || !acquired {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = r.m.st.ReleaseLease(releaseCtx, lease, owner)
	}()
	config := r.src.GetSchedule()
	schedule, err := parseSchedule(config)
	if err != nil {
		return err
	}
	limit := int(config.GetMaxRecords())
	if limit == 0 {
		limit = 10000
	}
	rate := int(r.src.GetPoll().GetMaxRecordsPerSecond())
	if rate <= 0 {
		rate = r.m.opts.MaxRate
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute+time.Duration(limit)*time.Second/time.Duration(rate))
	defer cancel()
	ctx, stopRenewal := r.renewScheduleLease(ctx, lease, owner, 2*time.Minute)
	defer stopRenewal()
	defer func() { err = reportLeaseLoss(ctx, err) }()
	status, err := r.m.st.SourceSchedule(ctx, r.src.GetProject(), r.src.GetName())
	if err != nil {
		return err
	}
	now := r.m.opts.Now().UTC()
	last := r.src.GetUpdatedAt().AsTime().UTC()
	if status.GetLastScheduledAt() != nil {
		last = status.GetLastScheduledAt().AsTime().UTC()
	}
	deadline := dur(config.GetStartingDeadline(), time.Hour)
	next := schedule.Next(last)
	iterations := 0
	for !next.IsZero() && now.Sub(next) > deadline {
		status.LastScheduledAt = timestamppb.New(next)
		status.MissedWindows++
		next = schedule.Next(next)
		iterations++
		if iterations == 10000 {
			return r.saveSchedule(ctx, status)
		}
	}
	if next.IsZero() || next.After(now) {
		return r.saveSchedule(ctx, status)
	}
	if id := status.GetLastRunId(); id != "" {
		holder, err := r.m.st.LeaseHolder(ctx, "run:"+id)
		if err != nil {
			return err
		}
		if holder != "" {
			return r.saveSchedule(ctx, status)
		}
		prior, err := r.m.st.GetRun(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if prior != nil && (prior.GetStatus() == evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING || prior.GetStatus() == evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING) {
			return r.saveSchedule(ctx, status)
		}
	}
	if r.m.opts.Runs == nil {
		return errors.New("scheduled runs are not configured")
	}
	if !r.compiled {
		r.adjust, err = CompileOverrides(r.src.GetOverrides())
		if err != nil {
			return err
		}
		r.compiled = true
	}
	start := next.Add(-config.GetWindow().AsDuration())
	var records []*evalsiv1alpha1.Record
	snapshotBytes, skipped := 0, 0
	seen := map[string]bool{}
	// scan lists the whole window once and takes the traces not yet seen.
	scan := func() (added int, err error) {
		cursor, cursors := "", map[string]bool{}
		for {
			page, err := connector.List(ctx, ListRequest{Since: start, Cursor: cursor, Limit: r.m.opts.PageSize})
			if err != nil {
				return added, err
			}
			var selected []Info
			for _, trace := range page.Infos {
				if trace.Started.Before(start) || !trace.Started.Before(next) || seen[trace.ID] {
					continue
				}
				if trace.InProgress {
					return added, errors.New("scheduled window contains an in-progress trace; waiting within starting_deadline")
				}
				seen[trace.ID] = true
				selected = append(selected, trace)
			}
			if len(seen) > limit {
				return added, errors.New("scheduled window exceeds max_records; narrow the window before retrying")
			}
			traces, err := r.collectComplete(ctx, connector, selected)
			if err != nil {
				return added, err
			}
			if len(traces) < len(selected) {
				r.log.Warn("scheduled traces still missing after asking again; skipping",
					"skipped", len(selected)-len(traces), "trace_ids", missingTraceIDs(selected, traces, 10))
				skipped += len(selected) - len(traces)
			}
			if err := sleep(ctx, time.Duration(float64(len(traces))/float64(rate)*float64(time.Second))); err != nil {
				return added, err
			}
			for _, trace := range traces {
				trace = r.m.opts.Redactor.Trace(trace)
				record, _ := ingest.ToRecord(trace)
				snapshotBytes += proto.Size(record)
				if snapshotBytes > 64<<20 {
					return added, errors.New("scheduled snapshot exceeds 64 MiB; narrow the window before retrying")
				}
				records = append(records, record)
			}
			added += len(selected)
			if page.Next == "" {
				return added, nil
			}
			if cursors[page.Next] {
				return added, errors.New("connector repeated a page cursor")
			}
			cursors[page.Next], cursor = true, page.Next
		}
	}
	// A connector that returns a short page without an error cannot be told
	// from one with nothing more, so the window is listed again until a pass
	// finds nothing new (at most verifyPasses). Traces only a later pass
	// returned were lost by an earlier one; if the last pass still finds
	// some, the window is recorded as incomplete.
	unstable := false
	for pass := 1; pass <= verifyPasses; pass++ {
		added, err := scan()
		if err != nil {
			return err
		}
		if pass > 1 && added > 0 {
			r.log.Warn("a later listing of the scheduled window returned traces an earlier one did not", "pass", pass, "traces", added)
			unstable = pass == verifyPasses
		}
		if pass > 1 && added == 0 {
			break
		}
	}
	if len(records) > 0 {
		raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(config)
		if err != nil {
			return err
		}
		identity := sha256.Sum256(append([]byte(r.src.GetProject()+"\x00"+r.src.GetName()+"\x00"+next.Format(time.RFC3339Nano)+"\x00"), raw...))
		id := "scheduled-" + hex.EncodeToString(identity[:16])
		spec := proto.Clone(config.GetRunSpec()).(*evalsiv1alpha1.RunSpec)
		spec.Dataset = &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: records}}}
		if err := r.alive(ctx); err != nil {
			return err
		}
		labels := map[string]string{"source": r.src.GetName(), "scheduled": "true", "window_end": next.Format(time.RFC3339)}
		// A window the store did not give up completely says so on its run:
		// listed traces it never returned (deleted, or lost), and a listing
		// that kept finding more.
		if skipped > 0 {
			labels["incomplete_traces"] = fmt.Sprint(skipped)
		}
		if unstable {
			labels["listing_unstable"] = "true"
		}
		run, err := r.m.opts.Runs.CreateScheduledRun(ctx, id, &evalsiv1alpha1.CreateRunRequest{
			Name: fmt.Sprintf("%s-%s", r.src.GetName(), next.Format("20060102T150405Z")), Project: r.src.GetProject(), Spec: spec,
			Labels: labels,
		})
		if err != nil {
			return err
		}
		status.LastRunId = run.GetId()
	}
	status.LastScheduledAt = timestamppb.New(next)
	status.LastPullAt = timestamppb.New(now)
	status.Pulled += int64(len(records))
	return r.saveSchedule(ctx, status)
}

// verifyPasses bounds how often a scheduled window is listed to find traces
// an earlier listing missed, and refetchTries how often traces a store listed
// but did not return are asked for again.
const (
	verifyPasses = 3
	refetchTries = 2
)

// collectComplete is collect, asking again (refetchTries times, after
// RefetchWait, doubling) for listed traces the store did not return: a trace
// deleted since it was listed stays missing, one a store dropped from a
// partial answer comes back.
func (r *runner) collectComplete(ctx context.Context, connector Connector, selected []Info) ([]ingest.Trace, error) {
	traces, err := r.collect(ctx, connector, selected)
	if err != nil {
		return nil, err
	}
	wait := r.m.opts.RefetchWait
	for try := 0; try < refetchTries && len(traces) < len(selected); try++ {
		have := make(map[string]bool, len(traces))
		for _, trace := range traces {
			have[sourceID(trace)] = true
		}
		var missing []Info
		for _, info := range selected {
			if !have[info.ID] {
				missing = append(missing, info)
			}
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
		wait *= 2
		more, err := r.collect(ctx, connector, missing)
		if err != nil {
			return nil, err
		}
		traces = append(traces, more...)
	}
	return traces, nil
}

var errLeaseLost =errors.New("scheduled source lease was lost")

// reportLeaseLoss says why a cycle stopped when its lease was lost: the pull
// then fails with a bare context.Canceled, which looks like a shutdown.
func reportLeaseLoss(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), errLeaseLost) {
		return fmt.Errorf("%w: %w", errLeaseLost, err)
	}
	return err
}

// missingTraceIDs names up to limit of the selected traces that were not fetched.
func missingTraceIDs(selected []Info, fetched []ingest.Trace, limit int) []string {
	have := make(map[string]bool, len(fetched))
	for _, trace := range fetched {
		have[sourceID(trace)] = true
	}
	var missing []string
	for _, info := range selected {
		if !have[info.ID] && len(missing) < limit {
			missing = append(missing, info.ID)
		}
	}
	return missing
}

func (r *runner) renewScheduleLease(ctx context.Context, lease, owner string, ttl time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				request, stop := context.WithTimeout(ctx, 5*time.Second)
				acquired, err := r.m.st.RenewLease(request, lease, owner, ttl)
				stop()
				if err != nil || !acquired {
					if err == nil {
						err = errLeaseLost
					}
					cancel(err)
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel(nil)
		<-done
	}
}

func (r *runner) saveSchedule(ctx context.Context, status *evalsiv1alpha1.SourceStatus) error {
	if err := r.alive(ctx); err != nil {
		return err
	}
	return r.m.st.PutSourceSchedule(ctx, r.src.GetProject(), r.src.GetName(), status)
}
