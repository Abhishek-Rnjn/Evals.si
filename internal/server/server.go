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
	"time"

	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"

	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/catalog"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
	"github.com/abhishek-rnjn/evals.si/internal/runs"
	"github.com/abhishek-rnjn/evals.si/internal/store"
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

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           Handler(svc, runManager, worker),
		Protocols:         protocols(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
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
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func protocols() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

// Handler routes every service. Exposed for tests.
func Handler(svc *evaluation.Service, runManager *runs.Manager, worker pluginhost.Worker) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(evalsiv1alpha1connect.NewEvaluationServiceHandler(svc))
	mux.Handle(evalsiv1alpha1connect.NewCatalogServiceHandler(svc))
	mux.Handle(evalsiv1alpha1connect.NewRunServiceHandler(runManager))
	services := []string{
		evalsiv1alpha1connect.EvaluationServiceName,
		evalsiv1alpha1connect.CatalogServiceName,
		evalsiv1alpha1connect.RunServiceName,
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
