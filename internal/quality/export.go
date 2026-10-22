package quality

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Exporter publishes the configured quality exports as Prometheus gauges.
// Each export is recomputed on its own interval from the stored results; the
// gauges show the last report, and a failed recomputation keeps the previous
// one (its age is evalsi_quality_report_age_seconds).
type Exporter struct {
	exports []config.QualityExport
	st      *store.Store
	scores  ScoresOf
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	reports map[string]*Report
	failed  map[string]int64
}

// NewExporter returns an exporter; it does nothing without exports.
func NewExporter(exports []config.QualityExport, st *store.Store, scores ScoresOf, log *slog.Logger) *Exporter {
	if log == nil {
		log = slog.Default()
	}
	return &Exporter{exports: exports, st: st, scores: scores, log: log, now: time.Now, reports: map[string]*Report{}, failed: map[string]int64{}}
}

func spec(q config.QualityExport) Spec {
	return Spec{
		Project: q.Project, Policy: q.Policy, Metric: q.Metric, GroupBy: q.GroupBy,
		Window: config.Duration(q.Window), MinTraces: q.MinTraces, MaxGroups: q.MaxGroups,
	}
}

func interval(q config.QualityExport) time.Duration {
	if d := config.Duration(q.Interval); d > 0 {
		return d
	}
	return time.Minute
}

// Refresh recomputes one export now.
func (e *Exporter) Refresh(ctx context.Context, q config.QualityExport) {
	s := spec(q)
	var projects []string
	if q.Project != "" {
		projects = []string{q.Project}
	}
	r, err := Build(ctx, s, StoreScan(e.st, projects, q.Policy), e.scores, e.now())
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			e.failed[q.Name]++
			e.log.Warn("quality export failed", "export", q.Name, "err", err)
		}
		return
	}
	e.reports[q.Name] = r
}

// Run keeps every export fresh until ctx ends.
func (e *Exporter) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, q := range e.exports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Refresh(ctx, q)
			tick := time.NewTicker(interval(q))
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					e.Refresh(ctx, q)
				}
			}
		}()
	}
	wg.Wait()
}

func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// WriteMetrics writes the gauges.
func (e *Exporter) WriteMetrics(w io.Writer) {
	if e == nil || len(e.exports) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, 0, len(e.reports))
	for n := range e.reports {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprint(w, "# HELP evalsi_quality_score Mean of an online policy's metric over a group's traces in the window.\n# TYPE evalsi_quality_score gauge\n")
	for _, n := range names {
		r := e.reports[n]
		for _, g := range r.Groups {
			metrics := make([]string, 0, len(g.Metrics))
			for m := range g.Metrics {
				metrics = append(metrics, m)
			}
			sort.Strings(metrics)
			for _, m := range metrics {
				if g.Metrics[m].Traces < r.MinTraces {
					continue // too few traces to act on: no gauge rather than a noisy one
				}
				fmt.Fprintf(w, "evalsi_quality_score{export=\"%s\",project=\"%s\",policy=\"%s\",metric=\"%s\",group=\"%s\"} %.6g\n",
					esc(n), esc(r.Project), esc(r.Policy), esc(m), esc(g.Group), g.Metrics[m].Mean)
			}
		}
	}
	fmt.Fprint(w, "# HELP evalsi_quality_traces Traces of a group with results from the policy in the window.\n# TYPE evalsi_quality_traces gauge\n")
	for _, n := range names {
		r := e.reports[n]
		for _, g := range r.Groups {
			fmt.Fprintf(w, "evalsi_quality_traces{export=\"%s\",project=\"%s\",policy=\"%s\",group=\"%s\"} %d\n", esc(n), esc(r.Project), esc(r.Policy), esc(g.Group), g.Traces)
		}
	}
	fmt.Fprint(w, "# HELP evalsi_quality_error_rate Share of a group's traces whose root span is an error.\n# TYPE evalsi_quality_error_rate gauge\n")
	for _, n := range names {
		r := e.reports[n]
		for _, g := range r.Groups {
			fmt.Fprintf(w, "evalsi_quality_error_rate{export=\"%s\",project=\"%s\",policy=\"%s\",group=\"%s\"} %.6g\n", esc(n), esc(r.Project), esc(r.Policy), esc(g.Group), g.ErrorRate)
		}
	}
	fmt.Fprint(w, "# HELP evalsi_quality_report_age_seconds Age of an export's last successful report.\n# TYPE evalsi_quality_report_age_seconds gauge\n")
	now := e.now()
	for _, n := range names {
		fmt.Fprintf(w, "evalsi_quality_report_age_seconds{export=\"%s\"} %g\n", esc(n), now.Sub(e.reports[n].GeneratedAt).Seconds())
	}
	fmt.Fprint(w, "# HELP evalsi_quality_export_failures_total Failed recomputations of an export.\n# TYPE evalsi_quality_export_failures_total counter\n")
	for _, q := range e.exports {
		fmt.Fprintf(w, "evalsi_quality_export_failures_total{export=\"%s\"} %d\n", esc(q.Name), e.failed[q.Name])
	}
}
