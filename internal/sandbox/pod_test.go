package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeKube stands in for the Kubernetes API on hosts without a cluster:
// creating a pod runs the real evalsi-guest agent on the host (no isolation
// at all), listening on its own loopback address as the pod IP.
type fakeKube struct {
	t     *testing.T
	guest string
	mu    sync.Mutex
	pods  map[string]*fakePod
	next  int
}

type fakePod struct {
	manifest map[string]any
	ip       string
	cmd      *exec.Cmd
	deleted  bool
}

func (f *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer kube-token" {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	base := "/api/v1/namespaces/sandboxes/pods"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == base:
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		f.next++
		name := fmt.Sprintf("evalsi-sandbox-%d", f.next)
		ip := fmt.Sprintf("127.0.0.%d", 10+f.next)
		c := m["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		args := []string{}
		for _, a := range c["command"].([]any)[1:] {
			args = append(args, strings.Replace(a.(string), "0.0.0.0", ip, 1))
		}
		token := c["env"].([]any)[0].(map[string]any)["value"].(string)
		cmd := exec.Command(f.guest, args...)
		cmd.Env = append(os.Environ(), "EVALSI_GUEST_TOKEN="+token, "IMAGE_VAR=from-image")
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			f.t.Errorf("starting the fake pod: %v", err)
		}
		f.pods[name] = &fakePod{manifest: m, ip: ip, cmd: cmd}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": name}})
	case strings.HasPrefix(r.URL.Path, base+"/"):
		p := f.pods[strings.TrimPrefix(r.URL.Path, base+"/")]
		if p == nil || p.deleted {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			p.deleted = true
			_ = p.cmd.Process.Kill()
			_, _ = p.cmd.Process.Wait()
			_, _ = w.Write([]byte("{}"))
			return
		}
		ready := false
		if c, err := net.DialTimeout("tcp", net.JoinHostPort(p.ip, "7070"), 100*time.Millisecond); err == nil {
			c.Close()
			ready = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{
			"phase": "Running", "podIP": p.ip, "containerStatuses": []any{map[string]any{"ready": ready, "state": map[string]any{}}},
		}})
	default:
		http.NotFound(w, r)
	}
}

func podRung(t *testing.T) (*Manager, *fakeKube) {
	t.Helper()
	// The fake pods are host processes, so the paths a pod would mount
	// (the default /workspace, these tests' /work) must exist here.
	for _, dir := range []string{workspaceInside, "/work"} {
		if err := os.MkdirAll(dir, 0o1777); err != nil || unix.Access(dir, unix.W_OK) != nil {
			msg := fmt.Sprintf("%s is not writable here (CI creates it: sudo install -d -m 1777 /workspace /work)", dir)
			if os.Getenv("EVALSI_REQUIRE_SANDBOX") != "" {
				t.Fatal(msg)
			}
			t.Skip(msg)
		}
	}
	helpers := buildHelpers(t)
	kube := &fakeKube{t: t, guest: helpers["guest"], pods: map[string]*fakePod{}}
	api := httptest.NewServer(kube)
	t.Cleanup(api.Close)
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("kube-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sb, err := New(Config{
		Ladder: []string{"pod"}, MinIsolation: "namespaced", WorkDir: t.TempDir(), CacheDir: t.TempDir(),
		Pod: &PodConfig{
			APIServer: api.URL, TokenFile: tokenFile, Namespace: "sandboxes",
			GuestImage: "ghcr.io/abhishek-rnjn/evalsi:test", DefaultImage: "python:3.13-slim",
			AdvertiseIP: "127.0.0.1", RuntimeClassName: "gvisor", Level: "kernel",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(sb)
	t.Cleanup(func() { m.Close(); sb.Close() })
	return m, kube
}

func TestPodRung(t *testing.T) {
	m, kube := podRung(t)
	ctx := context.Background()
	sess, err := m.Create(ctx, &Spec{Files: map[string][]byte{"in.txt": []byte("hello")}, Workdir: "/work/" + t.Name(), Resources: Resources{MemoryMB: 512}})
	if err != nil {
		t.Fatal(err)
	}
	iso := sess.Isolation()
	if iso.Driver != "pod" || iso.Level != "kernel" || iso.Enforcement != "partial" {
		t.Errorf("isolation %+v", iso)
	}
	// The manifest is hardened.
	kube.mu.Lock()
	var pod *fakePod // the session's: the probe's pod is gone
	for _, p := range kube.pods {
		if !p.deleted {
			pod = p
		}
	}
	kube.mu.Unlock()
	spec := pod.manifest["spec"].(map[string]any)
	if spec["automountServiceAccountToken"] != false || spec["enableServiceLinks"] != false || spec["runtimeClassName"] != "gvisor" {
		t.Errorf("pod spec %v", spec)
	}
	c := spec["containers"].([]any)[0].(map[string]any)
	sec := c["securityContext"].(map[string]any)
	if sec["allowPrivilegeEscalation"] != false || fmt.Sprint(sec["capabilities"].(map[string]any)["drop"]) != "[ALL]" {
		t.Errorf("security context %v", sec)
	}
	if c["image"] != "python:3.13-slim" || fmt.Sprint(c["resources"].(map[string]any)["limits"]) != "map[memory:512Mi]" {
		t.Errorf("container %v", c)
	}
	labels := pod.manifest["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels["evals.si/sandbox"] != "true" {
		t.Errorf("labels %v", labels)
	}

	// Commands run in the workdir with the container's environment, files
	// go both ways.
	var out bytes.Buffer
	res := sess.Exec(ctx, &Exec{Command: []string{"sh", "-c", "cat in.txt; echo \" $IMAGE_VAR\"; echo made > out.txt"}, Timeout: 10 * time.Second}, &out, io.Discard)
	if res.Outcome != OutcomeExit || res.ExitCode != 0 || out.String() != "hello from-image\n" {
		t.Fatalf("exec %+v %q", res, out.String())
	}
	files, _, _, err := sess.ReadFiles([]string{"out.txt"}, 0)
	if err != nil || len(files) != 1 || string(files[0].Content) != "made\n" {
		t.Fatalf("read %v %v", files, err)
	}
	// No snapshots on this rung.
	if _, err := m.Snapshot(ctx, sess.ID); !errors.Is(err, ErrNoSnapshots) {
		t.Errorf("snapshot: %v", err)
	}
	if err := m.Destroy(sess.ID); err != nil {
		t.Fatal(err)
	}
	kube.mu.Lock()
	deleted := pod.deleted
	kube.mu.Unlock()
	if !deleted {
		t.Error("the pod was not deleted")
	}
}

// With network, the pod's commands reach allowed hosts only through this
// process's logging egress proxy, over the guest's relay.
func TestPodRungEgress(t *testing.T) {
	m, _ := podRung(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("allowed")) }))
	defer upstream.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	sess, err := m.Create(ctx, &Spec{Network: Network{Mode: NetworkAllowlist, Allow: []string{host}}, Workdir: "/work/" + t.Name()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Destroy(sess.ID) }()
	fetch := `python3 - <<'PY'
import os, urllib.request
p = urllib.request.ProxyHandler({"http": os.environ["HTTP_PROXY"]})
for url in ["` + upstream.URL + `", "http://example.invalid/"]:
    try:
        print(urllib.request.build_opener(p).open(url, timeout=5).read().decode())
    except Exception as e:
        print("refused", getattr(e, "code", e))
PY`
	var out bytes.Buffer
	// The upstream is on loopback, which NO_PROXY exempts; this fake pod is
	// not confined, so clear it to make the request take the proxy.
	res := sess.Exec(ctx, &Exec{Command: []string{"sh", "-c", fetch}, Timeout: 30 * time.Second, Env: map[string]string{"NO_PROXY": "", "no_proxy": ""}}, &out, os.Stderr)
	if res.ExitCode != 0 || !strings.HasPrefix(out.String(), "allowed\nrefused 403") {
		t.Fatalf("egress %+v %q", res, out.String())
	}
	events := sess.Egress()
	if len(events) != 2 || !events[0].Allowed || events[1].Allowed {
		t.Errorf("egress log %+v", events)
	}
}

// restrictedViolations lists what keeps a pod from Pod Security
// "restricted" (the checks that apply to what the pod rung sets).
func restrictedViolations(m map[string]any) []string {
	var out []string
	b, _ := json.Marshal(m)
	var pod struct {
		Spec struct {
			SecurityContext struct {
				RunAsNonRoot   *bool `json:"runAsNonRoot"`
				SeccompProfile *struct {
					Type string `json:"type"`
				} `json:"seccompProfile"`
			} `json:"securityContext"`
			Containers     []json.RawMessage `json:"containers"`
			InitContainers []json.RawMessage `json:"initContainers"`
			Volumes        []map[string]any  `json:"volumes"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(b, &pod)
	sc := pod.Spec.SecurityContext
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		out = append(out, "runAsNonRoot")
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != "RuntimeDefault" {
		out = append(out, "seccompProfile")
	}
	for _, v := range pod.Spec.Volumes {
		if _, ok := v["hostPath"]; ok {
			out = append(out, "hostPath")
		}
	}
	for _, raw := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
		var c struct {
			Name            string `json:"name"`
			SecurityContext struct {
				Privileged               *bool `json:"privileged"`
				AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation"`
				Capabilities             struct {
					Add  []string `json:"add"`
					Drop []string `json:"drop"`
				} `json:"capabilities"`
			} `json:"securityContext"`
		}
		_ = json.Unmarshal(raw, &c)
		s := c.SecurityContext
		if s.Privileged != nil && *s.Privileged {
			out = append(out, c.Name+": privileged")
		}
		if s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation {
			out = append(out, c.Name+": allowPrivilegeEscalation")
		}
		if !slices.Contains(s.Capabilities.Drop, "ALL") {
			out = append(out, c.Name+": drop ALL")
		}
		for _, add := range s.Capabilities.Add {
			if add != "NET_BIND_SERVICE" {
				out = append(out, c.Name+": adds "+add)
			}
		}
	}
	return out
}

// With a non-root user and no capabilities, sandbox pods meet Pod Security
// "restricted", and the workdir becomes a volume seeded from the image.
func TestPodRungRestricted(t *testing.T) {
	sp := &Spec{Workdir: "/testbed"}
	root := &podDriver{cfg: &PodConfig{GuestImage: "evalsi:test"}}
	if v := restrictedViolations(root.manifest(sp, "python:3.13-slim", "t", "")); !slices.Contains(v, "runAsNonRoot") || !slices.Contains(v, "sandbox: adds CHOWN") {
		t.Errorf("the default pod: violations %v", v)
	}
	uid, none := int64(1000), []string{}
	d := &podDriver{cfg: &PodConfig{GuestImage: "evalsi:test", RunAsUser: &uid, Capabilities: &none}}
	m := d.manifest(sp, "swe:latest", "t", "")
	if v := restrictedViolations(m); len(v) > 0 {
		t.Errorf("violations %v", v)
	}
	b, _ := json.Marshal(m)
	for _, want := range []string{
		`"command":["/evalsi/bin/evalsi-guest","seed","/testbed","/evalsi/workdir"]`,
		`"image":"swe:latest","name":"workdir"`,
		`{"mountPath":"/testbed","name":"workdir"}`,
		`{"mountPath":"/tmp","name":"tmp"}`,
	} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("manifest lacks %s:\n%s", want, b)
		}
	}
	if bytes.Contains(b, []byte("readOnlyRootFilesystem")) {
		t.Error("a non-root pod got a read-only root it did not ask for")
	}
}
