package runs

import (
	"math"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// NaN fails every comparison, so a gate checked only with < and > would pass it.
func TestGatesFailNonFiniteValues(t *testing.T) {
	gates := []*evalsiv1alpha1.Gate{
		{Metric: "nan", Min: proto.Float64(0.9)},
		{Metric: "nan", Max: proto.Float64(0.1)},
		{Metric: "inf", Max: proto.Float64(10)},
		{Metric: "fine", Min: proto.Float64(0.5)},
	}
	sums := []*evalsiv1alpha1.MetricSummary{
		{Metric: "nan", Mean: proto.Float64(math.NaN())},
		{Metric: "inf", Mean: proto.Float64(math.Inf(1))},
		{Metric: "fine", Mean: proto.Float64(0.75)},
	}
	got := CheckGates(gates, sums)
	for i, want := range []bool{false, false, false, true} {
		if got[i].GetPassed() != want {
			t.Errorf("gate %d passed = %v (%s), want %v", i, got[i].GetPassed(), got[i].GetReason(), want)
		}
	}
}

// A confidence level is refused when the run is created, not when it
// finishes: summarizing with NaN would panic in the run's goroutine.
func TestCreateRunRefusesBadConfidenceLevel(t *testing.T) {
	h := newHarness(t)
	for _, level := range []float64{math.NaN(), 1.5} {
		spec := &evalsiv1alpha1.RunSpec{
			Dataset: inline("easy"), Evaluators: refs("exact-match"),
			Summary: &evalsiv1alpha1.SummaryOptions{ConfidenceLevel: level},
		}
		if _, err := h.tryCreate("p", spec); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("level %v: %v", level, err)
		}
	}
}

// The mean leaves errored records out, so a candidate that errors on most
// records and passes the rest has a perfect mean; an error-rate gate fails it.
func TestErrorRateGate(t *testing.T) {
	sums := []*evalsiv1alpha1.MetricSummary{
		{Metric: "task-success", N: 1, Errors: 3, Mean: proto.Float64(1)},
		{Metric: "clean", N: 4, Mean: proto.Float64(1)},
		{Metric: "all-skipped"},
	}
	errorRate := evalsiv1alpha1.GateStat_GATE_STAT_ERROR_RATE
	got := CheckGates([]*evalsiv1alpha1.Gate{
		{Metric: "task-success", Min: proto.Float64(0.9)},
		{Metric: "task-success", Stat: errorRate, Max: proto.Float64(0.05)},
		{Metric: "clean", Stat: errorRate, Max: proto.Float64(0.05)},
		{Metric: "all-skipped", Stat: errorRate, Max: proto.Float64(0.05)},
	}, sums)
	if !got[0].GetPassed() {
		t.Errorf("mean gate: %v", got[0])
	}
	if got[1].GetPassed() || got[1].GetValue() != 0.75 {
		t.Errorf("3 of 4 errored: %v", got[1])
	}
	if !got[2].GetPassed() || got[2].GetValue() != 0 {
		t.Errorf("none errored: %v", got[2])
	}
	if got[3].GetPassed() || got[3].Value != nil {
		t.Errorf("nothing ran: %v", got[3])
	}
}

// A run whose target fails on one record of three fails a 5% error-rate gate,
// though its mean over the records that ran passes.
func TestRunFailsItsErrorRateGate(t *testing.T) {
	h := newHarness(t)
	run := h.wait(t, h.create(t, &evalsiv1alpha1.RunSpec{
		Target: target(), Dataset: inline("easy", "easy", "broken"), Evaluators: refs("exact-match"),
		Gates: []*evalsiv1alpha1.Gate{
			{Metric: "exact-match", Min: proto.Float64(0.9)},
			{Metric: "exact-match", Stat: evalsiv1alpha1.GateStat_GATE_STAT_ERROR_RATE, Max: proto.Float64(0.05)},
		},
	}).GetId())
	if run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED {
		t.Fatalf("status %v (%s)", run.GetStatus(), run.GetError())
	}
	gates := run.GetGates()
	if !gates[0].GetPassed() || gates[1].GetPassed() || math.Abs(gates[1].GetValue()-1.0/3) > 1e-9 {
		t.Errorf("gates = %v", gates)
	}
}

func TestCreateRunRefusesUnknownGateStat(t *testing.T) {
	h := newHarness(t)
	spec := &evalsiv1alpha1.RunSpec{
		Dataset: inline("easy"), Evaluators: refs("exact-match"),
		Gates: []*evalsiv1alpha1.Gate{{Metric: "exact-match", Stat: 99, Max: proto.Float64(1)}},
	}
	if _, err := h.tryCreate("p", spec); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown stat: %v", err)
	}
}
