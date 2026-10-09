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
		got, err := m.resolvePath(rel)
		if err != nil {
			t.Fatalf("%q: %v", root, err)
		}
		want, _ := filepath.EvalSymlinks(filepath.Join(dir, "datasets", "sub", "a.jsonl"))
		if !filepath.IsAbs(got) || got != want {
			t.Errorf("%q: got %q, want %q", root, got, want)
		}
		if _, err := m.resolvePath("../x"); err == nil {
			t.Errorf("%q: a path outside the directory was accepted", root)
		}
	}
}
