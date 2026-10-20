package runs

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/datasetcatalog"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/stats"
	"google.golang.org/protobuf/proto"
)

var releasePin = regexp.MustCompile(`^[0-9a-f]{64}$`)
var releaseCommit = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

const maxReleaseCells = 100000
const maxReleaseChanges = 200

func validateReleasePolicy(p *pb.ReleasePolicy) error {
	if proto.Size(p) > 2<<20 {
		return invalid("release policy exceeds 2 MiB")
	}
	if !datasets.ValidName(p.GetName()) || !datasets.ValidName(p.GetVersion()) {
		return invalid("policy needs safe name/version")
	}
	ref := p.GetDataset()
	if !datasets.ValidName(ref.GetProject()) || !datasets.ValidName(ref.GetName()) || !datasets.ValidName(ref.GetVersion()) || !releasePin.MatchString(ref.GetSha256()) {
		return invalid("policy dataset requires explicit project/name/version/SHA-256")
	}
	if p.GetMinRecords() < 2 || p.GetMinRecords() > 10000 {
		return invalid("min_records must be 2-10000 independent records")
	}
	level := p.GetConfidenceLevel()
	if level != 0 && !(level > 0 && level < 1) {
		return invalid("confidence_level must be between 0 and 1")
	}
	if len(p.GetRules()) < 1 || len(p.GetRules()) > 32 {
		return invalid("policy needs 1-32 metric rules")
	}
	seen := map[string]bool{}
	for _, rule := range p.GetRules() {
		if rule.GetMetric() == "" || len(rule.GetMetric()) > 256 || seen[rule.GetMetric()] {
			return invalid("metric rules need unique nonempty names")
		}
		seen[rule.GetMetric()] = true
		if rule.GetDirection() != pb.MetricDirection_METRIC_DIRECTION_HIGHER && rule.GetDirection() != pb.MetricDirection_METRIC_DIRECTION_LOWER {
			return invalid("metric direction must be explicit")
		}
		if rule.MaxRegression == nil && rule.MinMean == nil && rule.MaxMean == nil && rule.MinCase == nil && rule.MaxCase == nil {
			return invalid("metric rule requires a threshold")
		}
		for _, bound := range []*float64{rule.MaxRegression, rule.MinMean, rule.MaxMean, rule.MinCase, rule.MaxCase} {
			if bound != nil && !finite(*bound) {
				return invalid("thresholds must be finite")
			}
		}
		if rule.MaxRegression != nil && rule.GetMaxRegression() < 0 {
			return invalid("max_regression must be nonnegative")
		}
		if rule.MinMean != nil && rule.MaxMean != nil && rule.GetMinMean() > rule.GetMaxMean() {
			return invalid("min_mean exceeds max_mean")
		}
		if rule.MinCase != nil && rule.MaxCase != nil && rule.GetMinCase() > rule.GetMaxCase() {
			return invalid("min_case exceeds max_case")
		}
		if len(rule.GetCaseIds()) > 10000 || (len(rule.GetCaseIds()) > 0 && rule.MinCase == nil && rule.MaxCase == nil) {
			return invalid("case_ids require critical case thresholds and are limited to 10000")
		}
		ids := map[string]bool{}
		for _, id := range rule.GetCaseIds() {
			if id == "" || len(id) > 256 || ids[id] {
				return invalid("critical case IDs must be unique nonempty strings")
			}
			ids[id] = true
		}
	}
	return nil
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type releaseValues struct {
	metrics map[string]map[[2]int]float64
	issues  []string
}

func snapshotValues(s *releaseSnapshot, d *pb.DatasetVersion) releaseValues {
	out := releaseValues{metrics: map[string]map[[2]int]float64{}}
	fail := func(reason string) {
		if len(out.issues) < 32 && !slices.Contains(out.issues, reason) {
			out.issues = append(out.issues, reason)
		}
	}
	run := s.run
	contract := run.GetEvaluationContract()
	trials := trialsOf(run.GetSpec())
	if run.GetImported() {
		fail("imported run is not server execution evidence")
	}
	if run.GetArchivedAt() != nil {
		fail("archived run has no complete record evidence")
	}
	if contract == nil || len(contract.GetEvaluators()) == 0 {
		fail("run predates captured evaluation contracts")
		return out
	}
	if len(s.records) != int(run.GetRecords()) || len(s.records) != len(d.GetContent().GetRecords()) {
		fail("run does not cover the full approved dataset")
		return out
	}
	if datasetHash(s.records) != run.GetDatasetSha256() {
		fail("stored source records differ from the admitted dataset hash")
	}
	for i, record := range s.records {
		original := proto.Clone(record).(*pb.Record)
		identity := record.GetMetadata()[datasetcatalog.IdentityKey].GetStructValue()
		if identity.GetFields()["sha256"].GetStringValue() != d.GetSha256() {
			fail("record lacks its admitted catalog identity")
		}
		delete(original.Metadata, datasetcatalog.IdentityKey)
		if !proto.Equal(original, d.GetContent().GetRecords()[i]) {
			fail("source record differs from approved catalog content")
			break
		}
	}
	evals := contract.GetEvaluators()
	for _, in := range evals {
		if in.GetManifest().GetScope() != pb.Scope_SCOPE_RECORD {
			fail("release checks currently require record-scoped evaluators")
		}
		if in.GetManifest().GetRequires().GetJudge() && in.GetJudgeSettingsSha256() == "" {
			fail("judge configuration identity was not captured")
		}
	}
	expected := len(s.records) * trials * len(evals)
	if len(s.results) != expected {
		fail("incomplete evaluator/record/trial result coverage")
	}
	seen := map[[3]int]bool{}
	for _, row := range s.results {
		if row.RecordIdx < 0 || row.RecordIdx >= len(s.records) || row.Trial < 0 || row.Trial >= trials || row.EvalIdx < 0 || row.EvalIdx >= len(evals) {
			fail("result has an unexpected record/trial/evaluator coordinate")
			continue
		}
		key := [3]int{row.RecordIdx, row.Trial, row.EvalIdx}
		if seen[key] {
			fail("duplicate result coordinate")
			continue
		}
		seen[key] = true
		result := row.Result
		bound := evals[row.EvalIdx]
		if result.GetRecordId() != s.records[row.RecordIdx].GetId() || int(result.GetTrial()) != row.Trial || result.GetEvaluator() != bound.GetName() || result.GetEvaluatorRef() != bound.GetManifest().GetName()+"@"+bound.GetManifest().GetVersion() {
			fail("result identity differs from captured evaluation contract")
			continue
		}
		if result.GetOutcome() != pb.Outcome_OUTCOME_SCORED {
			fail("skipped, errored or cancelled evaluation")
			continue
		}
		scoreNames := map[string]bool{}
		for _, score := range result.GetScores() {
			metric := evaluation.MetricKey(evaluation.Instance{Name: bound.GetName(), Manifest: bound.GetManifest()}, score.GetName())
			if strings.HasPrefix(metric, "usage.") {
				fail("evaluator metric uses the reserved usage namespace")
				continue
			}
			if scoreNames[metric] {
				fail("duplicate score metric")
				continue
			}
			scoreNames[metric] = true
			value := 0.0
			switch kind := score.GetValue().(type) {
			case *pb.Score_Number:
				value = kind.Number
			case *pb.Score_Passed:
				if kind.Passed {
					value = 1
				}
			default:
				continue
			}
			if !finite(value) {
				fail("nonfinite metric value")
				continue
			}
			if out.metrics[metric] == nil {
				out.metrics[metric] = map[[2]int]float64{}
			}
			cell := [2]int{row.RecordIdx, row.Trial}
			if _, exists := out.metrics[metric][cell]; exists {
				fail("metric name collides across evaluators")
				continue
			}
			out.metrics[metric][cell] = value
		}
	}
	for i, record := range s.records {
		for trial := 0; trial < trials; trial++ {
			observed := record
			if generates(run.GetSpec()) {
				output, ok := s.outputs[[2]int{i, trial}]
				if !ok || output.Record == nil || output.Error != "" || output.Record.GetId() != record.GetId() {
					fail("incomplete or failed target output coverage")
					continue
				}
				if !proto.Equal(output.Record.GetInput(), record.GetInput()) || !proto.Equal(output.Record.GetReference(), record.GetReference()) || !proto.Equal(&pb.Record{Context: output.Record.GetContext()}, &pb.Record{Context: record.GetContext()}) {
					fail("target output changed approved input, reference or context")
				}
				for _, key := range []string{datasetcatalog.IdentityKey, "evalsi.failure"} {
					if !proto.Equal(output.Record.GetMetadata()[key], record.GetMetadata()[key]) {
						fail("target output changed source identity metadata")
					}
				}
				observed = output.Record
			}
			usage := observed.GetUsage()
			if usage == nil {
				continue
			}
			for metric, value := range map[string]*float64{"usage.cost_usd": usage.CostUsd, "usage.latency_ms": nil} {
				if metric == "usage.latency_ms" && usage.GetLatency() != nil {
					ms := float64(usage.GetLatency().AsDuration()) / 1e6
					value = &ms
				}
				if value == nil || !finite(*value) || *value < 0 {
					continue
				}
				if out.metrics[metric] == nil {
					out.metrics[metric] = map[[2]int]float64{}
				}
				out.metrics[metric][[2]int{i, trial}] = *value
			}
		}
	}
	return out
}

func metricDecision(rule *pb.ReleaseMetricRule, policy *pb.ReleasePolicy, a, b releaseValues, records []*pb.Record, trials int) *pb.ReleaseMetricDecision {
	out := &pb.ReleaseMetricDecision{Metric: rule.GetMetric(), Outcome: pb.ReleaseOutcome_RELEASE_OUTCOME_PASS}
	av, bv := a.metrics[rule.GetMetric()], b.metrics[rule.GetMetric()]
	expected := len(records) * trials
	for i := range records {
		complete := true
		for trial := 0; trial < trials; trial++ {
			_, beforeOK := av[[2]int{i, trial}]
			_, afterOK := bv[[2]int{i, trial}]
			if !beforeOK || !afterOK {
				complete = false
				break
			}
		}
		if complete {
			out.PairedRecords++
		}
	}
	if len(av) != expected || len(bv) != expected || len(records) < int(policy.GetMinRecords()) {
		out.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT
		out.Reasons = []string{"metric lacks full record/trial coverage or minimum independent records"}
		return out
	}
	selected := map[string]bool{}
	for _, id := range rule.GetCaseIds() {
		selected[id] = false
	}
	var bases, cands, regressions []float64
	for i, record := range records {
		base, cand := 0.0, 0.0
		for trial := 0; trial < trials; trial++ {
			before, beforeOK := av[[2]int{i, trial}]
			after, afterOK := bv[[2]int{i, trial}]
			if !beforeOK || !afterOK {
				out.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT
				out.Reasons = []string{"metric pairing is incomplete"}
				return out
			}
			base += before
			cand += after
			_, criticalSelected := selected[record.GetId()]
			if criticalSelected {
				selected[record.GetId()] = true
			}
			critical := (len(selected) == 0 || criticalSelected) && ((rule.MinCase != nil && after < rule.GetMinCase()) || (rule.MaxCase != nil && after > rule.GetMaxCase()))
			loss := before - after
			if rule.GetDirection() == pb.MetricDirection_METRIC_DIRECTION_LOWER {
				loss = after - before
			}
			if !finite(loss) {
				out.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT
				out.Reasons = []string{"metric regression overflowed"}
				return out
			}
			if loss > 0 || critical {
				if len(out.Changes) < maxReleaseChanges {
					out.Changes = append(out.Changes, &pb.ReleaseCaseChange{RecordId: record.GetId(), Trial: int32(trial), Baseline: before, Candidate: after, CriticalFailure: critical})
				} else {
					out.ChangesOmitted++
				}
			}
			if critical {
				out.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK
			}
		}
		base /= float64(trials)
		cand /= float64(trials)
		bases = append(bases, base)
		cands = append(cands, cand)
		loss := base - cand
		if rule.GetDirection() == pb.MetricDirection_METRIC_DIRECTION_LOWER {
			loss = cand - base
		}
		regressions = append(regressions, loss)
	}
	for _, found := range selected {
		if !found {
			out.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT
			out.Reasons = append(out.Reasons, "selected critical case is absent")
			return out
		}
	}
	out.PairedRecords = int64(len(records))
	out.BaselineMean = proto.Float64(stats.Mean(bases))
	out.CandidateMean = proto.Float64(stats.Mean(cands))
	out.MeanRegression = proto.Float64(stats.Mean(regressions))
	if out.GetOutcome() == pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK {
		out.Reasons = append(out.Reasons, "critical per-case threshold failed")
	}
	if rule.MinMean != nil && out.GetCandidateMean() < rule.GetMinMean() || rule.MaxMean != nil && out.GetCandidateMean() > rule.GetMaxMean() {
		out.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK
		out.Reasons = append(out.Reasons, "candidate mean violates an absolute threshold")
	}
	if !finite(out.GetBaselineMean()) || !finite(out.GetCandidateMean()) || !finite(out.GetMeanRegression()) {
		out.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT
		out.Reasons = append(out.Reasons, "metric aggregation overflowed")
		return out
	}
	level := policy.GetConfidenceLevel()
	if level == 0 {
		level = .95
	}
	interval, ok := stats.TInterval(regressions, level)
	if ok && finite(interval.Low) && finite(interval.High) {
		out.RegressionLow = proto.Float64(interval.Low)
		out.RegressionHigh = proto.Float64(interval.High)
	}
	if rule.MaxRegression != nil {
		if out.RegressionHigh == nil {
			out.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_INSUFFICIENT
			out.Reasons = append(out.Reasons, "paired regression interval unavailable")
		} else if out.GetRegressionHigh() > rule.GetMaxRegression() {
			out.Outcome = pb.ReleaseOutcome_RELEASE_OUTCOME_BLOCK
			out.Reasons = append(out.Reasons, fmt.Sprintf("paired regression upper bound exceeds %.6g", rule.GetMaxRegression()))
		}
	}
	return out
}
