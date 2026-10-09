package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/abhishek-rnjn/evals.si/internal/cluster"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
	"github.com/abhishek-rnjn/evals.si/internal/pluginhost"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox/sandboxsvc"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"github.com/abhishek-rnjn/evals.si/internal/watch"
)

// startLocalWorker starts the Python worker, with the sandbox service it
// creates agent sandboxes through, and returns it with a function that stops
// both. Dataset objects on S3 are fetched next to it.
func startLocalWorker(ctx context.Context, cfg config.Config, log *slog.Logger, output io.Writer) (pluginhost.Worker, func(), error) {
	workerEnv, err := sandboxEnv(cfg)
	if err != nil {
		return nil, nil, err
	}
	if remote := cfg.Worker.SandboxService; remote != nil {
		// Sandboxes live in the sandbox pool; none run here.
		workerEnv = append(workerEnv,
			"EVALSI_SANDBOX_ADDR="+remote.Address, "EVALSI_SANDBOX_TLS_CA="+remote.CAFile,
			"EVALSI_SANDBOX_TLS_CERT="+remote.CertFile, "EVALSI_SANDBOX_TLS_KEY="+remote.KeyFile)
		if remote.ServerName != "" {
			workerEnv = append(workerEnv, "EVALSI_SANDBOX_TLS_SERVER_NAME="+remote.ServerName)
		}
		return startPython(ctx, cfg, workerEnv, log, output, nil)
	}
	// Agent harnesses in the worker create persistent sandboxes through
	// SandboxService on a private socket; it is never on the API port.
	sb, err := sandbox.New(cfg.Sandbox)
	if err != nil {
		return nil, nil, err
	}
	sandboxes := sandbox.NewManager(sb)
	var cleanups []func()
	stop := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	cleanups = append(cleanups, func() { sandboxes.Close() })
	sockDir, err := os.MkdirTemp("", "evalsid-sandbox-")
	if err != nil {
		stop()
		return nil, nil, err
	}
	cleanups = append(cleanups, func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "sandbox.sock")
	stopSandbox, err := sandboxsvc.Serve(ctx, sandboxes, socket)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("sandbox service: %w", err)
	}
	cleanups = append(cleanups, stopSandbox)
	workerEnv = append(workerEnv, "EVALSI_SANDBOX_ADDR=unix://"+socket)
	return startPython(ctx, cfg, workerEnv, log, output, cleanups)
}

// startPython starts the Python worker; cleanups run, in reverse, after it stops.
func startPython(ctx context.Context, cfg config.Config, workerEnv []string, log *slog.Logger, output io.Writer, cleanups []func()) (pluginhost.Worker, func(), error) {
	stop := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	proc, err := pluginhost.Start(ctx, pluginhost.Options{
		Env:          workerEnv,
		Command:      cfg.Worker.Command,
		Judges:       cfg.WorkerJudges(),
		NoCache:      cfg.Worker.NoCache,
		StartTimeout: cfg.StartTimeout(),
		Output:       output,
		Logger:       log,
	})
	if err != nil {
		stop()
		return nil, nil, err
	}
	cleanups = append(cleanups, proc.Stop)
	var objects *objstore.Client
	if s3 := cfg.Storage.S3; s3 != nil {
		if objects, err = objstore.New(*s3); err != nil {
			stop()
			return nil, nil, fmt.Errorf("storage.s3: %w", err)
		}
	}
	return pluginhost.WithObjects(proc, objects, filepath.Join(cfg.DataDir, "cache", "objects")), stop, nil
}

// RunWorker is `evalsid worker`: a Python worker serving the given pools
// from the cluster's work queues until ctx ends. In-flight tasks finish
// before it returns.
func RunWorker(ctx context.Context, cfg config.Config, pools []string, log *slog.Logger, output io.Writer) error {
	if cfg.Cluster == nil {
		return fmt.Errorf("evalsid worker needs a cluster section in the config")
	}
	if len(pools) == 0 {
		pools = cluster.Pools
	}
	for _, p := range pools {
		if !validPool(p) {
			return fmt.Errorf("unknown pool %q (pools: %v)", p, cluster.Pools)
		}
	}
	cl, err := cluster.Connect(ctx, *cfg.Cluster, "evalsid-worker", cfg.DataDir)
	if err != nil {
		return err
	}
	defer cl.Close()
	local, stop, err := startLocalWorker(ctx, cfg, log, output)
	if err != nil {
		return err
	}
	defer stop()
	log.Info("worker serving", "pools", pools, "concurrency", cfg.Worker.Concurrency)
	return cl.Serve(ctx, pools, local, cfg.Worker.Concurrency, log)
}

func validPool(p string) bool {
	for _, q := range cluster.Pools {
		if p == q {
			return true
		}
	}
	return false
}

// leadPolicyEngine competes for the policy-engine lease. The holder consumes
// the span stream into its assembler, evaluates policies and answers
// statistics; the others only forward spans. A replica that stalls past the
// lease loses it and steps down; traces it held in its assembler are lost.
func leadPolicyEngine(ctx context.Context, st *store.Store, cl *cluster.Cluster, assembler *ingest.Assembler, watcher *watch.Engine, log *slog.Logger) {
	const name, ttl = "policy-engine", 15 * time.Second
	owner := cl.Owner()
	var stepDown func()
	defer func() {
		if stepDown != nil {
			stepDown()
			_ = st.ReleaseLease(context.Background(), name, owner)
		}
	}()
	for {
		ok, err := st.AcquireLease(ctx, name, owner, ttl)
		if err != nil && ctx.Err() == nil {
			log.Warn("policy engine lease", "err", err)
		}
		switch {
		case ok && stepDown == nil:
			log.Info("elected policy engine", "owner", owner)
			_ = watcher.Reload(ctx)
			watcher.SetLeader(true)
			lctx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				err := cl.ConsumeSpans(lctx, func(rs *tracepb.ResourceSpans, project string, labels map[string]string) {
					assembler.Add(ingest.SpansOfResource(rs, project, labels))
				})
				if err != nil && lctx.Err() == nil {
					log.Error("consuming spans", "err", err)
				}
			}()
			stopStats, err := cl.ServeStats(watcher.LocalStats)
			if err != nil {
				log.Error("serving policy statistics", "err", err)
				stopStats = func() {}
			}
			stepDown = func() {
				cancel()
				<-done
				stopStats()
				watcher.SetLeader(false)
			}
		case !ok && stepDown != nil:
			log.Warn("lost the policy engine lease; stepping down", "owner", owner)
			stepDown()
			stepDown = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ttl / 3):
		}
	}
}

// optDuration parses an optional duration; zero when unset or invalid
// (config validation reports invalid ones).
func optDuration(s string) time.Duration {
	d, _ := time.ParseDuration(s)
	return d
}
