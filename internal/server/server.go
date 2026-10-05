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

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"

	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
	"github.com/abhishek-rnjn/evals.si/internal/runs"
	"github.com/abhishek-rnjn/evals.si/internal/sinks"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"github.com/abhishek-rnjn/evals.si/internal/watch"
)

// Run serves until ctx is cancelled, then shuts down gracefully.
// ready, if non-nil, receives the bound address once the server accepts connections.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, workerOutput io.Writer, ready chan<- string) error {
	workerEnv, err := sandboxEnv(cfg)
	if err != nil {
		return err
	}
	worker, err := pluginhost.Start(ctx, pluginhost.Options{
		Env:          workerEnv,
		Command:      cfg.Worker.Command,
		Judges:       cfg.Judges,
		NoCache:      cfg.Worker.NoCache,
		StartTimeout: cfg.StartTimeout(),
		Output:       workerOutput,
		Logger:       log,
	})
	if err != nil {
		return err
	}
	defer worker.Stop()
	return serve(ctx, cfg, worker, log, ready)
}

// serve runs everything above the worker; tests call it with a fake worker.
func serve(ctx context.Context, cfg config.Config, worker pluginhost.Worker, log *slog.Logger, ready chan<- string) error {
	manifests, err := worker.Describe(ctx)
	if err != nil {
		return fmt.Errorf("describing worker evaluators: %w", err)
	}
	judges := make([]string, 0, len(cfg.Judges))
	for name := range cfg.Judges {
		judges = append(judges, name)
	}
	sort.Strings(judges)
	svc := evaluation.New(worker, catalog.New(manifests), judges, cfg.DefaultJudge, cfg.Evaluate)

	st, err := store.Open(filepath.Join(cfg.DataDir, "evalsi.db"))
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
	runManager, err := runs.New(ctx, st, worker, svc, runs.Options{
		DatasetsDir:   cfg.DatasetsDir,
		MaxConcurrent: cfg.Runs.MaxConcurrent,
		Evaluate:      cfg.Evaluate,
		Logger:        log,
		OnFinished:    exports.Run,
	})
	if err != nil {
		return err
	}
	// Runs stop before the worker does (deferred calls run in reverse order).
	defer runManager.Shutdown()

	watcher, err := watch.New(ctx, st, svc, watch.Options{
		DatasetsDir: cfg.DatasetsDir, BatchSize: cfg.Evaluate.BatchSize, Logger: log,
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
	}, watcher.Ingest)
	bg, stopBG := context.WithCancel(context.Background())
	stopAssembler := make(chan struct{})
	var wg sync.WaitGroup
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
	d := deps{svc: svc, runs: runManager, watcher: watcher, store: st, assembler: assembler, worker: worker, authn: authn, gate: g, authSvc: authSvc}

	var tlsCfg *tls.Config
	if cfg.Auth.Enabled() && cfg.Auth.TLS != nil {
		if tlsCfg, err = auth.ServerTLS(cfg.Auth.TLS); err != nil {
			return err
		}
	}
	switch {
	case !cfg.AuthEnabled() && cfg.Auth != nil:
		log.Warn("authentication is turned off (auth.mode: none): anyone who can reach evalsid can use it")
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
	runs      *runs.Manager
	watcher   *watch.Engine
	store     *store.Store
	assembler *ingest.Assembler
	worker    pluginhost.Worker
	authn     *auth.Authenticator
	gate      *gate
	authSvc   *authz.Service
}

// registerOTLP mounts OTLP/gRPC (through the gate's interceptor) and
// OTLP/HTTP (through its HTTP guard). Each resource's project comes from
// the caller's credential (gate.assignTrace).
func registerOTLP(mux *http.ServeMux, d deps) {
	inner := http.NewServeMux()
	ingest.NewReceiver(d.assembler, d.gate.assignTrace, connect.WithInterceptors(d.gate.interceptor())).Register(inner)
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
	handlers := map[string]http.Handler{}
	for _, h := range []func() (string, http.Handler){
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewEvaluationServiceHandler(d.svc, gated) },
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewCatalogServiceHandler(d.svc, gated) },
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewRunServiceHandler(d.runs, gated) },
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewMonitorServiceHandler(d.watcher, gated) },
		func() (string, http.Handler) {
			return evalsiv1alpha1connect.NewTraceServiceHandler(watch.Traces{Store: d.store}, gated)
		},
		func() (string, http.Handler) { return evalsiv1alpha1connect.NewAuthServiceHandler(d.authSvc, gated) },
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
	mux.Handle("GET /metrics", d.gate.guardHTTP("metrics.read", false, watch.MetricsHandler(d.watcher, d.assembler)))
	services := []string{
		evalsiv1alpha1connect.EvaluationServiceName,
		evalsiv1alpha1connect.CatalogServiceName,
		evalsiv1alpha1connect.RunServiceName,
		evalsiv1alpha1connect.MonitorServiceName,
		evalsiv1alpha1connect.TraceServiceName,
		evalsiv1alpha1connect.AuthServiceName,
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
	return d.authn.Middleware(mux)
}

// sandboxEnv tells the worker how to reach the sandbox: code evaluators run
// `$EVALSID sandbox run` with the server's sandbox config. Rungs are probed
// there, on first use; `evalsid sandbox probe` reports them up front.
func sandboxEnv(cfg config.Config) ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg.Sandbox)
	if err != nil {
		return nil, err
	}
	return []string{"EVALSID=" + exe, "EVALSI_SANDBOX=" + string(raw)}, nil
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
