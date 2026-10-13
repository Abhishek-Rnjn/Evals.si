package helm

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhishek-rnjn/evals.si/operator/bootstrap"
)

// The demo chart (examples/demo) holds to the same placement rules as the main
// chart: every pod and Service it adds takes tolerations, nodeSelector, labels and
// the image registry as values (PRD R8).

const demoChart = "../../examples/demo"

func demo(t *testing.T, set ...string) []object {
	t.Helper()
	return renderDir(t, "evalsi-demo", demoChart, set...)
}

func workloads(objs []object) []object {
	var out []object
	for _, o := range objs {
		switch o.Kind {
		case "Deployment", "StatefulSet", "Job":
			out = append(out, o)
		}
	}
	return out
}

func podSpec(t *testing.T, o object) string {
	t.Helper()
	tpl, _ := o.Spec["template"].(map[string]any)
	return toYAML(t, tpl)
}

func TestDemoChartPlacement(t *testing.T) {
	objs := demo(t,
		"imageRegistry=mirror.example.com/team",
		"tolerations[0].key=dedicated", "tolerations[0].operator=Exists",
		"nodeSelector.pool=evals",
		"podLabels.team=evals", `serviceLabels.istio\.io/use-waypoint=none`,
		"mlflow.persistence.enabled=true", "mlflow.persistence.storageClassName=fast",
	)
	ws := workloads(objs)
	if len(ws) < 5 {
		t.Fatalf("expected the model, two agents, MLflow and the datasets Job; got %d workloads", len(ws))
	}
	for _, w := range ws {
		spec := podSpec(t, w)
		for _, want := range []string{"key: dedicated", "pool: evals", "team: evals", "image: mirror.example.com/team/"} {
			if !strings.Contains(spec, want) {
				t.Errorf("%s %s: no %q in %s", w.Kind, w.Metadata.Name, want, spec)
			}
		}
	}
	services := find2(objs, "Service")
	if len(services) == 0 {
		t.Fatal("no Services")
	}
	for _, s := range services {
		if s.Metadata.Labels["istio.io/use-waypoint"] != "none" {
			t.Errorf("Service %s labels %v", s.Metadata.Name, s.Metadata.Labels)
		}
	}
	pvc := toYAML(t, find(t, objs, "PersistentVolumeClaim", "evalsi-demo-mlflow").Spec)
	if !strings.Contains(pvc, "storageClassName: fast") {
		t.Errorf("MLflow PVC: %s", pvc)
	}
}

func TestDemoChartPodsAreRestricted(t *testing.T) {
	for _, w := range workloads(demo(t)) {
		if v := restrictedViolations(t, w); len(v) > 0 {
			t.Errorf("%s %s breaks the restricted Pod Security profile: %v", w.Kind, w.Metadata.Name, v)
		}
	}
}

func TestDemoMockUsesBundledPython(t *testing.T) {
	model := find(t, demo(t, "model.image=airgap.invalid/python:3.13-slim"), "Deployment", "evalsi-demo-model")
	if spec := podSpec(t, model); !strings.Contains(spec, "image: airgap.invalid/python:3.13-slim") {
		t.Errorf("mock model does not reuse the bundled image: %s", spec)
	}
}

func TestDemoServicesHaveTheNamesSpecsUse(t *testing.T) {
	objs := demo(t)
	for _, name := range []string{"evalsi-demo-model", "evalsi-demo-deepagents", "evalsi-demo-mlflow"} {
		find(t, objs, "Service", name)
	}
	// A release of another name keeps the prefix off the fixed names only when asked to.
	other := renderDir(t, "other", demoChart, "fullnameOverride=evalsi-demo")
	find(t, other, "Service", "evalsi-demo-model")
}

func TestDemoModelKey(t *testing.T) {
	// No key given: the chart expects the Secret to exist and creates nothing.
	for _, o := range demo(t) {
		if o.Kind == "Secret" {
			t.Errorf("a Secret was created without a key: %s", o.Metadata.Name)
		}
	}
	objs := demo(t, "agents.model.apiKey=sk-test")
	find(t, objs, "Secret", "evalsi-demo-model-key")
}

func TestDemoDatasetsJob(t *testing.T) {
	objs := demo(t)
	job := find(t, objs, "Job", "evalsi-demo-datasets")
	if job.Metadata.Annotations["helm.sh/hook"] != "post-install,post-upgrade" {
		t.Errorf("hook annotation %v", job.Metadata.Annotations)
	}
	if spec := podSpec(t, job); !strings.Contains(spec, "datasets") {
		t.Errorf("the Job does not run `evalsid datasets put`: %s", spec)
	}
	// The ConfigMap the Job mounts must still exist when the post-install hook
	// runs: an ordinary resource, never a hook Helm deletes once it succeeds.
	cm := find(t, objs, "ConfigMap", "evalsi-demo-datasets")
	if hook := cm.Metadata.Annotations["helm.sh/hook"]; hook != "" {
		t.Errorf("the datasets ConfigMap is a %q hook; Helm deletes it before the Job mounts it", hook)
	}
}

// The overlays render with the main chart and with the demo chart, and each does
// what its name says.
func TestDemoOverlays(t *testing.T) {
	abs, err := filepath.Abs("../../examples/demo/overlays")
	if err != nil {
		t.Fatal(err)
	}
	overlays := abs + "/"
	demoValues := overlays + "evalsi-demo.yaml"
	for _, shape := range []string{"kind", "tainted-nodes", "istio-ambient"} {
		t.Run(shape, func(t *testing.T) {
			main := renderDir(t, "evalsi", charts+"/evalsi", demoValues, overlays+"pod-pool.yaml", overlays+shape+".yaml")
			dm := renderDir(t, "evalsi-demo", demoChart, overlays+shape+".yaml")
			switch shape {
			case "tainted-nodes":
				for _, w := range append(workloads(main), workloads(dm)...) {
					spec := podSpec(t, w)
					if !strings.Contains(spec, "tolerations") || !strings.Contains(spec, "nodeSelector") {
						t.Errorf("%s %s has no placement: %s", w.Kind, w.Metadata.Name, spec)
					}
					if !strings.Contains(spec, "registry.example.com/mirror/") {
						t.Errorf("%s %s image is not from the mirror: %s", w.Kind, w.Metadata.Name, spec)
					}
				}
			case "istio-ambient":
				for _, s := range find2(dm, "Service") {
					if s.Metadata.Labels["istio.io/use-waypoint"] != "none" {
						t.Errorf("Service %s labels %v", s.Metadata.Name, s.Metadata.Labels)
					}
				}
				find(t, main, "PeerAuthentication", "evalsi-operator-webhook")
			}
			// R6: the server may reach the demo's plain-HTTP MLflow.
			if cfg := find(t, main, "ConfigMap", "evalsi").Data["evalsi.yaml"]; !strings.Contains(cfg, "- evalsi-demo-mlflow") {
				t.Errorf("%s: sources.allow_hosts lacks the demo MLflow:\n%s", shape, cfg)
			}
		})
	}
}

// M2: the research assistant's deploy gate. A release waits as the candidate,
// which traces nowhere; production carries its workflow and version on every
// trace and, with otlp set, sends them to Evals.si as well as to MLflow.
func TestDemoDeployGateCandidate(t *testing.T) {
	for _, o := range demo(t) {
		if o.Metadata.Name == "evalsi-demo-deepagents-candidate" {
			t.Errorf("a candidate %s without deepagents.candidate.enabled", o.Kind)
		}
	}
	objs := demo(t,
		"deepagents.candidate.enabled=true", "deepagents.candidate.version=v2", "deepagents.candidate.model=mock-regressed",
		"deepagents.otlp.endpoint=https://evalsi:4318/",
		"deepagents.otlp.apiKeySecret.name=evalsi-demo-ingest-key", "deepagents.otlp.caSecret.name=evalsi-server-tls",
	)
	find(t, objs, "Service", "evalsi-demo-deepagents-candidate")
	candidate := find(t, objs, "Deployment", "evalsi-demo-deepagents-candidate")
	spec := podSpec(t, candidate)
	for _, want := range []string{"value: openai:mock-regressed", "evalsi.label.version=v2", "evalsi.label.stage=candidate"} {
		if !strings.Contains(spec, want) {
			t.Errorf("candidate: no %q in %s", want, spec)
		}
	}
	for _, unwanted := range []string{"MLFLOW_TRACKING_URI", "OTEL_EXPORTER_OTLP", "evalsi-ca"} {
		if strings.Contains(spec, unwanted) {
			t.Errorf("candidate traces somewhere (%s): %s", unwanted, spec)
		}
	}
	if candidate.Metadata.Annotations["evals.si/version"] != "v2" {
		t.Errorf("candidate annotations %v", candidate.Metadata.Annotations)
	}

	production := find(t, objs, "Deployment", "evalsi-demo-deepagents")
	spec = podSpec(t, production)
	for _, want := range []string{
		"value: openai:mock", "evalsi.label.workflow=research-assistant,evalsi.label.version=v1,evalsi.label.stage=production",
		"MLFLOW_TRACKING_URI", "name: MLFLOW_TRACE_ENABLE_OTLP_DUAL_EXPORT", "value: https://evalsi:4318/v1/traces",
		"name: evalsi-demo-ingest-key", "authorization=Bearer%20$(EVALSI_INGEST_KEY)",
		"value: /etc/evalsi-ca/ca.crt", "secretName: evalsi-server-tls",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("production: no %q in %s", want, spec)
		}
	}
	if production.Metadata.Annotations["evals.si/version"] != "v1" {
		t.Errorf("production annotations %v", production.Metadata.Annotations)
	}

	// Without MLflow, OTLP is the only destination, so there is nothing to export twice to.
	spec = podSpec(t, find(t, demo(t, "mlflow.enabled=false", "deepagents.otlp.endpoint=https://evalsi:4318"), "Deployment", "evalsi-demo-deepagents"))
	if strings.Contains(spec, "DUAL_EXPORT") || strings.Contains(spec, "MLFLOW_TRACKING_URI") {
		t.Errorf("dual export without MLflow: %s", spec)
	}
}

// M2: the demo overlay's bootstrap gives the project an ingest key, a reader
// key and a default policy, in a file the bootstrap command accepts.
func TestDemoOverlayBootstrapsTheDefaultPolicy(t *testing.T) {
	abs, err := filepath.Abs("../../examples/demo/overlays")
	if err != nil {
		t.Fatal(err)
	}
	objs := renderDir(t, "evalsi", charts+"/evalsi", abs+"/evalsi-demo.yaml", abs+"/pod-pool.yaml")
	raw := find(t, objs, "ConfigMap", "evalsi-bootstrap").Data["bootstrap.json"]
	cfg, err := bootstrap.Load([]byte(raw))
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	secrets := map[string]bool{}
	for _, k := range cfg.APIKeys {
		secrets[k.Secret.Name] = true
	}
	if !secrets["evalsi-demo-ingest-key"] || !secrets["evalsi-demo-reader-key"] {
		t.Errorf("keys %+v", cfg.APIKeys)
	}
	if len(cfg.Policies) != 1 || !strings.Contains(string(cfg.Policies[0]), `"studio-default"`) {
		t.Errorf("policies %s", raw)
	}
}
