package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
)

func TestAKSFollowupFirecracker(t *testing.T) {
	if os.Getenv("EVALSI_AKS_FOLLOWUP_KVM") != "1" {
		t.Skip("explicit privileged disposable-pod opt-in required")
	}
	assets := os.Getenv("EVALSI_FOLLOWUP_KVM_ASSETS")
	if assets == "" {
		t.Fatal("EVALSI_FOLLOWUP_KVM_ASSETS is required")
	}
	dir, err := os.MkdirTemp("/work", "fc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	config := func(name string, parallel int, jailer bool) sandbox.Config {
		fc := &sandbox.FirecrackerConfig{
			Binary:       filepath.Join(assets, "firecracker"),
			Kernel:       filepath.Join(assets, "vmlinux"),
			Guest:        "/work/build/evalsi-guest",
			DefaultImage: "docker.io/library/alpine@sha256:3e9b4b680bfc9fb5269227cffbd6d42be39fbf7c0b908123913864aa4447e764",
			MemoryMB:     128, DiskMB: 64, VCPUs: 1, MaxParallelBoots: parallel,
		}
		if jailer {
			fc.Jailer, fc.JailerUID, fc.JailerGID = filepath.Join(assets, "jailer"), 4242, 4242
		}
		return sandbox.Config{
			Ladder: []string{"firecracker"}, MinIsolation: "vm", MaxSandboxes: 32,
			CacheDir: filepath.Join(dir, "cache"), WorkDir: filepath.Join(dir, name),
			Firecracker: fc,
		}
	}
	check := func(ctx context.Context, session *sandbox.Session) error {
		var stdout, stderr bytes.Buffer
		result := session.Exec(ctx, &sandbox.Exec{Command: []string{"cat", "input"}}, &stdout, &stderr)
		if result.Outcome != sandbox.OutcomeExit || result.ExitCode != 0 || stdout.String() != "original" {
			return fmt.Errorf("guest command: %+v stdout=%q stderr=%q", result, stdout.String(), stderr.String())
		}
		return nil
	}
	for _, parallel := range []int{2, 8, 16} {
		t.Run(fmt.Sprintf("parallel_%d", parallel), func(t *testing.T) {
			sb, err := sandbox.New(config(fmt.Sprintf("p%d", parallel), parallel, false))
			if err != nil {
				t.Fatal(err)
			}
			manager := sandbox.NewManager(sb)
			defer manager.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
			defer cancel()
			start := time.Now()
			var group sync.WaitGroup
			for range 16 {
				group.Go(func() {
					session, err := manager.Create(ctx, &sandbox.Spec{
						MinIsolation: "vm", Files: map[string][]byte{"input": []byte("original")},
					})
					if err != nil {
						t.Error(err)
						return
					}
					if err := check(ctx, session); err != nil {
						t.Error(err)
					}
					if err := manager.Destroy(session.ID); err != nil {
						t.Error(err)
					}
				})
			}
			group.Wait()
			t.Logf("requested_concurrent_sandboxes=16 max_parallel_boots=%d elapsed_seconds=%.3f", parallel, time.Since(start).Seconds())
		})
	}
	t.Run("jailer_snapshot_source_integrity", func(t *testing.T) {
		fingerprint := func(path string) string {
			t.Helper()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				t.Fatal("Linux inode ownership is required")
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%d:%d:%x", stat.Uid, stat.Gid, sha256.Sum256(body))
		}
		kernel := filepath.Join(assets, "vmlinux")
		before := fingerprint(kernel)
		sb, err := sandbox.New(config("jail", 2, true))
		if err != nil {
			t.Fatal(err)
		}
		manager := sandbox.NewManager(sb)
		defer manager.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		session, err := manager.Create(ctx, &sandbox.Spec{MinIsolation: "vm", Files: map[string][]byte{"input": []byte("original")}})
		if err != nil {
			t.Fatal(err)
		}
		if err := check(ctx, session); err != nil {
			t.Fatal(err)
		}
		snapshot, err := manager.Snapshot(ctx, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		sources, err := filepath.Glob(filepath.Join(dir, "jail", "evalsi-snap-*", "state", "*"))
		if err != nil || len(sources) != 3 {
			t.Fatalf("expected disk, memory and VM-state snapshot sources: count=%d err=%v", len(sources), err)
		}
		originals := map[string]string{kernel: before}
		for _, path := range sources {
			originals[path] = fingerprint(path)
		}
		clone, err := manager.Restore(ctx, snapshot, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := check(ctx, clone); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Snapshot(ctx, clone.ID); err != nil {
			t.Fatal(err)
		}
		for path, original := range originals {
			if after := fingerprint(path); after != original {
				t.Fatalf("source %s owner/checksum changed: before=%s after=%s", filepath.Base(path), original, after)
			}
		}
		t.Logf("real jailed guest, restore and resnapshot passed; unchanged_kernel_disk_memory_state_sources=%d source_kernel_owner_and_sha256=%s", len(originals), before)
	})
}
