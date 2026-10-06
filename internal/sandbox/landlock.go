package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"

	llsys "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// landlockDriver is the confined rung: the command shares the host's mount,
// PID and network namespaces, but Landlock limits it to reading system
// directories and its workspace and writing only the workspace, and seccomp
// blocks IP sockets when the network is denied.
type landlockDriver struct{ cfg *Config }

func (d *landlockDriver) name() string { return "landlock" }
func (d *landlockDriver) level() Level { return LevelConfined }

func abiVersion() (int, error) {
	if runtime.GOOS != "linux" {
		return 0, errors.New("landlock needs Linux")
	}
	v, err := llsys.LandlockGetABIVersion()
	if err != nil {
		return 0, fmt.Errorf("landlock is not enabled in this kernel: %w", err)
	}
	return v, nil
}

func (d *landlockDriver) available() error {
	_, err := abiVersion()
	return err
}

func (d *landlockDriver) proxyNetwork() string { return "tcp" }

func (d *landlockDriver) supports(sp *Spec) error {
	if sp.Image != "" {
		return errors.New("landlock shares the host's filesystem and cannot use an image root")
	}
	if sp.Network.Mode == NetworkAllowlist {
		if abi, err := abiVersion(); err == nil && abi < 4 {
			return fmt.Errorf("an allowlist needs Landlock TCP rules (ABI 4, Linux 6.7); this kernel has ABI %d", abi)
		}
	}
	return nil
}

func (d *landlockDriver) isolation(b *hostBackend) Isolation {
	abi, _ := abiVersion()
	iso := Isolation{
		Driver: d.name(), Level: d.level().String(), Enforcement: "full",
		Notes: []string{
			fmt.Sprintf("landlock ABI %d", abi),
			"shares the host's PID and mount namespaces",
			"workdir: " + b.work + " (paths cannot be remapped; see EVALSI_WORKDIR)",
		},
	}
	if c := b.sb.cgroups; c != nil {
		iso.Notes = append(iso.Notes, "limits: rlimits (memory, file size); "+c.note())
		if !c.has("pids") {
			iso.Notes = append(iso.Notes, "no process cap (no pids controller)")
		}
	} else {
		iso.Notes = append(iso.Notes, "limits: rlimits (memory, file size), no cgroup or process cap")
	}
	if abi < 3 {
		// Before ABI 3 Landlock cannot stop truncation of readable files.
		iso.Enforcement = "partial"
		iso.Notes = append(iso.Notes, "file truncation is not restricted (ABI < 3)")
	}
	switch b.sp.Network.Mode {
	case NetworkAllow:
		iso.Notes = append(iso.Notes, "network: allowed")
	case NetworkAllowlist:
		// Landlock rules name ports, not hosts: the proxy's port on any host is reachable.
		iso.Enforcement = "partial"
		iso.Notes = append(iso.Notes, "network: allowlist through the egress proxy; TCP connect is limited to the proxy's port, not its host")
	}
	return iso
}

func (d *landlockDriver) launch(b *hostBackend, e *Exec) (*launchSpec, error) {
	sp := b.sp
	rules := &landlockRules{
		ReadDirs:   append(append([]string{}, systemDirs...), "/proc"),
		ReadFiles:  append(append([]string{}, systemFiles...), "/dev/urandom", "/dev/random", "/dev/zero"),
		WriteFiles: []string{"/dev/null"},
		WriteDirs:  []string{b.tmp},
	}
	rules.ReadDirs = append(rules.ReadDirs, absPaths(d.cfg.ReadOnlyPaths)...)
	if sp.Mode == ModeReadOnly {
		rules.ReadDirs = append(rules.ReadDirs, b.work)
	} else {
		rules.WriteDirs = append(rules.WriteDirs, b.work)
	}
	denyInet := true
	switch sp.Network.Mode {
	case NetworkAllow:
		denyInet = false
	case NetworkAllowlist:
		denyInet = false
		rules.DenyTCP = true
		rules.ConnectPorts = []uint16{b.proxy.Port()}
	default:
		rules.DenyTCP = true
	}
	dir := b.work
	if e.Cwd != "" {
		dir = filepath.Join(b.work, filepath.FromSlash(e.Cwd))
	}
	spec := &launchSpec{
		Argv: e.Command, Env: b.env(e, b.work, b.tmp), Dir: dir, LookPath: true,
		Landlock: rules, Seccomp: "apply", DenyInet: denyInet,
	}
	limits(&sp.Resources, spec)
	// RLIMIT_NPROC counts every process of the (shared) uid here, so it would
	// fail unrelated forks; the process cap is the cgroup's pids.max, if any.
	spec.NoNprocRlimit = true
	return spec, nil
}
