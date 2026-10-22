// Package quality turns the scores online policies store into per-group
// quality figures (per route, backend or service) that something outside
// Evals.si can act on: a JSON endpoint and Prometheus gauges. It is the
// Evals.si half of feeding scores back into agentgateway (decision 0038);
// what consumes the figures is the operator's.
package quality

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// States of a group, for a spec with a metric and threshold.
const (
	StateOK               = "ok"
	StateDegraded         = "degraded"
	StateInsufficientData = "insufficient_data"
)

// Limits on one report.
const (
	DefaultMaxGroups = 50
	MaxGroupsLimit   = 500
	// Traces read for one report; past it the report says it is truncated.
	MaxTraces = 20000
	scanPage  = 500
)

// Spec selects the traces of a policy over a window and how to group them.
type Spec struct {
	Project string
	// The policy whose stored results are used (required).
	Policy string
	// Only this metric (a score name as the policy's stages produce it); empty
	// reports every metric. Required with Threshold.
	Metric string
	// "service", "name" (the root span's name), or "label:<key>".
	GroupBy string
	// How far back traces started (default 1h).
	Window time.Duration
	// Groups with fewer scored traces are insufficient_data (default 20).
	MinTraces int
	// At most this many groups, the busiest first (default 50).
	MaxGroups int
	// With Metric: groups whose mean is below it are degraded.
	Threshold *float64
}

// Normalize fills defaults and rejects what cannot be answered.
func (s *Spec) Normalize() error {
	if s.Policy == "" {
		return invalid("policy is required")
	}
	if s.GroupBy == "" {
		s.GroupBy = "service"
	}
	if k, ok := strings.CutPrefix(s.GroupBy, "label:"); (ok && k == "") || (!ok && s.GroupBy != "service" && s.GroupBy != "name") {
		return invalid("group_by must be service, name or label:<key>, got %q", s.GroupBy)
	}
	if s.Window <= 0 {
		s.Window = time.Hour
	}
	if s.Window > 7*24*time.Hour {
		return invalid("window must be at most 168h")
	}
	if s.MinTraces <= 0 {
		s.MinTraces = 20
	}
	if s.MaxGroups <= 0 {
		s.MaxGroups = DefaultMaxGroups
	}
	if s.MaxGroups > MaxGroupsLimit {
		return invalid("max_groups must be at most %d", MaxGroupsLimit)
	}
	if s.Threshold != nil && s.Metric == "" {
		return invalid("threshold needs metric")
	}
	if s.Threshold != nil && (math.IsNaN(*s.Threshold) || math.IsInf(*s.Threshold, 0)) {
		return invalid("threshold must be a number")
	}
	return nil
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// MetricStat is one metric over a group's scored traces.
type MetricStat struct {
	// Traces that have the metric.
	Traces int     `json:"traces"`
	Mean   float64 `json:"mean"`
}

// Group is one route, backend or service.
type Group struct {
	Group string `json:"group"`
	// Traces of the group with results from the policy, and the share of
	// them whose root span is an error.
	Traces    int                   `json:"traces"`
	ErrorRate float64               `json:"error_rate"`
	Metrics   map[string]MetricStat `json:"metrics"`
	// Set when the spec has a metric: ok, degraded or insufficient_data.
	State string `json:"state,omitempty"`
}

// Report is what the endpoint returns.
type Report struct {
	Project       string    `json:"project,omitempty"`
	Policy        string    `json:"policy"`
	Metric        string    `json:"metric,omitempty"`
	GroupBy       string    `json:"group_by"`
	WindowSeconds int64     `json:"window_seconds"`
	MinTraces     int       `json:"min_traces"`
	Threshold     *float64  `json:"threshold,omitempty"`
	GeneratedAt   time.Time `json:"generated_at"`
	Groups        []Group   `json:"groups"`
	// Groups and traces left out by max_groups.
	OmittedGroups int `json:"omitted_groups,omitempty"`
	// More traces were in the window than one report reads (MaxTraces): the
	// figures cover the newest ones.
	Truncated bool `json:"truncated,omitempty"`
}

// Scan yields the scored traces that started at or after since, newest
// first, until yield returns false or they run out. It reports whether it
// stopped early because it reached MaxTraces.
type Scan func(ctx context.Context, since time.Time, yield func(*evalsiv1alpha1.TraceSummary, []*evalsiv1alpha1.PolicyResults) bool) (truncated bool, err error)

// ScoresOf turns a policy's results for a trace into scores by metric name
// (watch.Engine.TraceScores).
type ScoresOf func(policy string, results []*evalsiv1alpha1.EvaluationResult) map[string]float64

type acc struct {
	traces, errors int
	sum            map[string]float64
	n              map[string]int
}

// Build reads the spec's window through scan and aggregates it.
func Build(ctx context.Context, spec Spec, scan Scan, scores ScoresOf, now time.Time) (*Report, error) {
	if err := spec.Normalize(); err != nil {
		return nil, err
	}
	groups := map[string]*acc{}
	truncated, err := scan(ctx, now.Add(-spec.Window), func(sum *evalsiv1alpha1.TraceSummary, policies []*evalsiv1alpha1.PolicyResults) bool {
		var results []*evalsiv1alpha1.EvaluationResult
		for _, p := range policies {
			if p.GetPolicy() == spec.Policy {
				results = append(results, p.GetResults()...)
			}
		}
		if len(results) == 0 {
			return true
		}
		key := groupKey(spec.GroupBy, sum)
		a := groups[key]
		if a == nil {
			a = &acc{sum: map[string]float64{}, n: map[string]int{}}
			groups[key] = a
		}
		a.traces++
		if sum.GetError() {
			a.errors++
		}
		for metric, v := range scores(spec.Policy, results) {
			if spec.Metric != "" && metric != spec.Metric {
				continue
			}
			a.sum[metric] += v
			a.n[metric]++
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	r := &Report{
		Project: spec.Project, Policy: spec.Policy, Metric: spec.Metric, GroupBy: spec.GroupBy,
		WindowSeconds: int64(spec.Window / time.Second), MinTraces: spec.MinTraces, Threshold: spec.Threshold,
		GeneratedAt: now.UTC(), Truncated: truncated, Groups: []Group{},
	}
	for key, a := range groups {
		g := Group{Group: key, Traces: a.traces, ErrorRate: float64(a.errors) / float64(a.traces), Metrics: map[string]MetricStat{}}
		for metric, n := range a.n {
			g.Metrics[metric] = MetricStat{Traces: n, Mean: a.sum[metric] / float64(n)}
		}
		if spec.Metric != "" {
			st := g.Metrics[spec.Metric]
			switch {
			case st.Traces < spec.MinTraces:
				g.State = StateInsufficientData
			case spec.Threshold != nil && st.Mean < *spec.Threshold:
				g.State = StateDegraded
			default:
				g.State = StateOK
			}
		}
		r.Groups = append(r.Groups, g)
	}
	sort.Slice(r.Groups, func(i, j int) bool {
		if r.Groups[i].Traces != r.Groups[j].Traces {
			return r.Groups[i].Traces > r.Groups[j].Traces
		}
		return r.Groups[i].Group < r.Groups[j].Group
	})
	if len(r.Groups) > spec.MaxGroups {
		r.OmittedGroups = len(r.Groups) - spec.MaxGroups
		r.Groups = r.Groups[:spec.MaxGroups]
	}
	return r, nil
}

// NoLabel is the group of traces that lack the label grouped by.
const NoLabel = "(none)"

func groupKey(by string, sum *evalsiv1alpha1.TraceSummary) string {
	var v string
	switch {
	case by == "service":
		v = sum.GetService()
	case by == "name":
		v = sum.GetName()
	default:
		v = sum.GetLabels()[strings.TrimPrefix(by, "label:")]
	}
	if v == "" {
		return NoLabel
	}
	return v
}

// StoreScan reads scored traces straight from the store, for what the server
// itself exports (no caller to authorize). projects nil reads every project.
func StoreScan(st *store.Store, projects []string, policy string) Scan {
	return func(ctx context.Context, since time.Time, yield func(*evalsiv1alpha1.TraceSummary, []*evalsiv1alpha1.PolicyResults) bool) (bool, error) {
		q := store.ScoreQuery{Projects: projects, Policy: policy, Since: since, Limit: scanPage}
		read := 0
		for {
			traces, err := st.ScoredTraces(ctx, q)
			if err != nil {
				return false, err
			}
			for _, t := range traces {
				if !yield(t.Summary, t.Policies) {
					return false, nil
				}
			}
			read += len(traces)
			if len(traces) < scanPage {
				return false, nil
			}
			if read >= MaxTraces {
				return true, nil
			}
			c := store.CursorOf(traces[len(traces)-1].Summary)
			q.After = &c
		}
	}
}

// ListScores is the part of TraceService the endpoint reads through, so the
// caller's access rules apply to every trace counted.
type ListScores interface {
	ListScores(ctx context.Context, req *connect.Request[evalsiv1alpha1.ListScoresRequest]) (*connect.Response[evalsiv1alpha1.ListScoresResponse], error)
}

// ServiceScan reads through ListScores with the caller's context.
func ServiceScan(ts ListScores, project, policy string) Scan {
	return func(ctx context.Context, since time.Time, yield func(*evalsiv1alpha1.TraceSummary, []*evalsiv1alpha1.PolicyResults) bool) (bool, error) {
		req := &evalsiv1alpha1.ListScoresRequest{Project: project, Policy: policy, PageSize: scanPage}
		req.Since = timestamppb.New(since)
		read := 0
		for {
			resp, err := ts.ListScores(ctx, connect.NewRequest(req))
			if err != nil {
				return false, err
			}
			for _, t := range resp.Msg.GetTraces() {
				if !yield(t.GetTrace(), t.GetPolicies()) {
					return false, nil
				}
			}
			read += len(resp.Msg.GetTraces())
			if resp.Msg.GetNextPageToken() == "" {
				return false, nil
			}
			if read >= MaxTraces {
				return true, nil
			}
			req.PageToken = resp.Msg.GetNextPageToken()
		}
	}
}
