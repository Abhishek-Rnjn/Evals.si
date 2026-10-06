package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
