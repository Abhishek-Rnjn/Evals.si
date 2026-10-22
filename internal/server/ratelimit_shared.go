package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abhishek-rnjn/evals.si/internal/config"
)

// rateStore is where replicas add up their calls: the store (PostgreSQL when
// there are several replicas).
type rateStore interface {
	AddRateCounts(ctx context.Context, startNs int64, add map[string]int64) (map[string]int64, error)
	PruneRateCounts(ctx context.Context, beforeNs int64) error
}

// sharedCounts bounds each caller's calls per class across replicas, in
// fixed windows counted in the store (rate_limits.shared, decision 0029).
// The store is not on the request path: a replica admits a call when the
// last total it read for the window plus what it admitted since is under
// the cap, and every sync interval adds what it admitted and reads the new
// total. Between syncs the replicas together can pass the cap by what each
// admits in one interval; their own buckets still bound that.
//
// While the store fails, the window check is skipped (each replica keeps
// only its own buckets), counted in evalsi_rate_limit_shared_errors_total
// and shown by evalsi_rate_limit_shared_fallback, until a sync succeeds.
type sharedCounts struct {
	window, sync time.Duration
	log          *slog.Logger

	mu     sync.Mutex
	keys   map[string]*sharedKey
	pruned time.Time
	logged time.Time

	failing atomic.Bool
	errors  atomic.Int64
}

// sharedKey is one bucket's (class|caller) count in the current window.
type sharedKey struct {
	start   int64 // window start, unix ns
	pending int64 // admitted here, not yet added to the store
	global  int64 // the store's total at the last sync, all replicas
	cap     int64
}

func newSharedCounts(cfg config.RateLimits, log *slog.Logger) *sharedCounts {
	s := &sharedCounts{window: time.Second, sync: 250 * time.Millisecond, log: log, keys: map[string]*sharedKey{}}
	if d := config.Duration(cfg.SharedWindow); d > 0 {
		s.window = d
	}
	if d := config.Duration(cfg.SharedSync); d > 0 {
		s.sync = d
	}
	return s
}

// windowCap is what one caller may make of a class in one window: what a
// single token bucket would allow over it.
func (s *sharedCounts) windowCap(r config.Rate) int64 {
	return int64(r.Burst) + int64(math.Ceil(r.PerSecond*s.window.Seconds()))
}

func (s *sharedCounts) windowStart(now time.Time) int64 {
	w := s.window.Nanoseconds()
	return now.UnixNano() / w * w
}

// take admits one call for key unless the window is full; then it reports
// how long until the window ends.
func (s *sharedCounts) take(key string, r config.Rate, now time.Time) (time.Duration, bool) {
	start := s.windowStart(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keys[key]
	if k == nil {
		k = &sharedKey{}
		s.keys[key] = k
	}
	if k.start != start {
		// A new window. What the last one did not sync is dropped with it.
		*k = sharedKey{start: start}
	}
	k.cap = s.windowCap(r)
	if !s.failing.Load() && k.global+k.pending >= k.cap {
		return time.Duration(start + s.window.Nanoseconds() - now.UnixNano()), false
	}
	k.pending++
	return 0, true
}

// run syncs every interval until ctx ends.
func (s *sharedCounts) run(ctx context.Context, st rateStore) {
	tick := time.NewTicker(s.sync)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			s.syncOnce(ctx, st, now)
		}
	}
}

// syncOnce adds what each bucket admitted in the current window since the
// last sync, reads the totals, and forgets buckets of past windows.
func (s *sharedCounts) syncOnce(ctx context.Context, st rateStore, now time.Time) {
	start := s.windowStart(now)
	s.mu.Lock()
	add := map[string]int64{}
	for key, k := range s.keys {
		switch {
		case k.start != start:
			delete(s.keys, key) // idle since an earlier window
		case k.pending > 0:
			// A bucket that admitted nothing since needs no new total: it
			// is refusing, and the window's count only grows.
			add[key] = k.pending
		}
	}
	s.mu.Unlock()
	prune := now.Sub(s.pruned) >= time.Minute
	if len(add) == 0 && !prune {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, max(s.sync, time.Second))
	defer cancel()
	var totals map[string]int64
	var err error
	if len(add) > 0 {
		totals, err = st.AddRateCounts(ctx, start, add)
	}
	if err == nil && prune {
		// Any replica may prune; windows a minute old are no one's.
		if err = st.PruneRateCounts(ctx, start-time.Minute.Nanoseconds()); err == nil {
			s.pruned = now
		}
	}
	if err != nil {
		s.errors.Add(1)
		if !s.failing.Swap(true) || now.Sub(s.logged) >= time.Minute {
			s.logged = now
			s.log.Warn("shared rate limits: the store failed; limiting per replica until it answers", "err", err)
		}
		return
	}
	if s.failing.Swap(false) {
		s.log.Info("shared rate limits: the store answers again")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, total := range totals {
		if k := s.keys[key]; k != nil && k.start == start {
			k.pending -= add[key]
			k.global = total
		}
	}
}

func (s *sharedCounts) writeMetrics(w io.Writer) {
	if s == nil {
		return
	}
	fallback := 0
	if s.failing.Load() {
		fallback = 1
	}
	fmt.Fprintf(w, "# HELP evalsi_rate_limit_shared_errors_total Shared rate-limit syncs the store failed.\n# TYPE evalsi_rate_limit_shared_errors_total counter\nevalsi_rate_limit_shared_errors_total %d\n", s.errors.Load())
	fmt.Fprintf(w, "# HELP evalsi_rate_limit_shared_fallback 1 while shared rate limits fall back to per-replica buckets.\n# TYPE evalsi_rate_limit_shared_fallback gauge\nevalsi_rate_limit_shared_fallback %d\n", fallback)
}
