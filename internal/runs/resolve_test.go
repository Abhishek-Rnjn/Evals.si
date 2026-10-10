package runs

import (
	"os"
	"path/filepath"
	"testing"
)

// datasets_dir may be relative (the example config says ./datasets); paths
// resolved inside it are absolute, which the worker requires.
func TestResolvePathWithARelativeDatasetsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "datasets", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "datasets", "sub", "a.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	for _, root := range []string{"datasets", "./datasets", "."} {
		m := &Manager{opts: Options{DatasetsDir: root}}
		rel := "sub/a.jsonl"
		if root == "." {
			rel = "datasets/sub/a.jsonl"
		}
		got, err := m.resolvePath(rel, "")
		if err != nil {
			t.Fatalf("%q: %v", root, err)
		}
		want, _ := filepath.EvalSymlinks(filepath.Join(dir, "datasets", "sub", "a.jsonl"))
		if !filepath.IsAbs(got) || got != want {
			t.Errorf("%q: got %q, want %q", root, got, want)
		}
		if _, err := m.resolvePath("../x", ""); err == nil {
			t.Errorf("%q: a path outside the directory was accepted", root)
		}
	}
}

// A relative datasets_dir resolves correctly when the working directory was
// reached through a symlink (macOS's /var -> /private/var, for one).
func TestResolvePathUnderASymlinkedWorkingDirectory(t *testing.T) {
	real := t.TempDir()
	if err := os.MkdirAll(filepath.Join(real, "datasets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "datasets", "a.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(link)
	m := &Manager{opts: Options{DatasetsDir: "./datasets"}}
	got, err := m.resolvePath("a.jsonl", "")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(real, "datasets", "a.jsonl"))
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A symlink cannot lead a path into another project's promotions, which
// access rules check by the path as spelled.
func TestResolvePathRefusesAliasesIntoAnotherProject(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "promoted", "support"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "promoted", "support", "secret.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "promoted", "support"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	m := &Manager{opts: Options{DatasetsDir: root}}
	if _, err := m.resolvePath("alias/secret.jsonl", "checkout"); err == nil {
		t.Error("an alias into support's promotions resolved for checkout")
	}
	if _, err := m.resolveURI("inspect://alias/secret.jsonl", "checkout"); err == nil {
		t.Error("an importer URI alias into support's promotions resolved for checkout")
	}
	// The project's own promotions, by any spelling, and the real path.
	for _, p := range []string{"alias/secret.jsonl", "promoted/support/secret.jsonl", "promoted/../promoted/support/secret.jsonl"} {
		if _, err := m.resolvePath(p, "support"); err != nil {
			t.Errorf("%s for support: %v", p, err)
		}
	}
	if _, err := m.resolvePath("promoted/support/secret.jsonl", "checkout"); err != nil {
		t.Errorf("the spelled path (checked by access rules): %v", err)
	}
}
