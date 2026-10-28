package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalImagesRefusedUnlessAllowed(t *testing.T) {
	dir := t.TempDir()
	st := newImageStore(filepath.Join(dir, "images"))
	for _, ref := range []string{"dir:" + dir, "oci-layout:" + dir, "docker-archive:" + dir} {
		if _, err := st.get(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "allow_local_images") {
			t.Errorf("%s: want a refusal naming allow_local_images, got %v", ref, err)
		}
	}
	st.allowLocal = true
	if _, err := st.get(context.Background(), "dir:"+dir); err != nil {
		t.Errorf("allowed dir: image: %v", err)
	}
}

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

// The workdir is created and chmodded on open and on restore; a symlink the
// image or a snapshot carries as the workdir must not redirect that to the host.
func TestMakeWorkdirRefusesSymlinks(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"absolute", "outside"},
		{"relative", "../outside"},
		{"missing", "outside/new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			root, outside := filepath.Join(dir, "root"), filepath.Join(dir, "outside")
			for _, d := range []string{root, outside} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			target := tc.target
			if tc.name != "relative" {
				target = filepath.Join(dir, tc.target)
			}
			if err := os.Symlink(target, filepath.Join(root, "work")); err != nil {
				t.Fatal(err)
			}
			b := &hostBackend{sp: &Spec{Workdir: "/work"}, root: root, work: filepath.Join(root, "work")}
			if err := b.makeWorkdir(); err == nil {
				t.Fatal("made a workdir through a symlink")
			}
			fi, err := os.Stat(outside)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o755 {
				t.Errorf("host directory mode changed to %v", fi.Mode().Perm())
			}
			if _, err := os.Stat(filepath.Join(outside, "new")); err == nil {
				t.Error("created a directory outside the sandbox root")
			}
		})
	}
}

func TestMakeWorkdirInsideRoot(t *testing.T) {
	root := t.TempDir()
	b := &hostBackend{sp: &Spec{Workdir: "/a/b"}, root: root, work: filepath.Join(root, "a/b")}
	if err := b.makeWorkdir(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(b.work)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o777 {
		t.Fatalf("workdir %v, want a 0777 directory", fi.Mode())
	}
}
