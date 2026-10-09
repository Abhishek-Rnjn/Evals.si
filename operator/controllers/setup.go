package controllers

import (
	"errors"
	"fmt"
	"slices"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Config configures the controllers Setup adds.
type Config struct {
	// Controllers to run: evalrun, onlineevalpolicy, evaluator, sandboxclass.
	Enabled                []string
	API                    APIConfig
	PollInterval           time.Duration
	StatsInterval          time.Duration
	Namespace              string
	Image                  string
	SandboxTLSSecret       string
	SandboxAllowClients    []string
	SandboxServiceAccount  string
	WorkerConfigMap        string
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
		if !slices.Contains([]string{"evalrun", "onlineevalpolicy", "evaluator", "sandboxclass"}, name) {
			return fmt.Errorf("unknown controller %q", name)
		}
	}
	if on("evalrun") || on("onlineevalpolicy") {
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
	}
	if on("evaluator") {
		if err := (&EvaluatorReconciler{Client: mgr.GetClient(), WorkerConfigMap: cfg.WorkerConfigMap, NATSMonitoringEndpoint: cfg.NATSMonitoringEndpoint, NamePrefix: cfg.NamePrefix}).SetupWithManager(mgr); err != nil {
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
