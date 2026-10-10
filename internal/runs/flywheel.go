package runs

// The flywheel (design §11): records from production traces and from earlier
// runs become datasets, failing results are promoted into regression
// datasets, and shadow replay scores a candidate against what production
// actually did.

import (
	"context"
	"sort"
	"strconv"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"github.com/abhishek-rnjn/evals.si/internal/watch"
)

// traceRecords loads stored traces of a project as records.
func (m *Manager) traceRecords(ctx context.Context, q *evalsiv1alpha1.TraceQuery, project string, limit int) ([]*evalsiv1alpha1.Record, error) {
	filter, err := watch.CompileTraceFilter(q.GetFilter())
	if err != nil {
		return nil, invalid("spec.dataset.traces.%v", err)
	}
	var since time.Time
	if d := q.GetLookback().AsDuration(); d > 0 {
		since = time.Now().Add(-d)
	}
	stored, err := m.store.QueryTraces(ctx, store.TraceQuery{Project: project, Service: q.GetService(), Policy: q.GetPolicy(), Since: since})
	if err != nil {
		return nil, err
	}
	var out []*evalsiv1alpha1.Record
	for _, t := range stored {
		// Only traces the caller may read, by the same rules as reading them.
		if !authz.Can(ctx, "traces.read", project, authz.TraceResource(t.Summary)) {
			continue
		}
		var scores map[string]float64
		if q.GetPolicy() != "" && m.opts.TraceScores != nil {
			scores = m.opts.TraceScores(q.GetPolicy(), t.Results)
		}
		if !filter.Match(t, scores) {
			continue
		}
		rec := proto.Clone(t.Record).(*evalsiv1alpha1.Record)
		rec.Id = t.Summary.GetTraceId()
		if rec.Metadata == nil {
			rec.Metadata = map[string]*structpb.Value{}
		}
		trace := map[string]any{"service": t.Summary.GetService(), "name": t.Summary.GetName(), "start_time": t.Summary.GetStartTime().AsTime().Format(time.RFC3339Nano)}
		if v, err := structpb.NewValue(trace); err == nil {
			rec.Metadata["trace"] = v
		}
		if len(scores) > 0 {
			s := map[string]any{}
			for k, v := range scores {
				s[k] = v
			}
			if v, err := structpb.NewValue(s); err == nil {
				rec.Metadata["online_scores"] = v
			}
		}
		rec.Provenance = &evalsiv1alpha1.Provenance{Source: &evalsiv1alpha1.Provenance_Trace{Trace: &evalsiv1alpha1.TraceProvenance{
			TraceId: t.Summary.GetTraceId(), PolicyId: q.GetPolicy(),
		}}}
		out = append(out, rec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// runRecords loads what an earlier run produced: its outputs for a trial (or
// every trial), or its dataset when it had no target.
func (m *Manager) runRecords(ctx context.Context, src *evalsiv1alpha1.RunOutputs) ([]*evalsiv1alpha1.Record, error) {
	run, err := m.store.GetRun(ctx, src.GetRunId())
	if err != nil {
		return nil, invalid("spec.dataset.run: no run %q", src.GetRunId())
	}
	records, err := m.store.Records(ctx, run.GetId())
	if err != nil {
		return nil, err
	}
	if !generates(run.GetSpec()) {
		return records, nil
	}
	outputs, err := m.store.Outputs(ctx, run.GetId())
	if err != nil {
		return nil, err
	}
	trials := []int{int(src.GetTrial())}
	if src.GetAllTrials() {
		trials = trials[:0]
		for t := range trialsOf(run.GetSpec()) {
			trials = append(trials, t)
		}
	}
	var out []*evalsiv1alpha1.Record
	for _, trial := range trials {
		for i := range records {
			o, ok := outputs[[2]int{i, trial}]
			if !ok || o.Record == nil {
				continue
			}
			rec := proto.Clone(o.Record).(*evalsiv1alpha1.Record)
			if src.GetAllTrials() {
				rec.Id = rec.GetId() + "#" + strconv.Itoa(trial)
			}
			out = append(out, rec)
		}
	}
	if len(out) == 0 {
		return nil, invalid("run %s has no outputs for that trial", run.GetId())
	}
	return out, nil
}

var promoteEnv = func() *cel.Env {
	env, err := cel.NewEnv(
		cel.Variable("scores", cel.MapType(cel.StringType, cel.DoubleType)),
		cel.Variable("errored", cel.BoolType),
		cel.Variable("trial", cel.IntType),
		cel.Variable("record", cel.MapType(cel.StringType, cel.DynType)),
		cel.CrossTypeNumericComparisons(true),
	)
	if err != nil {
		panic(err)
	}
	return env
}()

func compilePromote(expr string) (cel.Program, error) {
	if expr == "" {
		return nil, invalid("when is required; promoting every record is rarely intended")
	}
	ast, issues := promoteEnv.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, invalid("when: %v", issues.Err())
	}
	if ast.OutputType() != types.BoolType {
		return nil, invalid("when must be a boolean expression, not %s", ast.OutputType())
	}
	return promoteEnv.Program(ast, cel.EvalOptions(cel.OptOptimize), cel.CostLimit(100000))
}

func scoreValue(s *evalsiv1alpha1.Score) (float64, bool) {
	switch v := s.GetValue().(type) {
	case *evalsiv1alpha1.Score_Number:
		return v.Number, true
	case *evalsiv1alpha1.Score_Passed:
		if v.Passed {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Match is one of a run's records that matched a condition, for one trial.
type Match struct {
	// The record as the dataset had it, and as the run produced it (output,
	// trajectory), when it produced one.
	Record   *evalsiv1alpha1.Record
	Produced *evalsiv1alpha1.Record
	Trial    int
	// The run's scores for it, by metric.
	Scores  map[string]float64
	Errored bool
}

// MatchRecords returns the run's records for which the CEL condition over
// scores, errored, trial and record holds (every record when it is
// empty), in run order: the first matching trial of each record, or every
// matching trial with allTrials.
func (m *Manager) MatchRecords(ctx context.Context, run *evalsiv1alpha1.Run, when string, allTrials bool) ([]Match, error) {
	var prog cel.Program
	if when != "" {
		var err error
		if prog, err = compilePromote(when); err != nil {
			return nil, err
		}
	}
	insts, err := m.engine.Bind(run.GetSpec().GetEvaluators(), run.GetSpec().GetJudge())
	if err != nil {
		return nil, err
	}
	records, err := m.store.Records(ctx, run.GetId())
	if err != nil {
		return nil, err
	}
	outputs, err := m.store.Outputs(ctx, run.GetId())
	if err != nil {
		return nil, err
	}
	results, err := m.store.Results(ctx, run.GetId(), "", 0, 0)
	if err != nil {
		return nil, err
	}
	type key struct{ rec, trial int }
	scores := map[key]map[string]float64{}
	errored := map[key]bool{}
	var order []key
	for _, r := range results {
		if r.RecordIdx == store.DatasetIndex || r.EvalIdx >= len(insts) {
			continue
		}
		k := key{r.RecordIdx, r.Trial}
		if _, ok := scores[k]; !ok {
			scores[k] = map[string]float64{}
			order = append(order, k)
		}
		if r.Result.GetOutcome() == evalsiv1alpha1.Outcome_OUTCOME_ERROR {
			errored[k] = true
		}
		for _, s := range r.Result.GetScores() {
			if v, ok := scoreValue(s); ok {
				scores[k][evaluation.MetricKey(insts[r.EvalIdx], s.GetName())] = v
			}
		}
	}
	// Records without per-record results (a run whose evaluators are all
	// dataset-scope, or a record every evaluator skipped) are still the
	// run's records: list every record and trial, in run order.
	trials := trialsOf(run.GetSpec())
	for rec := range records {
		for trial := range trials {
			k := key{rec, trial}
			if _, ok := scores[k]; !ok {
				scores[k] = map[string]float64{}
				order = append(order, k)
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].rec != order[j].rec {
			return order[i].rec < order[j].rec
		}
		return order[i].trial < order[j].trial
	})
	done := map[int]bool{}
	var out []Match
	for _, k := range order {
		if (done[k.rec] && !allTrials) || k.rec >= len(records) {
			continue
		}
		rec := records[k.rec]
		if prog != nil {
			meta := map[string]any{}
			for name, v := range rec.GetMetadata() {
				meta[name] = v.AsInterface()
			}
			vars := map[string]any{
				"scores": scores[k], "errored": errored[k], "trial": int64(k.trial),
				"record": map[string]any{"id": rec.GetId(), "metadata": meta},
			}
			res, _, err := prog.Eval(vars)
			if b, ok := res.(types.Bool); err != nil || !ok || !bool(b) {
				continue
			}
		}
		done[k.rec] = true
		match := Match{Record: rec, Trial: k.trial, Scores: scores[k], Errored: errored[k]}
		if o, ok := outputs[[2]int{k.rec, k.trial}]; ok && o.Record != nil {
			match.Produced = o.Record
		}
		out = append(out, match)
	}
	return out, nil
}

// Run returns a stored run (for other services; authorization is theirs).
func (m *Manager) Run(ctx context.Context, id string) (*evalsiv1alpha1.Run, error) {
	return m.get(ctx, id)
}

// PromoteResults appends the run's records that match a condition to a
// project dataset, once per record (its first matching trial).
func (m *Manager) PromoteResults(ctx context.Context, req *connect.Request[evalsiv1alpha1.PromoteResultsRequest]) (*connect.Response[evalsiv1alpha1.PromoteResultsResponse], error) {
	msg := req.Msg
	if !datasets.ValidName(msg.GetDataset()) {
		return nil, invalid("dataset %q must be lowercase letters, digits, '.', '_' or '-'", msg.GetDataset())
	}
	if _, err := compilePromote(msg.GetWhen()); err != nil {
		return nil, err
	}
	run, err := m.get(ctx, msg.GetRunId())
	if err != nil {
		return nil, err
	}
	matches, err := m.MatchRecords(ctx, run, msg.GetWhen(), false)
	if err != nil {
		return nil, err
	}
	var rows [][]byte
	for _, match := range matches {
		row := proto.Clone(match.Record).(*evalsiv1alpha1.Record)
		if msg.GetIncludeOutputs() {
			if match.Produced != nil {
				row = proto.Clone(match.Produced).(*evalsiv1alpha1.Record)
			}
		} else if generates(run.GetSpec()) {
			// The case to replay is the input; keep the reference, drop what this run produced.
			row.Output, row.Trajectory, row.Check, row.Usage = nil, nil, nil, nil
		}
		line, err := datasets.Row(row, map[string]any{
			"promoted_from": map[string]any{"run_id": run.GetId(), "trial": match.Trial},
			"run_scores":    match.Scores,
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, line)
	}
	path := datasets.Path(run.GetProject(), msg.GetDataset())
	if len(rows) > 0 {
		if path, err = datasets.Append(ctx, m.opts.DatasetsDir, m.opts.Objects, run.GetProject(), msg.GetDataset(), rows); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
	}
	return connect.NewResponse(&evalsiv1alpha1.PromoteResultsResponse{Promoted: int64(len(rows)), Path: path}), nil
}

// CreateShadowReplay creates two runs over one dataset snapshot: the
// candidate re-runs the inputs; the baseline scores the recorded outputs
// with the same evaluators. CompareRuns(baseline, candidate) is the replay's
// verdict.
func (m *Manager) CreateShadowReplay(ctx context.Context, req *connect.Request[evalsiv1alpha1.CreateShadowReplayRequest]) (*connect.Response[evalsiv1alpha1.CreateShadowReplayResponse], error) {
	msg := req.Msg
	candidate := msg.GetCandidate()
	if candidate == nil || !generates(candidate) {
		return nil, invalid("a shadow replay's candidate needs a target or an agent")
	}
	insts, err := m.validate(ctx, msg.GetProject(), candidate)
	if err != nil {
		return nil, err
	}
	baseline := proto.Clone(candidate).(*evalsiv1alpha1.RunSpec)
	baseline.Target, baseline.Harness, baseline.Environment = nil, nil, nil
	baseline.Trials, baseline.Gates, baseline.Budget = 0, nil, nil
	baseInsts, err := m.validate(ctx, msg.GetProject(), baseline)
	if err != nil {
		return nil, err
	}
	project := projectOr(msg.GetProject())
	// A replay is two runs; the quota admits both or neither.
	if err := m.checkQuota(ctx, project, 2); err != nil {
		return nil, err
	}
	records, err := m.loadDataset(ctx, candidate.GetDataset(), project)
	if err != nil {
		return nil, err
	}
	recorded := 0
	for _, r := range records {
		if r.GetOutput() != nil || r.GetTrajectory() != nil {
			recorded++
		}
	}
	if recorded == 0 {
		return nil, invalid("the dataset has no recorded outputs to compare the candidate with (use traces, a promoted dataset with outputs, or an earlier run)")
	}
	name := msg.GetName()
	if name == "" {
		name = "shadow-replay"
	}
	m.admit.Lock()
	defer m.admit.Unlock()
	if err := m.checkQuota(ctx, project, 2); err != nil {
		return nil, err
	}
	base, err := m.storeRun(ctx, name+"-baseline", project, msg.GetLabels(), baseline, baseInsts, records)
	if err != nil {
		return nil, err
	}
	cand, err := m.storeRun(ctx, name, project, msg.GetLabels(), candidate, insts, records)
	if err != nil {
		// Neither runs: the baseline alone is no replay.
		base.Status, base.Error, base.FinishedAt = evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED, "the shadow replay's candidate could not be stored: "+err.Error(), timestamppb.Now()
		if uerr := m.store.UpdateRun(context.Background(), base); uerr != nil {
			m.log.Error("cancelling a shadow replay's baseline", "run", base.GetId(), "err", uerr)
		}
		return nil, err
	}
	m.start(base)
	m.start(cand)
	return connect.NewResponse(&evalsiv1alpha1.CreateShadowReplayResponse{Baseline: base, Candidate: cand}), nil
}

func projectOr(p string) string {
	if p == "" {
		return DefaultProject
	}
	return p
}
