package runs

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Limits on an imported run, as a dataset upload has (100000 records). The
// RPC's 64 MiB message bound applies too.
const (
	maxImportRecords = 100_000
	maxImportResults = 1_000_000
)

// ImportRun stores a run whose evaluation happened elsewhere (PRD E6): the
// records, results and summaries are taken as sent, the spec's gates are
// checked against the summaries, and the run is stored finished, so it lists,
// reports, compares and fires webhooks like any other.
func (m *Manager) ImportRun(ctx context.Context, req *connect.Request[evalsiv1alpha1.ImportRunRequest]) (*connect.Response[evalsiv1alpha1.ImportRunResponse], error) {
	msg := req.Msg
	spec := msg.GetSpec()
	if spec == nil || len(spec.GetEvaluators()) == 0 {
		return nil, invalid("spec with its evaluators is required")
	}
	for _, g := range spec.GetGates() {
		if g.GetMetric() == "" || (g.Min == nil && g.Max == nil) {
			return nil, invalid("every gate needs a metric and a min or max")
		}
	}
	records, results := msg.GetRecords(), msg.GetResults()
	switch {
	case len(records) == 0:
		return nil, invalid("records are required")
	case len(records) > maxImportRecords:
		return nil, invalid("at most %d records may be imported, not %d", maxImportRecords, len(records))
	case len(results) > maxImportResults:
		return nil, invalid("at most %d results may be imported, not %d", maxImportResults, len(results))
	}
	index := make(map[string]int, len(records))
	for i, r := range records {
		if r.GetId() == "" {
			return nil, invalid("records[%d] has no id", i)
		}
		if _, dup := index[r.GetId()]; dup {
			return nil, invalid("record id %q is used twice", r.GetId())
		}
		index[r.GetId()] = i
	}
	trials := trialsOf(spec)
	evaluators := map[string]int{}
	stored := make([]store.Result, 0, len(results))
	seen := map[store.Key]bool{}
	for i, r := range results {
		idx, ok := index[r.GetRecordId()]
		if !ok {
			return nil, invalid("results[%d] names record %q, which is not among the records", i, r.GetRecordId())
		}
		if r.GetEvaluator() == "" {
			return nil, invalid("results[%d] has no evaluator", i)
		}
		if t := int(r.GetTrial()); t < 0 || t >= trials {
			return nil, invalid("results[%d] is trial %d; the spec has %d trial(s)", i, t, trials)
		}
		e, ok := evaluators[r.GetEvaluator()]
		if !ok {
			e = len(evaluators)
			evaluators[r.GetEvaluator()] = e
		}
		key := store.Key{RecordIdx: idx, Trial: int(r.GetTrial()), EvalIdx: e}
		if seen[key] {
			return nil, invalid("results[%d] repeats record %q, trial %d, evaluator %q", i, r.GetRecordId(), r.GetTrial(), r.GetEvaluator())
		}
		seen[key] = true
		stored = append(stored, store.Result{RecordIdx: idx, Trial: key.Trial, EvalIdx: e, Result: r})
	}

	project := projectOr(msg.GetProject())
	now := timestamppb.New(m.opts.Now())
	kept := proto.Clone(spec).(*evalsiv1alpha1.RunSpec)
	if kept.GetDataset().GetInline() != nil {
		kept.Dataset.Source = &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{}}
	}
	run := &evalsiv1alpha1.Run{
		Id:            newID(),
		Name:          msg.GetName(),
		Project:       project,
		Labels:        msg.GetLabels(),
		CreatedBy:     auth.PrincipalFrom(ctx).ID(),
		Spec:          kept,
		CreatedAt:     now,
		StartedAt:     msg.GetStartedAt(),
		FinishedAt:    msg.GetFinishedAt(),
		Progress:      &evalsiv1alpha1.Progress{Total: int64(len(results)), Done: int64(len(results))},
		Summaries:     msg.GetSummaries(),
		Gates:         CheckGates(spec.GetGates(), msg.GetSummaries()),
		TargetUsage:   msg.GetTargetUsage(),
		JudgeUsage:    msg.GetJudgeUsage(),
		DatasetSha256: datasetHash(records),
		Records:       int64(len(records)),
		Status:        evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED,
		Imported:      true,
	}
	if run.StartedAt == nil {
		run.StartedAt = now
	}
	if run.FinishedAt == nil {
		run.FinishedAt = now
	}
	for _, g := range run.GetGates() {
		if !g.GetPassed() {
			run.Status = evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED
		}
	}
	all := make([]*evalsiv1alpha1.EvaluationResult, len(results))
	copy(all, results)
	if err := allErrored(all); err != nil {
		run.Status, run.Error = evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, err.Error()
	}

	m.admit.Lock()
	defer m.admit.Unlock()
	if err := m.checkQuota(ctx, project, 1); err != nil {
		return nil, err
	}
	if err := m.store.CreateRun(ctx, run, records); err != nil {
		return nil, err
	}
	if err := m.store.PutResults(ctx, run.GetId(), stored); err != nil {
		// Leave no run without its results; it is finished, so it can go.
		if derr := m.store.DeleteRun(context.Background(), run.GetId()); derr != nil {
			m.log.Error("removing a half-imported run", "run", run.GetId(), "err", derr)
		}
		return nil, fmt.Errorf("storing the results: %w", err)
	}
	m.mu.Lock()
	m.finished[run.GetStatus()]++
	m.mu.Unlock()
	if m.opts.OnFinished != nil {
		m.opts.OnFinished(proto.Clone(run).(*evalsiv1alpha1.Run))
	}
	return connect.NewResponse(&evalsiv1alpha1.ImportRunResponse{Run: run}), nil
}

// scoredBy is the evaluators a stored run's results came from, for reading
// them back (comparing, promoting). A run evaluated here binds its spec as it
// ran. An imported one may name evaluators this server does not have (a
// function in a training script) or judges it has none of, and nothing will
// run them, so its instances are only the names its results carry.
func (m *Manager) scoredBy(run *evalsiv1alpha1.Run) ([]evaluation.Instance, error) {
	if !run.GetImported() {
		return m.engine.Bind(run.GetSpec().GetEvaluators(), run.GetSpec().GetJudge())
	}
	refs := run.GetSpec().GetEvaluators()
	out := make([]evaluation.Instance, 0, len(refs))
	for _, ref := range refs {
		name, _, _ := strings.Cut(ref.GetRef(), "@")
		short := ref.GetName()
		if short == "" {
			short = catalog.ShortName(name)
		}
		out = append(out, evaluation.Instance{
			Name:     short,
			Manifest: &evalsiv1alpha1.EvaluatorManifest{Name: name},
			Params:   ref.GetParams(),
		})
	}
	return out, nil
}
