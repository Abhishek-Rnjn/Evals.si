package runs

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func builtin(b *evalsiv1alpha1.BuiltinHarness) *evalsiv1alpha1.HarnessVariant {
	return &evalsiv1alpha1.HarnessVariant{Harness: b}
}

func variant(name string, b *evalsiv1alpha1.BuiltinHarness) *evalsiv1alpha1.HarnessVariant {
	v := builtin(b)
	v.Name = name
	return v
}

func sweepSpec(variants ...*evalsiv1alpha1.HarnessVariant) *evalsiv1alpha1.RunSpec {
	spec := agentSpec("pass", "fail")
	spec.Gates = nil
	spec.Harness.GetBuiltin().Tools = &evalsiv1alpha1.Tools{Builtin: []string{"grep"}}
	spec.Sweep = &evalsiv1alpha1.HarnessSweep{Variants: variants}
	return spec
}

func TestSweepExpandsIntoOneRunPerVariant(t *testing.T) {
	h := newHarness(t)
	spec := sweepSpec(
		variant("base", &evalsiv1alpha1.BuiltinHarness{}),
		variant("minimal", &evalsiv1alpha1.BuiltinHarness{Profile: "minimal", Tools: &evalsiv1alpha1.Tools{}}),
		variant("compact", &evalsiv1alpha1.BuiltinHarness{Context: &evalsiv1alpha1.ContextManagement{Strategy: "compact", WindowTokens: 8000}}),
	)
	resp, err := h.m.CreateRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{
		Name: "sweep", Project: "p", Spec: spec, Labels: map[string]string{"team": "x"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	runs := resp.Msg.GetSweepRuns()
	if len(runs) != 3 || resp.Msg.GetRun().GetId() != runs[0].GetId() {
		t.Fatalf("runs %v", runs)
	}
	sweep := runs[0].GetLabels()[SweepLabel]
	for i, name := range []string{"base", "minimal", "compact"} {
		run := h.wait(t, runs[i].GetId())
		if run.GetName() != "sweep/"+name || run.GetLabels()[VariantLabel] != name || run.GetLabels()[SweepLabel] != sweep || run.GetLabels()["team"] != "x" {
			t.Errorf("run %d: name %q labels %v", i, run.GetName(), run.GetLabels())
		}
		if run.GetSpec().GetSweep() != nil {
			t.Errorf("%s kept the sweep in its spec", name)
		}
		if run.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
			t.Errorf("%s: %v %s", name, run.GetStatus(), run.GetError())
		}
		if run.GetRecords() != 2 || summary(run, "task-success") == nil {
			t.Errorf("%s: records %d summaries %v", name, run.GetRecords(), run.GetSummaries())
		}
	}
	if sweep == "" || runs[1].GetLabels()[SweepLabel] != sweep {
		t.Errorf("sweep label %q", sweep)
	}
	b := func(i int) *evalsiv1alpha1.BuiltinHarness {
		run, err := h.st.GetRun(context.Background(), runs[i].GetId())
		if err != nil {
			t.Fatal(err)
		}
		return run.GetSpec().GetHarness().GetBuiltin()
	}
	// The baseline inherits everything; a set field replaces the base's whole field.
	if got := b(0).GetTools().GetBuiltin(); len(got) != 1 || got[0] != "grep" || b(0).GetMaxSteps() != 10 {
		t.Errorf("base harness %v", b(0))
	}
	if b(1).GetProfile() != "minimal" || len(b(1).GetTools().GetBuiltin()) != 0 || b(1).GetMaxSteps() != 10 {
		t.Errorf("minimal harness %v", b(1))
	}
	if b(2).GetContext().GetStrategy() != "compact" || b(2).GetTools().GetBuiltin()[0] != "grep" {
		t.Errorf("compact harness %v", b(2))
	}
	// The runs are comparable like any two runs, paired by record and trial.
	cmp, err := h.m.CompareRuns(context.Background(), connect.NewRequest(&evalsiv1alpha1.CompareRunsRequest{
		BaselineRunId: runs[0].GetId(), CandidateRunId: runs[1].GetId(),
	}))
	if err != nil || len(cmp.Msg.GetComparisons()) == 0 || cmp.Msg.GetComparisons()[0].GetPairedN() != 2 {
		t.Fatalf("compare: %v %v", cmp, err)
	}
}

func TestSweepValidation(t *testing.T) {
	h := newHarness(t)
	empty := &evalsiv1alpha1.BuiltinHarness{}
	for name, tc := range map[string]struct {
		mutate func(*evalsiv1alpha1.RunSpec)
		want   string
	}{
		"no variants": {func(s *evalsiv1alpha1.RunSpec) { s.Sweep.Variants = nil }, "from 1 to"},
		"too many":    {func(s *evalsiv1alpha1.RunSpec) { s.Sweep.Variants = manyVariants(maxSweepVariants + 1) }, "from 1 to"},
		"duplicate name": {func(s *evalsiv1alpha1.RunSpec) {
			s.Sweep.Variants = []*evalsiv1alpha1.HarnessVariant{variant("a", empty), variant("a", empty)}
		}, "appears twice"},
		"bad name": {func(s *evalsiv1alpha1.RunSpec) {
			s.Sweep.Variants = []*evalsiv1alpha1.HarnessVariant{variant("a b", empty)}
		}, "letters, digits"},
		"no harness": {func(s *evalsiv1alpha1.RunSpec) { s.Sweep.Variants = []*evalsiv1alpha1.HarnessVariant{{Name: "a"}} }, "sets no harness fields"},
		"a bad variant is named": {func(s *evalsiv1alpha1.RunSpec) {
			s.Sweep.Variants = []*evalsiv1alpha1.HarnessVariant{variant("ok", empty), variant("broken", &evalsiv1alpha1.BuiltinHarness{Profile: "nope", Tools: &evalsiv1alpha1.Tools{}})}
		}, `sweep variant "broken"`},
		"profile with the base's tools": {func(s *evalsiv1alpha1.RunSpec) {
			s.Sweep.Variants = []*evalsiv1alpha1.HarnessVariant{variant("p", &evalsiv1alpha1.BuiltinHarness{Profile: "minimal"})}
		}, "sets the toolset"},
		"a bring-your-own agent": {func(s *evalsiv1alpha1.RunSpec) {
			s.Target.Agent = &evalsiv1alpha1.AgentTarget{Kind: &evalsiv1alpha1.AgentTarget_Cli{Cli: &evalsiv1alpha1.CLIAgent{Command: []string{"x"}}}}
		}, "bring-your-own"},
		"shared recording directory": {func(s *evalsiv1alpha1.RunSpec) {
			rec := &evalsiv1alpha1.BuiltinHarness{Recording: &evalsiv1alpha1.Recording{Mode: "record", Dir: "same"}}
			s.Sweep.Variants = []*evalsiv1alpha1.HarnessVariant{variant("a", rec), variant("b", rec)}
		}, "same directory"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := sweepSpec(variant("one", empty))
			tc.mutate(spec)
			_, err := h.m.CreateRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "p", Spec: spec}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want it to mention %q", err, tc.want)
			}
		})
	}
	// Nothing was stored by the failures.
	list, err := h.m.ListRuns(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{Project: "p"}))
	if err != nil || len(list.Msg.GetRuns()) != 0 {
		t.Fatalf("runs %v %v", list, err)
	}
}

func manyVariants(n int) []*evalsiv1alpha1.HarnessVariant {
	var out []*evalsiv1alpha1.HarnessVariant
	for i := range n {
		out = append(out, variant("v"+strings.Repeat("x", i+1), &evalsiv1alpha1.BuiltinHarness{}))
	}
	return out
}
