// Package server assembles evalsid: it starts the evaluator worker, builds the
// services and serves them on one port for gRPC (HTTP/2 without TLS),
// gRPC-Web and Connect HTTP/JSON.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"google.golang.org/protobuf/encoding/protojson"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"

	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
	"github.com/abhishek-rnjn/evals.si/internal/runs"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"github.com/abhishek-rnjn/evals.si/internal/watch"
)

// Run serves until ctx is cancelled, then shuts down gracefully.
// ready, if non-nil, receives the bound address once the server accepts connections.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger, workerOutput io.Writer, ready chan<- string) error {
	worker, err := pluginhost.Start(ctx, pluginhost.Options{
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
	runManager, err := runs.New(ctx, st, worker, svc, runs.Options{
		DatasetsDir:   cfg.DatasetsDir,
		MaxConcurrent: cfg.Runs.MaxConcurrent,
		Evaluate:      cfg.Evaluate,
		Logger:        log,
	})
	if err != nil {
		return err
	}
	// Runs stop before the worker does (deferred calls run in reverse order).
	defer runManager.Shutdown()

	watcher, err := watch.New(ctx, st, svc, watch.Options{DatasetsDir: cfg.DatasetsDir, BatchSize: cfg.Evaluate.BatchSize, Logger: log})
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
	wg.Add(3)
	go func() { defer wg.Done(); assembler.Run(stopAssembler, 250*time.Millisecond) }()
	go func() { defer wg.Done(); watcher.Run(bg) }()
	go func() { defer wg.Done(); retain(bg, st, config.Duration(cfg.Traces.Retention), log) }()
	defer func() {
		close(stopAssembler) // flushes buffered traces into the engine
		stopBG()
		wg.Wait()
	}()
	receiver := ingest.NewReceiver(assembler)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	handler := Handler(svc, runManager, watcher, st, receiver, assembler, worker)
	srv := &http.Server{
		Handler:           handler,
		Protocols:         protocols(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 4)
	go func() { errc <- srv.Serve(ln) }()
	// The standard OTLP ports serve only OTLP.
	otlpMux := http.NewServeMux()
	receiver.Register(otlpMux)
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
		s := &http.Server{Handler: otlpMux, Protocols: protocols(), ReadHeaderTimeout: 10 * time.Second}
		extra = append(extra, s)
		go func() { errc <- s.Serve(l) }()
		log.Info("otlp listening", "addr", l.Addr().String())
	}
	log.Info("evalsid listening", "addr", ln.Addr().String(), "evaluators", len(manifests), "judges", judges)
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

func protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// Handler routes every service. Exposed for tests.
func Handler(svc *evaluation.Service, runManager *runs.Manager, watcher *watch.Engine, st *store.Store,
	receiver *ingest.Receiver, assembler *ingest.Assembler, worker pluginhost.Worker,
) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(evalsiv1alpha1connect.NewEvaluationServiceHandler(svc))
	mux.Handle(evalsiv1alpha1connect.NewCatalogServiceHandler(svc))
	mux.Handle(evalsiv1alpha1connect.NewRunServiceHandler(runManager))
	mux.Handle(evalsiv1alpha1connect.NewMonitorServiceHandler(watcher))
	mux.Handle(evalsiv1alpha1connect.NewTraceServiceHandler(watch.Traces{Store: st}))
	receiver.Register(mux)
	mux.Handle("GET /metrics", watch.MetricsHandler(watcher, assembler))
	services := []string{
		evalsiv1alpha1connect.EvaluationServiceName,
		evalsiv1alpha1connect.CatalogServiceName,
		evalsiv1alpha1connect.RunServiceName,
		evalsiv1alpha1connect.MonitorServiceName,
		evalsiv1alpha1connect.TraceServiceName,
	}
	mux.Handle(grpchealth.NewHandler(grpchealth.NewStaticChecker(services...)))
	reflector := grpcreflect.NewStaticReflector(services...)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if _, err := worker.Describe(ctx); err != nil {
			http.Error(w, "worker unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}
