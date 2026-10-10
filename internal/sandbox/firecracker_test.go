package sandbox

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	buildOnce sync.Once
	built     map[string]string
	buildErr  error
)

// buildHelpers builds the static guest agent and the fake firecracker once.
func buildHelpers(t *testing.T) map[string]string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain needed")
	}
	for _, tool := range []string{"mkfs.ext4", "debugfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s needed", tool)
		}
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "evalsi-fc-helpers-")
		if err != nil {
			buildErr = err
			return
		}
		built = map[string]string{"guest": filepath.Join(dir, "evalsi-guest"), "fakefc": filepath.Join(dir, "firecracker")}
		for out, pkg := range map[string]string{built["guest"]: "../../cmd/evalsi-guest", built["fakefc"]: "./testdata/fakefc"} {
			cmd := exec.Command("go", "build", "-o", out, pkg)
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if b, err := cmd.CombinedOutput(); err != nil {
				buildErr = errors.New(string(b))
				return
			}
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return built
}

// fakeVMs is a firecracker rung over the fake: every "VM" is the guest agent
// running on the host, so this tests the driver's plumbing, not isolation.
func fakeVMs(t *testing.T, mutate func(*FirecrackerConfig)) (*Sandbox, string) {
	t.Helper()
	h := buildHelpers(t)
	dir := t.TempDir()
	kvm := filepath.Join(dir, "kvm")
	kernel := filepath.Join(dir, "vmlinux")
	for _, f := range []string{kvm, kernel} {
		if err := os.WriteFile(f, []byte("fake"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FAKEFC_GUEST", h["guest"])
	fc := &FirecrackerConfig{Binary: h["fakefc"], Kernel: kernel, Guest: h["guest"], KVMDevice: kvm, DiskMB: 64}
	if mutate != nil {
		mutate(fc)
	}
	work := filepath.Join(dir, "work")
	_ = os.MkdirAll(work, 0o700)
	s, err := New(Config{Ladder: []string{"firecracker"}, MinIsolation: "none", Firecracker: fc, CacheDir: filepath.Join(dir, "cache"), WorkDir: work})
	if err != nil {
		t.Fatal(err)
	}
	ref := testImage(t)
	fc.DefaultImage = ref
	return s, ref
}

func calls(t *testing.T, vmDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(vmDir, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestFirecrackerUnavailableWithoutKVM(t *testing.T) {
	s, _ := New(Config{Ladder: []string{"firecracker"}, Firecracker: &FirecrackerConfig{Kernel: "/x", KVMDevice: "/nonexistent/kvm", DefaultImage: "alpine"}})
	st := s.Probe(context.Background())[0]
	want := "no usable KVM"
	if runtime.GOOS != "linux" {
		want = "needs Linux" // the platform check comes first
	}
	if st.Available || !strings.Contains(st.Reason, want) {
		t.Fatalf("probe = %+v", st)
	}
	none, _ := New(Config{Ladder: []string{"firecracker"}})
	if st := none.Probe(context.Background())[0]; st.Available {
		t.Fatalf("unconfigured probe = %+v", st)
	}
	if err := (Config{Firecracker: &FirecrackerConfig{Jailer: "/usr/bin/jailer"}}).Validate(); err == nil {
		t.Error("a jailer without an unprivileged uid was accepted")
	}
}

func TestFirecrackerDriver(t *testing.T) {
	s, ref := fakeVMs(t, nil)
	if st := s.Probe(context.Background())[0]; !st.Available || st.Level != "vm" {
		t.Fatalf("probe = %+v", st)
	}
	m := NewManager(s)
	defer m.Close()
	sess, err := m.Create(context.Background(), &Spec{Image: ref, Files: map[string][]byte{"in.txt": []byte("hello")}, Workdir: "/work"})
	if err != nil {
		t.Fatal(err)
	}
	iso := sess.Isolation()
	if iso.Driver != "firecracker" || iso.Level != "vm" || iso.Enforcement != "full" {
		t.Errorf("isolation %+v", iso)
	}
	vm := sess.b.(*fcVM)
	log := calls(t, vm.dir)
	for _, want := range []string{
		`PUT /boot-source {"boot_args":"console=ttyS0 reboot=k panic=1 pci=off quiet root=/dev/vda rw init=/sbin/evalsi-guest","kernel_image_path":"vmlinux"}`,
		`PUT /drives/rootfs {"drive_id":"rootfs","is_read_only":false,"is_root_device":true,"path_on_host":"rootfs.ext4"}`,
		`PUT /vsock {"guest_cid":3,"uds_path":"v.sock"}`,
		`PUT /actions {"action_type":"InstanceStart"}`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("missing call %s in\n%s", want, log)
		}
	}
	var out, errb bytes.Buffer
	res := sess.Exec(context.Background(), &Exec{Command: []string{"sh", "-c", "cat in.txt; echo -n ' '$IMAGE_VAR; echo oops >&2; exit 3"}}, &out, &errb)
	if res.Outcome != OutcomeExit || res.ExitCode != 3 || out.String() != "hello yes" || errb.String() != "oops\n" {
		t.Fatalf("exec %+v %q %q", res, out.String(), errb.String())
	}
	res = sess.Exec(context.Background(), &Exec{Command: []string{"sleep", "5"}, Timeout: 200 * time.Millisecond}, &out, &errb)
	if res.Outcome != OutcomeTimeout {
		t.Errorf("timeout: %+v", res)
	}
	res = sess.Exec(context.Background(), &Exec{Command: []string{"no-such-command-evalsi"}}, &out, &errb)
	if res.Outcome == OutcomeRunnerFailure || res.ExitCode != 127 {
		t.Errorf("missing command: %+v", res)
	}
	if err := sess.WriteFiles([]File{{Path: "sub/a.txt", Content: []byte("a")}, {Path: "/etc/extra", Content: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	files, missing, _, err := sess.ReadFiles([]string{"sub", "/etc/motd", "nope"}, 0)
	if err != nil || len(files) != 2 || len(missing) != 1 || missing[0] != "nope" {
		t.Fatalf("read %v %v %v", files, missing, err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = string(f.Content)
	}
	if got["sub/a.txt"] != "a" || got["/etc/motd"] != "from the image\n" {
		t.Errorf("files %v", got)
	}

	// Snapshot, change the original, restore twice: clones start from the snapshot.
	snap, err := m.Snapshot(context.Background(), sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if log := calls(t, vm.dir); !strings.Contains(log, `PATCH /vm {"state":"Paused"}`) || !strings.Contains(log, "PUT /snapshot/create") || !strings.Contains(log, `PATCH /vm {"state":"Resumed"}`) {
		t.Errorf("snapshot calls:\n%s", log)
	}
	_ = sess.WriteFiles([]File{{Path: "in.txt", Content: []byte("changed")}})
	for range 2 {
		clone, err := m.Restore(context.Background(), snap, nil)
		if err != nil {
			t.Fatal(err)
		}
		out.Reset()
		clone.Exec(context.Background(), &Exec{Command: []string{"cat", "in.txt"}}, &out, &errb)
		if out.String() != "hello" {
			t.Errorf("clone saw %q", out.String())
		}
		cvm := clone.b.(*fcVM)
		if !strings.Contains(calls(t, cvm.dir), "PUT /snapshot/load") {
			t.Error("restore did not load the snapshot")
		}
		if raw, err := os.ReadFile(filepath.Join(cvm.dir, "fsroot", ".evalsi-refreshed")); err != nil || string(raw) != "1" {
			t.Errorf("the clone's entropy and clock were not refreshed: %q %v", raw, err)
		}
	}
	if err := m.Destroy(sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := vm.cmd.Process.Signal(nil); err == nil {
		t.Error("the VM process survived its sandbox")
	}
}

func TestFirecrackerEgressOverVsock(t *testing.T) {
	needPython(t)
	s, ref := fakeVMs(t, nil)
	srv := serveOn(t, "127.0.0.2:0", "through the proxy")
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	other := serveOn(t, "127.0.0.3:0", "secret")
	sess, err := s.Open(context.Background(), &Spec{Image: ref, Network: Network{Mode: NetworkAllowlist, Allow: []string{"127.0.0.2:" + port}}})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	get := `import sys, urllib.request
try:
    print(urllib.request.urlopen(sys.argv[1], timeout=10).read().decode())
except Exception as e:
    print("error:", e, file=sys.stderr); sys.exit(3)
`
	var out, errb bytes.Buffer
	res := sess.Exec(context.Background(), &Exec{Command: []string{"python3", "-c", get, srv.URL}, Env: map[string]string{"PATH": os.Getenv("PATH")}}, &out, &errb)
	if res.ExitCode != 0 || !strings.Contains(out.String(), "through the proxy") {
		t.Fatalf("allowed: %+v %q %q", res, out.String(), errb.String())
	}
	out.Reset()
	res = sess.Exec(context.Background(), &Exec{Command: []string{"python3", "-c", get, other.URL}, Env: map[string]string{"PATH": os.Getenv("PATH")}}, &out, &errb)
	if res.Outcome != OutcomeDenied || strings.Contains(out.String(), "secret") {
		t.Fatalf("denied: %+v %q", res, out.String())
	}
	if ev := sess.Egress(); len(ev) != 2 || !ev[0].Allowed || ev[1].Allowed {
		t.Errorf("egress log %+v", ev)
	}
	if !strings.Contains(strings.Join(sess.Isolation().Notes, ";"), "over vsock") {
		t.Errorf("notes %v", sess.Isolation().Notes)
	}
}

func TestFirecrackerWarmPool(t *testing.T) {
	s, ref := fakeVMs(t, func(c *FirecrackerConfig) { c.WarmPool = 1 })
	defer s.Close()
	first, err := s.Open(context.Background(), &Spec{Image: ref})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	d := s.drivers[0].(*firecrackerDriver)
	img, _ := s.images.get(context.Background(), ref)
	deadline := time.Now().Add(20 * time.Second)
	for {
		d.mu.Lock()
		ready := len(d.pools[d.poolKey(img)])
		d.mu.Unlock()
		if ready == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the warm pool was never filled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	second, err := s.Open(context.Background(), &Spec{Image: ref, Files: map[string][]byte{"x": []byte("warm")}})
	if err != nil {
		t.Fatal(err)
	}
	vm := second.b.(*fcVM)
	if vm.ownDir == "" {
		t.Error("the second sandbox did not come from the warm pool")
	}
	var out bytes.Buffer
	second.Exec(context.Background(), &Exec{Command: []string{"cat", "x"}}, &out, &out)
	if out.String() != "warm" {
		t.Errorf("warm VM: %q", out.String())
	}
	second.Close()
	if _, err := os.Stat(vm.ownDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the warm VM's directory was left behind: %v", err)
	}
}

func TestDialVsockHandshake(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "v.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 64)
			n, _ := c.Read(buf)
			if string(buf[:n]) == "CONNECT 1024\n" {
				c.Write([]byte("OK 5\nhello"))
			} else {
				c.Write([]byte("NO\n"))
			}
			c.Close()
		}
	}()
	c, err := dialVsock(context.Background(), sock, 1024)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if n, _ := c.Read(buf); string(buf[:n]) != "hello" {
		t.Errorf("data after the handshake: %q", buf[:n])
	}
	if _, err := dialVsock(context.Background(), sock, 7); err == nil {
		t.Error("a refused port connected")
	}
}

// The jailer path: a fake jailer checks the arguments and the prepared
// chroot, then runs the fake firecracker inside it (as the real one would,
// after chroot and dropping privileges).
func TestFirecrackerUnderTheJailer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the jailer needs root")
	}
	var jailer string
	s, ref := fakeVMs(t, func(c *FirecrackerConfig) {
		jailer = filepath.Join(t.TempDir(), "jailer")
		script := `#!/bin/sh
set -e
while [ $# -gt 0 ]; do
  case "$1" in
    --id) id=$2; shift 2;;
    --exec-file) exe=$2; shift 2;;
    --uid) uid=$2; shift 2;;
    --gid) gid=$2; shift 2;;
    --chroot-base-dir) base=$2; shift 2;;
    --) shift; break;;
    *) echo "unexpected $1" >&2; exit 2;;
  esac
done
root="$base/$(basename "$exe")/$id/root"
[ "$1" = "--api-sock" ] && [ "$2" = "/api.sock" ] || { echo "bad args $*" >&2; exit 2; }
[ -f "$root/rootfs.ext4" ] && [ -f "$root/vmlinux" ] || { echo "chroot not prepared" >&2; exit 2; }
[ "$(stat -c %u "$root/rootfs.ext4")" = "$uid" ] || { echo "not chowned" >&2; exit 2; }
cd "$root" && exec "$exe" --api-sock api.sock
`
		if err := os.WriteFile(jailer, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		c.Jailer, c.JailerUID, c.JailerGID = jailer, 4242, 4242
	})
	sess, err := s.Open(context.Background(), &Spec{Image: ref})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	vm := sess.b.(*fcVM)
	if !strings.Contains(vm.dir, "/jail/firecracker/evalsi-") || !strings.HasSuffix(vm.dir, "/root") {
		t.Errorf("chroot %s", vm.dir)
	}
	if !strings.Contains(strings.Join(sess.Isolation().Notes, ";"), "jailer") {
		t.Errorf("notes %v", sess.Isolation().Notes)
	}
	var out bytes.Buffer
	if res := sess.Exec(context.Background(), &Exec{Command: []string{"true"}}, &out, &out); res.ExitCode != 0 {
		t.Fatalf("exec under the jailer: %+v %s", res, out.String())
	}
}
