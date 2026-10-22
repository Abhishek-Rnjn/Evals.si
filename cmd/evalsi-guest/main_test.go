//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// RLIMIT_NPROC never binds uid 0; max_procs must still bind a command running
// as root, through a pids cgroup.
func TestLimitProcsBindsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if os.Getenv("EVALSI_TEST_PIDS_CGROUP") == "" {
		t.Skip("needs a writable root cgroup v2 with the pids controller, as in a guest: docker run --privileged --cgroupns=host -e EVALSI_TEST_PIDS_CGROUP=1")
	}
	bin := filepath.Join(t.TempDir(), "evalsi-guest")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	run := func(procs string) error {
		return exec.Command(bin, "limit", "0", procs, "0", "0", "--", "/bin/sh", "-c", "/bin/true; /bin/true").Run()
	}
	if err := run("0"); err != nil {
		t.Fatalf("without a process limit: %v", err)
	}
	// Every uid-0 process on the machine counts, so a limit of 1 is always reached.
	if err := run("1"); err == nil {
		t.Fatal("max_procs did not stop a root command from forking")
	}
}

func TestCopyTree(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "pkg"), 0o755))
	must(os.Mkdir(filepath.Join(src, "pkg", "sub"), 0o555))
	must(os.WriteFile(filepath.Join(src, "pkg", "a.py"), []byte("x = 1\n"), 0o444))
	must(os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o555))
	must(os.Symlink("pkg/a.py", filepath.Join(src, "link")))

	must(copyTree(src, dst))

	if b, err := os.ReadFile(filepath.Join(dst, "pkg", "a.py")); err != nil || string(b) != "x = 1\n" {
		t.Fatalf("a.py: %q %v", b, err)
	}
	// Copies stay executable where they were, and become writable.
	if fi, _ := os.Stat(filepath.Join(dst, "run.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dst, "pkg", "sub")); fi.Mode().Perm() != 0o755 {
		t.Errorf("pkg/sub mode %v", fi.Mode().Perm())
	}
	must(os.WriteFile(filepath.Join(dst, "pkg", "new.py"), nil, 0o644))
	if l, err := os.Readlink(filepath.Join(dst, "link")); err != nil || l != "pkg/a.py" {
		t.Errorf("link: %q %v", l, err)
	}

	// A missing source seeds nothing.
	empty := t.TempDir()
	must(copyTree(filepath.Join(src, "missing"), empty))
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("seeded %v from nothing", entries)
	}
}
