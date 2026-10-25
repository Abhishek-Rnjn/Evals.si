//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// serving is a service that serves a directory over HTTP with the host's Python.
func serving(name string, port int, dir string) ServiceSpec {
	return ServiceSpec{
		Name: name, Port: port, MemoryMB: 512, CPUMillis: 100,
		Command: []string{"python3", "-u", "-m", "http.server", strconv.Itoa(port), "--bind", "0.0.0.0", "--directory", dir},
	}
}

func servicesSpec(env map[string]string, services ...ServiceSpec) *Spec {
	sp := &Spec{Services: services, ServiceLifetime: 2 * time.Minute, Env: env}
	sp.defaults()
	return sp
}

// bwrapServices opens a sandbox with services on the bubblewrap rung, skipping where this
// host cannot run them (no user namespaces for the private network, or no nsenter).
func bwrapServices(t *testing.T, sp *Spec) (*Sandbox, *Session) {
	t.Helper()
	needPython(t)
	s := rung(t, "bwrap")
	if err := (&bwrapDriver{cfg: &s.cfg}).supportsServices(sp); err != nil {
		if os.Getenv("EVALSI_REQUIRE_SANDBOX") != "" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sess, err := s.Open(ctx, sp)
	if err != nil {
		if strings.Contains(err.Error(), "private network") && os.Getenv("EVALSI_REQUIRE_SANDBOX") == "" {
			t.Skipf("no private network here: %v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return s, sess
}

func svcExec(t *testing.T, sess *Session, script string) (string, *ExecResult) {
	t.Helper()
	var out, errb bytes.Buffer
	res := sess.Exec(context.Background(), &Exec{Command: sh(script), Timeout: 30 * time.Second}, &out, &errb)
	return out.String() + errb.String(), res
}

const fetch = `python3 -c "import sys,urllib.request; r=urllib.request.urlopen(sys.argv[1], timeout=5); sys.stdout.write(r.read().decode())" `

func TestBwrapServicesAreReachableByNameAndPrivate(t *testing.T) {
	// A listener on the host's loopback must not be reachable from inside.
	host, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	hostPort := host.Addr().(*net.TCPAddr).Port
	web := serving("web", 8101, "/tmp")
	web.Command = []string{"sh", "-c", "env > /tmp/env.txt; echo hello > /tmp/hello.txt; exec python3 -u -m http.server 8101 --bind 0.0.0.0 --directory /tmp"}
	web.Env = map[string]string{"SERVICE_ONLY": "yes"}
	web.Aliases = []string{"web.internal"}
	_, sess := bwrapServices(t, servicesSpec(map[string]string{"AGENT_SECRET": "s3cret"}, web))
	out, res := svcExec(t, sess, fetch+"http://web:8101/hello.txt")
	if res.Outcome != OutcomeExit || res.ExitCode != 0 || strings.TrimSpace(out) != "hello" {
		t.Fatalf("by name: %v %q %s", res.Outcome, out, res.Error)
	}
	if out, res = svcExec(t, sess, fetch+"http://web.internal:8101/hello.txt"); strings.TrimSpace(out) != "hello" {
		t.Fatalf("by alias: %q %s", out, res.Error)
	}
	env, _ := svcExec(t, sess, fetch+"http://web:8101/env.txt")
	if !strings.Contains(env, "SERVICE_ONLY=yes") || strings.Contains(env, "AGENT_SECRET") {
		t.Fatalf("service environment: %q", env)
	}
	// The private network has no way out, and the host's loopback is not in it.
	_, res = svcExec(t, sess, fetch+"http://127.0.0.1:"+strconv.Itoa(hostPort)+"/")
	if res.ExitCode == 0 {
		t.Fatal("reached a listener on the host's loopback")
	}
	// The service does not see the agent's files.
	if _, res = svcExec(t, sess, "echo secret > agent.txt"); res.ExitCode != 0 {
		t.Fatal(res.Error)
	}
	if _, res = svcExec(t, sess, fetch+"http://web:8101/workspace/agent.txt"); res.ExitCode == 0 {
		t.Fatal("the service served an agent file")
	}
}

func TestBwrapServicesStatusLogsAndHealth(t *testing.T) {
	_, sess := bwrapServices(t, servicesSpec(nil, serving("web", 8102, "/tmp")))
	st, err := sess.Services(context.Background(), true)
	if err != nil || len(st) != 1 || !st[0].Ready || st[0].Name != "web" {
		t.Fatalf("status: %+v %v", st, err)
	}
	if _, res := svcExec(t, sess, fetch+"http://web:8102/"); res.ExitCode != 0 {
		t.Fatal(res.Error)
	}
	st, _ = sess.Services(context.Background(), true)
	if !strings.Contains(st[0].Logs, "GET /") {
		t.Fatalf("logs: %q", st[0].Logs)
	}
	// A service that dies is an infrastructure failure at the next command, never a score.
	g := sess.b.(*hostBackend).group
	g.kill(g.procs[0])
	_, res := svcExec(t, sess, "true")
	if res.Outcome != OutcomeRunnerFailure || !strings.Contains(res.Error, "service web is unhealthy") {
		t.Fatalf("after a crash: %s %q", res.Outcome, res.Error)
	}
	st, _ = sess.Services(context.Background(), false)
	if st[0].Ready || !strings.Contains(st[0].Reason, "exited") {
		t.Fatalf("status after a crash: %+v", st)
	}
}

func TestBwrapServiceOrderOneshotVolumesAndWorkdirMount(t *testing.T) {
	init := ServiceSpec{
		Name: "seed", Oneshot: true, MemoryMB: 128, CPUMillis: 100,
		Command: []string{"sh", "-c", "echo seeded > /shared/hello.txt"},
		Volumes: []string{"data:/shared"},
	}
	web := serving("web", 8103, "/shared")
	web.Volumes = []string{"data:/shared"}
	web.DependsOn = []string{"seed"}
	// Ready by a command in its own sandbox rather than a TCP check.
	web.ReadyCommand = []string{"python3", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8103/hello.txt')"}
	files := serving("files", 8104, "/agent")
	files.WorkdirMount = "/agent"
	_, sess := bwrapServices(t, servicesSpec(nil, web, init, files))
	if out, res := svcExec(t, sess, fetch+"http://web:8103/hello.txt"); strings.TrimSpace(out) != "seeded" {
		t.Fatalf("the one-shot's volume write: %q %s", out, res.Error)
	}
	// What the agent writes in its workdir is what the service with the workdir mounted serves.
	if _, res := svcExec(t, sess, "echo from-agent > note.txt"); res.ExitCode != 0 {
		t.Fatal(res.Error)
	}
	if out, _ := svcExec(t, sess, fetch+"http://files:8104/note.txt"); strings.TrimSpace(out) != "from-agent" {
		t.Fatalf("workdir mount: %q", out)
	}
	st, _ := sess.Services(context.Background(), false)
	if len(st) != 3 || !st[0].Ready || st[0].Name != "seed" {
		t.Fatalf("dependency order or oneshot status: %+v", st)
	}
}

func TestBwrapServicesAreKilledWithTheSandbox(t *testing.T) {
	_, sess := bwrapServices(t, servicesSpec(nil, serving("web", 8105, "/tmp")))
	g := sess.b.(*hostBackend).group
	pid := g.procs[0].cmd.Process.Pid
	holder := g.holder.pid
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []int{pid, holder} {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat("/proc/" + strconv.Itoa(p)); err != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if data, err := os.ReadFile("/proc/" + strconv.Itoa(p) + "/stat"); err == nil && !strings.Contains(string(data), ") Z") {
			t.Fatalf("process %d survived the sandbox", p)
		}
	}
}

func TestBwrapServiceStartupFailureNamesTheService(t *testing.T) {
	needPython(t)
	s := rung(t, "bwrap")
	bad := ServiceSpec{Name: "broken", Port: 8106, MemoryMB: 128, CPUMillis: 100, Command: []string{"sh", "-c", "echo boom >&2; exit 3"}}
	if err := (&bwrapDriver{cfg: &s.cfg}).supportsServices(servicesSpec(nil, bad)); err != nil {
		t.Skip(err)
	}
	_, err := s.Open(context.Background(), servicesSpec(nil, bad))
	if err == nil || !strings.Contains(err.Error(), "service broken exited during startup") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("startup failure: %v", err)
	}
}

func TestBwrapServicesRefuseWhatTheyCannotKeep(t *testing.T) {
	needPython(t)
	s := rung(t, "bwrap")
	d := &bwrapDriver{cfg: &s.cfg}
	if _, err := exec.LookPath("nsenter"); err != nil {
		t.Skip("no nsenter")
	}
	sp := servicesSpec(nil, serving("web", 8107, "/tmp"))
	sp.Network = Network{Mode: NetworkAllow}
	if err := d.supportsServices(sp); err == nil || !strings.Contains(err.Error(), "network: deny") {
		t.Fatalf("network: %v", err)
	}
	withFiles := serving("web", 8107, "/tmp")
	withFiles.Files = map[string]string{"a": "b"}
	if err := d.supportsServices(servicesSpec(nil, withFiles)); err == nil || !strings.Contains(err.Error(), "needs an image") {
		t.Fatalf("files without an image: %v", err)
	}
	// The pod backend does not take the extra fields, rather than ignoring them.
	pod := serving("web", 8107, "/tmp")
	pod.Image = "x"
	pod.Oneshot = true
	if err := (&podDriver{cfg: &PodConfig{}}).supports(servicesSpec(nil, pod)); err == nil || !strings.Contains(err.Error(), "oneshot is supported on the bubblewrap rung only") {
		t.Fatalf("pod: %v", err)
	}
	// And the Landlock rung has no private network to give.
	ll, err := New(Config{Ladder: []string{"landlock"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ll.Open(context.Background(), servicesSpec(nil, serving("web", 8107, "/tmp"))); err == nil || !strings.Contains(err.Error(), "services need the pod or bubblewrap rung") {
		t.Fatalf("landlock: %v", err)
	}
}

func TestServiceExtrasValidation(t *testing.T) {
	for name, change := range map[string]func(*ServiceSpec){
		"alias collides with a service": func(s *ServiceSpec) { s.Aliases = []string{"db"} },
		"bad volume":                    func(s *ServiceSpec) { s.Volumes = []string{"nocolon"} },
		"relative volume path":          func(s *ServiceSpec) { s.Volumes = []string{"v:rel"} },
		"bad setup network":             func(s *ServiceSpec) { s.SetupNetwork = "wide" },
		"relative workdir":              func(s *ServiceSpec) { s.Workdir = "app" },
		"file escapes":                  func(s *ServiceSpec) { s.Files = map[string]string{"../x": ""} },
		"oneshot with a ready command":  func(s *ServiceSpec) { s.Oneshot, s.ReadyCommand = true, []string{"true"} },
		"extra port clashes":            func(s *ServiceSpec) { s.Ports = []int{6379} },
	} {
		t.Run(name, func(t *testing.T) {
			svc := testService("db", 6379)
			svc.Name = "cache"
			other := testService("db", 6380)
			change(&svc)
			s := &Spec{Services: []ServiceSpec{svc, other}, ServiceLifetime: time.Hour}
			if s.Validate() == nil {
				t.Fatal("accepted")
			}
		})
	}
	oneshot := ServiceSpec{Name: "init", Command: []string{"true"}, MemoryMB: 16, CPUMillis: 1, Oneshot: true}
	if err := (&Spec{Services: []ServiceSpec{oneshot}, ServiceLifetime: time.Hour}).Validate(); err != nil {
		t.Fatalf("a one-shot needs no port: %v", err)
	}
}
