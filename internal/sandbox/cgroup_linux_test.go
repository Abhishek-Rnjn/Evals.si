//go:build linux

package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// testCgroup makes a cgroup v2 directory for one test: under
// EVALSI_TEST_CGROUP, else under a writable cgroup v2 mount (as root).
func testCgroup(t *testing.T) string {
	t.Helper()
	parent := os.Getenv("EVALSI_TEST_CGROUP")
	if parent == "" {
		for _, c := range []string{cgroupMount, filepath.Join(cgroupMount, "unified")} {
			if isCgroup2(c) == nil && unix.Access(filepath.Join(c, "cgroup.procs"), unix.W_OK) == nil {
				parent = c
				break
			}
		}
	}
	if parent == "" {
		if os.Getenv("EVALSI_REQUIRE_CGROUP") != "" {
			t.Fatal("no writable cgroup v2 directory (set EVALSI_TEST_CGROUP)")
		}
		t.Skip("no writable cgroup v2 directory here (set EVALSI_TEST_CGROUP)")
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	dir := filepath.Join(parent, "evalsi-test-"+hex.EncodeToString(b[:]))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Skipf("cannot create a cgroup under %s: %v", parent, err)
	}
	t.Cleanup(func() { removeCgroup(dir) })
	return dir
}

func cgroupRung(t *testing.T, name, dir string) *Sandbox {
	t.Helper()
	s, err := New(Config{Ladder: []string{name}, MinIsolation: "none", Cgroup: dir})
	if err != nil {
		t.Fatal(err)
	}
	if st := s.Probe(context.Background())[0]; !st.Available {
		if os.Getenv("EVALSI_REQUIRE_SANDBOX") != "" {
			t.Fatalf("%s unavailable: %s", name, st.Reason)
		}
		t.Skipf("%s unavailable here: %s", name, st.Reason)
	}
	return s
}

func needControllers(t *testing.T, s *Sandbox, want ...string) {
	t.Helper()
	for _, c := range want {
		if !s.cgroups.has(c) {
			if os.Getenv("EVALSI_REQUIRE_CGROUP") != "" {
				t.Fatalf("the %s controller is not delegated to %s", c, s.cgroups.dir)
			}
			t.Skipf("the %s controller is not delegated to %s", c, s.cgroups.dir)
		}
	}
}

func TestCgroupConfig(t *testing.T) {
	if _, err := setupCgroups("relative/path", 0); err == nil {
		t.Error("a relative path was accepted")
	}
	if _, err := setupCgroups(t.TempDir(), 0); err == nil || !strings.Contains(err.Error(), "not a cgroup v2") {
		t.Errorf("a plain directory: %v", err)
	}
	if p, err := setupCgroups("off", 0); p != nil || err != nil {
		t.Errorf("off: %v %v", p, err)
	}
	if err := (Config{CgroupCPUs: -1}).Validate(); err == nil {
		t.Error("negative cgroup_cpus was accepted")
	}
}

// A daemon the command detaches (a new session, so not in the process
// group) is killed with the cgroup when the execution ends.
func TestCgroupKillsDetachedProcesses(t *testing.T) {
	dir := testCgroup(t)
	s := cgroupRung(t, "landlock", dir)
	res := run(t, s, &Request{Command: sh(`setsid sleep 300 </dev/null >/dev/null 2>&1 & echo $!`)})
	if res.Outcome != OutcomeExit || res.ExitCode != 0 {
		t.Fatalf("%+v", res)
	}
	if !slices.ContainsFunc(res.Isolation.Notes, func(n string) bool { return strings.Contains(n, dir) }) {
		t.Errorf("notes %v do not name the cgroup", res.Isolation.Notes)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatalf("stdout %q", res.Stdout)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		cmd, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
		if err != nil || !strings.Contains(string(cmd), "300") {
			break
		}
		if time.Now().After(deadline) {
			unix.Kill(pid, unix.SIGKILL)
			t.Fatal("the detached process outlived its execution")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "sandbox-*")); len(left) > 0 {
		t.Errorf("cgroups left behind: %v", left)
	}
}

// On the Landlock rung, which shares the host's uid, pids.max is the only
// process cap.
func TestCgroupProcessCap(t *testing.T) {
	s := cgroupRung(t, "landlock", testCgroup(t))
	needControllers(t, s, "pids")
	res := run(t, s, &Request{
		Command:  sh(`i=0; while [ $i -lt 40 ]; do sleep 2 & i=$((i+1)); done; wait`),
		MaxProcs: 8, TimeoutS: 20,
	})
	if res.ExitCode == 0 && res.Stderr == "" {
		t.Fatalf("40 processes ran under a cap of 8: %+v", res)
	}
	if !strings.Contains(res.Stderr, "fork") && !strings.Contains(res.Stderr, "Resource temporarily unavailable") {
		t.Errorf("stderr %q", res.Stderr)
	}
}

// rlimits bound each process; memory.max bounds the command as a whole.
func TestCgroupMemoryLimitCoversTheProcessTree(t *testing.T) {
	needPython(t)
	dir := testCgroup(t)
	for _, name := range []string{"bwrap", "landlock"} {
		t.Run(name, func(t *testing.T) {
			s := cgroupRung(t, name, dir)
			needControllers(t, s, "memory")
			// Each process stays under the 256 MB address-space limit; together they do not.
			grow := `python3 -c "import time; x = b'x' * (100 << 20); time.sleep(3)"`
			res := run(t, s, &Request{
				Command:  sh(grow + " & " + grow + " & " + grow + " & " + grow + " & wait"),
				MemoryMB: 256, TimeoutS: 30,
			})
			if !slices.Contains(res.Denials, memoryLimitDenial) {
				t.Fatalf("no out-of-memory kill reported: %+v", res)
			}
		})
	}
}
