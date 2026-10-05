package sandbox

import (
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
)

// bwrapDriver is the namespaced rung: bubblewrap with every namespace
// unshared. Unlike the deepseek-harness profile, which binds the host root
// read-only, the command sees only an image root (or the host's system
// directories), its workdir, a private /tmp, /proc and a minimal /dev.
type bwrapDriver struct{ cfg *Config }

// Inside the sandbox, the egress forwarder and its socket live on the
// private /tmp tmpfs, so even a read-only image root needs no mount points.
const (
	insideHelperDir = "/tmp/.evalsi"
	insideLauncher  = insideHelperDir + "/evalsid"
	insideSocket    = insideHelperDir + "/egress.sock"
	forwardAddr     = "127.0.0.1:3128"
)

func (d *bwrapDriver) name() string         { return "bwrap" }
func (d *bwrapDriver) level() Level         { return LevelNamespaced }
func (d *bwrapDriver) proxyNetwork() string { return "unix" }

func (d *bwrapDriver) binary() (string, error) {
	if d.cfg.BwrapPath != "" {
		return d.cfg.BwrapPath, nil
	}
	return exec.LookPath("bwrap")
}

func (d *bwrapDriver) available() error {
	if runtime.GOOS != "linux" {
		return errors.New("bubblewrap needs Linux")
	}
	if _, err := d.binary(); err != nil {
		return fmt.Errorf("bubblewrap not found: %w", err)
	}
	return nil
}

func (d *bwrapDriver) supports(sp *Spec) error {
	if sp.Network.Mode == NetworkAllowlist && sp.Image != "" && !staticBinary(d.cfg.Launcher) {
		// The forwarder is evalsid itself, run inside the image's root.
		return errors.New("an allowlist with an image root needs a statically linked evalsid (build with CGO_ENABLED=0)")
	}
	return nil
}

func (d *bwrapDriver) isolation(b *hostBackend) Isolation {
	iso := Isolation{Driver: d.name(), Level: d.level().String(), Enforcement: "full"}
	switch {
	case b.root != "":
		iso.Notes = append(iso.Notes, "root: image "+b.image.Digest+", private writable copy")
	case b.roRoot != "":
		iso.Notes = append(iso.Notes, "root: image "+b.image.Digest+", read-only")
	case d.cfg.Rootfs != "":
		iso.Notes = append(iso.Notes, "root: "+d.cfg.Rootfs)
	default:
		iso.Notes = append(iso.Notes, "root: host system directories, read-only")
	}
	switch b.sp.Network.Mode {
	case NetworkAllow:
		iso.Notes = append(iso.Notes, "network: shared with the host")
	case NetworkAllowlist:
		iso.Notes = append(iso.Notes, "network: allowlist through the logging egress proxy")
	}
	iso.Notes = append(iso.Notes, "limits: rlimits (memory, processes, file size), no cgroup")
	return iso
}

func (d *bwrapDriver) launch(b *hostBackend, e *Exec) (*launchSpec, error) {
	bin, err := d.binary()
	if err != nil {
		return nil, err
	}
	sp := b.sp
	args := []string{bin,
		"--unshare-all", "--unshare-user", "--uid", "65534", "--gid", "65534",
		"--die-with-parent", "--new-session", "--cap-drop", "ALL",
	}
	if sp.Network.Mode == NetworkAllow {
		args = append(args, "--share-net")
	}
	writable := sp.Mode != ModeReadOnly
	switch {
	case b.root != "":
		bind := "--bind"
		if !writable {
			bind = "--ro-bind"
		}
		args = append(args, bind, b.root, "/")
	case b.roRoot != "":
		// Each top-level entry separately, over bwrap's own tmpfs root, so
		// the workdir mount point can be created even when the image has none.
		entries, err := os.ReadDir(b.roRoot)
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			p := filepath.Join(b.roRoot, ent.Name())
			switch {
			case slices.Contains([]string{"proc", "dev", "tmp", "sys"}, ent.Name()):
			case ent.Type()&os.ModeSymlink != 0:
				if target, err := os.Readlink(p); err == nil {
					args = append(args, "--symlink", target, "/"+ent.Name())
				}
			default:
				args = append(args, "--ro-bind", p, "/"+ent.Name())
			}
		}
	case d.cfg.Rootfs != "":
		args = append(args, "--ro-bind", d.cfg.Rootfs, "/")
	default:
		for _, dir := range systemDirs {
			if target, ok := isSymlink(dir); ok {
				args = append(args, "--symlink", target, dir)
			} else if exists(dir) {
				args = append(args, "--ro-bind", dir, dir)
			}
		}
		for _, f := range systemFiles {
			args = append(args, "--ro-bind-try", f, f)
		}
		for _, p := range absPaths(d.cfg.ReadOnlyPaths) {
			args = append(args, "--ro-bind-try", p, p)
		}
	}
	if sp.Network.Mode == NetworkAllow && b.image != nil {
		args = append(args, "--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf")
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	if b.root == "" {
		bind := "--bind"
		if !writable {
			bind = "--ro-bind"
		}
		args = append(args, bind, b.work, sp.Workdir)
	}
	cwd := sp.Workdir
	if e.Cwd != "" {
		cwd = path.Join(sp.Workdir, e.Cwd)
	}
	env := b.env(e, sp.Workdir, "/tmp")
	spec := &launchSpec{Dir: "/", Seccomp: "fd"}
	command := e.Command
	if b.proxy != nil {
		// The forwarder is a Go program and must not run under the command's
		// memory limits: it starts the command through the launcher, which
		// applies them to the command alone.
		inner := &launchSpec{Argv: e.Command, Env: env, Dir: cwd, LookPath: true}
		limits(&sp.Resources, inner)
		raw, err := json.Marshal(inner)
		if err != nil {
			return nil, err
		}
		args = append(args, "--ro-bind", d.cfg.Launcher, insideLauncher, "--bind", b.proxy.Addr(), insideSocket)
		command = []string{insideLauncher, "sandbox-forward", "--listen", forwardAddr, "--socket", insideSocket, "--", insideLauncher, "sandbox-exec"}
		env = []string{specEnv + "=" + string(raw)}
		spec.MaxProcs = inner.MaxProcs
	} else {
		limits(&sp.Resources, spec)
	}
	args = append(args, "--chdir", cwd, "--seccomp", "3", "--")
	spec.Argv, spec.Env = append(args, command...), env
	return spec, nil
}

// staticBinary reports whether an ELF file has no program interpreter, so it
// runs in any root filesystem.
func staticBinary(p string) bool {
	f, err := elf.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return false
		}
	}
	return true
}
