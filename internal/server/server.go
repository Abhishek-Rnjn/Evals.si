// Package server assembles evalsid: it starts the evaluator worker, builds the
// services and serves them on one port for gRPC (HTTP/2 without TLS),
// gRPC-Web and Connect HTTP/JSON.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/abhishek-rnjn/evals.si/gen/go/agentgateway/dev/ext_mcp/ext_mcpconnect"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/annotate"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/cluster"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/guardrail"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/mcp"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
	"github.com/abhishek-rnjn/evals.si/internal/rewards"
	"github.com/abhishek-rnjn/evals.si/internal/runs"
	"github.com/abhishek-rnjn/evals.si/internal/sinks"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"github.com/abhishek-rnjn/evals.si/internal/wasmeval"
	"github.com/abhishek-rnjn/evals.si/internal/watch"
)

// Run serves until ctx is cancelled, then shuts down gracefully.
// ready, if non-nil, receives the bound address once the server accepts connections.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, workerOutput io.Writer, ready chan<- string) error {
	var cl *cluster.Cluster
	if cfg.Cluster != nil {
		var err error
		if cl, err = cluster.Connect(ctx, *cfg.Cluster, "evalsid", cfg.DataDir); err != nil {
			return err
		}
		defer cl.Close()
	}
	// A single replica drives its own Python worker. In a cluster, work goes
	// to the pools' queues, and this process serves the pools listed in
	// worker.pools, if any (an all-in-one node).
	var worker pluginhost.Worker
	if cl == nil || len(cfg.Worker.Pools) > 0 {
		local, stop, err := startLocalWorker(ctx, cfg, log, workerOutput)
		if err != nil {
			return err
		}
		defer stop()
		worker = local
		if cl != nil {
			for _, p := range cfg.Worker.Pools {
				if !validPool(p) {
					return fmt.Errorf("worker.pools: unknown pool %q (pools: %v)", p, cluster.Pools)
				}
			}
			wctx, cancel := context.WithCancel(context.Background())
			served := make(chan struct{})
			go func() {
				defer close(served)
				if err := cl.Serve(wctx, cfg.Worker.Pools, local, cfg.Worker.Concurrency, log); err != nil {
					log.Error("serving worker pools", "err", err)
				}
			}()
			// Stop pulling tasks (and finish in-flight ones) before the worker stops.
			defer func() { cancel(); <-served }()
		}
	}
	if cl != nil {
		worker = cluster.NewWorker(cl)
	}
	return serve(ctx, cfg, worker, cl, log, ready)
}

// serve runs everything above the worker; tests call it with a fake worker.
func serve(ctx context.Context, cfg config.Config, worker pluginhost.Worker, cl *cluster.Cluster, log *slog.Logger, ready chan<- string) error {
	wasm, err := loadWasm(ctx, cfg, log)
	if err != nil {
		return err
	}
	if wasm != nil {
		defer wasm.Close(context.Background())
		worker = wasmeval.Wrap(worker, wasm)
	}
	if cl != nil {
		log.Info("waiting for a cpu worker to describe the evaluators")
	}
	manifests, err := worker.Describe(ctx)
	if err != nil {
		return fmt.Errorf("describing evaluators: %w", err)
	}
	judges := make([]string, 0, len(cfg.Judges))
	for name := range cfg.Judges {
		judges = append(judges, name)
	}
	sort.Strings(judges)
	svc := evaluation.New(worker, catalog.New(manifests), judges, cfg.DefaultJudge, cfg.Evaluate)

	st, objects, err := openStorage(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	exports, err := sinks.New(cfg.Sinks, nil, log)
	if err != nil {
		return err
	}
	exports.Start(2)
	// Pending exports get a few seconds after runs and policies have stopped.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		exports.Close(ctx)
	}()
	authn, err := auth.New(cfg.Auth, st, nil, log)
	if err != nil {
		return err
	}
	engine, err := authz.NewEngine(ctx, authz.Options{
		Enabled: cfg.AuthEnabled(), RBAC: cfg.RBAC, Authorization: cfg.Authorization,
		ClaimRoles: claimRoles(cfg.Auth), Store: st, Logger: log,
	})
	if err != nil {
		return err
	}
	auditor := authz.NewAuditor(st, exports.Audit, log)
	var watcher *watch.Engine // created below; runs read its policies' score names
	runManager, err := runs.New(ctx, st, worker, svc, runs.Options{
		DatasetsDir:   cfg.DatasetsDir,
		Objects:       objects,
		MaxConcurrent: cfg.Runs.MaxConcurrent,
		Evaluate:      cfg.Evaluate,
		Agents:        cfg.Agents,
		Logger:        log,
		OnFinished:    exports.Run,
		Cluster:       runCluster(cl),
		LeaseTTL:      optDuration(cfg.Runs.LeaseTTL),
		AdoptInterval: optDuration(cfg.Runs.AdoptInterval),
		Quotas:        cfg.Quotas,
		TraceScores: func(policy string, results []*evalsiv1alpha1.EvaluationResult) map[string]float64 {
			return watcher.TraceScores(policy, results)
		},
	})
	if err != nil {
		return err
	}
	// Runs stop before the worker does (deferred calls run in reverse order).
	defer runManager.Shutdown()

	watcher, err = watch.New(ctx, st, svc, watch.Options{
		DatasetsDir: cfg.DatasetsDir, Objects: objects, BatchSize: cfg.Evaluate.BatchSize, Logger: log,
		Changed: func() {
			if cl != nil {
				cl.PoliciesChanged()
			}
		},
		RemoteStats: func(name string) (*evalsiv1alpha1.PolicyStats, bool, error) {
			if cl == nil {
				return nil, false, fmt.Errorf("no cluster")
			}
			return cl.Stats(name)
		},
		OnResults: func(policy string, rec *evalsiv1alpha1.Record, info ingest.TraceInfo, results []*evalsiv1alpha1.EvaluationResult) {
			exports.Trace(&sinks.Trace{
				TraceID: rec.GetId(), RootSpanID: rootSpan(rec), Service: info.Service,
				Policy: policy, Results: results, Time: time.Now(),
			})
		},
	})
	if err != nil {
		return err
	}
	for i, raw := range cfg.Policies {
		p := &evalsiv1alpha1.OnlineEvalPolicy{}
		if err := protojson.Unmarshal(raw, p); err != nil {
			return fmt.Errorf("policies[%d]: %w", i, err)
		}
		if err := watcher.Apply(ctx, p); err != nil {
			return fmt.Errorf("policies[%d] (%s): %w", i, p.GetName(), err)
		}
	}
	assembler := ingest.NewAssembler(ingest.AssemblerOptions{
		Grace: config.Duration(cfg.OTLP.Grace), MaxTraces: cfg.OTLP.MaxTraces,
	}, watcher.Enqueue)
	bg, stopBG := context.WithCancel(context.Background())
	stopAssembler := make(chan struct{})
	var wg sync.WaitGroup
	if cl != nil {
		// Policies live in the database; replicas reload on any change, and
		// only the elected policy engine sees traces.
		watcher.SetLeader(false)
		stopReloads, err := cl.OnPoliciesChanged(func() {
			if err := watcher.Reload(bg); err != nil {
				log.Warn("reloading policies", "err", err)
			}
		})
		if err != nil {
			stopBG()
			return err
		}
		defer stopReloads()
		wg.Add(1)
		go func() { defer wg.Done(); leadPolicyEngine(bg, st, cl, assembler, watcher, log) }()
	}
	wg.Add(4)
	go func() { defer wg.Done(); assembler.Run(stopAssembler, 250*time.Millisecond) }()
	go func() { defer wg.Done(); watcher.Run(bg) }()
	go func() { defer wg.Done(); retain(bg, st, config.Duration(cfg.Traces.Retention), log) }()
	go func() { defer wg.Done(); auditor.Retain(bg, cfg.Audit.RetentionDuration()) }()
	defer func() {
		close(stopAssembler) // flushes buffered traces into the engine
		stopBG()
		wg.Wait()
	}()

	authSvc := authz.NewService(engine, st, auditor, authn.ConfigKeys())
	g := newGate(engine, auditor, st, watcher, authSvc, svc.RunsCode, log)
	rewardSvc := rewards.New(svc, cfg.Rewards, cfg.Quotas)
	if cfg.Rewards.SharedCache {
		rewardSvc.UseSharedCache(st, log)
		go pruneRewardCache(ctx, st, cfg.Rewards.SharedCacheTTL, log)
	}
	d := deps{svc: svc, rewards: rewardSvc, runs: runManager, watcher: watcher, store: st, assembler: assembler, worker: worker, authn: authn, gate: g, authSvc: authSvc, mcp: cfg.MCP, log: log}
	if cl != nil {
		d.forward = cl.PublishSpans
	}

	var tlsCfg *tls.Config
	if cfg.Auth.Enabled() && cfg.Auth.TLS != nil {
		if tlsCfg, err = auth.ServerTLS(cfg.Auth.TLS); err != nil {
			return err
		}
	}
	switch {
	case !cfg.AuthEnabled() && cfg.Auth != nil:
		log.Warn("AUTHENTICATION IS OFF (auth.mode: none, --no-auth or EVALSID_NO_AUTH): every caller is an owner; "+
			"use this for development only", "listen", cfg.Listen, "loopback", auth.IsLoopback(cfg.Listen))
	case !cfg.AuthEnabled():
		log.Info("no auth section: serving unauthenticated on loopback only")
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           Handler(d),
		Protocols:         protocols(tlsCfg != nil),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsCfg,
	}
	errc := make(chan error, 4)
	go func() { errc <- serveOn(srv, ln) }()
	// The standard OTLP ports serve only OTLP, with the same authentication.
	var extra []*http.Server
	for _, addr := range []string{cfg.OTLP.GRPCListen, cfg.OTLP.HTTPListen} {
		if addr == "" {
			continue
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			_ = srv.Close()
			return fmt.Errorf("otlp listener: %w", err)
		}
		s := &http.Server{Handler: otlpHandler(d), Protocols: protocols(tlsCfg != nil), ReadHeaderTimeout: 10 * time.Second, TLSConfig: tlsCfg}
		extra = append(extra, s)
		go func() { errc <- serveOn(s, l) }()
		log.Info("otlp listening", "addr", l.Addr().String())
	}
	if cfg.Metrics.Listen != "" {
		l, err := net.Listen("tcp", cfg.Metrics.Listen)
		if err != nil {
			_ = srv.Close()
			return fmt.Errorf("metrics listener: %w", err)
		}
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", watch.MetricsHandler(watcher, assembler))
		s := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		extra = append(extra, s)
		go func() { errc <- s.Serve(l) }()
		log.Info("metrics listening", "addr", l.Addr().String())
	}
	log.Info("evalsid listening", "addr", ln.Addr().String(), "evaluators", len(manifests), "judges", judges,
		"auth", cfg.AuthEnabled(), "tls", tlsCfg != nil)
	if ready != nil {
		ready <- ln.Addr().String()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range extra {
		_ = s.Shutdown(shutdownCtx)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func serveOn(s *http.Server, l net.Listener) error {
	if s.TLSConfig != nil {
		return s.ServeTLS(l, "", "")
	}
	return s.Serve(l)
}

// claimRoles lists every role the providers' role_claims can map to.
func claimRoles(c *auth.Config) []string {
	if c == nil || c.JWT == nil {
		return nil
	}
	var out []string
	for _, p := range c.JWT.Providers {
		if p.RoleClaims == nil {
			continue
		}
		for _, projects := range p.RoleClaims.Map {
			for _, roles := range projects {
				out = append(out, roles...)
			}
		}
	}
	return out
}

// retain deletes traces older than the retention period, hourly.
func retain(ctx context.Context, st *store.Store, retention time.Duration, log *slog.Logger) {
	if retention <= 0 {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.DeleteTracesBefore(ctx, time.Now().Add(-retention)); err != nil {
			log.Error("trace retention", "err", err)
		} else if n > 0 {
			log.Info("trace retention", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func protocols(tls bool) *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	if tls {
		p.SetHTTP2(true)
	} else {
		p.SetUnencryptedHTTP2(true)
	}
	return p
}

// deps is everything the HTTP handler serves.
type deps struct {
	svc       *evaluation.Service
	rewards   *rewards.Service
	runs      *runs.Manager
	watcher   *watch.Engine
	store     *store.Store
	assembler *ingest.Assembler
	worker    pluginhost.Worker
	authn     *auth.Authenticator
	gate      *gate
	authSvc   *authz.Service
	// In a cluster, received spans go to the span stream.
	forward ingest.Forward
	mcp     mcp.Config
	log     *slog.Logger
}

// registerOTLP mounts OTLP/gRPC (through the gate's interceptor) and
// OTLP/HTTP (through its HTTP guard). Each resource's project comes from
// the caller's credential (gate.assignTrace).
func registerOTLP(mux *http.ServeMux, d deps) {
	inner := http.NewServeMux()
	recv := ingest.NewReceiver(d.assembler, d.gate.assignTrace, connect.WithInterceptors(d.gate.interceptor()))
	if d.forward != nil {
		recv.SetForward(d.forward)
	}
	recv.Register(inner)
	mux.Handle(ingest.TraceExportProcedure, inner)
	mux.Handle("POST /v1/traces", d.gate.guardHTTP("traces.write", true, inner))
}

func otlpHandler(d deps) http.Handler {
	mux := http.NewServeMux()
	registerOTLP(mux, d)
	return d.authn.Middleware(mux)
}

// Handler routes every service behind authentication and the gate.
func Handler(d deps) http.Handler {
	gated := connect.WithInterceptors(d.gate.interceptor())
	mux := http.NewServeMux()
	log := d.log
	if log == nil {
		log = slog.Default()
	}
	guard := guardrail.New(d.store, d.svc, log)
	handlers := map[string]http.Handler{}
	for _, h := range []func() (string, http.Handler){
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewEvaluationServiceHandler(d.svc, gated) },
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewCatalogServiceHandler(d.svc, gated) },
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewRewardServiceHandler(d.rewards, gated) },
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewRunServiceHandler(d.runs, gated) },
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewMonitorServiceHandler(d.watcher, gated) },
		func() (string, http.Handler) {
			return evalsiv1alpha1connect.NewTraceServiceHandler(watch.Traces{Store: d.store}, gated)
		},
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewAuthServiceHandler(d.authSvc, gated) },
		func() (string, http.Handler) {
			return evalsiv1alpha1connect.NewAnnotationServiceHandler(annotate.New(d.store, d.runs), gated)
		},
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewGuardrailServiceHandler(guard, gated) },
	} {
		path, handler := h()
		mux.Handle(path, handler)
		// Service paths are "/<package>.<Service>/"; Vanguard wants the bare name.
		handlers[strings.Trim(path, "/")] = handler
	}
	rest, err := restHandler(handlers)
	if err != nil {
		// The routes are static; failing here is a programming error caught by tests.
		panic(fmt.Sprintf("REST routes: %v", err))
	}
	mux.Handle("/v1alpha1/", rest)
	registerOTLP(mux, d)
	// agentgateway's inline guardrail protocols: the prompt-guard webhook
	// (LLM traffic) and the ExtMcp processor service (MCP traffic, gRPC).
	mux.Handle("POST /guardrails/{project}/{guardrail}/{phase}", guard.Webhook(d.gate.authorizeGuardrail))
	mux.Handle(ext_mcpconnect.NewExtMcpHandler(guard.ExtMcp(), gated))
	mux.Handle("GET /metrics", d.gate.guardHTTP("metrics.read", false, watch.MetricsHandler(d.watcher, d.assembler, d.rewards.WriteMetrics, d.runs.WriteMetrics, guard.WriteMetrics)))
	services := []string{
		evalsiv1alpha1connect.EvaluationServiceName,
		evalsiv1alpha1connect.CatalogServiceName,
		evalsiv1alpha1connect.RewardServiceName,
		evalsiv1alpha1connect.RunServiceName,
		evalsiv1alpha1connect.MonitorServiceName,
		evalsiv1alpha1connect.TraceServiceName,
		evalsiv1alpha1connect.AuthServiceName,
		evalsiv1alpha1connect.AnnotationServiceName,
		evalsiv1alpha1connect.GuardrailServiceName,
	}
	// Health reports liveness only and stays open, like /healthz.
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(services...)))
	reflector := grpcreflect.NewStaticReflector(services...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector, gated))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector, gated))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if _, err := d.worker.Describe(ctx); err != nil {
			http.Error(w, "worker unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	// What `evalsi login` needs; public by design.
	mux.HandleFunc("GET /.well-known/evalsi-auth", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{"auth_enabled": d.authn.Enabled()}
		if l := d.authn.CLILogin(); l != nil {
			doc["issuer"], doc["client_id"], doc["scopes"], doc["audience"] = l.Issuer, l.ClientID, l.Scopes, l.Audience
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	root := d.authn.Middleware(mux)
	if !d.mcp.Disabled {
		mcp.New(d.mcp, root, d.gate.authorizeTool, d.authn.Issuers, log).Register(mux)
	}
	return root
}

// sandboxEnv tells the worker how to reach the sandbox: code evaluators run
// `$EVALSID sandbox run` with the server's sandbox config. Rungs are probed
// there, on first use; `evalsid sandbox probe` reports them up front. It also
// carries the agent trust policy: on a server, the worker executes only the
// commands and Python references the config lists (the harness enforces it).
func sandboxEnv(cfg config.Config) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg.Sandbox)
	if err != nil {
		return nil, err
	}
	trust, err := json.Marshal(map[string]any{
		"commands": append([][]string{}, cfg.Agents.TrustedCommands...),
		"python":   append([]string{}, cfg.Agents.TrustedPython...),
	})
	if err != nil {
		return nil, err
	}
	return []string{"EVALSID=" + exe, "EVALSI_SANDBOX=" + string(raw), "EVALSI_AGENT_TRUST=" + string(trust)}, nil
}

// rootSpan is the ID of the trace's root span, which exported scores attach to.
func rootSpan(rec *evalsiv1alpha1.Record) string {
	for _, s := range rec.GetTrajectory().GetSteps() {
		if s.GetParentSpanId() == "" {
			return s.GetSpanId()
		}
	}
	return ""
}

// runCluster adapts the cluster to the run manager (nil stays nil: an
// interface holding a nil pointer would not).
func runCluster(cl *cluster.Cluster) runs.Coordinator {
	if cl == nil {
		return nil
	}
	return cl
}

// pruneRewardCache drops shared reward cache entries older than ttl
// (default a week), hourly; every replica may run it.
func pruneRewardCache(ctx context.Context, st *store.Store, ttl string, log *slog.Logger) {
	keep := 7 * 24 * time.Hour
	if d, err := time.ParseDuration(ttl); err == nil && d > 0 {
		keep = d
	}
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		if n, err := st.PruneRewardCache(ctx, time.Now().Add(-keep)); err != nil && ctx.Err() == nil {
			log.Warn("pruning the reward cache", "err", err)
		} else if n > 0 {
			log.Info("pruned the reward cache", "entries", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// loadWasm loads the Wasm evaluator plugins (nil when there are none).
func loadWasm(ctx context.Context, cfg config.Config, log *slog.Logger) (*wasmeval.Host, error) {
	if cfg.Wasm.Disabled {
		return nil, nil
	}
	dirs := cfg.Wasm.PluginDirs
	if len(dirs) == 0 {
		dirs = []string{filepath.Join(cfg.DataDir, "plugins")}
	}
	cache := cfg.Wasm.CacheDir
	switch cache {
	case "":
		cache = filepath.Join(cfg.DataDir, "wasm-cache")
	case "off":
		cache = ""
	}
	h, err := wasmeval.NewHost(cache)
	if err != nil {
		return nil, err
	}
	if err := h.LoadDirs(ctx, dirs); err != nil {
		_ = h.Close(ctx)
		return nil, fmt.Errorf("loading Wasm plugins: %w", err)
	}
	n := len(h.Manifests())
	if n == 0 {
		_ = h.Close(ctx)
		return nil, nil
	}
	log.Info("loaded Wasm evaluators", "count", n, "dirs", dirs)
	return h, nil
}
