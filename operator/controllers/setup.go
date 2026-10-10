package controllers

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Config configures the controllers Setup adds.
type Config struct {
	// Controllers to run: evalrun, onlineevalpolicy, tracesource, evaluator, sandboxclass.
	Enabled               []string
	API                   APIConfig
	PollInterval          time.Duration
	StatsInterval         time.Duration
	Namespace             string
	Image                 string
	SandboxTLSSecret      string
	SandboxAllowClients   []string
	SandboxServiceAccount string
	WorkerConfigMap       string
	// What workers using WorkerConfigMap also need, as the chart's own
	// workers get it: the Secret with their mutual-TLS client certificate for
	// remote sandbox pools, and the one with S3 credentials (access_key,
	// secret_key). Empty when the release uses neither.
	WorkerTLSSecret        string
	WorkerS3Secret         string
	NATSMonitoringEndpoint string
	// Names of what the operator creates start with this (default "evalsi"),
	// so releases of the chart can share a namespace.
	NamePrefix string
}

// childName names an object the operator makes: the prefix (default
// "evalsi"), what it is, and the resource it belongs to.
func childName(prefix, what, name string) string {
	if prefix == "" {
		prefix = "evalsi"
	}
	return prefix + "-" + what + "-" + name
}

// Setup adds the enabled controllers to mgr.
func Setup(mgr ctrl.Manager, cfg Config) error {
	on := func(name string) bool { return slices.Contains(cfg.Enabled, name) }
	for _, name := range cfg.Enabled {
		if !slices.Contains([]string{"evalrun", "onlineevalpolicy", "tracesource", "evaluator", "sandboxclass"}, name) {
			return fmt.Errorf("unknown controller %q", name)
		}
	}
	if on("evalrun") || on("onlineevalpolicy") || on("tracesource") {
		api, err := NewAPI(cfg.API)
		if err != nil {
			return err
		}
		if on("evalrun") {
			if err := (&EvalRunReconciler{Client: mgr.GetClient(), API: api, PollInterval: cfg.PollInterval}).SetupWithManager(mgr); err != nil {
				return err
			}
		}
		if on("onlineevalpolicy") {
			if err := (&PolicyReconciler{Client: mgr.GetClient(), API: api, StatsInterval: cfg.StatsInterval}).SetupWithManager(mgr); err != nil {
				return err
			}
		}
		// Helm does not upgrade CRDs: an evalsi-crds release older than the
		// TraceSource CRD leaves it out, and the operator runs without it.
		if on("tracesource") {
			if _, err := mgr.GetRESTMapper().RESTMapping(schema.GroupKind{Group: "evals.si", Kind: "TraceSource"}, "v1alpha1"); meta.IsNoMatchError(err) {
				ctrl.Log.Info("the TraceSource CRD is not installed; apply the evalsi-crds chart's CRDs to manage trace sources", "controller", "tracesource")
			} else if err := (&TraceSourceReconciler{Client: mgr.GetClient(), API: api, StatsInterval: cfg.StatsInterval}).SetupWithManager(mgr); err != nil {
				return err
			}
		}
	}
	if on("evaluator") {
		if err := (&EvaluatorReconciler{Client: mgr.GetClient(), WorkerConfigMap: cfg.WorkerConfigMap, WorkerTLSSecret: cfg.WorkerTLSSecret, WorkerS3Secret: cfg.WorkerS3Secret, NATSMonitoringEndpoint: cfg.NATSMonitoringEndpoint, NamePrefix: cfg.NamePrefix}).SetupWithManager(mgr); err != nil {
			return err
		}
	}
	if on("sandboxclass") {
		if cfg.Namespace == "" || cfg.Image == "" {
			return errors.New("sandbox pools need a namespace and the evalsi image (--namespace, --image)")
		}
		r := &SandboxClassReconciler{
			Client: mgr.GetClient(), Namespace: cfg.Namespace, Image: cfg.Image, TLSSecret: cfg.SandboxTLSSecret,
			AllowClients: cfg.SandboxAllowClients, ServiceAccount: cfg.SandboxServiceAccount, NamePrefix: cfg.NamePrefix,
		}
		if err := r.SetupWithManager(mgr); err != nil {
			return err
		}
	}
	return nil
}
