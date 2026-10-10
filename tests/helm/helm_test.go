// Package helm checks the Helm charts: they render, every evalsi.yaml they
// render is a valid config, and the CRDs match the generated ones.
package helm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	Rules    []rule `json:"rules"`
	Metadata struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Data     map[string]string `json:"data"`
	Webhooks []map[string]any  `json:"webhooks"`
	Spec     map[string]any    `json:"spec"`
}

type rule struct {
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	ResourceNames []string `json:"resourceNames"`
	Verbs         []string `json:"verbs"`
}

// render runs helm template with --set values, and -f for those that end
// in .yaml (relative to the chart).
func render(t *testing.T, chart string, set ...string) []object {
	t.Helper()
	// Releases are named as the docs name them, so names stay evalsi-*.
	return renderAs(t, chart, chart, set...)
}

// renderAs is render for a release of another name.
func renderAs(t *testing.T, release, chart string, set ...string) []object {
	t.Helper()
	return renderDir(t, release, filepath.Join(charts, chart), set...)
}

// renderDir renders the chart in dir. Values ending in .yaml are files, relative to dir.
func renderDir(t *testing.T, release, dir string, set ...string) []object {
	t.Helper()
	args := []string{"template", release, dir, "-n", "evalsi", "--kube-version", "1.34.0"}
	for _, s := range set {
		if strings.HasSuffix(s, ".yaml") {
			if !filepath.IsAbs(s) {
				s = filepath.Join(dir, s)
			}
			args = append(args, "-f", s)
		} else {
			args = append(args, "--set", s)
		}
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
				if op := toYAML(t, find(t, objs, "Deployment", "evalsi-operator").Spec); strings.Contains(op, "--worker-tls-secret") || strings.Contains(op, "--worker-s3-secret") {
					t.Errorf("operator names Secrets the release does not have: %s", op)
				}
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
				// Evaluators on the release's worker config get its Secrets.
				for _, arg := range []string{"--worker-tls-secret=evalsi-worker-tls", "--worker-s3-secret=s3"} {
					if !strings.Contains(toYAML(t, out.Spec), arg) {
						t.Errorf("operator without %s", arg)
					}
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

// Clusters whose nodes are all tainted, behind a service mesh, with images
// mirrored under one registry path.
func TestEvalsiChartSchedulingAndLabels(t *testing.T) {
	objs := render(t, "evalsi",
		"global.imageRegistry=mirror.example.com/team", "devPostgres.enabled=true",
		"workers.pools.cpu.image=mirror.example.com/team/evalsi-custom:1",
		"sandbox.pool.enabled=true", "sandbox.podRung.enabled=true",
		`sandbox.pool.pod.labels.istio\.io/use-waypoint=none`,
		`nats.serviceLabels.istio\.io/use-waypoint=none`, `devPostgres.serviceLabels.istio\.io/use-waypoint=none`,
		"operator.tolerations[0].key=dedicated", "operator.tolerations[0].operator=Exists",
		"nats.tolerations[0].key=dedicated", "nats.tolerations[0].operator=Exists",
		"devPostgres.tolerations[0].key=dedicated", "devPostgres.tolerations[0].operator=Exists")
	for _, o := range []struct{ kind, name string }{{"Deployment", "evalsi-operator"}, {"StatefulSet", "evalsi-nats"}, {"StatefulSet", "evalsi-dev-postgres"}} {
		if spec := toYAML(t, find(t, objs, o.kind, o.name).Spec); !strings.Contains(spec, "key: dedicated") {
			t.Errorf("%s has no toleration: %s", o.name, spec)
		}
	}
	for _, name := range []string{"evalsi-nats", "evalsi-dev-postgres"} {
		if l := find(t, objs, "Service", name).Metadata.Labels; l["istio.io/use-waypoint"] != "none" {
			t.Errorf("Service %s labels %v", name, l)
		}
	}
	if cpu := toYAML(t, find(t, objs, "Deployment", "evalsi-worker-cpu").Spec); !strings.Contains(cpu, "image: mirror.example.com/team/evalsi-custom:1") {
		t.Errorf("an image already under the registry was moved again: %s", cpu)
	}
	pool := find(t, objs, "ConfigMap", "evalsi-sandbox-pool").Data["evalsi.yaml"]
	if !strings.Contains(pool, "istio.io/use-waypoint: none") || !strings.Contains(pool, "evals.si/sandbox-pool: evalsi-sandbox-pool") {
		t.Errorf("sandbox pod labels: %s", pool)
	}
}

func TestOperatorPeerAuthentication(t *testing.T) {
	for _, o := range find2(render(t, "evalsi"), "PeerAuthentication") {
		t.Errorf("PeerAuthentication rendered by default: %s", o.Metadata.Name)
	}
	pa := find(t, render(t, "evalsi", "operator.istio.peerAuthentication=true"), "PeerAuthentication", "evalsi-operator-webhook")
	if y := toYAML(t, pa.Spec); !strings.Contains(y, `"9443":`) || !strings.Contains(y, "mode: PERMISSIVE") {
		t.Errorf("port 9443 is not PERMISSIVE: %s", y)
	}
	for _, o := range find2(render(t, "evalsi", "operator.istio.peerAuthentication=true", "operator.webhooks=false"), "PeerAuthentication") {
		t.Errorf("PeerAuthentication rendered without webhooks: %s", o.Metadata.Name)
	}
}

// find2 returns every object of a kind.
func find2(objs []object, kind string) []object {
	var out []object
	for _, o := range objs {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	return out
}

func TestSandboxdChart(t *testing.T) {
	for _, mode := range []string{"bwrap", "privileged", "firecracker"} {
		set := []string{"mode=" + mode, "global.imageRegistry=registry.internal:5000/mirror"}
		if mode == "firecracker" {
			set = append(set, "firecracker.defaultImage=python:3.13-slim")
		}
		objs := render(t, "evalsi-sandboxd", set...)
		cfg := load(t, find(t, objs, "ConfigMap", "evalsi-sandboxd"))
		if mode == "firecracker" && (cfg.Sandbox.Firecracker == nil || cfg.Sandbox.Ladder[0] != "firecracker" ||
			cfg.Sandbox.Firecracker.DefaultImage != "registry.internal:5000/mirror/python:3.13-slim") {
			t.Errorf("%s: %+v", mode, cfg.Sandbox)
		}
		if ds := toYAML(t, find(t, objs, "DaemonSet", "evalsi-sandboxd").Spec); !strings.Contains(ds, "image: registry.internal:5000/mirror/abhishek-rnjn/evalsi:") {
			t.Errorf("%s: image not under the registry: %s", mode, ds)
		}
		// These pools need privileges: the restricted check must say so.
		if v := restrictedViolations(t, find(t, objs, "DaemonSet", "evalsi-sandboxd")); len(v) == 0 {
			t.Errorf("%s: the sandboxd DaemonSet passes as restricted", mode)
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
	if len(generated) != 5 {
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

// The namespace-only install: nothing cluster-scoped, no CRDs needed, only
// what the built-in admin role may grant, and every pod admissible under
// Pod Security "restricted".
func TestNamespacedInstall(t *testing.T) {
	for name, set := range map[string][]string{
		"profile":          {"values-namespaced.yaml"},
		"with dev storage": {"values-namespaced.yaml", "devPostgres.enabled=true", "server.replicas=2", "workers.pools.graders.replicas=1", "workers.pools.graders.concurrency=4", "workers.pools.graders.image=registry.internal/graders:1.4"},
		"landlock only":    {"values-namespaced.yaml", "sandbox.pool.ladder={landlock}"},
		"static keys":      {"values-namespaced.yaml", "auth.kubernetes.issuer=https://oidc.example.com/id/1", "auth.kubernetes.jwksConfigMap=cluster-keys"},
	} {
		t.Run(name, func(t *testing.T) {
			objs := render(t, "evalsi", set...)
			for _, o := range objs {
				switch o.Kind {
				case "ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition", "ValidatingWebhookConfiguration",
					"MutatingWebhookConfiguration", "Namespace", "PriorityClass", "StorageClass", "ScaledObject":
					t.Errorf("%s %s in a namespace-only install", o.Kind, o.Metadata.Name)
				case "Role":
					for _, r := range o.Rules {
						for _, g := range r.APIGroups {
							// The built-in admin role grants these; a namespace
							// admin can grant no more (RBAC escalation checks).
							if g != "" && g != "apps" {
								t.Errorf("Role %s needs %s", o.Metadata.Name, g)
							}
						}
					}
				case "Deployment", "StatefulSet", "DaemonSet", "Job":
					if v := restrictedViolations(t, o); len(v) > 0 {
						t.Errorf("%s %s is not restricted: %v", o.Kind, o.Metadata.Name, v)
					}
					if o.Metadata.Name == "evalsi-operator" {
						t.Error("the operator runs without its CRDs")
					}
				}
			}
			worker := load(t, find(t, objs, "ConfigMap", "evalsi-worker"))
			if s := worker.Worker.SandboxService; s == nil || s.Address != "tls://evalsi-sandbox-pool.evalsi.svc:7443" {
				t.Errorf("workers' sandbox service %+v", s)
			}
			pool := load(t, find(t, objs, "ConfigMap", "evalsi-sandbox-pool")).Sandbox
			switch name {
			case "with dev storage":
				if dep := toYAML(t, find(t, objs, "Deployment", "evalsi-worker-graders").Spec); !strings.Contains(dep, "image: registry.internal/graders:1.4") {
					t.Errorf("graders pool image:\n%s", dep)
				}
			case "landlock only":
				if pool.Pod != nil || strings.Join(pool.Ladder, ",") != "landlock" {
					t.Errorf("pool %+v", pool)
				}
				if dep := toYAML(t, find(t, objs, "Deployment", "evalsi-sandbox-pool").Spec); !strings.Contains(dep, "automountServiceAccountToken: false") {
					t.Errorf("a Landlock pool with an API token:\n%s", dep)
				}
			case "static keys":
				p := load(t, find(t, objs, "ConfigMap", "evalsi")).Auth.JWT.Providers[0]
				if p.Issuer != "https://oidc.example.com/id/1" || p.JWKS.File != "/etc/evalsi/jwks/keys.json" || p.Kubernetes == nil {
					t.Errorf("provider %+v", p)
				}
				if dep := toYAML(t, find(t, objs, "Deployment", "evalsi").Spec); !strings.Contains(dep, "name: cluster-keys") {
					t.Errorf("keys not mounted:\n%s", dep)
				}
			default:
				if strings.Join(pool.Ladder, ",") != "pod,landlock" || pool.Pod == nil || pool.Pod.RunAsUser == nil || *pool.Pod.RunAsUser != 65532 ||
					pool.Pod.Capabilities == nil || len(*pool.Pod.Capabilities) != 0 || pool.Pod.GuestImage == "" {
					t.Errorf("pool %+v %+v", pool, pool.Pod)
				}
			}
		})
	}
	// Rungs that need privileges are refused in the chart's pool.
	cmd := exec.Command(helm(t), "template", "t", filepath.Join(charts, "evalsi"), "--kube-version", "1.34.0", "--set", "sandbox.pool.enabled=true", "--set", "sandbox.pool.ladder={bwrap}")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "needs privileges") {
		t.Errorf("a bwrap pool: %v %s", err, out)
	}
	// A key set without its issuer is refused.
	cmd = exec.Command(helm(t), "template", "t", filepath.Join(charts, "evalsi"), "--kube-version", "1.34.0", "--set", "auth.kubernetes.jwksConfigMap=keys")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "needs auth.kubernetes.issuer") {
		t.Errorf("jwks without an issuer: %v %s", err, out)
	}
}

// The cluster-scoped chart grants the operator nothing cluster-wide unless
// asked.
func TestCRDsChartRBAC(t *testing.T) {
	for _, o := range render(t, "evalsi-crds") {
		if o.Kind == "ClusterRoleBinding" {
			t.Errorf("ClusterRoleBinding %s by default", o.Metadata.Name)
		}
	}
	objs := render(t, "evalsi-crds", "operator.sandboxClasses=true")
	role := find(t, objs, "ClusterRole", "evalsi-operator")
	for _, r := range role.Rules {
		if strings.Join(r.APIGroups, ",") != "evals.si" || !strings.HasPrefix(strings.Join(r.Resources, ","), "sandboxclasses") {
			t.Errorf("sandboxClasses grants %+v", r)
		}
	}
	find(t, objs, "ClusterRoleBinding", "evalsi-operator")
}

// restrictedViolations lists what keeps a workload's pods from Pod Security
// "restricted" (v1.34).
func restrictedViolations(t *testing.T, o object) []string {
	t.Helper()
	var w struct {
		Template struct {
			Spec struct {
				HostNetwork     bool `json:"hostNetwork"`
				HostPID         bool `json:"hostPID"`
				HostIPC         bool `json:"hostIPC"`
				SecurityContext struct {
					RunAsNonRoot   *bool  `json:"runAsNonRoot"`
					RunAsUser      *int64 `json:"runAsUser"`
					SeccompProfile *struct {
						Type string `json:"type"`
					} `json:"seccompProfile"`
				} `json:"securityContext"`
				Containers     []container      `json:"containers"`
				InitContainers []container      `json:"initContainers"`
				Volumes        []map[string]any `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	}
	if err := yaml.Unmarshal([]byte(toYAML(t, o.Spec)), &w); err != nil {
		t.Fatal(err)
	}
	pod := w.Template.Spec
	var out []string
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		out = append(out, "host namespaces")
	}
	for _, v := range pod.Volumes {
		ok := false
		for _, kind := range []string{"configMap", "secret", "emptyDir", "projected", "persistentVolumeClaim", "downwardAPI", "ephemeral", "csi"} {
			if _, has := v[kind]; has {
				ok = true
			}
		}
		if !ok {
			out = append(out, fmt.Sprintf("volume %v", v["name"]))
		}
	}
	podNonRoot := pod.SecurityContext.RunAsNonRoot != nil && *pod.SecurityContext.RunAsNonRoot
	if pod.SecurityContext.RunAsUser != nil && *pod.SecurityContext.RunAsUser == 0 {
		out = append(out, "runAsUser 0")
	}
	podSeccomp := pod.SecurityContext.SeccompProfile != nil
	for _, c := range append(pod.Containers, pod.InitContainers...) {
		s := c.SecurityContext
		if s.Privileged != nil && *s.Privileged {
			out = append(out, c.Name+": privileged")
		}
		if s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation {
			out = append(out, c.Name+": allowPrivilegeEscalation")
		}
		if !podNonRoot && (s.RunAsNonRoot == nil || !*s.RunAsNonRoot) {
			out = append(out, c.Name+": runAsNonRoot")
		}
		if s.RunAsUser != nil && *s.RunAsUser == 0 {
			out = append(out, c.Name+": runAsUser 0")
		}
		if !podSeccomp && s.SeccompProfile == nil {
			out = append(out, c.Name+": seccompProfile")
		}
		if s.SeccompProfile != nil && s.SeccompProfile.Type == "Unconfined" {
			out = append(out, c.Name+": seccomp Unconfined")
		}
		if s.ProcMount != "" && s.ProcMount != "Default" {
			out = append(out, c.Name+": procMount")
		}
		if !slices.Contains(s.Capabilities.Drop, "ALL") {
			out = append(out, c.Name+": drop ALL")
		}
		for _, add := range s.Capabilities.Add {
			if add != "NET_BIND_SERVICE" {
				out = append(out, c.Name+": adds "+add)
			}
		}
		for _, p := range c.Ports {
			if p.HostPort != 0 {
				out = append(out, c.Name+": hostPort")
			}
		}
	}
	return out
}

type container struct {
	Name            string `json:"name"`
	SecurityContext struct {
		Privileged               *bool  `json:"privileged"`
		AllowPrivilegeEscalation *bool  `json:"allowPrivilegeEscalation"`
		RunAsNonRoot             *bool  `json:"runAsNonRoot"`
		RunAsUser                *int64 `json:"runAsUser"`
		ProcMount                string `json:"procMount"`
		SeccompProfile           *struct {
			Type string `json:"type"`
		} `json:"seccompProfile"`
		Capabilities struct {
			Add  []string `json:"add"`
			Drop []string `json:"drop"`
		} `json:"capabilities"`
	} `json:"securityContext"`
	Ports []struct {
		HostPort int `json:"hostPort"`
	} `json:"ports"`
}

// TestDashboardMetricsExist: every metric the Grafana dashboard queries is
// one evalsid exports, so renames cannot silently break the dashboard.
func TestDashboardMetricsExist(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/helm/evalsi/dashboards/evalsi.json")
	if err != nil {
		t.Fatal(err)
	}
	var dash map[string]any
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("dashboard is not JSON: %v", err)
	}
	exported := map[string]bool{}
	name := regexp.MustCompile(`evalsi_[a-z_]+`)
	err = filepath.WalkDir("../../internal", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		for _, m := range name.FindAllString(string(src), -1) {
			exported[m] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	used := name.FindAllString(string(raw), -1)
	if len(used) == 0 {
		t.Fatal("the dashboard queries no metrics")
	}
	for _, m := range used {
		if !exported[m] {
			t.Errorf("the dashboard queries %s, which evalsid does not export", m)
		}
	}
	cm := find(t, render(t, "evalsi", "grafana.dashboard.enabled=true"), "ConfigMap", "evalsi-dashboard")
	if cm.Data["evalsi.json"] != strings.TrimSuffix(string(raw), "\n") {
		t.Error("the chart's dashboard ConfigMap does not carry the dashboard")
	}
}
