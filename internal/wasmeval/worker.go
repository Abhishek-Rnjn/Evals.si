package wasmeval

import (
	"context"
	"fmt"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
)

// Worker sends calls for Wasm evaluators to the host and everything else to
// the wrapped worker, so Wasm evaluators work wherever evaluators do: the
// evaluation API, runs, online policies, guardrails and rewards.
type Worker struct {
	pluginhost.Worker
	Host *Host
}

// Wrap returns w with the host's evaluators added; with no host (or no
// plugins) it returns w.
func Wrap(w pluginhost.Worker, h *Host) pluginhost.Worker {
	if h == nil || len(h.Manifests()) == 0 {
		return w
	}
	return Worker{Worker: w, Host: h}
}

// Describe lists the inner worker's evaluators and the host's. A Python
// worker that loaded the same Wasm plugin itself (from ~/.evalsi/plugins)
// defers to the host; any other clash is an error.
func (w Worker) Describe(ctx context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	inner, err := w.Worker.Describe(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*evalsiv1alpha1.EvaluatorManifest, 0, len(inner))
	for _, m := range inner {
		if w.Host.Has(m.GetName()) {
			if m.GetRuntime() == "wasm" {
				continue
			}
			return nil, fmt.Errorf("evaluator %s is defined both by the worker and by a Wasm plugin", m.GetName())
		}
		out = append(out, m)
	}
	return append(out, w.Host.Manifests()...), nil
}

// Evaluate runs Wasm evaluators here.
func (w Worker) Evaluate(ctx context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	if w.Host.Has(req.GetEvaluator()) {
		return w.Host.Evaluate(ctx, req)
	}
	return w.Worker.Evaluate(ctx, req)
}

// Reduce runs dataset-scope Wasm evaluators here.
func (w Worker) Reduce(ctx context.Context, req *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	if w.Host.Has(req.GetEvaluator()) {
		return w.Host.Reduce(ctx, req)
	}
	return w.Worker.Reduce(ctx, req)
}
