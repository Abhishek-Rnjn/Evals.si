package helm

import (
	"fmt"
	"strings"
	"testing"
)

// everything switches on every optional part of the evalsi chart.
var everything = []string{
	"devPostgres.enabled=true", "sandbox.pool.enabled=true", "keda.enabled=true",
	"grafana.dashboard.enabled=true", "operator.istio.peerAuthentication=true",
	"workers.pools.gpu.replicas=1", "server.persistence.enabled=true",
	"devMinio.enabled=true", "devClickhouse.enabled=true",
}

func nested(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// podSpecs returns the pod template spec and labels of each workload.
func podSpecs(objs []object) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, o := range objs {
		switch o.Kind {
		case "Deployment", "StatefulSet", "DaemonSet":
			if spec, ok := nested(o.Spec, "template", "spec").(map[string]any); ok {
				out[o.Kind+"/"+o.Metadata.Name] = spec
			}
		}
	}
	return out
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// Every name a release creates starts with the release's full name, and the
// parts refer to each other by those names: releases can share a namespace.
func TestReleaseNamesAreConsistent(t *testing.T) {
	for _, c := range []struct{ release, prefix string }{
		{"evalsi", "evalsi"},        // the usual release keeps the short names
		{"team-a", "team-a-evalsi"}, // any other gets its name in front
		{"prod-evalsi", "prod-evalsi"},
	} {
		t.Run(c.release, func(t *testing.T) {
			objs := renderAs(t, c.release, "evalsi", everything...)
			have := map[string]bool{}
			for _, o := range objs {
				have[o.Kind+"/"+o.Metadata.Name] = true
				if !strings.HasPrefix(o.Metadata.Name, c.prefix) {
					t.Errorf("%s %s does not start with %s", o.Kind, o.Metadata.Name, c.prefix)
				}
			}
			// The operator's webhook certificate comes from the evalsi-crds chart.
			have["Secret/"+c.prefix+"-operator-webhook-tls"] = true
			exists := func(kind, name string) bool { return have[kind+"/"+name] }
			// Pods refer to existing service accounts, Secrets, ConfigMaps and claims.
			for who, spec := range podSpecs(objs) {
				if sa, _ := spec["serviceAccountName"].(string); sa != "" && !exists("ServiceAccount", sa) {
					t.Errorf("%s: service account %s is not created", who, sa)
				}
				for _, v := range list(spec["volumes"]) {
					vm := v.(map[string]any)
					if n, _ := nested(vm, "secret", "secretName").(string); n != "" && !exists("Secret", n) {
						t.Errorf("%s: Secret %s is not created", who, n)
					}
					if n, _ := nested(vm, "configMap", "name").(string); n != "" && !exists("ConfigMap", n) {
						t.Errorf("%s: ConfigMap %s is not created", who, n)
					}
					if n, _ := nested(vm, "persistentVolumeClaim", "claimName").(string); n != "" && !exists("PersistentVolumeClaim", n) {
						t.Errorf("%s: claim %s is not created", who, n)
					}
				}
			}
			// Services select the pods of a workload of this release, and no other.
			labelsOf := map[string]map[string]any{}
			for _, o := range objs {
				if o.Kind == "Deployment" || o.Kind == "StatefulSet" || o.Kind == "DaemonSet" {
					if l, ok := nested(o.Spec, "template", "metadata", "labels").(map[string]any); ok {
						labelsOf[o.Kind+"/"+o.Metadata.Name] = l
					}
				}
			}
			for _, o := range objs {
				if o.Kind != "Service" {
					continue
				}
				sel, _ := o.Spec["selector"].(map[string]any)
				matched := false
				for _, l := range labelsOf {
					all := len(sel) > 0
					for k, v := range sel {
						if l[k] != v {
							all = false
						}
					}
					matched = matched || all
				}
				if !matched {
					t.Errorf("Service %s selects %v, which no workload carries", o.Metadata.Name, sel)
				}
			}
			// The operator is told the names of what it must find.
			op := podSpecs(objs)["Deployment/"+c.prefix+"-operator"]
			if op == nil {
				t.Fatalf("no operator; have %v", have)
			}
			args := fmt.Sprint(nested(list(op["containers"])[0].(map[string]any), "args"))
			for flag, kind := range map[string]string{
				"--sandbox-tls-secret=": "Secret", "--worker-config-map=": "ConfigMap",
				"--sandbox-service-account=": "ServiceAccount", "--sandbox-allow-client=": "",
			} {
				name := argValue(args, flag)
				if name == "" {
					t.Errorf("operator has no %s", flag)
				} else if kind != "" && !exists(kind, name) {
					t.Errorf("operator is told %s%s, which is not created", flag, name)
				}
			}
			if got := argValue(args, "--name-prefix="); got != c.prefix {
				t.Errorf("--name-prefix=%q, want %q", got, c.prefix)
			}
			// Configuration points at this release's NATS and server.
			cfg := find(t, objs, "ConfigMap", c.prefix).Data["evalsi.yaml"]
			if want := "nats://" + c.prefix + "-nats.evalsi.svc:4222"; !strings.Contains(cfg, want) {
				t.Errorf("server config does not point at %s:\n%s", want, cfg)
			}
		})
	}
}

func argValue(args, flag string) string {
	_, rest, ok := strings.Cut(args, flag)
	if !ok {
		return ""
	}
	end := strings.IndexAny(rest, " ]")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// Two releases in one namespace share no object name.
func TestTwoReleasesDoNotCollide(t *testing.T) {
	names := func(release string) map[string]bool {
		out := map[string]bool{}
		for _, o := range renderAs(t, release, "evalsi", everything...) {
			out[o.Kind+"/"+o.Metadata.Name] = true
		}
		return out
	}
	a, b := names("team-a"), names("team-b")
	for n := range a {
		if b[n] {
			t.Errorf("%s is created by both releases", n)
		}
	}
}

// The cluster-scoped chart names its objects by its own release, and refers to
// the evalsi release's operator by operator.fullname.
func TestCRDsChartNames(t *testing.T) {
	objs := renderAs(t, "team-a-crds", "evalsi-crds", "operator.fullname=team-a-evalsi", "operator.sandboxClasses=true")
	find(t, objs, "ClusterRole", "team-a-evalsi-operator")
	find(t, objs, "ClusterRoleBinding", "team-a-evalsi-operator")
	find(t, objs, "ClusterRole", "team-a-evalsi-edit")
	find(t, objs, "ValidatingWebhookConfiguration", "team-a-evalsi-validating")
	find(t, objs, "MutatingWebhookConfiguration", "team-a-evalsi-mutating")
	found := false
	for _, o := range objs {
		if o.Kind == "Secret" && o.Metadata.Name == "team-a-evalsi-operator-webhook-tls" {
			found = true
		}
	}
	if !found {
		t.Error("no webhook serving Secret named for the operator's release")
	}
}

func TestSandboxdChartNames(t *testing.T) {
	objs := renderAs(t, "pool-a", "evalsi-sandboxd", "tlsSecret=team-a-evalsi-sandbox-tls", "allowClients={team-a-evalsi-worker}")
	for _, kind := range []string{"ConfigMap", "DaemonSet", "Service"} {
		find(t, objs, kind, "pool-a-sandboxd")
	}
}

// Postgres, NATS, S3 and ClickHouse can each be bundled (for trials) or point
// at an existing one; a bundled one fills in the storage settings.
func TestStorageDependenciesBundledOrExternal(t *testing.T) {
	bundled := render(t, "evalsi", "devPostgres.enabled=true", "devMinio.enabled=true", "devClickhouse.enabled=true", "storage.s3.bucket=data")
	cfg := load(t, find(t, bundled, "ConfigMap", "evalsi"))
	if cfg.Storage.S3 == nil || cfg.Storage.S3.Endpoint != "evalsi-dev-minio.evalsi.svc:9000" || !cfg.Storage.S3.Insecure || cfg.DatasetsDir != "s3://data/datasets" {
		t.Errorf("bundled S3: %+v datasets %s", cfg.Storage.S3, cfg.DatasetsDir)
	}
	if cfg.Storage.ClickHouse == nil || cfg.Storage.ClickHouse.URL != "http://evalsi-dev-clickhouse.evalsi.svc:8123" || cfg.Storage.ClickHouse.User != "evalsi" {
		t.Errorf("bundled ClickHouse: %+v", cfg.Storage.ClickHouse)
	}
	for _, kind := range []struct{ kind, name string }{
		{"StatefulSet", "evalsi-dev-postgres"}, {"StatefulSet", "evalsi-dev-minio"}, {"StatefulSet", "evalsi-dev-clickhouse"},
		{"Service", "evalsi-dev-minio"}, {"Service", "evalsi-dev-clickhouse"}, {"Secret", "evalsi-dev-minio"}, {"Secret", "evalsi-dev-clickhouse"},
	} {
		find(t, bundled, kind.kind, kind.name)
	}
	// Both the server and the workers get the generated S3 credentials.
	for _, d := range []string{"evalsi", "evalsi-worker-cpu"} {
		if y := toYAML(t, find(t, bundled, "Deployment", d).Spec); !strings.Contains(y, "name: evalsi-dev-minio") {
			t.Errorf("%s does not read the bundled S3 credentials:\n%s", d, y)
		}
	}

	// Pointing at existing ones renders none of the bundles.
	external := render(t, "evalsi",
		"storage.postgres.dsnSecret.name=pg", "nats.url=nats://nats.infra.svc:4222",
		"storage.s3.bucket=data", "storage.s3.endpoint=s3.example.com:9000", "storage.s3.credentialsSecret=s3",
		"storage.clickhouse.url=http://ch.infra.svc:8123", "storage.clickhouse.user=u", "storage.clickhouse.passwordSecret.name=ch")
	for _, o := range external {
		if strings.Contains(o.Metadata.Name, "dev-") || strings.HasSuffix(o.Metadata.Name, "-nats") {
			t.Errorf("%s %s rendered with every dependency external", o.Kind, o.Metadata.Name)
		}
	}
	ext := load(t, find(t, external, "ConfigMap", "evalsi"))
	if ext.Storage.S3.Endpoint != "s3.example.com:9000" || ext.Storage.ClickHouse.URL != "http://ch.infra.svc:8123" || ext.Cluster.URL != "nats://nats.infra.svc:4222" {
		t.Errorf("external: %+v / %+v / %s", ext.Storage.S3, ext.Storage.ClickHouse, ext.Cluster.URL)
	}
}
