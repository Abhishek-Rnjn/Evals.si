package sandbox

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

// bwrapDriver is the namespaced rung: bubblewrap with every namespace
// unshared. Unlike the deepseek-harness profile, which binds the host root
// read-only, the command sees only system directories (or a configured
// rootfs), its workspace, a private /tmp, /proc and a minimal /dev.
type bwrapDriver struct{ cfg *Config }

func (d *bwrapDriver) name() string { return "bwrap" }
func (d *bwrapDriver) level() Level { return LevelNamespaced }

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

func (d *bwrapDriver) spec(req *Request, ws string) (*launchSpec, Isolation, error) {
	bin, err := d.binary()
	if err != nil {
		return nil, Isolation{}, err
	}
	iso := Isolation{Driver: d.name(), Level: d.level().String(), Enforcement: "full"}
	args := []string{bin,
		"--unshare-all", "--unshare-user", "--uid", "65534", "--gid", "65534",
		"--die-with-parent", "--new-session", "--cap-drop", "ALL",
	}
	if req.Network == "allow" {
		args = append(args, "--share-net")
		iso.Notes = append(iso.Notes, "network: shared with the host")
	}
	if d.cfg.Rootfs != "" {
		args = append(args, "--ro-bind", d.cfg.Rootfs, "/")
		iso.Notes = append(iso.Notes, "root: "+d.cfg.Rootfs)
	} else {
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
		iso.Notes = append(iso.Notes, "root: host system directories, read-only")
	}
	for _, p := range absPaths(d.cfg.ReadOnlyPaths) {
		args = append(args, "--ro-bind-try", p, p)
	}
	bind := "--bind"
	if req.Mode == ModeReadOnly {
		bind = "--ro-bind"
	}
	args = append(args,
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		bind, ws, workspaceInside, "--chdir", workspaceInside,
		"--seccomp", "3", "--")
	args = append(args, req.Command...)

	vars := env(req, workspaceInside)
	for i, v := range vars {
		if len(v) > 7 && v[:7] == "TMPDIR=" {
			vars[i] = "TMPDIR=/tmp"
		}
	}
	spec := &launchSpec{Argv: args, Env: vars, Dir: "/", Seccomp: "fd"}
	limits(req, spec)
	iso.Notes = append(iso.Notes, "limits: rlimits (memory, processes, file size), no cgroup")
	return spec, iso, nil
}
