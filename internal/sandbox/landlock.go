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

func (d *landlockDriver) spec(req *Request, ws string) (*launchSpec, Isolation, error) {
	abi, err := abiVersion()
	if err != nil {
		return nil, Isolation{}, err
	}
	iso := Isolation{
		Driver: d.name(), Level: d.level().String(), Enforcement: "full",
		Notes: []string{
			fmt.Sprintf("landlock ABI %d", abi),
			"shares the host's PID and mount namespaces",
			"limits: rlimits (memory, file size), no cgroup or process cap",
		},
	}
	if abi < 3 {
		// Before ABI 3 Landlock cannot stop truncation of readable files.
		iso.Enforcement = "partial"
		iso.Notes = append(iso.Notes, "file truncation is not restricted (ABI < 3)")
	}
	rules := &landlockRules{
		ReadDirs:   append(append([]string{}, systemDirs...), "/proc"),
		ReadFiles:  append(append([]string{}, systemFiles...), "/dev/urandom", "/dev/random", "/dev/zero"),
		WriteFiles: []string{"/dev/null"},
	}
	rules.ReadDirs = append(rules.ReadDirs, absPaths(d.cfg.ReadOnlyPaths)...)
	if req.Mode == ModeReadOnly {
		rules.ReadDirs = append(rules.ReadDirs, ws)
		rules.WriteDirs = []string{filepath.Join(ws, ".tmp")}
	} else {
		rules.WriteDirs = []string{ws}
	}
	deny := req.Network != "allow"
	rules.DenyTCP = deny
	if !deny {
		iso.Notes = append(iso.Notes, "network: allowed")
	}
	spec := &launchSpec{
		Argv: req.Command, Env: env(req, ws), Dir: ws, LookPath: true,
		Landlock: rules, Seccomp: "apply", DenyInet: deny,
	}
	limits(req, spec)
	// RLIMIT_NPROC counts every process of the (shared) uid here, so it would
	// fail unrelated forks; only bwrap's user namespace gives a private count.
	spec.MaxProcs = 0
	return spec, iso, nil
}
