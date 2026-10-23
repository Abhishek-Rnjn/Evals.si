package quota

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func testMeter(t *testing.T, limit int64) (*SandboxMeter, *store.Store, *time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	q := config.Quotas{Default: config.QuotaLimits{SandboxSecondsPerDay: limit}}
	m := NewSandboxMeter(st, q, slog.New(slog.DiscardHandler))
	clock := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return clock }
	return m, st, &clock
}

func TestMeterRefusesAProjectAtItsQuotaAndCountsAcrossTheDay(t *testing.T) {
	ctx := context.Background()
	m, st, clock := testMeter(t, 10)
	if err := m.Check(ctx, "p", "policy"); err != nil {
		t.Fatal(err)
	}
	m.Charge("p", 6*time.Second)
	if err := m.Check(ctx, "p", "policy"); err != nil {
		t.Fatalf("under the quota: %v", err)
	}
	// Pending charges count at once, before any flush.
	m.Charge("p", 4*time.Second)
	err := m.Check(ctx, "p", "guardrail")
	if connect.CodeOf(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "sandbox_seconds_per_day 10") {
		t.Fatalf("at the quota: %v", err)
	}
	// Another project is not affected.
	if err := m.Check(ctx, "q", "policy"); err != nil {
		t.Fatalf("another project: %v", err)
	}
	m.Flush(ctx)
	u, err := st.UsageOn(ctx, "p", "2026-10-22")
	if err != nil || u.SandboxMillis != 10_000 {
		t.Fatalf("stored usage %+v, %v", u, err)
	}
	// After the flush the stored total is what counts (another replica's work too).
	if _, err := st.AddUsage(ctx, "q", "2026-10-22", store.Usage{SandboxMillis: 11_000}); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(5 * time.Second) // past the read cache
	if err := m.Check(ctx, "q", "policy"); err == nil {
		t.Fatal("work another replica recorded was ignored")
	}
	// A new UTC day starts at zero.
	*clock = clock.Add(24 * time.Hour)
	if err := m.Check(ctx, "p", "policy"); err != nil {
		t.Fatalf("next day: %v", err)
	}

	var buf bytes.Buffer
	m.WriteMetrics(&buf)
	for _, want := range []string{`evalsi_sandbox_quota_refused_total{path="guardrail"} 1`, `evalsi_sandbox_quota_refused_total{path="policy"} 1`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, buf.String())
		}
	}
}

func TestMeterDoesNothingWithoutAQuota(t *testing.T) {
	ctx := context.Background()
	m, st, _ := testMeter(t, 0)
	m.Charge("p", time.Hour)
	if err := m.Check(ctx, "p", "policy"); err != nil {
		t.Fatal(err)
	}
	m.Flush(ctx)
	if u, _ := st.UsageOn(ctx, "p", "2026-10-22"); u.SandboxMillis != 0 {
		t.Fatalf("unlimited project metered: %+v", u)
	}
	// A per-project quota applies to that project only.
	m.quotas.Projects = map[string]config.QuotaLimits{"tight": {SandboxSecondsPerDay: 1}}
	m.Charge("tight", 2*time.Second)
	if err := m.Check(ctx, "tight", "reward"); err == nil {
		t.Fatal("per-project quota ignored")
	}
}

// A database error must not become a refusal of live work, and pending
// charges are kept for the next flush.
func TestMeterFailsOpenAndKeepsChargesOnStoreErrors(t *testing.T) {
	ctx := context.Background()
	m, st, _ := testMeter(t, 5)
	m.Charge("p", 3*time.Second)
	_ = st.Close()
	if err := m.Check(ctx, "p", "policy"); err != nil {
		t.Fatalf("store down: %v", err)
	}
	m.Flush(ctx)
	if m.pending["p"] != 3000 {
		t.Fatalf("pending after a failed flush: %v", m.pending)
	}
}
