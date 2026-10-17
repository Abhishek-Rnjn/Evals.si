package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http/httpproxy"
)

func TestServiceProxyRouting(t *testing.T) {
	for _, mode := range []string{NetworkAllow, NetworkAllowlist} {
		for _, override := range []string{"default", "spec", "exec"} {
			t.Run(mode+"/"+override, func(t *testing.T) {
				sp := &Spec{Network: Network{Mode: mode}, Services: []ServiceSpec{testService("api", 8080), testService("db", 6379)}, ServiceLifetime: time.Hour}
				if mode == NetworkAllowlist {
					sp.Network.Allow = []string{"external.example:8080"}
				}
				if err := sp.Validate(); err != nil {
					t.Fatal(err)
				}
				e := &Exec{}
				if override == "spec" {
					sp.Env = map[string]string{"NO_PROXY": "custom.internal", "no_proxy": "lower.internal"}
				} else if override == "exec" {
					e.Env = map[string]string{"NO_PROXY": "custom.internal", "no_proxy": "lower.internal"}
				}
				vm := &guestConn{driver: "pod", sp: sp, proxy: &egressProxy{}, proxyAddr: "127.0.0.1:9090"}
				env := map[string]string{}
				for _, kv := range vm.env(e) {
					k, v, _ := strings.Cut(kv, "=")
					env[k] = v
				}
				for _, key := range []string{"NO_PROXY", "no_proxy"} {
					proxy := (&httpproxy.Config{HTTPProxy: env["HTTP_PROXY"], NoProxy: env[key]}).ProxyFunc()
					for _, host := range []string{"api:8080", "db:6379", "external.example:8080"} {
						u, _ := url.Parse("http://" + host)
						got, err := proxy(u)
						if err != nil || (got != nil) != strings.HasPrefix(host, "external.") {
							t.Fatalf("%s routing %s: proxy=%v error=%v; bypass=%q", key, host, got, err, env[key])
						}
					}
					want := "localhost,127.0.0.1,::1"
					if override != "default" {
						want = "custom.internal"
						if key == "no_proxy" {
							want = "lower.internal"
						}
					}
					if !strings.HasPrefix(env[key], want+",") {
						t.Fatalf("lost existing %s exclusions: %q", key, env[key])
					}
				}
			})
		}
	}
}

func testService(name string, port int) ServiceSpec {
	return ServiceSpec{Name: name, Image: "redis:7-alpine", Command: []string{"redis-server", "--port", "6379"}, Port: port, MemoryMB: 64, CPUMillis: 100}
}

func TestServiceValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Spec)
	}{
		{"lifetime", func(s *Spec) { s.ServiceLifetime = 0 }},
		{"duplicate", func(s *Spec) { s.Services = append(s.Services, s.Services[0]) }},
		{"reserved port", func(s *Spec) { s.Services[0].Port = 7070 }},
		{"bad name", func(s *Spec) { s.Services[0].Name = "../db" }},
		{"missing command", func(s *Spec) { s.Services[0].Command = nil }},
		{"missing memory", func(s *Spec) { s.Services[0].MemoryMB = 0 }},
		{"missing dependency", func(s *Spec) { s.Services[0].DependsOn = []string{"missing"} }},
		{"cycle", func(s *Spec) { s.Services[0].DependsOn = []string{"db"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Spec{Services: []ServiceSpec{testService("db", 6379)}, ServiceLifetime: time.Hour}
			tc.change(s)
			if s.Validate() == nil {
				t.Fatal("accepted invalid services")
			}
		})
	}
	s := &Spec{Services: []ServiceSpec{testService("db", 6379)}, ServiceLifetime: time.Hour}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	sb, err := New(Config{Ladder: []string{"landlock"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	if _, err = sb.Open(t.Context(), s); err == nil || !strings.Contains(err.Error(), "services require the pod backend") {
		t.Fatalf("unsupported backend: %v", err)
	}
}

func TestServicePodManifest(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "ns")
	t.Setenv("POD_NAME", "pool-0")
	t.Setenv("POD_UID", "pool-uid")
	d := &podDriver{cfg: &PodConfig{Namespace: "ns", GuestImage: "guest", NetworkPolicyEnforced: true}, serviceInstance: "instance"}
	api, db := testService("api", 8080), testService("db", 6379)
	api.DependsOn = []string{"db"}
	db.Env = map[string]string{"SERVICE_ONLY": "yes"}
	s := &Spec{Services: []ServiceSpec{api, db}, ServiceLifetime: time.Hour, Env: map[string]string{"API_KEY": "secret"}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	m := d.manifest(s, "agent", strings.Repeat("a", 48), "")
	p := m["spec"].(map[string]any)
	init := p["initContainers"].([]map[string]any)
	if init[1]["name"] != "svc-db" || init[2]["name"] != "svc-api" {
		t.Fatalf("dependency order: %v", init)
	}
	for _, c := range init[1:] {
		if c["restartPolicy"] != "Always" || c["startupProbe"] == nil || c["readinessProbe"] == nil {
			t.Fatal("missing lifecycle probes")
		}
		if c["volumeMounts"] != nil {
			t.Fatal("service can read agent files")
		}
		raw, _ := json.Marshal(c)
		if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), podTokenEnv) {
			t.Fatal("service inherited credentials")
		}
	}
	if p["activeDeadlineSeconds"] != int64(3600) {
		t.Fatal("missing hard deadline")
	}
	meta := m["metadata"].(map[string]any)
	if meta["ownerReferences"] == nil || meta["name"] == nil || meta["generateName"] != nil {
		t.Fatal("missing ownership or deterministic cleanup name")
	}
	if d.supports(s) != nil {
		t.Fatal("valid pod rejected")
	}
	d.netLeak.Store(true)
	if d.supports(s) == nil {
		t.Fatal("accepted unenforced network")
	}
}

func TestServiceReadinessAndFailure(t *testing.T) {
	ready := false
	restarts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"phase": "Running", "podIP": "127.0.0.1", "containerStatuses": []any{map[string]any{"ready": true}}, "initContainerStatuses": []any{map[string]any{"name": "svc-db", "ready": ready, "restartCount": restarts}}}})
	}))
	defer server.Close()
	d := &podDriver{cfg: &PodConfig{Namespace: "ns", StartTimeout: "10ms"}, kube: &kube{base: server.URL, hc: server.Client()}}
	services := []ServiceSpec{testService("db", 6379)}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := d.waitRunning(ctx, "p", services); err == nil {
		t.Fatal("agent started before service readiness")
	}
	ready = true
	if _, err := d.waitRunning(t.Context(), "p", services); err != nil {
		t.Fatal(err)
	}
	p := &podSandbox{d: d, pod: "p"}
	p.sp = &Spec{Services: services}
	restarts = 1
	// No guest connection exists: an unhealthy service must refuse execution first.
	res := p.exec(t.Context(), &Exec{Command: []string{"true"}}, io.Discard, io.Discard)
	if res.Outcome != OutcomeRunnerFailure || !strings.Contains(res.Error, "restarts=1") {
		t.Fatalf("service crash scored as agent result: %+v", res)
	}
}

func TestServiceStartupFailureDeletesPod(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "ns")
	t.Setenv("POD_NAME", "pool")
	t.Setenv("POD_UID", "uid")
	deleted := ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"name": "created"}})
		case "DELETE":
			deleted = r.URL.Path
			w.WriteHeader(200)
		default:
			if r.URL.Query().Get("labelSelector") != "" {
				_, _ = w.Write([]byte(`{"items":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":{"phase":"Pending","initContainerStatuses":[{"name":"svc-db","state":{"waiting":{"reason":"ErrImagePull"}}}]}}`))
		}
	}))
	defer api.Close()
	d := &podDriver{cfg: &PodConfig{Namespace: "ns", GuestImage: "guest", NetworkPolicyEnforced: true}, kube: &kube{base: api.URL, hc: api.Client()}, serviceInstance: "now"}
	d.netVerified.Store(true)
	sp := &Spec{Image: "agent", Services: []ServiceSpec{testService("db", 6379)}, ServiceLifetime: time.Hour}
	_ = sp.Validate()
	if _, err := d.open(t.Context(), sp, t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "ErrImagePull") {
		t.Fatalf("startup failure: %v", err)
	}
	if deleted != "/api/v1/namespaces/ns/pods/created" {
		t.Fatalf("orphaned pod: %s", deleted)
	}
}

func TestServicePoolRestartReconcilesOnlyOwnPods(t *testing.T) {
	t.Setenv("POD_UID", "mine")
	var deleted []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleted = append(deleted, r.URL.Path)
			return
		}
		items := []any{}
		for _, v := range []struct{ name, owner, instance string }{{"old", "mine", "old"}, {"current", "mine", "now"}, {"other", "other", "old"}} {
			items = append(items, map[string]any{"metadata": map[string]any{"name": v.name, "labels": map[string]string{serviceOwnerLabel: v.owner, serviceInstanceLabel: v.instance}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	defer api.Close()
	d := &podDriver{cfg: &PodConfig{Namespace: "ns"}, kube: &kube{base: api.URL, hc: api.Client()}, serviceInstance: "now"}
	for range 2 {
		if err := d.reapPreviousServices(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(deleted) != 1 || !strings.HasSuffix(deleted[0], "/old") {
		t.Fatalf("incorrect reconciliation: %v", deleted)
	}
}

func TestServiceLogsAreOptInAndBounded(t *testing.T) {
	logs := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			logs++
			if r.URL.Query().Get("container") != "svc-db" || r.URL.Query().Get("limitBytes") != "16384" {
				t.Error("unbounded or wrong log request")
			}
			_, _ = w.Write([]byte(strings.Repeat("x", 32768)))
			return
		}
		_, _ = w.Write([]byte(`{"status":{"phase":"Running","initContainerStatuses":[{"name":"svc-db","ready":true}]}}`))
	}))
	defer api.Close()
	p := &podSandbox{pod: "p", d: &podDriver{cfg: &PodConfig{Namespace: "ns"}, kube: &kube{base: api.URL, hc: api.Client()}}}
	p.sp = &Spec{Services: []ServiceSpec{testService("db", 6379)}}
	states, err := p.services(t.Context(), false)
	if err != nil || logs != 0 || len(states) != 1 {
		t.Fatalf("implicit logs: %v %v", states, err)
	}
	states, err = p.services(t.Context(), true)
	if err != nil || logs != 1 || len(states[0].Logs) != 16384 {
		t.Fatalf("log bounds: %v, requests %d", err, logs)
	}
}

func TestServiceCreationRequiresSuccessfulCanary(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "ns")
	t.Setenv("POD_NAME", "pool")
	t.Setenv("POD_UID", "uid")
	d := &podDriver{cfg: &PodConfig{Namespace: "ns", NetworkPolicyEnforced: true}}
	sp := &Spec{Image: "agent", Services: []ServiceSpec{testService("db", 6379)}, ServiceLifetime: time.Hour}
	if _, err := d.open(t.Context(), sp, t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "successful NetworkPolicy canary") {
		t.Fatalf("unverified policy accepted: %v", err)
	}
}

func TestServiceRejectsCrossNamespaceOwner(t *testing.T) {
	t.Setenv("POD_NAME", "pool")
	t.Setenv("POD_UID", "uid")
	t.Setenv("POD_NAMESPACE", "pool-ns")
	d := &podDriver{cfg: &PodConfig{Namespace: "other-ns", NetworkPolicyEnforced: true}}
	if err := d.supports(&Spec{Services: []ServiceSpec{testService("db", 6379)}}); err == nil || !strings.Contains(err.Error(), "owning pool's namespace") {
		t.Fatalf("invalid cross-namespace owner accepted: %v", err)
	}
}

func TestServiceReadinessToleratesOneSlowProbe(t *testing.T) {
	t.Setenv("POD_UID", "uid")
	d := &podDriver{cfg: &PodConfig{Namespace: "ns", GuestImage: "guest"}}
	s := &Spec{Services: []ServiceSpec{testService("db", 6379)}, ServiceLifetime: time.Hour}
	init := d.manifest(s, "agent", "token", "")["spec"].(map[string]any)["initContainers"].([]map[string]any)
	if got := init[1]["readinessProbe"].(map[string]any)["failureThreshold"]; got != 3 {
		t.Fatalf("readiness failureThreshold %v: one missed probe would fail a command", got)
	}
}

func TestServiceHealthRetriesAPIErrors(t *testing.T) {
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"status":{"phase":"Running","initContainerStatuses":[{"name":"svc-db","ready":true}]}}`))
	}))
	defer api.Close()
	p := &podSandbox{pod: "p", d: &podDriver{cfg: &PodConfig{Namespace: "ns"}, kube: &kube{base: api.URL, hc: api.Client()}}}
	p.sp = &Spec{Services: []ServiceSpec{testService("db", 6379)}}
	// A cancelled caller must not turn a finished command into a runner failure.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.checkServices(ctx); err != nil || calls != 2 {
		t.Fatalf("throttled status read failed the check: %v (%d calls)", err, calls)
	}
	if !p.recentlyHealthy() {
		t.Fatal("healthy check not remembered for the next pre-check")
	}
}

func TestServiceStartTimeoutCoversStartupProbes(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":{"phase":"Pending","initContainerStatuses":[{"name":"svc-db","ready":false}]}}`))
	}))
	defer api.Close()
	d := &podDriver{cfg: &PodConfig{Namespace: "ns", StartTimeout: "10ms"}, kube: &kube{base: api.URL, hc: api.Client()}}
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	_, err := d.waitRunning(ctx, "p", []ServiceSpec{testService("db", 6379)})
	if err == nil || strings.Contains(err.Error(), "did not start within") {
		t.Fatalf("start timeout ignores the services' startup probes: %v", err)
	}
}

func TestServiceLogErrorsKeepStatus(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"status":{"phase":"Running","initContainerStatuses":[{"name":"svc-db","ready":true}]}}`))
	}))
	defer api.Close()
	p := &podSandbox{pod: "p", d: &podDriver{cfg: &PodConfig{Namespace: "ns"}, kube: &kube{base: api.URL, hc: api.Client()}}}
	p.sp = &Spec{Services: []ServiceSpec{testService("db", 6379)}}
	states, err := p.services(t.Context(), true)
	if err != nil || len(states) != 1 || !states[0].Ready || !strings.HasPrefix(states[0].Logs, "logs unavailable") {
		t.Fatalf("log failure lost service status: %+v %v", states, err)
	}
}

func TestServiceReapingOnlyByPoolServer(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "ns")
	t.Setenv("POD_NAME", "pool")
	t.Setenv("POD_UID", "uid")
	for _, owner := range []bool{false, true} {
		listed := false
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Query().Get("labelSelector") != "":
				listed = true
				_, _ = w.Write([]byte(`{"items":[]}`))
			case r.Method == "POST":
				_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"name": "created"}})
			case r.Method == "GET":
				_, _ = w.Write([]byte(`{"status":{"phase":"Failed","reason":"test"}}`))
			}
		}))
		sb := &Sandbox{}
		if owner {
			sb.OwnServicePods()
		}
		d := &podDriver{sb: sb, cfg: &PodConfig{Namespace: "ns", GuestImage: "guest", NetworkPolicyEnforced: true}, kube: &kube{base: api.URL, hc: api.Client()}, serviceInstance: "now"}
		d.netVerified.Store(true)
		sp := &Spec{Image: "agent", Services: []ServiceSpec{testService("db", 6379)}, ServiceLifetime: time.Hour}
		_ = sp.Validate()
		_, _ = d.open(t.Context(), sp, t.TempDir(), "")
		api.Close()
		if listed != owner {
			t.Fatalf("owner=%t reaped=%t: only the pool server may reap service pods", owner, listed)
		}
	}
}
