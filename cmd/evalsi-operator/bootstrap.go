package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/abhishek-rnjn/evals.si/operator/bootstrap"
	"github.com/abhishek-rnjn/evals.si/operator/controllers"
)

// runBootstrap is `evalsi-operator bootstrap`: the chart's bootstrap Job.
// It creates the projects, API keys (kept in Secrets), policies and webhooks
// a bootstrap file lists, and can be run again safely.
func runBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	var (
		api       controllers.APIConfig
		file      string
		namespace = os.Getenv("POD_NAMESPACE")
		wait      = 10 * time.Minute
	)
	fs.StringVar(&file, "config", "", "the bootstrap file (JSON)")
	fs.StringVar(&api.URL, "evalsid-url", os.Getenv("EVALSI_SERVER"), "the evalsid API")
	fs.StringVar(&api.TokenFile, "token-file", "", "bearer token for the API (a projected service-account token)")
	fs.StringVar(&api.CAFile, "ca-file", "", "CA for an https evalsid URL")
	fs.StringVar(&namespace, "namespace", namespace, "where the Secrets are kept (default: $POD_NAMESPACE)")
	fs.DurationVar(&wait, "wait", wait, "how long to wait for evalsid to answer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if file == "" || namespace == "" {
		return fmt.Errorf("--config and --namespace are required")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	cfg, err := bootstrap.Load(raw)
	if err != nil {
		return err
	}
	client, err := controllers.NewAPI(api)
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(ctrl.GetConfigOrDie())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r := &bootstrap.Runner{
		API: client, Log: log, Wait: wait,
		Secrets: bootstrap.KubeSecrets{Client: kube, Namespace: namespace, Labels: map[string]string{
			"app.kubernetes.io/part-of": "evalsi", "app.kubernetes.io/managed-by": "evalsi-bootstrap",
		}},
	}
	if err := r.Run(ctx, cfg); err != nil {
		return err
	}
	log.Info("bootstrap done")
	return nil
}
