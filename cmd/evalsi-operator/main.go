// Command evalsi-operator reconciles evals.si resources: EvalRun and
// OnlineEvalPolicy through the evalsid API, Evaluator and SandboxClass as
// worker and sandbox-pool Deployments. It also serves the admission
// webhooks.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/abhishek-rnjn/evals.si/internal/version"
	v1 "github.com/abhishek-rnjn/evals.si/operator/api/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/operator/controllers"
	evalsiwebhook "github.com/abhishek-rnjn/evals.si/operator/webhook"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "evalsi-operator: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		api           controllers.APIConfig
		allowClients  list
		watchNS       list
		namespace     = os.Getenv("POD_NAMESPACE")
		enabled       = "evalrun,onlineevalpolicy,evaluator,sandboxclass"
		image         string
		tlsSecret     = "evalsi-sandbox-tls"
		sandboxSA     = "evalsi-sandboxd"
		workerConfig  = "evalsi-worker"
		natsMonitor   string
		poll          = 5 * time.Second
		statsInterval = 30 * time.Second
		webhooks      = true
		webhookPort   = 9443
		certDir       string
		metricsAddr   = ":8080"
		healthAddr    = ":8081"
		leaderElect   = true
		showVersion   bool
	)
	flag.StringVar(&api.URL, "evalsid-url", os.Getenv("EVALSI_SERVER"), "the evalsid API (https://evalsi.<namespace>.svc:8080)")
	flag.StringVar(&api.TokenFile, "token-file", "", "bearer token for the API, read per call (a projected service-account token)")
	flag.StringVar(&api.CAFile, "ca-file", "", "CA for an https evalsid URL")
	flag.StringVar(&api.CertFile, "cert-file", "", "client certificate for mutual TLS to evalsid")
	flag.StringVar(&api.KeyFile, "key-file", "", "its key")
	flag.Var(&watchNS, "watch-namespace", "only watch this namespace (repeatable; default: all)")
	flag.StringVar(&namespace, "namespace", namespace, "where sandbox pools run (default: $POD_NAMESPACE)")
	flag.StringVar(&enabled, "controllers", enabled, "controllers to run")
	flag.StringVar(&image, "image", os.Getenv("EVALSI_IMAGE"), "the evalsi image, for sandbox pools")
	flag.StringVar(&tlsSecret, "sandbox-tls-secret", tlsSecret, "TLS Secret (with ca.crt) for sandbox pools")
	flag.Var(&allowClients, "sandbox-allow-client", "a client allowed into sandbox pools (repeatable)")
	flag.StringVar(&sandboxSA, "sandbox-service-account", sandboxSA, "service account of sandbox pools")
	flag.StringVar(&workerConfig, "worker-config-map", workerConfig, "default ConfigMap with the workers' evalsi.yaml")
	flag.StringVar(&natsMonitor, "nats-monitoring-endpoint", "", "NATS monitoring host:port, for KEDA scaling")
	flag.DurationVar(&poll, "poll-interval", poll, "how often running runs are polled")
	flag.DurationVar(&statsInterval, "stats-interval", statsInterval, "how often policy counters are refreshed")
	flag.BoolVar(&webhooks, "webhooks", webhooks, "serve the admission webhooks")
	flag.IntVar(&webhookPort, "webhook-port", webhookPort, "")
	flag.StringVar(&certDir, "webhook-cert-dir", "", "directory with tls.crt and tls.key for the webhooks")
	flag.StringVar(&metricsAddr, "metrics-bind-address", metricsAddr, "")
	flag.StringVar(&healthAddr, "health-probe-bind-address", healthAddr, "")
	flag.BoolVar(&leaderElect, "leader-elect", leaderElect, "elect one active replica")
	flag.BoolVar(&showVersion, "version", false, "print the version")
	debug := flag.Bool("debug", false, "log at debug level")
	flag.Parse()
	if showVersion {
		fmt.Println(version.Version)
		return nil
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1.AddToScheme(scheme); err != nil {
		return err
	}
	mo := ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  healthAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "evalsi-operator.evals.si",
		LeaderElectionNamespace: namespace,
	}
	if len(watchNS) > 0 {
		mo.Cache.DefaultNamespaces = map[string]cache.Config{}
		for _, ns := range watchNS {
			mo.Cache.DefaultNamespaces[ns] = cache.Config{}
		}
	}
	if webhooks {
		mo.WebhookServer = webhook.NewServer(webhook.Options{Port: webhookPort, CertDir: certDir})
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mo)
	if err != nil {
		return err
	}
	if err := controllers.Setup(mgr, controllers.Config{
		Enabled: strings.Split(enabled, ","), API: api, PollInterval: poll, StatsInterval: statsInterval,
		Namespace: namespace, Image: image, SandboxTLSSecret: tlsSecret, SandboxAllowClients: allowClients,
		SandboxServiceAccount: sandboxSA, WorkerConfigMap: workerConfig, NATSMonitoringEndpoint: natsMonitor,
	}); err != nil {
		return err
	}
	if webhooks {
		evalsiwebhook.Register(mgr.GetWebhookServer())
		if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			return err
		}
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if !webhooks {
		if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
			return err
		}
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
