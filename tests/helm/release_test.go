package helm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// The bundled MinIO can write its runtime directories (PID file, mc
	// config) as the non-root group it runs as.
	minio := toYAML(t, find(t, bundled, "StatefulSet", "evalsi-dev-minio").Spec)
	for _, path := range []string{"/opt/bitnami/minio/tmp", "/.mc"} {
		if !strings.Contains(minio, "mountPath: "+path) {
			t.Errorf("MinIO cannot write %s:\n%s", path, minio)
		}
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

func TestKindLandlockDeferral(t *testing.T) {
	source, err := os.ReadFile("../../deploy/e2e/kind-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, block, ok := strings.Cut(string(source), "if [ \"$skip_landlock\" = 1 ]; then")
	if !ok {
		t.Fatal("missing explicit Landlock opt-out")
	}
	block, _, ok = strings.Cut(block, "step \"namespace-only: a static issuer")
	if !ok {
		t.Fatal("Landlock deferral must end before authentication checks")
	}
	for _, test := range []struct {
		name, skip, helmExit string
		wantError            bool
	}{
		{name: "default"},
		{name: "explicit default", skip: "0"},
		{name: "deferred", skip: "1"},
		{name: "default failure", helmExit: "1", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SKIP_LANDLOCK", test.skip)
			t.Setenv("HELM_EXIT", test.helmExit)
			probe := `set -e
skip_landlock="${SKIP_LANDLOCK:-0}"
work=/tmp/bundle
here=/tmp/specs
ns2=evalsi-restricted
step() { printf '%s\n' "$*"; }
helm() { printf 'helm\n'; return "${HELM_EXIT:-0}"; }
kubectl() { printf 'kubectl\n'; }
cli() { printf 'cli\n'; }
want() { printf 'want %s %s\n' "$2" "$3"; }
if [ "$skip_landlock" = 1 ]; then` + block
			out, err := exec.Command("bash", "-c", probe).CombinedOutput()
			if (err != nil) != test.wantError {
				t.Fatalf("error %v, want failure %v: %s", err, test.wantError, out)
			}
			if test.skip == "1" {
				if !strings.Contains(string(out), "DEFERRED: Landlock") || strings.Contains(string(out), "helm\n") || strings.Contains(string(out), "cli\n") {
					t.Fatalf("deferred Landlock ran or was not reported: %s", out)
				}
			} else if !test.wantError && !strings.Contains(string(out), "want unit-tests 0.5") {
				t.Fatalf("default omitted the Landlock scoring check: %s", out)
			}
		})
	}
}

func TestAirgapBundlePlatform(t *testing.T) {
	for _, test := range []struct {
		name, platform, failure string
		oldDocker               bool
	}{
		{name: "default", platform: "linux/amd64"},
		{name: "arm64", platform: "linux/arm64"},
		{name: "missing platform", platform: "linux/arm64", failure: "inspect"},
		{name: "export failure", platform: "linux/arm64", failure: "save"},
		{name: "docker without platform flags", platform: "linux/amd64", oldDocker: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CALLS", calls)
			t.Setenv("FAILURE", test.failure)
			t.Setenv("PULL", "0")
			t.Setenv("OLD_DOCKER", map[bool]string{false: "0", true: "1"}[test.oldDocker])
			mock := `#!/bin/sh
command="${0##*/}"
printf '%s %s\n' "$command" "$*" >> "$CALLS"
if [ "$command" = docker ]; then
  case " $* " in
    *" --help "*)
      # An older Docker's help does not list --platform for these commands.
      [ "$OLD_DOCKER" = 1 ] || echo "      --platform string   Set platform"
      exit 0 ;;
  esac
  if [ "$OLD_DOCKER" = 1 ] && [ "$1" != pull ]; then
    # Older Docker: inspect, save and load reject --platform (pull accepts it).
    for a in "$@"; do
      if [ "$a" = --platform ]; then echo "unknown flag: --platform" >&2; exit 125; fi
    done
  fi
  if [ "$FAILURE" = inspect ] && { [ "$1" = image ] || [ "$1" = pull ]; }; then exit 1; fi
  if [ "$1" = save ]; then
    [ "$FAILURE" != save ] || exit 1
    while [ "$#" -gt 0 ]; do
      if [ "$1" = -o ]; then : > "$2"; break; fi
      shift
    done
  fi
fi
`
			for _, name := range []string{"docker", "helm", "kind", "kubectl"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(mock), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			bundle := filepath.Join(dir, "bundle")
			args := []string{"../../deploy/airgap/bundle.sh", "--tag", "e2e", "--out", bundle}
			if test.name != "default" {
				args = append(args, "--platform", test.platform)
			}
			out, err := exec.Command("bash", args...).CombinedOutput()
			if test.failure != "" {
				if err == nil {
					t.Fatalf("bundle accepted %s: %s", test.failure, out)
				}
				if _, err := os.Stat(filepath.Join(bundle, "platform")); !os.IsNotExist(err) {
					t.Fatalf("failed export recorded a platform: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("bundle: %v\n%s", err, out)
			}
			platform, err := os.ReadFile(filepath.Join(bundle, "platform"))
			if err != nil || string(platform) != test.platform+"\n" {
				t.Fatalf("platform %q: %v", platform, err)
			}
			out, err = exec.Command("bash", filepath.Join(bundle, "install.sh"), "--kind", "test", "--registry", "airgap.invalid").CombinedOutput()
			if err != nil {
				t.Fatalf("install: %v\n%s", err, out)
			}
			log, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			wants := []string{
				"docker image inspect --platform " + test.platform,
				"docker save --platform " + test.platform + " -o " + bundle + "/images.tar",
				"docker load --platform " + test.platform + " -q -i " + bundle + "/images.tar",
				"docker save --platform " + test.platform + " airgap.invalid/",
				"kind load image-archive " + bundle + "/.kind-images.tar --name test",
			}
			if test.oldDocker {
				wants = []string{
					"docker save -o " + bundle + "/images.tar",
					"docker load -q -i " + bundle + "/images.tar",
					"kind load image-archive " + bundle + "/.kind-images.tar --name test",
				}
				if strings.Contains(string(log), "docker image inspect --platform") || strings.Contains(string(log), "docker load --platform") {
					t.Errorf("an older Docker was given --platform:\n%s", log)
				}
			}
			for _, want := range wants {
				if !strings.Contains(string(log), want) {
					t.Errorf("missing %q:\n%s", want, log)
				}
			}
		})
	}
}
