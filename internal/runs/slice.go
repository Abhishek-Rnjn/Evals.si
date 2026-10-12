package runs

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

const (
	sliceDefaultRuns = 50
	sliceMaxRuns     = 500
	// A slice by a field with many values (a record id, say) would answer with
	// a row for each; past this many groups the request is refused.
	sliceMaxGroups = 10000
	slicePage      = 1000
	sliceNone      = "(none)"
)

// resultMetric names a score the way a run's summary does: the instance name
// for the evaluator's primary score, else "<instance>.<score>". It is
// evalsi.results.result_metric, which `evalsi analyze` names metrics with.
func resultMetric(evaluator, ref, score string) string {
	short := evaluator
	if ref != "" {
		name, _, _ := strings.Cut(ref, "@")
		short = name[strings.LastIndex(name, "/")+1:]
	}
	if score == "" || score == short {
		return evaluator
	}
	return evaluator + "." + score
}

// welford accumulates a count, mean and sum of squared deviations in one pass.
type welford struct {
	n    int64
	mean float64
	m2   float64
}

func (w *welford) add(x float64) {
	w.n++
	d := x - w.mean
	w.mean += d / float64(w.n)
	w.m2 += d * (x - w.mean)
}

type sliceKey struct{ run, group string }

// SliceRuns computes a metric per run and per value of a record metadata
// field on the server, over the runs the caller may read: what `evalsi
// analyze slice` computes after downloading every result of every run. The
// numbers match it (the same metric names, the same output-record-else-dataset-
// record lookup, a sample standard deviation and a normal interval); only runs
// are read, never another store, so access rules apply as they do to GetRun.
func (m *Manager) SliceRuns(ctx context.Context, req *connect.Request[evalsiv1alpha1.SliceRunsRequest]) (*connect.Response[evalsiv1alpha1.SliceRunsResponse], error) {
	r := req.Msg
	if r.GetMetric() == "" || r.GetBy() == "" {
		return nil, invalid("metric and by are required")
	}
	path := strings.Split(r.GetBy(), ".")
	for _, p := range path {
		if p == "" {
			return nil, invalid("by %q is not a dotted path", r.GetBy())
		}
	}
	level := r.GetConfidenceLevel()
	if level == 0 {
		level = 0.95
	}
	z, ok := map[float64]float64{0.9: 1.6449, 0.95: 1.96, 0.99: 2.5758}[level]
	if !ok {
		return nil, invalid("confidence_level must be 0.9, 0.95 or 0.99")
	}
	runs, err := m.sliceRuns(ctx, r)
	if err != nil {
		return nil, err
	}
	stats := map[sliceKey]*welford{}
	for _, run := range runs {
		if err := m.sliceRun(ctx, run, r.GetMetric(), path, stats); err != nil {
			return nil, err
		}
	}
	byRun := map[string]*evalsiv1alpha1.Run{}
	for _, run := range runs {
		byRun[run.GetId()] = run
	}
	resp := &evalsiv1alpha1.SliceRunsResponse{Runs: int32(len(runs))}
	for k, w := range stats {
		run := byRun[k.run]
		row := &evalsiv1alpha1.SliceRow{RunId: k.run, RunName: run.GetName(), Key: k.group, N: w.n, Mean: w.mean}
		if w.n > 1 {
			half := z * math.Sqrt(w.m2/float64(w.n-1)) / math.Sqrt(float64(w.n))
			lo, hi := w.mean-half, w.mean+half
			row.Low, row.High = &lo, &hi
		}
		resp.Rows = append(resp.Rows, row)
	}
	created := func(id string) int64 { return byRun[id].GetCreatedAt().AsTime().UnixNano() }
	sort.Slice(resp.Rows, func(i, j int) bool {
		a, b := resp.Rows[i], resp.Rows[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if ca, cb := created(a.RunId), created(b.RunId); ca != cb {
			return ca < cb
		}
		return a.RunId < b.RunId
	})
	return connect.NewResponse(resp), nil
}

// sliceRuns selects the runs: the ones named, or the newest that carry the
// labels. Either way a run the caller may not read is not there; naming one
// fails as GetRun on a missing run does, so the two look alike.
func (m *Manager) sliceRuns(ctx context.Context, r *evalsiv1alpha1.SliceRunsRequest) ([]*evalsiv1alpha1.Run, error) {
	readable := func(run *evalsiv1alpha1.Run) bool {
		return authz.Can(ctx, "runs.read", run.GetProject(), authz.RunResourceFor(ctx, run))
	}
	if len(r.GetRunIds()) > 0 {
		if len(r.GetRunIds()) > sliceMaxRuns {
			return nil, invalid("at most %d run_ids", sliceMaxRuns)
		}
		seen := map[string]bool{}
		var out []*evalsiv1alpha1.Run
		for _, id := range r.GetRunIds() {
			if seen[id] {
				continue
			}
			seen[id] = true
			run, err := m.get(ctx, id)
			if err != nil {
				return nil, err
			}
			if !readable(run) {
				return nil, connect.NewError(connect.CodeNotFound, errors.New("no run "+strconv.Quote(id)))
			}
			out = append(out, run)
		}
		return out, nil
	}
	limit := int(r.GetLimit())
	if limit <= 0 {
		limit = sliceDefaultRuns
	}
	limit = min(limit, sliceMaxRuns)
	projects := authz.Projects(ctx, "runs.read")
	if p := r.GetProject(); p != "" {
		if projects != nil && !slices.Contains(projects, p) {
			return nil, nil
		}
		projects = []string{p}
	}
	var out []*evalsiv1alpha1.Run
	token := ""
	for len(out) < limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, next, err := m.store.ListRuns(ctx, projects, 200, token)
		if err != nil {
			return nil, invalid("%v", err)
		}
		for _, run := range page {
			if len(out) < limit && hasLabels(run, r.GetLabels()) && readable(run) {
				out = append(out, run)
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	return out, nil
}

func hasLabels(run *evalsiv1alpha1.Run, want map[string]string) bool {
	for k, v := range want {
		if got, ok := run.GetLabels()[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// sliceRun adds a run's scores of metric to stats, grouped by the metadata
// field at path.
func (m *Manager) sliceRun(ctx context.Context, run *evalsiv1alpha1.Run, metric string, path []string, stats map[sliceKey]*welford) error {
	records, err := m.store.Records(ctx, run.GetId())
	if err != nil {
		return err
	}
	outputs, err := m.store.Outputs(ctx, run.GetId())
	if err != nil {
		return err
	}
	for offset := 0; ; offset += slicePage {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := m.store.Results(ctx, run.GetId(), "", slicePage, offset)
		if err != nil {
			return err
		}
		for _, sr := range page {
			if err := addScores(run.GetId(), sr, metric, path, records, outputs, stats); err != nil {
				return err
			}
		}
		if len(page) < slicePage {
			return nil
		}
	}
}

func addScores(runID string, sr store.Result, metric string, path []string, records []*evalsiv1alpha1.Record, outputs map[[2]int]store.Output, stats map[sliceKey]*welford) error {
	res := sr.Result
	if res.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
		return nil
	}
	group := sliceNone
	if sr.RecordIdx >= 0 {
		// The output of the score's own trial, else the dataset record.
		if o, ok := outputs[[2]int{sr.RecordIdx, sr.Trial}]; ok && o.Record != nil {
			group = metadataText(o.Record.GetMetadata(), path)
		} else if sr.RecordIdx < len(records) {
			group = metadataText(records[sr.RecordIdx].GetMetadata(), path)
		}
	}
	for _, s := range res.GetScores() {
		if resultMetric(res.GetEvaluator(), res.GetEvaluatorRef(), s.GetName()) != metric {
			continue
		}
		var v float64
		switch x := s.GetValue().(type) {
		case *evalsiv1alpha1.Score_Number:
			v = x.Number
		case *evalsiv1alpha1.Score_Passed:
			if x.Passed {
				v = 1
			}
		default:
			continue
		}
		k := sliceKey{runID, group}
		w := stats[k]
		if w == nil {
			if len(stats) >= sliceMaxGroups {
				return invalid("more than %d groups; slice by a field with fewer values", sliceMaxGroups)
			}
			w = &welford{}
			stats[k] = w
		}
		w.add(v)
	}
	return nil
}

// metadataText is the value at path in a record's metadata as text, the way
// DuckDB's json_extract_string gives it to the client-side slice: a string as
// is, a number or boolean as JSON, and (none) when it is missing or null.
func metadataText(md map[string]*structpb.Value, path []string) string {
	v, ok := md[path[0]]
	for _, p := range path[1:] {
		if !ok || v == nil {
			return sliceNone
		}
		s, isStruct := v.GetKind().(*structpb.Value_StructValue)
		if !isStruct {
			return sliceNone
		}
		v, ok = s.StructValue.GetFields()[p]
	}
	if !ok || v == nil {
		return sliceNone
	}
	switch x := v.GetKind().(type) {
	case *structpb.Value_StringValue:
		return x.StringValue
	case *structpb.Value_NumberValue:
		return strconv.FormatFloat(x.NumberValue, 'f', -1, 64)
	case *structpb.Value_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *structpb.Value_NullValue, nil:
		return sliceNone
	default:
		b, err := json.Marshal(v.AsInterface())
		if err != nil {
			return sliceNone
		}
		return string(b)
	}
}
