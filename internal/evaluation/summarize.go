package evaluation

import (
	"fmt"
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

// This file mirrors Summarize() in the Python SDK's evalsi/results.py.

// MetricKey names a metric: the instance name, or "<instance>.<score>" for extra scores.
func MetricKey(in Instance, scoreName string) string {
	if scoreName == "" || scoreName == catalog.ShortName(in.Manifest.GetName()) {
		return in.Name
	}
	return in.Name + "." + scoreName
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
	if key == RecordCluster {
		return "\x00record:" + recordID
	}
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

// RecordCluster as cluster_by clusters by record id; runs use it for trials.
const RecordCluster = "@record"

// Summarize computes per-metric summaries. With trials above 1, intervals
// cluster by record unless opts names another key, and pass/fail metrics also
// get "<metric>.pass@k" and "<metric>.pass^k".
func Summarize(insts []Instance, results []*evalsiv1alpha1.EvaluationResult, records []*evalsiv1alpha1.Record, opts *evalsiv1alpha1.SummaryOptions, trials int) ([]*evalsiv1alpha1.MetricSummary, error) {
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
	if trials > 1 && clusterBy == "" {
		clusterBy = RecordCluster
	}
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
		for _, o := range in.Manifest.GetOutputs() {
			key := MetricKey(in, o.GetName())
			if _, ok := values[key]; !ok {
				order = append(order, key)
				values[key] = nil
			}
			specs[key] = o
		}
		for _, r := range results {
			if r.GetEvaluator() != in.Name {
				continue
			}
			switch r.GetOutcome() {
			case evalsiv1alpha1.Outcome_OUTCOME_SKIPPED:
				skipped++
			case evalsiv1alpha1.Outcome_OUTCOME_ERROR:
				errored++
			}
			for _, sc := range r.GetScores() {
				key := MetricKey(in, sc.GetName())
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
				if trials > 1 && sum.GetKind() == evalsiv1alpha1.MetricKind_METRIC_KIND_PROPORTION {
					out = append(out, passAtK(key, in.Name, values[key], trials, level)...)
				}
			}
		}
	}
	return out, nil
}

func summarizeMetric(key string, in Instance, spec *evalsiv1alpha1.MetricSpec, values []scored, byID map[string]*evalsiv1alpha1.Record, level float64, clusterBy, method string) (*evalsiv1alpha1.MetricSummary, error) {
	typ := scoreType(spec, values)
	sum := &evalsiv1alpha1.MetricSummary{Metric: key, Evaluator: in.Name}
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
	if in.Dataset() {
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

// passAtK mirrors _pass_at_k in the Python SDK: pass@k is passed in at least
// one trial, pass^k is passed in every trial.
func passAtK(key, evaluator string, values []scored, k int, level float64) []*evalsiv1alpha1.MetricSummary {
	var order []string
	byRecord := map[string][]bool{}
	for _, v := range values {
		p, ok := v.score.GetValue().(*evalsiv1alpha1.Score_Passed)
		if !ok {
			continue
		}
		if _, seen := byRecord[v.recordID]; !seen {
			order = append(order, v.recordID)
		}
		byRecord[v.recordID] = append(byRecord[v.recordID], p.Passed)
	}
	var out []*evalsiv1alpha1.MetricSummary
	for _, variant := range []struct {
		name string
		all  bool
	}{{fmt.Sprintf("pass@%d", k), false}, {fmt.Sprintf("pass^%d", k), true}} {
		var nums []float64
		for _, id := range order {
			passes := byRecord[id]
			hit := variant.all
			for _, p := range passes {
				if variant.all {
					hit = hit && p
				} else {
					hit = hit || p
				}
			}
			if hit {
				nums = append(nums, 1)
			} else {
				nums = append(nums, 0)
			}
		}
		sum := &evalsiv1alpha1.MetricSummary{
			Metric: key + "." + variant.name, Evaluator: evaluator,
			Kind: evalsiv1alpha1.MetricKind_METRIC_KIND_PROPORTION, N: int64(len(nums)),
		}
		if len(nums) > 0 {
			sum.Mean = proto.Float64(stats.Mean(nums))
			if len(nums) > 1 {
				sum.Std = proto.Float64(stats.StdDev(nums))
			}
			if iv, ok, _ := stats.ForMetric(nums, true, level, nil, "auto"); ok {
				sum.Ci = &evalsiv1alpha1.ConfidenceInterval{Low: iv.Low, High: iv.High, Level: iv.Level, Method: iv.Method}
			}
		}
		out = append(out, sum)
	}
	return out
}
