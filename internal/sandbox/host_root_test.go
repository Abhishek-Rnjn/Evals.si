package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// With a writable image root the workdir lives inside a tree the sandboxed
// command controls; a symlink planted as the workdir must not redirect file
// transfer to the host.
func TestHostFilesStayInsideRootWhenWorkdirIsSymlink(t *testing.T) {
	dir := t.TempDir()
	root, outside := filepath.Join(dir, "root"), filepath.Join(dir, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "work")); err != nil {
		t.Fatal(err)
	}
	b := &hostBackend{sp: &Spec{Workdir: "/work"}, root: root, work: filepath.Join(root, "work")}
	files, _, _, err := b.readFiles([]string{"secret"}, 1<<20)
	if len(files) != 0 {
		t.Fatalf("read a host file through the workdir symlink: %q (err %v)", files[0].Content, err)
	}
	if err := b.writeFile(File{Path: "marker", Content: []byte("x")}); err == nil {
		t.Fatal("write through the workdir symlink succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "marker")); err == nil {
		t.Fatal("wrote outside the sandbox root")
	}
}
