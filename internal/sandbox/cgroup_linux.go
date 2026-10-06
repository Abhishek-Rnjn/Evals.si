//go:build linux

package sandbox

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const cgroupMount = "/sys/fs/cgroup"

// cgroupPool is a delegated cgroup v2 directory under which each execution
// of the bubblewrap and Landlock rungs gets its own child: memory.max and
// pids.max cover every process the command starts (rlimits are per process),
// cgroup.kill reaches processes that left the process group, and
// memory.events tells an out-of-memory kill from a crash.
type cgroupPool struct {
	dir         string
	controllers []string
	cpus        float64
}

// setupCgroups resolves sandbox.cgroup:
//   - "off": none;
//   - "" or "auto": this process's own cgroup, if it is delegated to us
//     (writable) and holds no other process: evalsid moves itself into a
//     "supervisor" child, as cgroup v2 allows processes only in leaves of a
//     subtree with controllers (a systemd unit with Delegate=yes, or a
//     container with a writable cgroup mount, gives exactly this);
//   - a path: that directory, which must already be delegated (a systemd
//     slice with Delegate=yes, or a directory made for evalsid as root).
//
// In auto mode a missing or read-only cgroup is not an error: the rungs
// fall back to rlimits and say so in their isolation report.
func setupCgroups(mode string, cpus float64) (*cgroupPool, error) {
	switch mode {
	case "off":
		return nil, nil
	case "", "auto":
		own, err := ownCgroup()
		if err != nil {
			return nil, nil
		}
		dir := filepath.Join(cgroupMount, own)
		if unix.Access(filepath.Join(dir, "cgroup.subtree_control"), unix.W_OK) != nil ||
			unix.Access(filepath.Join(dir, "cgroup.procs"), unix.W_OK) != nil {
			return nil, nil
		}
		if err := leaveRoot(dir); err != nil {
			return nil, nil
		}
		p, err := newCgroupPool(dir, cpus)
		if err != nil {
			return nil, nil
		}
		return p, nil
	default:
		if !filepath.IsAbs(mode) {
			return nil, fmt.Errorf("sandbox.cgroup: %q is not off, auto or an absolute path", mode)
		}
		if err := isCgroup2(mode); err != nil {
			return nil, fmt.Errorf("sandbox.cgroup: %w", err)
		}
		p, err := newCgroupPool(mode, cpus)
		if err != nil {
			return nil, fmt.Errorf("sandbox.cgroup: %w", err)
		}
		return p, nil
	}
}

// ownCgroup is this process's cgroup v2 path, relative to the mount.
func ownCgroup() (string, error) {
	if err := isCgroup2(cgroupMount); err != nil {
		return "", err
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		if p, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return p, nil
		}
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
}

func isCgroup2(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return err
	}
	if st.Type != unix.CGROUP2_SUPER_MAGIC {
		return fmt.Errorf("%s is not a cgroup v2 file system", dir)
	}
	return nil
}

// leaveRoot moves this process into dir/supervisor, so dir can enable
// controllers for its children. Only this process: if dir holds others (a
// shell's session scope), it is not ours to rearrange, and auto mode gives up.
func leaveRoot(dir string) error {
	procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return err
	}
	self := strconv.Itoa(os.Getpid())
	for _, pid := range strings.Fields(string(procs)) {
		if pid != self {
			return fmt.Errorf("%s holds other processes", dir)
		}
	}
	if len(procs) == 0 {
		return nil
	}
	sup := filepath.Join(dir, "supervisor")
	if err := os.Mkdir(sup, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return os.WriteFile(filepath.Join(sup, "cgroup.procs"), []byte(self), 0)
}

func newCgroupPool(dir string, cpus float64) (*cgroupPool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return nil, err
	}
	var enable []string
	for _, c := range []string{"memory", "pids", "cpu"} {
		if slices.Contains(strings.Fields(string(raw)), c) {
			enable = append(enable, "+"+c)
		}
	}
	if len(enable) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(strings.Join(enable, " ")), 0); err != nil {
			return nil, fmt.Errorf("enabling %s controllers in %s: %w", strings.Join(enable, " "), dir, err)
		}
	}
	on, err := os.ReadFile(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return nil, err
	}
	p := &cgroupPool{dir: dir, controllers: strings.Fields(string(on)), cpus: cpus}
	if err := p.sweep(); err != nil {
		return nil, err
	}
	return p, nil
}

// sweep removes children left by an evalsid that did not exit cleanly.
func (p *cgroupPool) sweep() error {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "sandbox-") {
			removeCgroup(filepath.Join(p.dir, e.Name()))
		}
	}
	return nil
}

func (p *cgroupPool) has(controller string) bool { return slices.Contains(p.controllers, controller) }

// note describes what the pool enforces, for isolation reports.
func (p *cgroupPool) note() string {
	var what []string
	if p.has("memory") {
		what = append(what, "memory")
	}
	if p.has("pids") {
		what = append(what, "processes")
	}
	if p.has("cpu") && p.cpus > 0 {
		what = append(what, "cpu "+strconv.FormatFloat(p.cpus, 'f', -1, 64))
	}
	if len(what) == 0 {
		return "cgroup " + p.dir + " (no controllers; kill only)"
	}
	return "cgroup v2 (" + strings.Join(what, ", ") + ") under " + p.dir
}

// cgroupLease is one execution's cgroup.
type cgroupLease struct {
	dir string
	fd  int
}

// lease makes a child for one execution with spec's limits.
func (p *cgroupPool) lease(spec *launchSpec) (*cgroupLease, error) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	dir := filepath.Join(p.dir, "sandbox-"+hex.EncodeToString(b[:]))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, err
	}
	set := func(file, value string) error {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(value), 0); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		return nil
	}
	var err error
	if p.has("memory") && spec.MemoryBytes > 0 {
		err = errors.Join(err, set("memory.max", strconv.FormatUint(spec.MemoryBytes, 10)))
		// Swap would let the command exceed memory.max unnoticed; not every kernel has it.
		if _, statErr := os.Stat(filepath.Join(dir, "memory.swap.max")); statErr == nil {
			err = errors.Join(err, set("memory.swap.max", "0"))
		}
		// Kill the whole command, not one of its processes, when it runs out.
		if _, statErr := os.Stat(filepath.Join(dir, "memory.oom.group")); statErr == nil {
			err = errors.Join(err, set("memory.oom.group", "1"))
		}
	}
	if p.has("pids") && spec.MaxProcs > 0 {
		err = errors.Join(err, set("pids.max", strconv.FormatUint(spec.MaxProcs, 10)))
	}
	if p.has("cpu") && p.cpus > 0 {
		const period = 100000
		err = errors.Join(err, set("cpu.max", fmt.Sprintf("%d %d", int(p.cpus*period), period)))
	}
	if err != nil {
		removeCgroup(dir)
		return nil, err
	}
	fd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		removeCgroup(dir)
		return nil, err
	}
	return &cgroupLease{dir: dir, fd: fd}, nil
}

// kill kills every process in the cgroup.
func (l *cgroupLease) kill() error {
	return os.WriteFile(filepath.Join(l.dir, "cgroup.kill"), []byte("1"), 0)
}

// oomKilled reports whether the kernel killed a process for exceeding memory.max.
func (l *cgroupLease) oomKilled() bool {
	raw, err := os.ReadFile(filepath.Join(l.dir, "memory.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
			n, _ := strconv.Atoi(v)
			return n > 0
		}
	}
	return false
}

// close kills what is left (a daemon the command started) and removes the cgroup.
func (l *cgroupLease) close() {
	unix.Close(l.fd)
	removeCgroup(l.dir)
}

func removeCgroup(dir string) {
	_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0)
	// rmdir fails with EBUSY until the killed processes are reaped.
	for i := 0; i < 50; i++ {
		if err := unix.Rmdir(dir); err == nil || errors.Is(err, unix.ENOENT) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// attach starts cmd directly in the lease's cgroup (clone3 CLONE_INTO_CGROUP),
// so no process of the command ever runs outside it.
func (l *cgroupLease) attach(attr *syscall.SysProcAttr) {
	attr.UseCgroupFD, attr.CgroupFD = true, l.fd
}
