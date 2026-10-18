//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFirecrackerRealKVM is deliberately separate from the fakefc contract
// tests. Explicit opt-in makes absent hardware/configuration a failure, not a
// successful skip; the only configured driver is Firecracker.
func TestFirecrackerRealKVM(t *testing.T) {
	if os.Getenv("EVALSI_TEST_FIRECRACKER") != "1" {
		t.Skip("set EVALSI_TEST_FIRECRACKER=1 on a real KVM host; see tests/verification/README.md")
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("real KVM required: %v", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		t.Fatalf("/dev/kvm must be a character device: %v", err)
	}
	api, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0xae00, 0) // KVM_GET_API_VERSION
	if errno != 0 || api != 12 {
		t.Fatalf("KVM API access failed: version=%d errno=%v", api, errno)
	}
	kernel, image := os.Getenv("EVALSI_FIRECRACKER_KERNEL"), os.Getenv("EVALSI_FIRECRACKER_IMAGE")
	if kernel == "" || !strings.Contains(image, "@sha256:") {
		t.Fatal("set EVALSI_FIRECRACKER_KERNEL and a digest-pinned EVALSI_FIRECRACKER_IMAGE containing sh/cat/sleep/true")
	}
	binary := os.Getenv("EVALSI_FIRECRACKER_BINARY")
	if binary == "" {
		binary = "firecracker"
	}
	version, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil || !strings.HasPrefix(string(version), "Firecracker v") {
		t.Fatalf("real Firecracker binary required: %s %v", version, err)
	}
	// A short parent path also keeps Firecracker's Unix sockets below sun_path.
	dir, err := os.MkdirTemp("", "evalsi-kvm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	guest := filepath.Join(dir, "evalsi-guest")
	build := exec.Command("go", "build", "-o", guest, "../../cmd/evalsi-guest")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real guest: %v\n%s", err, out)
	}
	hostOnly := filepath.Join(dir, "host-only")
	if err := os.WriteFile(hostOnly, []byte("host sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	for _, warm := range []int{0, 1} {
		t.Run(fmt.Sprintf("warm_pool_%d", warm), func(t *testing.T) {
			work := filepath.Join(dir, fmt.Sprintf("work-%d", warm))
			if err := os.MkdirAll(work, 0o700); err != nil {
				t.Fatal(err)
			}
			sb, err := New(Config{Ladder: []string{"firecracker"}, MinIsolation: "vm",
				CacheDir: filepath.Join(dir, "cache"), WorkDir: work,
				Firecracker: &FirecrackerConfig{Binary: binary, Kernel: kernel, Guest: guest,
					DefaultImage: image, WarmPool: warm, MemoryMB: 256}})
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(sb)
			defer m.Close()
			if probe := sb.Probe(ctx); len(probe) != 1 || !probe[0].Available || probe[0].Driver != "firecracker" {
				t.Fatalf("real boot probe: %+v", probe)
			}
			if warm > 0 {
				d := sb.drivers[0].(*firecrackerDriver)
				img, err := sb.images.get(ctx, image)
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(30 * time.Second)
				for {
					d.mu.Lock()
					ready := len(d.pools[d.poolKey(img)]) == warm
					d.mu.Unlock()
					if ready {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("real warm pool did not become ready")
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			started := time.Now()
			sess, err := m.Create(ctx, &Spec{MinIsolation: "vm", Network: Network{Mode: NetworkDeny},
				Files: map[string][]byte{"input": []byte("original")}})
			if err != nil {
				t.Fatal(err)
			}
			if warm > 0 && sess.b.(*fcVM).ownDir == "" {
				t.Fatal("sandbox did not consume a real prebooted VM")
			}
			iso := sess.Isolation()
			if iso.Driver != "firecracker" || iso.Level != "vm" || iso.Enforcement != "full" {
				t.Fatalf("unexpected isolation: %+v", iso)
			}
			run := func(session *Session, command []string, want string, code int) {
				t.Helper()
				var stdout, stderr bytes.Buffer
				result := session.Exec(ctx, &Exec{Command: command}, &stdout, &stderr)
				if result.Outcome != OutcomeExit || result.ExitCode != code || stdout.String() != want {
					t.Fatalf("execution: %+v stdout=%q stderr=%q", result, stdout.String(), stderr.String())
				}
			}
			run(sess, []string{"sh", "-c", "test ! -e \"$1\" && cat input", "sh", hostOnly}, "original", 0)
			run(sess, []string{"sh", "-c", "exit 7"}, "", 7)
			snap, err := m.Snapshot(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.WriteFiles([]File{{Path: "input", Content: []byte("changed")}}); err != nil {
				t.Fatal(err)
			}
			clone, err := m.Restore(ctx, snap, nil)
			if err != nil {
				t.Fatal(err)
			}
			run(clone, []string{"cat", "input"}, "original", 0)
			var output bytes.Buffer
			result := clone.Exec(ctx, &Exec{Command: []string{"sleep", "30"}, Timeout: 100 * time.Millisecond}, &output, &output)
			if result.Outcome != OutcomeTimeout {
				t.Fatalf("timeout not enforced: %+v", result)
			}
			t.Logf("real KVM controls passed: warm_pool=%d elapsed=%s version=%s", warm, time.Since(started), strings.TrimSpace(string(version)))
		})
	}
}
