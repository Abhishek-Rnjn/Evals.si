package helm

import (
	"path/filepath"
	"strings"
	"testing"
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
			main := renderDir(t, "evalsi", charts+"/evalsi", demoValues, overlays+shape+".yaml")
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
		})
	}
}
