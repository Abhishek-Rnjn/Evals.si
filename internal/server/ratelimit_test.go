package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

func testLimiter(t *testing.T, rl config.RateLimits) (*limiter, *time.Time) {
	t.Helper()
	cfg := config.Default()
	cfg.Auth = &auth.Config{APIKeys: &auth.APIKeysConfig{}}
	cfg.RateLimits = rl
	l := newLimiter(cfg, slog.New(slog.DiscardHandler))
	if l == nil {
		t.Fatal("limiter is off")
	}
	clock := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return clock }
	return l, &clock
}

func TestLimiterBucketsRefillAndRetryAfter(t *testing.T) {
	l, clock := testLimiter(t, config.RateLimits{API: config.Rate{PerSecond: 2, Burst: 3}})
	for i := range 3 {
		if _, ok := l.take(classAPI, "key:a"); !ok {
			t.Fatalf("call %d refused within the burst", i)
		}
	}
	wait, ok := l.take(classAPI, "key:a")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("past the burst: ok=%v wait=%v, want refused with 500ms", ok, wait)
	}
	// Buckets are per caller and per class.
	if _, ok := l.take(classAPI, "key:b"); !ok {
		t.Fatal("another caller was limited")
	}
	if _, ok := l.take(classIngest, "key:a"); !ok {
		t.Fatal("an unconfigured class was limited")
	}
	*clock = clock.Add(time.Second)
	for i := range 2 {
		if _, ok := l.take(classAPI, "key:a"); !ok {
			t.Fatalf("refilled call %d refused", i)
		}
	}
	if _, ok := l.take(classAPI, "key:a"); ok {
		t.Fatal("one second refilled more than per_second")
	}
	if got := l.limited[classAPI].Load(); got != 2 {
		t.Fatalf("limited counter = %d, want 2", got)
	}
	if ce := rateLimited(classAPI, 300*time.Millisecond); ce.Code() != connect.CodeResourceExhausted || ce.Meta().Get("Retry-After") != "1" {
		t.Fatalf("refusal = %v, Retry-After %q", ce, ce.Meta().Get("Retry-After"))
	}
}

func TestLimiterOverridesAndUnlimitedClasses(t *testing.T) {
	l, _ := testLimiter(t, config.RateLimits{
		API:    config.Rate{PerSecond: 1, Burst: 1},
		Ingest: config.Rate{PerSecond: 1, Burst: 1},
		Principals: map[string]config.RateOverride{
			"key:collector": {Ingest: &config.Rate{}},                      // unlimited ingest
			"key:tight":     {API: &config.Rate{PerSecond: 0.1, Burst: 1}}, // ingest keeps the default
		},
	})
	for range 50 {
		if _, ok := l.take(classIngest, "key:collector"); !ok {
			t.Fatal("an unlimited override was limited")
		}
	}
	l.take(classAPI, "key:collector")
	if _, ok := l.take(classAPI, "key:collector"); ok {
		t.Fatal("an override of one class lifted another")
	}
	l.take(classAPI, "key:tight")
	if wait, ok := l.take(classAPI, "key:tight"); ok || wait != 10*time.Second {
		t.Fatalf("override rate: ok=%v wait=%v", ok, wait)
	}
}

func TestLimiterForgetsIdleCallers(t *testing.T) {
	l, clock := testLimiter(t, config.RateLimits{API: config.Rate{PerSecond: 1, Burst: 1}})
	l.take(classAPI, "key:a")
	*clock = clock.Add(bucketIdle + time.Minute)
	l.take(classAPI, "key:b")
	if _, kept := l.buckets[bucketKey{classAPI, "key:a"}]; kept || len(l.buckets) != 1 {
		t.Fatalf("idle bucket kept: %v", l.buckets)
	}
}

// A slow bucket idle past bucketIdle but not yet refilled is kept, or a
// caller could reset it by waiting just past the idle time.
func TestLimiterKeepsIdleBucketsUntilRefilled(t *testing.T) {
	l, clock := testLimiter(t, config.RateLimits{API: config.Rate{PerSecond: 0.001, Burst: 1}})
	l.take(classAPI, "key:slow")
	*clock = clock.Add(bucketIdle + time.Minute)
	if _, ok := l.take(classAPI, "key:slow"); ok {
		t.Fatal("an idle but unrefilled bucket was reset")
	}
	*clock = clock.Add(1000 * time.Second)
	l.take(classAPI, "key:other") // sweeps
	if _, kept := l.buckets[bucketKey{classAPI, "key:slow"}]; kept {
		t.Fatal("a refilled idle bucket was kept")
	}
}

func TestLimiterOffWithoutAuthentication(t *testing.T) {
	cfg := config.Default()
	if newLimiter(cfg, slog.New(slog.DiscardHandler)) != nil {
		t.Fatal("limits on without authentication: every caller is one anonymous owner")
	}
	cfg.Auth = &auth.Config{APIKeys: &auth.APIKeysConfig{}}
	cfg.RateLimits.Disabled = true
	if newLimiter(cfg, slog.New(slog.DiscardHandler)) != nil {
		t.Fatal("rate_limits.disabled ignored")
	}
}

func TestRateClassesAndCallers(t *testing.T) {
	for path, want := range map[string]string{
		ingest.TraceExportProcedure:                classIngest,
		"/v1/traces":                               classIngest,
		"/guardrails/support/pii/request":          classGuardrails,
		"/agentgateway.dev.ext_mcp.ExtMcp/Process": classGuardrails,
		"/evalsi.v1alpha1.RunService/ListRuns":     classAPI,
		"/v1alpha1/runs":                           classAPI,
		"/metrics":                                 classAPI,
		"/mcp":                                     "",
		"/healthz":                                 "",
		"/ui/":                                     "",
		"/.well-known/evalsi-auth":                 "",
		"/grpc.health.v1.Health/Check":             "",
	} {
		if got := rateClass(path); got != want {
			t.Errorf("rateClass(%s) = %q, want %q", path, got, want)
		}
	}
	if got := callerOf(auth.Anonymous(auth.MethodNone), "10.0.0.7:5555"); got != "anonymous@10.0.0.7" {
		t.Errorf("anonymous caller = %q", got)
	}
	if got := callerOf(&auth.Principal{Kind: auth.KindAPIKey, KeyName: "ci"}, "10.0.0.7:5555"); got != "key:ci" {
		t.Errorf("key caller = %q", got)
	}
}

func TestRateLimitConfigValidation(t *testing.T) {
	for name, rl := range map[string]config.RateLimits{
		"negative":   {API: config.Rate{PerSecond: -1}},
		"no burst":   {Ingest: config.Rate{PerSecond: 5}},
		"bad id":     {Principals: map[string]config.RateOverride{"otel": {}}},
		"bad nested": {Principals: map[string]config.RateOverride{"key:otel": {Guardrails: &config.Rate{PerSecond: 3}}}},
	} {
		cfg := config.Default()
		cfg.RateLimits = rl
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "rate_limits") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
}

// End to end: one key is limited on every surface it uses, with the
// protocol's own refusal, and other keys are not.
func TestRateLimitsOnTheServer(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) {
		c.OTLP.Grace = "10ms"
		c.RateLimits.Principals = map[string]config.RateOverride{
			"key:viewer": {API: &config.Rate{PerSecond: 0.001, Burst: 2}},
			"key:ingest": {Ingest: &config.Rate{PerSecond: 0.001, Burst: 1}},
		}
	})
	ctx := t.Context()
	list := func(key string, opts ...connect.ClientOption) error {
		c := evalsiv1alpha1connect.NewCatalogServiceClient(s.http, s.url, append(opts, as(s.keys[key]))...)
		_, err := c.ListEvaluators(ctx, connect.NewRequest(&evalsiv1alpha1.ListEvaluatorsRequest{}))
		return err
	}
	for i := range 2 {
		if err := list("viewer"); err != nil {
			t.Fatalf("call %d within the burst: %v", i, err)
		}
	}
	err := list("viewer")
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted || ce.Meta().Get("Retry-After") == "" {
		t.Fatalf("Connect past the burst: %v", err)
	}
	if err := list("viewer", connect.WithGRPC()); codeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("gRPC past the burst: %v", err)
	}
	if err := list("runner"); err != nil {
		t.Fatalf("another key was limited: %v", err)
	}

	do := func(method, path, key string, body []byte, contentType string) *http.Response {
		req, _ := http.NewRequestWithContext(ctx, method, s.url+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.keys[key])
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if resp := do(http.MethodGet, "/v1alpha1/runs", "viewer", nil, ""); resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("REST past the burst = %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	otlp := otlpRequest("agent", nil)
	if resp := do(http.MethodPost, "/v1/traces", "ingest", otlp, "application/x-protobuf"); resp.StatusCode != http.StatusOK {
		t.Fatalf("first export = %d", resp.StatusCode)
	}
	if resp := do(http.MethodPost, "/v1/traces", "ingest", otlp, "application/x-protobuf"); resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("export past the burst = %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	// Ingest has its own bucket: the ingest key's API calls still pass.
	if resp := do(http.MethodGet, "/v1alpha1/whoami", "ingest", nil, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest key's API call = %d", resp.StatusCode)
	}
	// A rejected credential is a 401, not a 429, and costs no one's tokens.
	if resp := do(http.MethodGet, "/v1alpha1/runs", "nobody", nil, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad credential = %d", resp.StatusCode)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+s.keys["owner"])
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`evalsi_rate_limited_total{class="api"} 3`, `evalsi_rate_limited_total{class="ingest"} 1`} {
		if !strings.Contains(string(metrics), want) {
			t.Errorf("metrics lack %s:\n%s", want, metrics)
		}
	}
}
