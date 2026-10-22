package evaluation

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

// timedWorker reports each result as having taken a second.
type timedWorker struct{ fakeWorker }

func (w *timedWorker) Evaluate(ctx context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	resp, err := w.fakeWorker.Evaluate(ctx, req)
	if err == nil {
		for _, r := range resp.GetResults() {
			r.Duration = durationpb.New(time.Second)
		}
	}
	return resp, err
}

type recordingMeter struct {
	refuse  error
	checks  []string
	charged map[string]time.Duration
}

func (m *recordingMeter) Check(_ context.Context, project, path string) error {
	m.checks = append(m.checks, project+"/"+path)
	return m.refuse
}

func (m *recordingMeter) Charge(project string, d time.Duration) {
	if m.charged == nil {
		m.charged = map[string]time.Duration{}
	}
	m.charged[project] += d
}

func inst(name, pool string) Instance {
	return Instance{Name: name, Manifest: &evalsiv1alpha1.EvaluatorManifest{
		Name: "builtin/" + name, Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD,
		Scheduling: &evalsiv1alpha1.Scheduling{Pool: pool},
	}}
}

func TestRunRecordsForMetersSandboxAndWasmOnly(t *testing.T) {
	svc := New(&timedWorker{}, catalog.New(nil), nil, "", config.Evaluate{BatchSize: 10, Parallelism: 2, MaxRecords: 100})
	meter := &recordingMeter{}
	svc.UseSandboxMeter(meter)
	recs := []*evalsiv1alpha1.Record{record("a", "x", "x", ""), record("b", "y", "y", "")}
	ctx := context.Background()

	// cpu-pool evaluators are not metered, and not even checked.
	if _, err := svc.RunRecordsFor(ctx, "p", "policy", []Instance{inst("exact-match", "cpu")}, recs); err != nil {
		t.Fatal(err)
	}
	if len(meter.checks) != 0 || len(meter.charged) != 0 {
		t.Fatalf("cpu work was metered: %v %v", meter.checks, meter.charged)
	}

	// A sandbox evaluator and a Wasm one are: 2 records x 1s each.
	for _, pool := range []string{"sandbox", "wasm"} {
		if _, err := svc.RunRecordsFor(ctx, "p", "guardrail", []Instance{inst("exact-match", pool)}, recs); err != nil {
			t.Fatal(err)
		}
	}
	if meter.charged["p"] != 4*time.Second || len(meter.checks) != 2 || meter.checks[0] != "p/guardrail" {
		t.Fatalf("checks %v, charged %v", meter.checks, meter.charged)
	}

	// A project over its quota is refused before anything runs.
	meter.refuse = connect.NewError(connect.CodeResourceExhausted, errors.New("over"))
	w := &timedWorker{}
	svc = New(w, catalog.New(nil), nil, "", config.Evaluate{BatchSize: 10, Parallelism: 2, MaxRecords: 100})
	svc.UseSandboxMeter(meter)
	_, err := svc.RunRecordsFor(ctx, "p", "reward", []Instance{inst("exact-match", "sandbox")}, recs)
	if connect.CodeOf(err) != connect.CodeResourceExhausted || w.calls.Load() != 0 {
		t.Fatalf("err %v, worker calls %d", err, w.calls.Load())
	}

	// Without a meter RunRecordsFor is RunRecords.
	svc = New(&timedWorker{}, catalog.New(nil), nil, "", config.Evaluate{BatchSize: 10, Parallelism: 2, MaxRecords: 100})
	if _, err := svc.RunRecordsFor(ctx, "p", "policy", []Instance{inst("exact-match", "sandbox")}, recs); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxTimeCountsOnlyMeteredEvaluators(t *testing.T) {
	insts := []Instance{inst("a", "sandbox"), inst("b", "cpu"), inst("c", "wasm")}
	results := []*evalsiv1alpha1.EvaluationResult{
		{Evaluator: "a", Duration: durationpb.New(2 * time.Second)},
		{Evaluator: "b", Duration: durationpb.New(9 * time.Second)},
		{Evaluator: "c", Duration: durationpb.New(time.Second)},
		{Evaluator: "a"},
	}
	if got := SandboxTime(insts, results); got != 3*time.Second {
		t.Fatalf("SandboxTime = %v", got)
	}
}
