package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func TestLimiterUpdateKeepsTokensAndAppliesNewRates(t *testing.T) {
	l, clock := testLimiter(t, config.RateLimits{API: config.Rate{PerSecond: 1, Burst: 2}})
	l.take(classAPI, "key:a")
	l.take(classAPI, "key:a")
	if _, ok := l.take(classAPI, "key:a"); ok {
		t.Fatal("past the burst")
	}
	// A larger bucket does not refill the empty one at once...
	l.update(config.RateLimits{API: config.Rate{PerSecond: 10, Burst: 20}})
	if _, ok := l.take(classAPI, "key:a"); ok {
		t.Fatal("the reload refilled the bucket")
	}
	// ...but refills at the new rate.
	*clock = clock.Add(time.Second)
	for i := range 10 {
		if _, ok := l.take(classAPI, "key:a"); !ok {
			t.Fatalf("call %d at the new rate refused", i)
		}
	}
	// An override added by a reload applies to an existing bucket.
	l.update(config.RateLimits{API: config.Rate{PerSecond: 10, Burst: 20}, Principals: map[string]config.RateOverride{"key:a": {API: &config.Rate{}}}})
	for range 100 {
		if _, ok := l.take(classAPI, "key:a"); !ok {
			t.Fatal("a lifted limit still applies")
		}
	}
	// disabled: true turns the limits off without a restart.
	l.update(config.RateLimits{Disabled: true, API: config.Rate{PerSecond: 0.001, Burst: 1}})
	for range 10 {
		if _, ok := l.take(classAPI, "key:b"); !ok {
			t.Fatal("disabled limits still apply")
		}
	}
}

func openRateStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Two replicas sharing a store: together they stay within one window's cap
// (burst + per_second * window), where each alone would allow its burst.
func TestSharedLimitAcrossReplicas(t *testing.T) {
	st := openRateStore(t)
	rl := config.RateLimits{Shared: true, SharedWindow: "10s", API: config.Rate{PerSecond: 0.1, Burst: 4}} // cap 4 + 1 = 5
	a, clock := testLimiter(t, rl)
	b, _ := testLimiter(t, rl)
	b.now = a.now
	ctx := context.Background()
	for i := range 4 {
		if _, ok := a.take(classAPI, "key:k"); !ok {
			t.Fatalf("replica a call %d refused", i)
		}
	}
	a.shared.syncOnce(ctx, st, *clock)
	if _, ok := b.take(classAPI, "key:k"); !ok {
		t.Fatal("replica b's first call refused (the window has room for one)")
	}
	b.shared.syncOnce(ctx, st, *clock)
	wait, ok := b.take(classAPI, "key:k")
	if ok {
		t.Fatal("replica b passed the shared cap; its own bucket still had tokens")
	}
	if wait <= 0 || wait > 10*time.Second {
		t.Fatalf("wait = %v, want until the window ends", wait)
	}
	if got := b.limited[classAPI].Load(); got != 1 {
		t.Fatalf("limited = %d", got)
	}
	// Another caller is unaffected.
	if _, ok := b.take(classAPI, "key:other"); !ok {
		t.Fatal("another caller was limited")
	}
	// The next window starts afresh (b's bucket kept its tokens: the refused
	// call gave its token back).
	*clock = clock.Add(10 * time.Second)
	if _, ok := b.take(classAPI, "key:k"); !ok {
		t.Fatal("a new window was refused")
	}
}

type failingRateStore struct{ calls atomic.Int64 }

func (f *failingRateStore) AddRateCounts(context.Context, int64, map[string]int64) (map[string]int64, error) {
	f.calls.Add(1)
	return nil, errors.New("connection refused")
}

func (f *failingRateStore) PruneRateCounts(context.Context, int64) error { return nil }

// When the store fails, each replica keeps its own buckets and says so.
func TestSharedLimitFallsBackPerReplica(t *testing.T) {
	rl := config.RateLimits{Shared: true, SharedWindow: "10s", API: config.Rate{PerSecond: 0.1, Burst: 10}} // cap 11
	l, clock := testLimiter(t, rl)
	ctx := context.Background()
	l.take(classAPI, "key:k")
	// Pretend another replica filled the window.
	l.shared.keys["api|key:k"].global = 11
	if _, ok := l.take(classAPI, "key:k"); ok {
		t.Fatal("full window admitted")
	}
	bad := &failingRateStore{}
	l.shared.syncOnce(ctx, bad, *clock)
	if bad.calls.Load() != 1 {
		t.Fatal("store not called")
	}
	if _, ok := l.take(classAPI, "key:k"); !ok {
		t.Fatal("fallback did not use the replica's own bucket")
	}
	var buf bytes.Buffer
	l.writeMetrics(&buf)
	for _, want := range []string{"evalsi_rate_limit_shared_errors_total 1", "evalsi_rate_limit_shared_fallback 1"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, buf.String())
		}
	}
	// The store answers again: counts flush and the window applies.
	st := openRateStore(t)
	l.shared.syncOnce(ctx, st, *clock)
	buf.Reset()
	l.writeMetrics(&buf)
	if !strings.Contains(buf.String(), "evalsi_rate_limit_shared_fallback 0") {
		t.Errorf("still falling back:\n%s", buf.String())
	}
	// Another replica fills the window; this one learns it at its next sync.
	if _, err := st.AddRateCounts(ctx, l.shared.windowStart(*clock), map[string]int64{"api|key:k": 11}); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.take(classAPI, "key:k"); !ok {
		t.Fatal("refused before the sync showed the window full")
	}
	l.shared.syncOnce(ctx, st, *clock)
	if _, ok := l.take(classAPI, "key:k"); ok {
		t.Fatal("the window's cap no longer applies after recovery")
	}
}

func TestSharedLimitConfigValidation(t *testing.T) {
	cfg := config.Default()
	cfg.RateLimits.Shared, cfg.RateLimits.SharedWindow = true, "1ms"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "rate_limits.shared_window") {
		t.Fatalf("err = %v", err)
	}
}

// A reloaded config changes a caller's rate on the running server.
func TestRateLimitsReload(t *testing.T) {
	old := reloadInterval
	reloadInterval = 20 * time.Millisecond
	t.Cleanup(func() { reloadInterval = old })
	var next atomic.Pointer[config.Config]
	s := startAuthServer(t, func(c *config.Config) {
		c.RateLimits.Principals = map[string]config.RateOverride{"key:viewer": {API: &config.Rate{PerSecond: 0.001, Burst: 1}}}
		c.Reload = func() (config.Config, error) {
			if n := next.Load(); n != nil {
				return *n, nil
			}
			return *c, nil
		}
		cp := *c
		next.Store(&cp)
	})
	list := func() error {
		c := evalsiv1alpha1connect.NewCatalogServiceClient(s.http, s.url, as(s.keys["viewer"]))
		_, err := c.ListEvaluators(t.Context(), connect.NewRequest(&evalsiv1alpha1.ListEvaluatorsRequest{}))
		return err
	}
	_ = list()
	if err := list(); codeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("past the burst: %v", err)
	}
	cp := *next.Load()
	cp.RateLimits.Principals = map[string]config.RateOverride{"key:viewer": {API: &config.Rate{}}}
	next.Store(&cp)
	deadline := time.Now().Add(5 * time.Second)
	for list() != nil {
		if time.Now().After(deadline) {
			t.Fatal("the lifted limit was not picked up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// newLimiter makes no shared counts unless asked.
func TestSharedCountsAreOptIn(t *testing.T) {
	cfg := config.Default()
	cfg.Auth = &auth.Config{APIKeys: &auth.APIKeysConfig{}}
	if l := newLimiter(cfg, slog.New(slog.DiscardHandler)); l.shared != nil {
		t.Fatal("shared counts on by default")
	}
	cfg.RateLimits.Shared = true
	if l := newLimiter(cfg, slog.New(slog.DiscardHandler)); l.shared == nil || l.shared.window != time.Second || l.shared.sync != 250*time.Millisecond {
		t.Fatalf("shared defaults: %+v", l.shared)
	}
}
