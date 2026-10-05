package evaluation

import (
	"math"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/stats"
)

// This file mirrors summarize() in the Python SDK's evalsi/results.py.

func metricKey(in instance, scoreName string) string {
	if scoreName == "" || scoreName == catalog.ShortName(in.manifest.GetName()) {
		return in.name
	}
	return in.name + "." + scoreName
}

type scored struct {
	score    *evalsiv1alpha1.Score
	recordID string
}

func scoreType(spec *evalsiv1alpha1.MetricSpec, values []scored) evalsiv1alpha1.ScoreType {
	if spec != nil {
		return spec.GetType()
	}
	if len(values) == 0 {
		return evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER
	}
	switch values[0].score.GetValue().(type) {
	case *evalsiv1alpha1.Score_Passed:
		return evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED
	case *evalsiv1alpha1.Score_Label:
		return evalsiv1alpha1.ScoreType_SCORE_TYPE_LABEL
	case *evalsiv1alpha1.Score_Structured:
		return evalsiv1alpha1.ScoreType_SCORE_TYPE_STRUCTURED
	default:
		return evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER
	}
}

func numeric(s *evalsiv1alpha1.Score) (float64, bool) {
	switch v := s.GetValue().(type) {
	case *evalsiv1alpha1.Score_Number:
		return v.Number, true
	case *evalsiv1alpha1.Score_Passed:
		if v.Passed {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// clusterOf returns the cluster key of a record; records without the key are
// their own cluster.
func clusterOf(r *evalsiv1alpha1.Record, key, recordID string) string {
	if r != nil {
		if v, ok := r.GetMetadata()[key]; ok {
			if s, isString := v.GetKind().(*structpb.Value_StringValue); isString {
				return s.StringValue
			}
			raw, _ := protojson.Marshal(v)
			return string(raw)
		}
	}
	return "\x00record:" + recordID
}

func summarize(insts []instance, results []*evalsiv1alpha1.EvaluationResult, records []*evalsiv1alpha1.Record, opts *evalsiv1alpha1.SummaryOptions) ([]*evalsiv1alpha1.MetricSummary, error) {
	level := opts.GetConfidenceLevel()
	if level == 0 {
		level = 0.95
	}
	if level <= 0 || level >= 1 {
		return nil, invalid("confidence_level must be between 0 and 1")
	}
	method := "auto"
	if opts.GetCiMethod() == evalsiv1alpha1.CiMethod_CI_METHOD_BOOTSTRAP {
		method = "bootstrap"
	}
	clusterBy := strings.TrimPrefix(opts.GetClusterBy(), "metadata.")
	byID := make(map[string]*evalsiv1alpha1.Record, len(records))
	for _, r := range records {
		byID[r.GetId()] = r
	}

	var out []*evalsiv1alpha1.MetricSummary
	for _, in := range insts {
		var skipped, errored int64
		var order []string
		values := map[string][]scored{}
		specs := map[string]*evalsiv1alpha1.MetricSpec{}
		for _, o := range in.manifest.GetOutputs() {
			key := metricKey(in, o.GetName())
			if _, ok := values[key]; !ok {
				order = append(order, key)
				values[key] = nil
			}
			specs[key] = o
		}
		for _, r := range results {
			if r.GetEvaluator() != in.name {
				continue
			}
			switch r.GetOutcome() {
			case evalsiv1alpha1.Outcome_OUTCOME_SKIPPED:
				skipped++
			case evalsiv1alpha1.Outcome_OUTCOME_ERROR:
				errored++
			}
			for _, sc := range r.GetScores() {
				key := metricKey(in, sc.GetName())
				if _, ok := values[key]; !ok {
					order = append(order, key)
				}
				values[key] = append(values[key], scored{score: sc, recordID: r.GetRecordId()})
			}
		}
		for _, key := range order {
			sum, err := summarizeMetric(key, in, specs[key], values[key], byID, level, clusterBy, method)
			if err != nil {
				return nil, err
			}
			if sum != nil {
				sum.Skipped, sum.Errors = skipped, errored
				out = append(out, sum)
			}
		}
	}
	return out, nil
}

func summarizeMetric(key string, in instance, spec *evalsiv1alpha1.MetricSpec, values []scored, byID map[string]*evalsiv1alpha1.Record, level float64, clusterBy, method string) (*evalsiv1alpha1.MetricSummary, error) {
	typ := scoreType(spec, values)
	sum := &evalsiv1alpha1.MetricSummary{Metric: key, Evaluator: in.name}
	switch typ {
	case evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER:
		sum.Kind = evalsiv1alpha1.MetricKind_METRIC_KIND_NUMBER
	case evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED:
		sum.Kind = evalsiv1alpha1.MetricKind_METRIC_KIND_PROPORTION
	case evalsiv1alpha1.ScoreType_SCORE_TYPE_LABEL:
		sum.Kind = evalsiv1alpha1.MetricKind_METRIC_KIND_LABEL
		sum.Labels = map[string]int64{}
		for _, v := range values {
			sum.Labels[v.score.GetLabel()]++
		}
		sum.N = int64(len(values))
		return sum, nil
	default:
		return nil, nil // structured scores stay per record
	}
	var nums []float64
	var ids []string
	for _, v := range values {
		if x, ok := numeric(v.score); ok {
			nums = append(nums, x)
			ids = append(ids, v.recordID)
		}
	}
	sum.N = int64(len(nums))
	if len(nums) == 0 {
		return sum, nil
	}
	sum.Mean = proto.Float64(stats.Mean(nums))
	if len(nums) > 1 {
		sum.Std = proto.Float64(stats.StdDev(nums))
	}
	if in.dataset() {
		return sum, nil // one value over the whole dataset; no sampling interval
	}
	var clusters []string
	if clusterBy != "" {
		distinct := map[string]bool{}
		for _, id := range ids {
			c := clusterOf(byID[id], clusterBy, id)
			clusters = append(clusters, c)
			distinct[c] = true
		}
		sum.Clusters = proto.Int64(int64(len(distinct)))
	}
	iv, ok, err := stats.ForMetric(nums, typ == evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED, level, clusters, method)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if !ok {
		return sum, nil
	}
	if typ == evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED {
		lo, hi := 0.0, 1.0
		iv = iv.Clipped(&lo, &hi)
	} else if spec != nil {
		iv = iv.Clipped(spec.Min, spec.Max)
	}
	if math.IsNaN(iv.Low) || math.IsNaN(iv.High) {
		return sum, nil
	}
	sum.Ci = &evalsiv1alpha1.ConfidenceInterval{Low: iv.Low, High: iv.High, Level: iv.Level, Method: iv.Method}
	return sum, nil
}
