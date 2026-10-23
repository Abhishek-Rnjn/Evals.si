package quality

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

var now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func scored(route string, isError bool, judge float64) (*evalsiv1alpha1.TraceSummary, []*evalsiv1alpha1.PolicyResults) {
	sum := &evalsiv1alpha1.TraceSummary{
		Project: "p", TraceId: fmt.Sprintf("%032x", time.Now().UnixNano()), Service: "gateway", Name: "chat",
		StartTime: timestamppb.New(now.Add(-time.Minute)), Error: isError, Labels: map[string]string{"route": route},
	}
	return sum, []*evalsiv1alpha1.PolicyResults{
		{Policy: "other", Results: []*evalsiv1alpha1.EvaluationResult{{Evaluator: "judge"}}},
		{Policy: "prod", Results: []*evalsiv1alpha1.EvaluationResult{{Evaluator: "judge", Scores: []*evalsiv1alpha1.Score{
			{Name: "judge", Value: &evalsiv1alpha1.Score_Number{Number: judge}},
		}}}},
	}
}

// scoresOf stands in for watch.Engine.TraceScores.
func scoresOf(_ string, results []*evalsiv1alpha1.EvaluationResult) map[string]float64 {
	out := map[string]float64{}
	for _, r := range results {
		for _, s := range r.GetScores() {
			out[r.GetEvaluator()] = s.GetNumber()
		}
	}
	return out
}

type item struct {
	sum *evalsiv1alpha1.TraceSummary
	pr  []*evalsiv1alpha1.PolicyResults
}

func fakeScan(items []item, truncated bool) Scan {
	return func(_ context.Context, since time.Time, yield func(*evalsiv1alpha1.TraceSummary, []*evalsiv1alpha1.PolicyResults) bool) (bool, error) {
		for _, it := range items {
			if it.sum.GetStartTime().AsTime().Before(since) {
				continue
			}
			if !yield(it.sum, it.pr) {
				break
			}
		}
		return truncated, nil
	}
}

func TestBuildGroupsScoresAndStates(t *testing.T) {
	var items []item
	add := func(route string, n int, isError bool, judge float64) {
		for range n {
			s, p := scored(route, isError, judge)
			items = append(items, item{s, p})
		}
	}
	add("billing", 30, false, 0.9)
	add("refunds", 25, false, 0.5)
	add("refunds", 5, true, 0.7)
	add("search", 3, false, 0.1) // too few to act on
	s, _ := scored("old", false, 1)
	s.StartTime = timestamppb.New(now.Add(-3 * time.Hour)) // outside the window
	items = append(items, item{s, []*evalsiv1alpha1.PolicyResults{{Policy: "prod", Results: []*evalsiv1alpha1.EvaluationResult{{Evaluator: "judge"}}}}})
	// A trace the policy never scored is not counted.
	un, _ := scored("billing", false, 0)
	items = append(items, item{un, []*evalsiv1alpha1.PolicyResults{{Policy: "other"}}})

	threshold := 0.8
	r, err := Build(t.Context(), Spec{Policy: "prod", Metric: "judge", GroupBy: "label:route", Threshold: &threshold}, fakeScan(items, false), scoresOf, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Groups) != 3 || r.Groups[0].Group != "billing" || r.Groups[1].Group != "refunds" || r.Groups[2].Group != "search" {
		t.Fatalf("groups: %+v", r.Groups)
	}
	b, rf, se := r.Groups[0], r.Groups[1], r.Groups[2]
	if b.Traces != 30 || b.State != StateOK || b.Metrics["judge"].Mean < 0.899 || b.Metrics["judge"].Mean > 0.901 {
		t.Errorf("billing: %+v", b)
	}
	if rf.Traces != 30 || rf.State != StateDegraded || rf.ErrorRate < 0.166 || rf.ErrorRate > 0.167 {
		t.Errorf("refunds: %+v", rf)
	}
	if want := (25*0.5 + 5*0.7) / 30; rf.Metrics["judge"].Mean < want-1e-9 || rf.Metrics["judge"].Mean > want+1e-9 {
		t.Errorf("refunds mean %v want %v", rf.Metrics["judge"].Mean, want)
	}
	if se.State != StateInsufficientData {
		t.Errorf("search: %+v", se)
	}
	if r.WindowSeconds != 3600 || r.MinTraces != 20 || r.Truncated {
		t.Errorf("report: %+v", r)
	}

	// max_groups keeps the busiest and says what it left out; a truncated scan says so.
	r, err = Build(t.Context(), Spec{Policy: "prod", GroupBy: "label:route", MaxGroups: 1}, fakeScan(items, true), scoresOf, now)
	if err != nil || len(r.Groups) != 1 || r.OmittedGroups != 2 || !r.Truncated || r.Groups[0].State != "" {
		t.Fatalf("max_groups: %+v %v", r, err)
	}
	// Traces without the label share one group.
	r, _ = Build(t.Context(), Spec{Policy: "prod", GroupBy: "label:nope"}, fakeScan(items, false), scoresOf, now)
	if len(r.Groups) != 1 || r.Groups[0].Group != NoLabel || r.Groups[0].Traces != 63 {
		t.Fatalf("unlabelled: %+v", r.Groups)
	}
}

func TestSpecValidation(t *testing.T) {
	nan := 0.0
	for name, s := range map[string]Spec{
		"no policy":       {},
		"bad group_by":    {Policy: "p", GroupBy: "route"},
		"empty label":     {Policy: "p", GroupBy: "label:"},
		"huge window":     {Policy: "p", Window: 30 * 24 * time.Hour},
		"huge max_groups": {Policy: "p", MaxGroups: 1000},
		"threshold alone": {Policy: "p", Threshold: &nan},
	} {
		if err := s.Normalize(); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

type pagedScores struct {
	pages [][]*evalsiv1alpha1.TraceScores
}

func (p *pagedScores) ListScores(_ context.Context, req *connect.Request[evalsiv1alpha1.ListScoresRequest]) (*connect.Response[evalsiv1alpha1.ListScoresResponse], error) {
	i := 0
	if req.Msg.GetPageToken() != "" {
		fmt.Sscanf(req.Msg.GetPageToken(), "%d", &i)
	}
	resp := &evalsiv1alpha1.ListScoresResponse{Traces: p.pages[i]}
	if i+1 < len(p.pages) {
		resp.NextPageToken = fmt.Sprint(i + 1)
	}
	return connect.NewResponse(resp), nil
}

func TestServiceScanPagesThroughListScores(t *testing.T) {
	var pages [][]*evalsiv1alpha1.TraceScores
	for range 3 {
		var page []*evalsiv1alpha1.TraceScores
		for range 4 {
			s, p := scored("billing", false, 0.8)
			page = append(page, &evalsiv1alpha1.TraceScores{Trace: s, Policies: p})
		}
		pages = append(pages, page)
	}
	r, err := Build(t.Context(), Spec{Policy: "prod", GroupBy: "label:route"}, ServiceScan(&pagedScores{pages}, "p", "prod"), scoresOf, now)
	if err != nil || len(r.Groups) != 1 || r.Groups[0].Traces != 12 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestStoreScanAndExporter(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(filepath.Join(t.TempDir(), "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	put := func(project, id, route string, judge float64, age time.Duration) {
		sum := &evalsiv1alpha1.TraceSummary{Project: project, TraceId: id, Service: "gateway", Name: "chat",
			StartTime: timestamppb.New(now.Add(-age)), Labels: map[string]string{"route": route}}
		if err := st.PutTrace(ctx, sum, &evalsiv1alpha1.Record{Id: id}); err != nil {
			t.Fatal(err)
		}
		res := []*evalsiv1alpha1.EvaluationResult{{Evaluator: "judge", Scores: []*evalsiv1alpha1.Score{{Name: "judge", Value: &evalsiv1alpha1.Score_Number{Number: judge}}}}}
		if err := st.PutTraceResults(ctx, project, id, "prod", res); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 6 {
		put("p", fmt.Sprintf("a%d", i), "billing", 0.9, time.Duration(i+1)*time.Minute)
		put("p", fmt.Sprintf("b%d", i), "refunds", 0.4, time.Duration(i+1)*time.Minute)
	}
	put("p", "old", "billing", 0.0, 5*time.Hour)
	put("other", "x", "billing", 0.0, time.Minute)

	r, err := Build(ctx, Spec{Project: "p", Policy: "prod", Metric: "judge", GroupBy: "label:route", MinTraces: 5}, StoreScan(st, []string{"p"}, "prod"), scoresOf, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Groups) != 2 || r.Groups[0].Traces != 6 || r.Groups[1].Traces != 6 {
		t.Fatalf("store scan: %+v", r.Groups)
	}

	e := NewExporter([]config.QualityExport{{Name: "routes", Project: "p", Policy: "prod", Metric: "judge", GroupBy: "label:route", MinTraces: 5}}, st, scoresOf, slog.New(slog.DiscardHandler))
	e.now = func() time.Time { return now }
	e.Refresh(ctx, e.exports[0])
	var buf bytes.Buffer
	e.WriteMetrics(&buf)
	out := buf.String()
	for _, want := range []string{
		`evalsi_quality_score{export="routes",project="p",policy="prod",metric="judge",group="billing"} 0.9`,
		`evalsi_quality_score{export="routes",project="p",policy="prod",metric="judge",group="refunds"} 0.4`,
		`evalsi_quality_traces{export="routes",project="p",policy="prod",group="billing"} 6`,
		`evalsi_quality_error_rate{export="routes",project="p",policy="prod",group="billing"} 0`,
		`evalsi_quality_report_age_seconds{export="routes"} 0`,
		`evalsi_quality_export_failures_total{export="routes"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
	// A group with too few traces gets no score gauge.
	e2 := NewExporter([]config.QualityExport{{Name: "strict", Project: "p", Policy: "prod", GroupBy: "label:route", MinTraces: 50}}, st, scoresOf, slog.New(slog.DiscardHandler))
	e2.now = func() time.Time { return now }
	e2.Refresh(ctx, e2.exports[0])
	buf.Reset()
	e2.WriteMetrics(&buf)
	if strings.Contains(buf.String(), "evalsi_quality_score{") {
		t.Errorf("a score gauge for a group under min_traces:\n%s", buf.String())
	}
	// No exports: no output; a nil exporter is fine.
	buf.Reset()
	(*Exporter)(nil).WriteMetrics(&buf)
	NewExporter(nil, st, scoresOf, nil).WriteMetrics(&buf)
	if buf.Len() != 0 {
		t.Errorf("output without exports: %s", buf.String())
	}
}

func TestExportConfigValidation(t *testing.T) {
	cfg := config.Default()
	cfg.QualityExports = []config.QualityExport{{Name: "a", Policy: "p", GroupBy: "label:route"}, {Name: "a", Policy: "p"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Errorf("duplicate names: %v", err)
	}
	cfg.QualityExports = []config.QualityExport{{Name: "a"}, {Name: "b", Policy: "p", GroupBy: "route"}, {Name: "c", Policy: "p", Window: "1s"}}
	err := cfg.Validate()
	for _, want := range []string{"quality_exports[0]", "quality_exports[1].group_by", "quality_exports[2].window"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	cfg.QualityExports = []config.QualityExport{{Name: "ok", Policy: "p", GroupBy: "label:route", Window: "30m", Interval: "30s", MinTraces: 10, MaxGroups: 20}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid export: %v", err)
	}
}
