// Package helm checks the Helm charts: they render, every evalsi.yaml they
// render is a valid config, and the CRDs match the generated ones.
package helm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/abhishek-rnjn/evals.si/internal/config"
)

const charts = "../../deploy/helm"

func helm(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("HELM")
	if bin == "" {
		bin = "helm"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		if os.Getenv("EVALSI_REQUIRE_HELM") != "" {
			t.Fatal("helm is not installed")
		}
		t.Skip("helm is not installed")
	}
	return path
}

type object struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
	Spec map[string]any    `json:"spec"`
}

func render(t *testing.T, chart string, set ...string) []object {
	t.Helper()
	args := []string{"template", "t", filepath.Join(charts, chart), "-n", "evalsi", "--kube-version", "1.34.0"}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	var out, stderr bytes.Buffer
	cmd := exec.Command(helm(t), args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm %v: %v\n%s", args, err, stderr.String())
	}
	var objs []object
	for _, doc := range strings.Split(out.String(), "\n---") {
		var o object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("%v\n%s", err, doc)
		}
		if o.Kind != "" {
			objs = append(objs, o)
		}
	}
	return objs
}

func find(t *testing.T, objs []object, kind, name string) object {
	t.Helper()
	for _, o := range objs {
		if o.Kind == kind && o.Metadata.Name == name {
			return o
		}
	}
	t.Fatalf("no %s %s", kind, name)
	return object{}
}

// load checks a rendered evalsi.yaml the way evalsid does at startup.
func load(t *testing.T, cm object) config.Config {
	t.Helper()
	// The pod's environment and volumes, here.
	for _, env := range []string{"EVALSI_POSTGRES_DSN", "EVALSI_CLICKHOUSE_PASSWORD", "EVALSI_S3_ACCESS_KEY", "EVALSI_S3_SECRET_KEY"} {
		t.Setenv(env, "set-by-a-secret")
	}
	dir := t.TempDir()
	for _, d := range []string{"data", "datasets"} {
		_ = os.MkdirAll(filepath.Join(dir, d), 0o700)
	}
	f := filepath.Join(dir, "evalsi.yaml")
	text := strings.ReplaceAll(cm.Data["evalsi.yaml"], "/var/lib/evalsi/", dir+"/")
	if err := os.WriteFile(f, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("%s: %v\n%s", cm.Metadata.Name, err, cm.Data["evalsi.yaml"])
	}
	return cfg
}

func TestEvalsiChart(t *testing.T) {
	for name, set := range map[string][]string{
		"defaults": nil,
		"production": {
			"server.replicas=3", "storage.postgres.dsnSecret.name=pg",
			"storage.clickhouse.url=https://clickhouse:8443", "storage.clickhouse.passwordSecret.name=ch",
			"storage.s3.bucket=evals", "storage.s3.credentialsSecret=s3", "keda.enabled=true",
			"sandbox.address=tls://evalsi-sandboxd.evalsi.svc:7443", "operator.sandboxClasses=true",
			"nats.url=nats://nats.nats.svc:4222", "nats.streamReplicas=3", "global.imageRegistry=registry.internal:5000",
		},
		"plaintext": {"server.tls=false", "server.persistence.enabled=true", "auth.kubernetes.enabled=false", "auth.config.mode=none"},
	} {
		t.Run(name, func(t *testing.T) {
			objs := render(t, "evalsi", set...)
			server := load(t, find(t, objs, "ConfigMap", "evalsi"))
			worker := load(t, find(t, objs, "ConfigMap", "evalsi-worker"))
			if server.Cluster == nil || worker.Cluster == nil || server.Cluster.URL != worker.Cluster.URL {
				t.Errorf("cluster %+v %+v", server.Cluster, worker.Cluster)
			}
			for _, pool := range []string{"cpu", "judge", "sandbox", "harness"} {
				find(t, objs, "Deployment", "evalsi-worker-"+pool)
			}
			switch name {
			case "defaults":
				p := server.Auth.JWT.Providers[0]
				if p.Kubernetes == nil || p.Audiences[0] != "evals.si" || server.Auth.TLS == nil {
					t.Errorf("auth %+v", server.Auth)
				}
				if server.RBAC.Owners[0] != "user:kubernetes/system:serviceaccount:evalsi:evalsi-operator" {
					t.Errorf("owners %v", server.RBAC.Owners)
				}
				find(t, objs, "StatefulSet", "evalsi-nats")
				find(t, objs, "NetworkPolicy", "evalsi-sandbox-pods")
			case "production":
				if server.Storage.Postgres.DSNEnv == "" || server.Storage.ClickHouse == nil || server.DatasetsDir != "s3://evals/datasets" || server.Cluster.Replicas != 3 {
					t.Errorf("storage %+v %q", server.Storage, server.DatasetsDir)
				}
				if s := worker.Worker.SandboxService; s == nil || s.CertFile == "" || worker.Storage.S3 == nil || worker.Storage.Postgres != nil {
					t.Errorf("worker %+v %+v", worker.Worker, worker.Storage)
				}
				find(t, objs, "ScaledObject", "evalsi-worker-judge")
				for _, o := range objs {
					if o.Kind == "StatefulSet" {
						t.Errorf("%s with an external NATS", o.Metadata.Name)
					}
				}
				out := find(t, objs, "Deployment", "evalsi-operator")
				if !strings.Contains(toYAML(t, out.Spec), "--image=registry.internal:5000/abhishek-rnjn/evalsi:") || !strings.Contains(toYAML(t, out.Spec), "sandboxclass") {
					t.Errorf("operator %s", toYAML(t, out.Spec))
				}
			}
		})
	}
	// Several replicas without a shared database are refused.
	cmd := exec.Command(helm(t), "template", "t", filepath.Join(charts, "evalsi"), "--kube-version", "1.34.0", "--set", "server.replicas=2")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "needs storage.postgres") {
		t.Errorf("replicas without postgres: %v %s", err, out)
	}
}

func TestSandboxdChart(t *testing.T) {
	for _, mode := range []string{"bwrap", "privileged", "firecracker"} {
		objs := render(t, "evalsi-sandboxd", "mode="+mode)
		cfg := load(t, find(t, objs, "ConfigMap", "evalsi-sandboxd"))
		if mode == "firecracker" && (cfg.Sandbox.Firecracker == nil || cfg.Sandbox.Ladder[0] != "firecracker") {
			t.Errorf("%s: %+v", mode, cfg.Sandbox)
		}
		ds := toYAML(t, find(t, objs, "DaemonSet", "evalsi-sandboxd").Spec)
		if (mode == "bwrap") != strings.Contains(ds, "hostUsers: false") {
			t.Errorf("%s: %s", mode, ds)
		}
	}
}

// The chart ships the CRDs controller-gen writes (make operator-gen).
func TestCRDsAreCurrent(t *testing.T) {
	generated, _ := filepath.Glob("../../operator/config/crd/*.yaml")
	if len(generated) != 4 {
		t.Fatalf("%d CRDs", len(generated))
	}
	for _, g := range generated {
		want, _ := os.ReadFile(g)
		got, err := os.ReadFile(filepath.Join(charts, "evalsi-crds", "crds", filepath.Base(g)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s differs from the chart's copy (make operator-gen)", filepath.Base(g))
		}
	}
	objs := render(t, "evalsi-crds")
	find(t, objs, "MutatingWebhookConfiguration", "evalsi-mutating")
	if s := find(t, objs, "Secret", "evalsi-operator-webhook-tls"); s.Metadata.Namespace != "evalsi" {
		t.Errorf("webhook secret in %q", s.Metadata.Namespace)
	}
}

func toYAML(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
