// Package quota meters sandbox time for the work that happens outside a run
// (decision 0037): online policy evaluation, guardrail checks and
// reward-service rollouts. The run manager meters its own runs.
package quota

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Usage is the part of the store the meter reads and writes.
type Usage interface {
	AddUsage(ctx context.Context, project, day string, add store.Usage) (store.Usage, error)
	UsageOn(ctx context.Context, project, day string) (store.Usage, error)
}

// usageTTL is how long a project's total read from the store is reused, so a
// busy path does not read the database per call.
const usageTTL = 2 * time.Second

// SandboxMeter counts the wall-clock time of sandbox-pool and Wasm
// evaluators toward quotas.*.sandbox_seconds_per_day, and refuses new work
// from a project that is at its quota. Charges are kept in memory and added
// to the project's day by Run every second, so the count lags by about that
// long (and replicas count separately between writes, as the other quotas
// do).
type SandboxMeter struct {
	st     Usage
	quotas config.Quotas
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	pending map[string]int64 // project -> milliseconds not yet written
	cached  map[string]cachedUsage
	refused map[string]int64 // by path, for /metrics
}

type cachedUsage struct {
	day    string
	millis int64
	at     time.Time
}

// NewSandboxMeter returns a meter for the configured quotas.
func NewSandboxMeter(st Usage, quotas config.Quotas, log *slog.Logger) *SandboxMeter {
	if log == nil {
		log = slog.Default()
	}
	return &SandboxMeter{st: st, quotas: quotas, log: log, now: time.Now,
		pending: map[string]int64{}, cached: map[string]cachedUsage{}, refused: map[string]int64{}}
}

func project(p string) string {
	if p == "" {
		return "default"
	}
	return p
}

func (m *SandboxMeter) day() string { return m.now().UTC().Format("2006-01-02") }

// Charge adds time a project's online work spent in the sandbox. It does
// nothing for a project without a sandbox_seconds_per_day quota.
func (m *SandboxMeter) Charge(p string, d time.Duration) {
	p = project(p)
	if d <= 0 || m.quotas.For(p).SandboxSecondsPerDay <= 0 {
		return
	}
	m.mu.Lock()
	m.pending[p] += d.Milliseconds()
	m.mu.Unlock()
}

// Check refuses work, with ResourceExhausted, when the project has used up
// the day's sandbox time. path (policy, guardrail or reward) names the kind
// of work in the refusal count.
func (m *SandboxMeter) Check(ctx context.Context, p, path string) error {
	p = project(p)
	limit := m.quotas.For(p).SandboxSecondsPerDay
	if limit <= 0 {
		return nil
	}
	day, now := m.day(), m.now()
	m.mu.Lock()
	c, ok := m.cached[p]
	pending := m.pending[p]
	m.mu.Unlock()
	if !ok || c.day != day || now.Sub(c.at) >= usageTTL {
		u, err := m.st.UsageOn(ctx, p, day)
		if err != nil {
			// The quota cannot be read: do not turn a database hiccup into a
			// refusal of live work. The next flush or check tries again.
			m.log.Warn("sandbox quota: reading usage", "project", p, "err", err)
			return nil
		}
		c = cachedUsage{day: day, millis: u.SandboxMillis, at: now}
		m.mu.Lock()
		m.cached[p] = c
		m.mu.Unlock()
	}
	if c.millis+pending >= limit*1000 {
		m.mu.Lock()
		m.refused[path]++
		m.mu.Unlock()
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(
			"project %s used %d seconds of sandboxed work today, its quota (sandbox_seconds_per_day %d)", p, (c.millis+pending)/1000, limit))
	}
	return nil
}

// Flush writes the pending charges to the project's day.
func (m *SandboxMeter) Flush(ctx context.Context) {
	m.mu.Lock()
	batch := m.pending
	m.pending = map[string]int64{}
	m.mu.Unlock()
	day := m.day()
	for p, millis := range batch {
		u, err := m.st.AddUsage(ctx, p, day, store.Usage{SandboxMillis: millis})
		m.mu.Lock()
		if err != nil {
			m.pending[p] += millis // try again at the next flush
			m.mu.Unlock()
			m.log.Warn("sandbox quota: writing usage", "project", p, "err", err)
			continue
		}
		m.cached[p] = cachedUsage{day: day, millis: u.SandboxMillis, at: m.now()}
		m.mu.Unlock()
	}
}

// Run flushes every second until ctx ends, then once more.
func (m *SandboxMeter) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Flush(context.WithoutCancel(ctx))
			return
		case <-tick.C:
			m.Flush(ctx)
		}
	}
}

// WriteMetrics adds evalsi_sandbox_quota_refused_total{path}.
func (m *SandboxMeter) WriteMetrics(w io.Writer) {
	if m == nil {
		return
	}
	const name = "evalsi_sandbox_quota_refused_total"
	fmt.Fprintf(w, "# HELP %s Online evaluation, guardrail and reward work refused for a project's sandbox_seconds_per_day quota.\n# TYPE %s counter\n", name, name)
	m.mu.Lock()
	defer m.mu.Unlock()
	paths := make([]string, 0, len(m.refused))
	for p := range m.refused {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(w, "%s{path=%q} %d\n", name, p, m.refused[p])
	}
}
