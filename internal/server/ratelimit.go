package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/time/rate"

	"github.com/abhishek-rnjn/evals.si/gen/go/agentgateway/dev/ext_mcp/ext_mcpconnect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

// Traffic classes, each with its own bucket per caller.
const (
	classAPI        = "api"
	classIngest     = "ingest"
	classGuardrails = "guardrails"
)

var rateClasses = []string{classAPI, classIngest, classGuardrails}

// bucketIdle is how long an unused bucket is kept at least. It is dropped
// only once it has also refilled, so a caller back later starts with the
// full bucket it would have had anyway, however slow its rate.
const bucketIdle = 10 * time.Minute

// limiter keeps a token bucket per caller and class (config.RateLimits). The
// middleware takes one token per HTTP request, so a stream costs one call,
// and stores a refusal on the request; gate.authenticate returns it, in the
// error form of the caller's protocol. With rate_limits.shared, a caller's
// calls are also counted across replicas (sharedCounts).
type limiter struct {
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	cfg     config.RateLimits // replaced by update on a config reload
	buckets map[bucketKey]*bucket
	swept   time.Time
	limited map[string]*atomic.Int64
	shared  *sharedCounts // nil unless rate_limits.shared was set at start
}

type bucketKey struct{ class, caller string }

type bucket struct {
	lim    *rate.Limiter
	seen   time.Time
	logged time.Time
}

// newLimiter returns nil, which limits nothing, when the limits are off or
// authentication is: without it every caller is the same anonymous owner.
func newLimiter(cfg config.Config, log *slog.Logger) *limiter {
	if cfg.RateLimits.Disabled || !cfg.Auth.Enabled() {
		return nil
	}
	l := &limiter{cfg: cfg.RateLimits, log: log, now: time.Now, buckets: map[bucketKey]*bucket{}, limited: map[string]*atomic.Int64{}}
	for _, c := range rateClasses {
		l.limited[c] = &atomic.Int64{}
	}
	if cfg.RateLimits.Shared {
		l.shared = newSharedCounts(cfg.RateLimits, log)
	}
	return l
}

// update applies reloaded settings. Existing buckets take the new rate and
// size at once, keeping the tokens they hold (up to the new size), so a
// reload neither refills nor drains them.
func (l *limiter) update(cfg config.RateLimits) {
	if l == nil {
		return
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg = cfg
	for k, b := range l.buckets {
		r := l.rateFor(k.class, k.caller)
		if r.PerSecond <= 0 {
			delete(l.buckets, k)
			continue
		}
		b.lim.SetLimitAt(now, rate.Limit(r.PerSecond))
		b.lim.SetBurstAt(now, r.Burst)
	}
}

// rateFor is a caller's rate for a class: its override, else the default.
// The caller holds l.mu.
func (l *limiter) rateFor(class, caller string) config.Rate {
	r, o := l.cfg.API, l.cfg.Principals[caller]
	override := o.API
	switch class {
	case classIngest:
		r, override = l.cfg.Ingest, o.Ingest
	case classGuardrails:
		r, override = l.cfg.Guardrails, o.Guardrails
	}
	if override != nil {
		r = *override
	}
	return r
}

// take spends one token. When none is left it reports how long until one is.
func (l *limiter) take(class, caller string) (time.Duration, bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.rateFor(class, caller)
	if l.cfg.Disabled || r.PerSecond <= 0 {
		return 0, true
	}
	if now.Sub(l.swept) >= time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) >= bucketIdle && b.lim.TokensAt(now) >= float64(b.lim.Burst()) {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	k := bucketKey{class, caller}
	b := l.buckets[k]
	if b == nil {
		b = &bucket{lim: rate.NewLimiter(rate.Limit(r.PerSecond), r.Burst)}
		l.buckets[k] = b
	}
	b.seen = now
	res := b.lim.ReserveN(now, 1)
	wait := res.DelayFrom(now)
	if res.OK() && wait == 0 {
		if l.shared == nil {
			return 0, true
		}
		// The replica's bucket allows it; the cluster's window may not.
		swait, ok := l.shared.take(class+"|"+caller, r, now)
		if ok {
			return 0, true
		}
		wait = swait
	}
	res.CancelAt(now)
	if !res.OK() {
		wait = time.Second
	}
	l.limited[class].Add(1)
	if now.Sub(b.logged) >= time.Minute {
		b.logged = now
		l.log.Warn("rate limit exceeded", "caller", caller, "class", class, "per_second", r.PerSecond, "burst", r.Burst)
	}
	return wait, false
}

// runShared syncs the shared counts with the store until ctx ends.
func (l *limiter) runShared(ctx context.Context, st rateStore) {
	if l == nil || l.shared == nil {
		return
	}
	l.shared.run(ctx, st)
}

// rateClass is the class of a request path; "" for routes that are open or
// whose work is limited elsewhere: /mcp tool calls re-enter the API in
// process and are counted there.
func rateClass(path string) string {
	switch {
	case path == ingest.TraceExportProcedure || path == "/v1/traces":
		return classIngest
	case strings.HasPrefix(path, "/guardrails/") || strings.HasPrefix(path, "/"+ext_mcpconnect.ExtMcpName+"/"):
		return classGuardrails
	case path == "/healthz" || path == "/mcp" || path == "/ui" || strings.HasPrefix(path, "/ui/") ||
		strings.HasPrefix(path, "/.well-known/") || strings.HasPrefix(path, "/grpc.health.v1.Health/"):
		return ""
	}
	return classAPI
}

// callerOf names the bucket's owner. Anonymous callers (optional
// authentication) are told apart by source address only.
func callerOf(p *auth.Principal, remote string) string {
	if !p.Anonymous() {
		return p.ID()
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	return "anonymous@" + host
}

type limitKey struct{}

// middleware runs after authentication. A request whose credential was
// rejected is not counted: the gate refuses it anyway.
func (l *limiter) middleware(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class := rateClass(r.URL.Path)
		res := auth.FromContext(r.Context())
		if class != "" && res.Err == nil {
			if wait, ok := l.take(class, callerOf(auth.PrincipalFrom(r.Context()), r.RemoteAddr)); !ok {
				r = r.WithContext(context.WithValue(r.Context(), limitKey{}, rateLimited(class, wait)))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimited is ResourceExhausted (HTTP 429) with Retry-After in seconds.
func rateLimited(class string, wait time.Duration) *connect.Error {
	secs := max(int(math.Ceil(wait.Seconds())), 1)
	ce := connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("rate limit exceeded for %s calls; retry in %ds", class, secs))
	ce.Meta().Set("Retry-After", strconv.Itoa(secs))
	return ce
}

// limitedFrom is the refusal the middleware stored on a request, if any.
func limitedFrom(ctx context.Context) error {
	if ce, ok := ctx.Value(limitKey{}).(*connect.Error); ok {
		return ce
	}
	return nil
}

func (l *limiter) writeMetrics(w io.Writer) {
	if l == nil {
		return
	}
	const name = "evalsi_rate_limited_total"
	fmt.Fprintf(w, "# HELP %s Calls refused by a per-caller rate limit.\n# TYPE %s counter\n", name, name)
	for _, c := range rateClasses {
		fmt.Fprintf(w, "%s{class=%q} %d\n", name, c, l.limited[c].Load())
	}
	l.shared.writeMetrics(w)
}
