package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/quality"
	"github.com/abhishek-rnjn/evals.si/internal/watch"
)

// registerQuality serves GET /v1alpha1/quality: an online policy's scores
// per group (service, root span name or trace label) over a window, as JSON,
// for whatever decides routing (decision 0038). It reads through
// TraceService.ListScores, so it needs the permission that does (traces.read)
// and counts only the traces the caller may read.
//
//	?policy=<name>        required
//	&project=<project>    default: every project the caller may read
//	&metric=<score>       default: every metric
//	&group_by=service|name|label:<key>   default service
//	&window=1h            at most 168h
//	&min_traces=20        groups with fewer scored traces are insufficient_data
//	&max_groups=50
//	&threshold=0.8        with metric: groups whose mean is below it are degraded
func registerQuality(mux *http.ServeMux, d deps) {
	procedure := evalsiv1alpha1connect.TraceServiceListScoresProcedure
	traces := watch.Traces{Store: d.store}
	mux.HandleFunc("GET /v1alpha1/quality", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for k := range q {
			switch k {
			case "project", "policy", "metric", "group_by", "window", "min_traces", "max_groups", "threshold":
			default:
				datasetHTTPError(w, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown query parameter %q", k)))
				return
			}
		}
		spec, err := qualitySpec(q.Get("project"), q.Get("policy"), q.Get("metric"), q.Get("group_by"), q.Get("window"), q.Get("min_traces"), q.Get("max_groups"), q.Get("threshold"))
		if err == nil {
			err = spec.Normalize()
		}
		if err != nil {
			datasetHTTPError(w, err)
			return
		}
		ctx, checker, err := d.gate.authenticate(r.Context(), procedure, "http", r.RemoteAddr, r.Header)
		if err == nil {
			err = d.gate.check(ctx, checker, procedure, &evalsiv1alpha1.ListScoresRequest{Project: spec.Project, Policy: spec.Policy})
		}
		if err != nil {
			datasetHTTPError(w, err)
			return
		}
		report, err := quality.Build(ctx, spec, quality.ServiceScan(traces, spec.Project, spec.Policy), d.watcher.TraceScores, time.Now())
		if err != nil {
			datasetHTTPError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	})
}

func qualitySpec(project, policy, metric, groupBy, window, minTraces, maxGroups, threshold string) (quality.Spec, error) {
	bad := func(name string, err error) error {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", name, err))
	}
	s := quality.Spec{Project: project, Policy: policy, Metric: metric, GroupBy: groupBy}
	if window != "" {
		d, err := time.ParseDuration(window)
		if err != nil || d <= 0 {
			return s, bad("window", errors.New("must be a duration such as 1h"))
		}
		s.Window = d
	}
	for name, p := range map[string]struct {
		raw string
		to  *int
	}{"min_traces": {minTraces, &s.MinTraces}, "max_groups": {maxGroups, &s.MaxGroups}} {
		if p.raw == "" {
			continue
		}
		n, err := strconv.Atoi(p.raw)
		if err != nil || n < 0 {
			return s, bad(name, errors.New("must be a whole number, 0 or more"))
		}
		*p.to = n
	}
	if threshold != "" {
		v, err := strconv.ParseFloat(threshold, 64)
		if err != nil {
			return s, bad("threshold", errors.New("must be a number"))
		}
		s.Threshold = &v
	}
	return s, nil
}
