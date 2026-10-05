package sandbox

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The test binary doubles as the launcher, as evalsid does in production.
func TestMain(m *testing.M) {
	if os.Getenv(specEnv) != "" && len(os.Args) > 1 && os.Args[1] == "sandbox-exec" {
		os.Exit(Launch(os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "sandbox-forward" {
		os.Exit(Forward(os.Args[2:], os.Stderr))
	}
	os.Exit(m.Run())
}

func rung(t *testing.T, name string) *Sandbox {
	t.Helper()
	s, err := New(Config{Ladder: []string{name}, MinIsolation: "none"})
	if err != nil {
		t.Fatal(err)
	}
	st := s.Probe(context.Background())[0]
	if !st.Available {
		if os.Getenv("EVALSI_REQUIRE_SANDBOX") != "" {
			t.Fatalf("%s unavailable: %s", name, st.Reason)
		}
		t.Skipf("%s unavailable here: %s", name, st.Reason)
	}
	return s
}

func run(t *testing.T, s *Sandbox, req *Request) *Result {
	t.Helper()
	res, err := s.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func sh(script string) []string { return []string{"sh", "-c", script} }

func needPython(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
}

func eachRung(t *testing.T, f func(t *testing.T, s *Sandbox)) {
	for _, name := range []string{"bwrap", "landlock"} {
		t.Run(name, func(t *testing.T) { f(t, rung(t, name)) })
	}
}

func TestRunsCommandsInAWorkspace(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{
			Command: sh("cat in.txt; echo out > out.txt && cat out.txt; pwd"),
			Files:   map[string]string{"in.txt": "hello\n"},
			Stdin:   "ignored",
		})
		if res.Outcome != OutcomeExit || res.ExitCode != 0 {
			t.Fatalf("%+v", res)
		}
		if !strings.HasPrefix(res.Stdout, "hello\nout\n") {
			t.Errorf("stdout %q", res.Stdout)
		}
		if res.Isolation.Driver == "" || res.Isolation.Enforcement != "full" {
			t.Errorf("isolation %+v", res.Isolation)
		}
	})
}

func TestHostFilesAreNotReadableOrWritable(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("api-key"), 0o644); err != nil {
		t.Fatal(err)
	}
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{Command: sh("cat " + secret)})
		if res.ExitCode == 0 || strings.Contains(res.Stdout, "api-key") {
			t.Fatalf("read a host file: %+v", res)
		}
		res = run(t, s, &Request{Command: sh("echo x > /usr/evalsi-escape")})
		if res.Outcome != OutcomeDenied {
			t.Fatalf("write outside the workspace: %+v", res)
		}
		if _, err := os.Stat("/usr/evalsi-escape"); err == nil {
			os.Remove("/usr/evalsi-escape")
			t.Fatal("the file was created on the host")
		}
	})
}

func TestReadOnlyMode(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{Command: sh("cat a; echo x > b"), Files: map[string]string{"a": "A"}, Mode: ModeReadOnly})
		if res.Outcome != OutcomeDenied || res.Stdout != "A" {
			t.Fatalf("%+v", res)
		}
	})
}

func TestEnvironmentIsCleared(t *testing.T) {
	t.Setenv("EVALSI_TEST_SECRET", "leak")
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{Command: []string{"env"}, Env: map[string]string{"GIVEN": "yes"}})
		if strings.Contains(res.Stdout, "leak") || !strings.Contains(res.Stdout, "GIVEN=yes") {
			t.Fatalf("env: %q", res.Stdout)
		}
	})
}

func TestNetworkDeniedByDefault(t *testing.T) {
	needPython(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	script := "import socket,sys\nsocket.create_connection(('127.0.0.1', int(sys.argv[1])), timeout=2)\nprint('connected')"
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{Command: []string{"python3", "c.py", strconv.Itoa(port)}, Files: map[string]string{"c.py": script}})
		if res.ExitCode == 0 || strings.Contains(res.Stdout, "connected") {
			t.Fatalf("reached the host network: %+v", res)
		}
	})
}

func TestNamespacesAndPtraceAreBlocked(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{Command: []string{"unshare", "-U", "true"}})
		if res.ExitCode == 0 {
			t.Fatalf("created a user namespace: %+v", res)
		}
	})
}

func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{Command: sh("sleep 30 & sleep 30"), TimeoutS: 0.5})
		if res.Outcome != OutcomeTimeout || res.DurationMS > 5000 {
			t.Fatalf("%+v", res)
		}
	})
}

func TestMemoryLimit(t *testing.T) {
	needPython(t)
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{Command: []string{"python3", "-c", "x = bytearray(1 << 30)"}, MemoryMB: 256})
		if res.ExitCode == 0 || !strings.Contains(res.Stderr, "MemoryError") {
			t.Fatalf("%+v", res)
		}
	})
}

func TestOutputIsCapped(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{Command: sh("yes | head -c 100000"), OutputLimit: 1000})
		if len(res.Stdout) != 1000 || !res.Truncated {
			t.Fatalf("got %d bytes, truncated=%v", len(res.Stdout), res.Truncated)
		}
	})
}

func TestMissingCommandIsNotARunnerFailure(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		res := run(t, s, &Request{Command: []string{"no-such-command-evalsi"}})
		if res.Outcome == OutcomeRunnerFailure || res.ExitCode != 127 {
			t.Fatalf("%+v", res)
		}
	})
}

func TestFailsClosed(t *testing.T) {
	s, err := New(Config{Ladder: []string{"landlock"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Run(context.Background(), &Request{Command: []string{"true"}, MinIsolation: "namespaced"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	broken, _ := New(Config{Ladder: []string{"bwrap"}, BwrapPath: "/bin/false", MinIsolation: "none"})
	_, err = broken.Run(context.Background(), &Request{Command: []string{"true"}})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("a broken runner must fail closed: %v", err)
	}
	if _, err := New(Config{Ladder: []string{"kata"}}); err == nil {
		t.Fatal("Kubernetes-only rungs must be rejected")
	}
}

func TestWorkspacePathsStayInside(t *testing.T) {
	for _, name := range []string{"../x", "/etc/x", "a/../../x"} {
		sp := &Spec{Files: map[string][]byte{name: []byte("x")}}
		if err := sp.Validate(); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		driver, stderr string
		code           int
		want           string
	}{
		{"bwrap", "bwrap: setting up uid map: Permission denied\n", 1, OutcomeRunnerFailure},
		{"landlock", launcherPrefix + "landlock: boom\n", exitLauncherFailure, OutcomeRunnerFailure},
		{"landlock", "sh: 1: cannot create /x: Read-only file system\n", 2, OutcomeDenied},
		{"bwrap", "Traceback...\nAssertionError\n", 1, OutcomeExit},
	}
	for _, c := range cases {
		res := &ExecResult{Outcome: OutcomeExit, ExitCode: c.code}
		classify(c.driver, res, c.stderr)
		if res.Outcome != c.want {
			t.Errorf("%q: got %s, want %s", c.stderr, res.Outcome, c.want)
		}
	}
}
