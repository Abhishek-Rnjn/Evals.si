package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func open(t *testing.T, s *Sandbox, sp *Spec) *Session {
	t.Helper()
	sess, err := s.Open(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

type output struct {
	*ExecResult
	stdout, stderr string
}

func execIn(t *testing.T, sess *Session, cmd ...string) output {
	t.Helper()
	var out, errb bytes.Buffer
	res := sess.Exec(context.Background(), &Exec{Command: cmd, Timeout: 20 * time.Second}, &out, &errb)
	return output{res, out.String(), errb.String()}
}

func TestSessionKeepsItsWorkspace(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		sess := open(t, s, &Spec{Files: map[string][]byte{"seed.txt": []byte("seed")}})
		if r := execIn(t, sess, "sh", "-c", "mkdir -p sub && echo one > sub/a.txt"); r.ExitCode != 0 {
			t.Fatalf("%+v %s", r.ExecResult, r.stderr)
		}
		r := execIn(t, sess, "sh", "-c", "cat seed.txt sub/a.txt; echo $HOME | grep -q . && echo home")
		if r.stdout != "seedone\nhome\n" {
			t.Fatalf("second exec saw %q (%s)", r.stdout, r.stderr)
		}
		var out bytes.Buffer
		res := sess.Exec(context.Background(), &Exec{Command: []string{"cat", "a.txt"}, Cwd: "sub"}, &out, io.Discard)
		if res.ExitCode != 0 || out.String() != "one\n" {
			t.Fatalf("cwd: %+v %q", res, out.String())
		}
		if err := sess.WriteFiles([]File{{Path: "dir/new.py", Content: []byte("print(1)"), Mode: 0o755}}); err != nil {
			t.Fatal(err)
		}
		if r := execIn(t, sess, "sh", "-c", "test -x dir/new.py && cat dir/new.py"); r.stdout != "print(1)" {
			t.Fatalf("written file: %q %s", r.stdout, r.stderr)
		}
		files, missing, truncated, err := sess.ReadFiles([]string{"sub", "seed.txt", "nope"}, 0)
		if err != nil || truncated || len(missing) != 1 || missing[0] != "nope" {
			t.Fatalf("read: %v %v %v", missing, truncated, err)
		}
		got := map[string]string{}
		for _, f := range files {
			got[f.Path] = string(f.Content)
		}
		if got["sub/a.txt"] != "one\n" || got["seed.txt"] != "seed" || len(got) != 2 {
			t.Fatalf("files %v", got)
		}
		if execs, _, _ := sess.Stats(); execs != 4 {
			t.Errorf("execs = %d", execs)
		}
	})
}

// A command can plant symlinks and FIFOs in its workspace; reading or
// writing files from the host must neither follow them out nor hang.
func TestHostFileAccessIsNotFooledByTheSandbox(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		sess := open(t, s, &Spec{})
		r := execIn(t, sess, "sh", "-c", "ln -s /etc/passwd leak && ln -s / rootlink && mkfifo pipe && mkdir d && ln -s /etc d/up")
		if r.ExitCode != 0 {
			t.Fatalf("%+v %s", r.ExecResult, r.stderr)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			files, _, _, err := sess.ReadFiles([]string{"leak", "pipe", "d"}, 0)
			for _, f := range files {
				if strings.Contains(string(f.Content), "root:") {
					t.Errorf("read a host file through %s", f.Path)
				}
			}
			_ = err
			if _, _, _, err := sess.ReadFiles([]string{"rootlink/etc/passwd"}, 0); err == nil {
				t.Error("followed a symlink out of the workspace")
			}
			if err := sess.WriteFiles([]File{{Path: "leak", Content: []byte("x")}}); err == nil {
				t.Error("wrote through a symlink")
			}
			if err := sess.WriteFiles([]File{{Path: "pipe", Content: []byte("x")}}); err == nil {
				t.Error("wrote to a FIFO")
			}
			if err := sess.WriteFiles([]File{{Path: "rootlink/tmp/evalsi-escape", Content: []byte("x")}}); err == nil {
				t.Error("wrote through a directory symlink")
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("host file access hung on a FIFO")
		}
		if _, err := os.Stat("/tmp/evalsi-escape"); err == nil {
			os.Remove("/tmp/evalsi-escape")
			t.Fatal("escaped the workspace")
		}
	})
}

func TestSnapshotAndRestore(t *testing.T) {
	eachRung(t, func(t *testing.T, s *Sandbox) {
		m := NewManager(s)
		defer m.Close()
		sess, err := m.Create(context.Background(), &Spec{})
		if err != nil {
			t.Fatal(err)
		}
		execIn(t, sess, "sh", "-c", "echo setup > state.txt")
		snap, err := m.Snapshot(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		execIn(t, sess, "sh", "-c", "echo changed > state.txt")
		for range 2 {
			clone, err := m.Restore(context.Background(), snap, nil)
			if err != nil {
				t.Fatal(err)
			}
			if r := execIn(t, clone, "cat", "state.txt"); r.stdout != "setup\n" {
				t.Fatalf("restored %q", r.stdout)
			}
			execIn(t, clone, "sh", "-c", "echo clone > state.txt")
			if clone.Driver != sess.Driver {
				t.Errorf("restored on %s, taken on %s", clone.Driver, sess.Driver)
			}
		}
		open, err := m.Restore(context.Background(), snap, &Network{Mode: NetworkAllow})
		if err != nil || open.Spec.Network.Mode != NetworkAllow {
			t.Fatalf("restore with a network override: %v", err)
		}
		if r := execIn(t, sess, "cat", "state.txt"); r.stdout != "changed\n" {
			t.Fatalf("original %q", r.stdout)
		}
		if err := m.DeleteSnapshot(snap); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Restore(context.Background(), snap, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted snapshot: %v", err)
		}
	})
}

func TestManagerLimitsAndReaps(t *testing.T) {
	s := rung(t, "landlock")
	s.cfg.MaxSandboxes = 2
	m := NewManager(s)
	defer m.Close()
	a, err := m.Create(context.Background(), &Spec{IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(context.Background(), &Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), &Spec{}); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("limit: %v", err)
	}
	m.reapOnce(time.Now().Add(2 * time.Minute))
	if _, err := m.Get(a.ID); !errors.Is(err, ErrNotFound) {
		t.Error("idle sandbox was not reaped")
	}
	if _, err := m.Get(b.ID); err != nil {
		t.Error("the 30-minute default reaped too early")
	}
	if err := m.Destroy(b.ID); err != nil {
		t.Fatal(err)
	}
	if r := execIn(t, b, "true"); r.Outcome != OutcomeRunnerFailure {
		t.Errorf("exec in a destroyed sandbox: %+v", r.ExecResult)
	}
}

func TestEgressAllowlist(t *testing.T) {
	needPython(t)
	// Not 127.0.0.1, which is in NO_PROXY (loopback inside the sandbox is its own).
	srv := serveOn(t, "127.0.0.2:0", "hello from the allowed host")
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	other := serveOn(t, "127.0.0.3:0", "secret")
	script := `import sys, urllib.request
try:
    print(urllib.request.urlopen(sys.argv[1], timeout=10).read().decode())
except Exception as e:
    print("error:", e, file=sys.stderr); sys.exit(3)
`
	eachRung(t, func(t *testing.T, s *Sandbox) {
		sp := &Spec{Network: Network{Mode: NetworkAllowlist, Allow: []string{"127.0.0.2:" + port}}, Files: map[string][]byte{"get.py": []byte(script)}}
		if err := s.drivers[0].supports(sp); err != nil {
			t.Skip(err)
		}
		sess := open(t, s, sp)
		r := execIn(t, sess, "python3", "get.py", srv.URL)
		if r.ExitCode != 0 || !strings.Contains(r.stdout, "hello from the allowed host") {
			t.Fatalf("allowed host: %+v %q %q", r.ExecResult, r.stdout, r.stderr)
		}
		r = execIn(t, sess, "python3", "get.py", other.URL)
		if r.Outcome != OutcomeDenied || strings.Contains(r.stdout, "secret") {
			t.Fatalf("other host: %+v %q %q", r.ExecResult, r.stdout, r.stderr)
		}
		events := sess.Egress()
		if len(events) != 2 || !events[0].Allowed || events[1].Allowed || events[0].BytesReceived == 0 {
			t.Fatalf("egress log %+v", events)
		}
		// Bypassing the proxy finds no route.
		_, otherPort, _ := net.SplitHostPort(strings.TrimPrefix(other.URL, "http://"))
		r = execIn(t, sess, "python3", "-c", "import socket; socket.create_connection(('127.0.0.3', "+otherPort+"), timeout=5)")
		if r.ExitCode == 0 {
			t.Fatalf("a direct connection got out: %+v", r.ExecResult)
		}
	})
}

func serveOn(t *testing.T, addr, body string) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })}}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestAllowlistRules(t *testing.T) {
	rules, err := parseAllowlist([]string{"api.example.com", "*.pypi.org", "files.example.net:8443"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host string
		port int
		want bool
	}{
		{"api.example.com", 443, true}, {"API.example.com", 80, true}, {"api.example.com", 22, false},
		{"evil.example.com", 443, false}, {"files.pythonhosted.pypi.org", 443, true}, {"pypi.org", 443, false},
		{"files.example.net", 8443, true}, {"files.example.net", 443, false},
	} {
		if got := allowed(rules, c.host, c.port); got != c.want {
			t.Errorf("%s:%d = %v", c.host, c.port, got)
		}
	}
	for _, bad := range []string{"", "*", "http://x", "a:0", "a b"} {
		if _, err := parseAllowlist([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, sp := range []*Spec{
		{Network: Network{Mode: NetworkAllowlist}},
		{Network: Network{Mode: NetworkDeny, Allow: []string{"x"}}},
		{Network: Network{Mode: "open"}},
		{Workdir: "relative"}, {Workdir: "/proc/x"}, {Workdir: "/"},
		{Env: map[string]string{"A=B": "c"}},
		// Absolute files need an image with a writable root.
		{Files: map[string][]byte{"/etc/x": nil}},
		{Image: "alpine", ReadOnlyRoot: true, Files: map[string][]byte{"/etc/x": nil}},
		{Image: "alpine", Files: map[string][]byte{"/": nil}},
		{Files: map[string][]byte{"../x": nil}},
	} {
		if err := sp.Validate(); err == nil {
			t.Errorf("%+v accepted", sp)
		}
	}
	ok := &Spec{Image: "alpine", Files: map[string][]byte{"/etc/x": nil, "rel/y": nil}}
	if err := ok.Validate(); err != nil {
		t.Error(err)
	}
}

// testImage builds an OCI layout holding a static helper at /bin/tool and an /etc/motd.
func testImage(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain needed to build the image's helper")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "tool")
	build := exec.Command("go", "build", "-o", bin, "./testdata/tool")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the helper: %v\n%s", err, out)
	}
	tool, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range []struct {
		name string
		body []byte
		mode int64
		typ  byte
		link string
	}{
		{name: "bin/", mode: 0o755, typ: tar.TypeDir},
		{name: "bin/tool", body: tool, mode: 0o4755, typ: tar.TypeReg},
		{name: "etc/", mode: 0o555, typ: tar.TypeDir},
		{name: "etc/motd", body: []byte("from the image\n"), mode: 0o444, typ: tar.TypeReg},
		{name: "etc/motd.link", typ: tar.TypeSymlink, link: "motd"},
		{name: "dev/null", typ: tar.TypeChar},
	} {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Linkname: e.link, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write(e.body)
	}
	tw.Close()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := img.ConfigFile()
	cfg = cfg.DeepCopy()
	cfg.Config.Env = []string{"PATH=/bin", "IMAGE_VAR=yes"}
	if img, err = mutate.ConfigFile(img, cfg); err != nil {
		t.Fatal(err)
	}
	lp := filepath.Join(dir, "layout")
	p, err := layout.Write(lp, empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AppendImage(img); err != nil {
		t.Fatal(err)
	}
	return "oci-layout:" + lp
}

func TestImageRoots(t *testing.T) {
	s := rung(t, "bwrap")
	s.cfg.CacheDir = t.TempDir()
	s.images = newImageStore(filepath.Join(s.cfg.CacheDir, "images"))
	ref := testImage(t)

	sess := open(t, s, &Spec{Image: ref})
	if r := execIn(t, sess, "/bin/tool", "cat", "/etc/motd.link"); r.stdout != "from the image\n" {
		t.Fatalf("image root: %+v %q %q", r.ExecResult, r.stdout, r.stderr)
	}
	if r := execIn(t, sess, "tool", "env", "IMAGE_VAR"); r.stdout != "yes\n" {
		t.Fatalf("image env: %q %q", r.stdout, r.stderr)
	}
	// The root is a private writable copy: changes persist in this sandbox only.
	if r := execIn(t, sess, "tool", "write", "/etc/motd", "changed"); r.ExitCode != 0 {
		t.Fatalf("writable root: %+v %s", r.ExecResult, r.stderr)
	}
	if r := execIn(t, sess, "tool", "cat", "/etc/motd"); r.stdout != "changed" {
		t.Fatalf("root write did not persist: %q", r.stdout)
	}
	files, _, _, err := sess.ReadFiles([]string{"/etc/motd"}, 0)
	if err != nil || len(files) != 1 || string(files[0].Content) != "changed" {
		t.Fatalf("absolute read: %v %v", files, err)
	}
	if !strings.HasPrefix(sess.ImageDigest(), "sha256:") {
		t.Errorf("digest %q", sess.ImageDigest())
	}
	// A second sandbox from the cached image starts clean.
	fresh := open(t, s, &Spec{Image: ref})
	if r := execIn(t, fresh, "tool", "cat", "/etc/motd"); r.stdout != "from the image\n" {
		t.Fatalf("cache was modified: %q", r.stdout)
	}
	img, _ := s.images.get(context.Background(), ref)
	if fi, err := os.Stat(filepath.Join(img.Root, "bin/tool")); err != nil || fi.Mode()&os.ModeSetuid != 0 {
		t.Errorf("setuid bit kept: %v %v", fi.Mode(), err)
	}

	ro := open(t, s, &Spec{Image: ref, ReadOnlyRoot: true, Workdir: "/work"})
	if r := execIn(t, ro, "tool", "write", "/etc/motd", "x"); r.ExitCode == 0 {
		t.Fatal("read-only root was writable")
	}
	if r := execIn(t, ro, "tool", "write", "/work/out", "ok"); r.ExitCode != 0 {
		t.Fatalf("workdir under a read-only root: %+v %s", r.ExecResult, r.stderr)
	}

	ll := rung(t, "landlock")
	if _, err := ll.Open(context.Background(), &Spec{Image: ref}); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "image root") {
		t.Fatalf("landlock with an image: %v", err)
	}
}

func TestExtractTarStaysInside(t *testing.T) {
	for name, entries := range map[string][]tar.Header{
		"dotdot":          {{Name: "../../escape", Typeflag: tar.TypeReg}},
		"through-link":    {{Name: "out", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, {Name: "out/evalsi-escape", Typeflag: tar.TypeReg}},
		"hardlink-escape": {{Name: "h", Typeflag: tar.TypeLink, Linkname: "../../etc/passwd"}},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			for _, h := range entries {
				h.Mode = 0o644
				tw.WriteHeader(&h)
			}
			tw.Close()
			dst := filepath.Join(t.TempDir(), "root")
			err := extractTar(context.Background(), &buf, dst)
			if _, serr := os.Stat("/tmp/evalsi-escape"); serr == nil {
				os.Remove("/tmp/evalsi-escape")
				t.Fatal("escaped")
			}
			if name == "dotdot" {
				// Cleaned to a path inside the root.
				if _, serr := os.Stat(filepath.Join(dst, "escape")); err != nil || serr != nil {
					t.Fatalf("dotdot: %v %v", err, serr)
				}
				return
			}
			if name == "hardlink-escape" {
				if fi, serr := os.Lstat(filepath.Join(dst, "h")); err == nil && serr == nil && fi.Size() > 0 {
					t.Fatal("hard-linked a host file")
				}
				return
			}
			if err == nil {
				t.Fatal("wrote through a symlink")
			}
		})
	}
}
