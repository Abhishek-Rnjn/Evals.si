package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// ApplyPolicy implements MonitorService.
func (e *Engine) ApplyPolicy(ctx context.Context, req *connect.Request[evalsiv1alpha1.ApplyPolicyRequest]) (*connect.Response[evalsiv1alpha1.ApplyPolicyResponse], error) {
	p := req.Msg.GetPolicy()
	if p == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("policy is required"))
	}
	if req.Msg.GetValidateOnly() {
		if err := e.Validate(ctx, p); err != nil {
			return nil, err
		}
		return connect.NewResponse(&evalsiv1alpha1.ApplyPolicyResponse{Policy: p}), nil
	}
	if err := e.Apply(ctx, p); err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.ApplyPolicyResponse{Policy: p}), nil
}

// ListPolicies implements MonitorService. It lists only the policies the
// caller may read.
func (e *Engine) ListPolicies(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListPoliciesRequest]) (*connect.Response[evalsiv1alpha1.ListPoliciesResponse], error) {
	var out []*evalsiv1alpha1.OnlineEvalPolicy
	for _, p := range e.Policies(req.Msg.GetProject()) {
		if authz.Can(ctx, "policies.read", p.GetProject(), authz.PolicyResourceFor(ctx, p)) {
			out = append(out, p)
		}
	}
	return connect.NewResponse(&evalsiv1alpha1.ListPoliciesResponse{Policies: out}), nil
}

// DeletePolicy implements MonitorService.
func (e *Engine) DeletePolicy(ctx context.Context, req *connect.Request[evalsiv1alpha1.DeletePolicyRequest]) (*connect.Response[evalsiv1alpha1.DeletePolicyResponse], error) {
	if err := e.Delete(ctx, req.Msg.GetName()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.DeletePolicyResponse{}), nil
}

// GetPolicyStats implements MonitorService.
func (e *Engine) GetPolicyStats(_ context.Context, req *connect.Request[evalsiv1alpha1.GetPolicyStatsRequest]) (*connect.Response[evalsiv1alpha1.GetPolicyStatsResponse], error) {
	stats, ok := e.Stats(req.Msg.GetName())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no policy %q", req.Msg.GetName()))
	}
	return connect.NewResponse(&evalsiv1alpha1.GetPolicyStatsResponse{Stats: stats}), nil
}

// Traces implements TraceService over the store.
type Traces struct{ Store *store.Store }

// ListTraces implements TraceService. It lists only traces the caller may read.
func (t Traces) ListTraces(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListTracesRequest]) (*connect.Response[evalsiv1alpha1.ListTracesResponse], error) {
	size := int(req.Msg.GetPageSize())
	if size <= 0 || size > 500 {
		size = 50
	}
	projects := authz.Projects(ctx, "traces.read")
	if p := req.Msg.GetProject(); p != "" {
		if projects != nil && !slices.Contains(projects, p) {
			projects = []string{}
		} else {
			projects = []string{p}
		}
	}
	traces, next, err := t.Store.ListTraces(ctx, store.TraceFilter{Projects: projects, Service: req.Msg.GetService()}, size, req.Msg.GetPageToken())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	visible := traces[:0]
	for _, tr := range traces {
		if authz.Can(ctx, "traces.read", tr.GetProject(), authz.TraceResource(tr)) {
			visible = append(visible, tr)
		}
	}
	return connect.NewResponse(&evalsiv1alpha1.ListTracesResponse{Traces: visible, NextPageToken: next}), nil
}

// GetTrace implements TraceService. A trace id is unique within a project;
// without a project, the one readable trace with that id is returned.
func (t Traces) GetTrace(ctx context.Context, req *connect.Request[evalsiv1alpha1.GetTraceRequest]) (*connect.Response[evalsiv1alpha1.GetTraceResponse], error) {
	id := strings.ToLower(req.Msg.GetTraceId())
	notFound := connect.NewError(connect.CodeNotFound, fmt.Errorf("no trace %q", req.Msg.GetTraceId()))
	candidates := []string{req.Msg.GetProject()}
	if req.Msg.GetProject() == "" {
		var err error
		if candidates, err = t.Store.TraceProjects(ctx, id); err != nil {
			return nil, err
		}
	}
	var summary *evalsiv1alpha1.TraceSummary
	var record *evalsiv1alpha1.Record
	for _, project := range candidates {
		sum, rec, err := t.Store.GetTrace(ctx, project, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !authz.Can(ctx, "traces.read", project, authz.TraceResource(sum)) {
			continue
		}
		if summary != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("trace %q exists in projects %s and %s; set project", id, summary.GetProject(), project))
		}
		summary, record = sum, rec
	}
	if summary == nil {
		return nil, notFound
	}
	results, err := t.Store.TraceResults(ctx, summary.GetProject(), record.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&evalsiv1alpha1.GetTraceResponse{Record: record, Policies: results}), nil
}

// MetricsHandler serves Prometheus text-format metrics for ingest and policies.
func MetricsHandler(e *Engine, a *ingest.Assembler, extra ...func(io.Writer)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		var b strings.Builder
		metric := func(name, help, typ string) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		}
		metric("evalsi_traces_ingested_total", "Traces assembled from OTLP spans.", "counter")
		fmt.Fprintf(&b, "evalsi_traces_ingested_total %d\n", e.TracesIngested.Load())
		metric("evalsi_traces_dropped_total", "Traces not evaluated because the queue was full.", "counter")
		fmt.Fprintf(&b, "evalsi_traces_dropped_total %d\n", e.TracesDropped.Load())
		metric("evalsi_spans_dropped_total", "Spans dropped because a trace exceeded the span limit.", "counter")
		fmt.Fprintf(&b, "evalsi_spans_dropped_total %d\n", a.Dropped())
		metric("evalsi_traces_pending", "Traces waiting for late spans.", "gauge")
		fmt.Fprintf(&b, "evalsi_traces_pending %d\n", a.Pending())

		e.mu.Lock()
		names := make([]string, 0, len(e.policies))
		for n := range e.policies {
			names = append(names, n)
		}
		e.mu.Unlock()
		sort.Strings(names)
		metric("evalsi_policy_traces_total", "Traces per policy and stage of the pipeline.", "counter")
		var windows, alerts strings.Builder
		for _, n := range names {
			st, ok := e.Stats(n)
			if !ok {
				continue
			}
			for stage, v := range map[string]int64{"seen": st.TracesSeen, "matched": st.TracesMatched, "sampled": st.TracesSampled, "evaluated": st.TracesEvaluated, "promoted": st.TracesPromoted, "error": st.EvaluationErrors} {
				fmt.Fprintf(&b, "evalsi_policy_traces_total{policy=%s,stage=%s} %d\n", quote(n), quote(stage), v)
			}
			for _, m := range st.GetMetrics() {
				if m.Mean != nil {
					fmt.Fprintf(&windows, "evalsi_policy_metric_mean{policy=%s,metric=%s} %s\n", quote(n), quote(m.GetMetric()), strconv.FormatFloat(m.GetMean(), 'g', -1, 64))
				}
				fmt.Fprintf(&windows, "evalsi_policy_metric_samples{policy=%s,metric=%s} %d\n", quote(n), quote(m.GetMetric()), m.GetN())
			}
			for _, al := range st.GetAlerts() {
				v := 0
				if al.GetFiring() {
					v = 1
				}
				fmt.Fprintf(&alerts, "evalsi_policy_alert_firing{policy=%s,metric=%s} %d\n", quote(n), quote(al.GetAlert().GetMetric()), v)
			}
		}
		metric("evalsi_policy_metric_mean", "Window mean of an online metric.", "gauge")
		metric("evalsi_policy_metric_samples", "Values in the window of an online metric.", "gauge")
		b.WriteString(windows.String())
		metric("evalsi_policy_alert_firing", "1 while an alert fires.", "gauge")
		b.WriteString(alerts.String())
		for _, write := range extra {
			write(&b)
		}
		_, _ = io.WriteString(w, b.String())
	})
}

func quote(s string) string { return strconv.Quote(s) }
