//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

// Exec is `evalsid sandbox-exec`: it applies resource limits, Landlock and
// seccomp to itself, then replaces itself with the target. It only returns
// on failure, with the launcher's exit code.
func Exec(stderr io.Writer) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, launcherPrefix+format+"\n", args...)
		return exitLauncherFailure
	}
	var spec launchSpec
	if err := json.Unmarshal([]byte(os.Getenv(specEnv)), &spec); err != nil {
		return fail("bad launch spec: %v", err)
	}
	if len(spec.Argv) == 0 {
		return fail("empty command")
	}
	if spec.Landlock != nil {
		if err := applyLandlock(spec.Landlock); err != nil {
			return fail("landlock: %v", err)
		}
	}
	if spec.Seccomp != "" {
		prog, err := seccompProgram(spec.DenyInet)
		if err != nil {
			return fail("seccomp: %v", err)
		}
		switch spec.Seccomp {
		case "apply":
			err = loadSeccomp(prog)
		case "fd":
			err = seccompFD(prog, 3)
		}
		if err != nil {
			return fail("seccomp: %v", err)
		}
	}
	if err := os.Chdir(spec.Dir); err != nil {
		return fail("workspace: %v", err)
	}
	path := spec.Argv[0]
	if spec.LookPath && !strings.Contains(path, "/") {
		found, err := lookPath(path, spec.Env)
		if err != nil {
			fmt.Fprintf(stderr, "sandbox: command not found: %s\n", path)
			return exitNotFound
		}
		path = found
	}
	err := execWithLimits(&spec, path)
	if errors.Is(err, unix.ENOENT) {
		fmt.Fprintf(stderr, "sandbox: command not found: %s\n", path)
		return exitNotFound
	}
	if errors.Is(err, unix.EACCES) {
		// Landlock denied executing it: the command's problem, not the runner's.
		fmt.Fprintf(stderr, "sandbox: %s: Permission denied\n", path)
		return 126
	}
	return fail("exec %s: %v", path, err)
}

func lookPath(name string, env []string) (string, error) {
	dirs := sandboxPath
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			dirs = v
		}
	}
	for _, dir := range filepath.SplitList(dirs) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

// execWithLimits sets the resource limits as the very last step and then
// calls execve directly: once the address-space limit is lower than what the
// Go runtime has mapped, any further allocation in this process would crash it.
func execWithLimits(spec *launchSpec, path string) error {
	argv0, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	argv, err := cStrings(spec.Argv)
	if err != nil {
		return err
	}
	envv, err := cStrings(spec.Env)
	if err != nil {
		return err
	}
	if err := setLimits(spec); err != nil {
		return fmt.Errorf("resource limits: %w", err)
	}
	_, _, errno := unix.RawSyscall(unix.SYS_EXECVE,
		uintptr(unsafe.Pointer(argv0)), uintptr(unsafe.Pointer(&argv[0])), uintptr(unsafe.Pointer(&envv[0])))
	return errno
}

// cStrings is a NULL-terminated array of C strings for execve.
func cStrings(ss []string) ([]*byte, error) {
	out := make([]*byte, len(ss)+1)
	for i, s := range ss {
		p, err := unix.BytePtrFromString(s)
		if err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

func setLimits(spec *launchSpec) error {
	set := func(res int, v uint64) error {
		if v == 0 {
			return nil
		}
		return unix.Setrlimit(res, &unix.Rlimit{Cur: v, Max: v})
	}
	for _, l := range []struct {
		res int
		v   uint64
	}{
		{unix.RLIMIT_AS, spec.MemoryBytes},
		{unix.RLIMIT_CPU, spec.CPUSeconds},
		{unix.RLIMIT_NPROC, spec.MaxProcs},
		{unix.RLIMIT_FSIZE, spec.MaxFile},
	} {
		if err := set(l.res, l.v); err != nil {
			return err
		}
	}
	// No core dumps: they would land in the workspace and could hold secrets.
	return unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{})
}

func applyLandlock(r *landlockRules) error {
	rules := []landlock.Rule{
		landlock.RODirs(r.ReadDirs...).IgnoreIfMissing(),
		landlock.ROFiles(r.ReadFiles...).IgnoreIfMissing(),
		landlock.RWDirs(r.WriteDirs...),
		landlock.RWFiles(r.WriteFiles...).IgnoreIfMissing(),
	}
	cfg := landlock.V7.BestEffort()
	if r.DenyTCP {
		// Every network right is handled and none is granted.
		return cfg.Restrict(rules...)
	}
	if err := cfg.RestrictPaths(rules...); err != nil {
		return err
	}
	return cfg.RestrictScoped()
}

func loadSeccomp(prog []unix.SockFilter) error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog)))
	if errno != 0 {
		return errno
	}
	return nil
}

// seccompFD writes the program to a memfd on descriptor fd for bwrap --seccomp.
func seccompFD(prog []unix.SockFilter, fd int) error {
	mfd, err := unix.MemfdCreate("evalsi-seccomp", 0)
	if err != nil {
		return err
	}
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&prog[0])), len(prog)*int(unsafe.Sizeof(prog[0])))
	if _, err := unix.Write(mfd, buf); err != nil {
		return err
	}
	if _, err := unix.Seek(mfd, 0, 0); err != nil {
		return err
	}
	if mfd != fd {
		if err := unix.Dup3(mfd, fd, 0); err != nil {
			return err
		}
		unix.Close(mfd)
	}
	return nil
}
