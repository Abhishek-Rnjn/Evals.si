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
