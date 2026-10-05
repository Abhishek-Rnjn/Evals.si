package runs

import (
	"context"
	"errors"
	"math"
	"sort"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/stats"
)

// perRecord maps metric -> record id -> mean value over trials.
func (m *Manager) perRecord(ctx context.Context, run *evalsiv1alpha1.Run) (map[string]map[string]float64, []string, error) {
	insts, err := m.engine.Bind(run.GetSpec().GetEvaluators(), run.GetSpec().GetJudge())
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]evaluation.Instance{}
	for _, in := range insts {
		byName[in.Name] = in
	}
	stored, err := m.store.Results(ctx, run.GetId(), "", 0, 0)
	if err != nil {
		return nil, nil, err
	}
	sums := map[string]map[string][2]float64{}
	var order []string
	for _, r := range stored {
		res := r.Result
		in, ok := byName[res.GetEvaluator()]
		if !ok || r.RecordIdx < 0 || res.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
			continue
		}
		for _, s := range res.GetScores() {
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
			key := evaluation.MetricKey(in, s.GetName())
			if sums[key] == nil {
				sums[key] = map[string][2]float64{}
				order = append(order, key)
			}
			acc := sums[key][res.GetRecordId()]
			sums[key][res.GetRecordId()] = [2]float64{acc[0] + v, acc[1] + 1}
		}
	}
	out := map[string]map[string]float64{}
	for key, recs := range sums {
		out[key] = map[string]float64{}
		for id, acc := range recs {
			out[key][id] = acc[0] / acc[1]
		}
	}
	return out, order, nil
}

// CompareRuns compares two runs record by record: for each metric both runs
// have, the mean per-record difference (candidate - baseline) and a paired
// t interval over the records they share.
func (m *Manager) CompareRuns(ctx context.Context, req *connect.Request[evalsiv1alpha1.CompareRunsRequest]) (*connect.Response[evalsiv1alpha1.CompareRunsResponse], error) {
	level := req.Msg.GetConfidenceLevel()
	if level == 0 {
		level = 0.95
	}
	if level <= 0 || level >= 1 {
		return nil, invalid("confidence_level must be between 0 and 1")
	}
	base, err := m.get(ctx, req.Msg.GetBaselineRunId())
	if err != nil {
		return nil, err
	}
	cand, err := m.get(ctx, req.Msg.GetCandidateRunId())
	if err != nil {
		return nil, err
	}
	b, order, err := m.perRecord(ctx, base)
	if err != nil {
		return nil, err
	}
	c, _, err := m.perRecord(ctx, cand)
	if err != nil {
		return nil, err
	}
	resp := &evalsiv1alpha1.CompareRunsResponse{}
	for _, key := range order {
		cm, ok := c[key]
		if !ok {
			continue
		}
		ids := make([]string, 0, len(b[key]))
		for id := range b[key] {
			if _, shared := cm[id]; shared {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		cmp := &evalsiv1alpha1.MetricComparison{Metric: key, PairedN: int64(len(ids))}
		if len(ids) > 0 {
			var bs, cs, diffs []float64
			for _, id := range ids {
				bs = append(bs, b[key][id])
				cs = append(cs, cm[id])
				diffs = append(diffs, cm[id]-b[key][id])
			}
			cmp.BaselineMean = proto.Float64(stats.Mean(bs))
			cmp.CandidateMean = proto.Float64(stats.Mean(cs))
			cmp.Diff = proto.Float64(stats.Mean(diffs))
			if iv, ok := stats.TInterval(diffs, level); ok && !math.IsNaN(iv.Low) {
				cmp.DiffCi = &evalsiv1alpha1.ConfidenceInterval{Low: iv.Low, High: iv.High, Level: iv.Level, Method: "paired-t"}
				cmp.Significant = iv.Low > 0 || iv.High < 0
			}
		}
		resp.Comparisons = append(resp.Comparisons, cmp)
	}
	if len(resp.Comparisons) == 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errNoSharedMetrics)
	}
	return connect.NewResponse(resp), nil
}

var errNoSharedMetrics = errors.New("the runs share no metrics to compare")
