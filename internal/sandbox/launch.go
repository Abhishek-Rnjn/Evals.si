package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// launchSpec tells the sandbox-exec launcher what to set up before it
// executes Argv. It travels in the launcher's environment, which is then
// cleared: the command sees only Env.
type launchSpec struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
	// Resolve Argv[0] against PATH from Env.
	LookPath bool `json:"look_path,omitempty"`

	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
	CPUSeconds  uint64 `json:"cpu_seconds,omitempty"`
	MaxProcs    uint64 `json:"max_procs,omitempty"`
	MaxFile     uint64 `json:"max_file_bytes,omitempty"`
	// MaxProcs is for the cgroup only: RLIMIT_NPROC counts every process
	// of the uid, which the Landlock rung shares with the host.
	NoNprocRlimit bool `json:"no_nproc_rlimit,omitempty"`

	// Landlock rules to apply to the launcher before exec (landlock rung).
	Landlock *landlockRules `json:"landlock,omitempty"`
	// "apply": load the seccomp filter in the launcher before exec.
	// "fd": hand it to bwrap on file descriptor 3 (bwrap loads it for the command).
	Seccomp string `json:"seccomp,omitempty"`
	// Deny AF_INET and AF_INET6 sockets in the seccomp filter.
	DenyInet bool `json:"deny_inet,omitempty"`
}

type landlockRules struct {
	ReadDirs   []string `json:"read_dirs,omitempty"`
	ReadFiles  []string `json:"read_files,omitempty"`
	WriteDirs  []string `json:"write_dirs,omitempty"`
	WriteFiles []string `json:"write_files,omitempty"`
	DenyTCP    bool     `json:"deny_tcp,omitempty"`
	// With DenyTCP, TCP connections to these ports are still allowed (the
	// egress proxy on loopback).
	ConnectPorts []uint16 `json:"connect_ports,omitempty"`
}

const (
	specEnv = "EVALSI_SANDBOX_SPEC"
	// Exit codes of the launcher itself, as in docker run.
	exitLauncherFailure = 125
	exitNotFound        = 127
	launcherPrefix      = "evalsi-sandbox: "
)

func limits(req *Resources, spec *launchSpec) {
	mem := req.MemoryMB
	if mem <= 0 {
		mem = defaultMemoryMB
	}
	procs := req.MaxProcs
	if procs <= 0 {
		procs = defaultMaxProcs
	}
	file := req.MaxFileMB
	if file <= 0 {
		file = defaultMaxFileMB
	}
	spec.MemoryBytes = uint64(mem) << 20
	spec.MaxProcs = uint64(procs)
	spec.MaxFile = uint64(file) << 20
	if req.CPUSeconds > 0 {
		spec.CPUSeconds = uint64(req.CPUSeconds)
	}
}

// launch runs the launcher with spec and waits, enforcing the wall-clock
// timeout on the whole process group.
func (s *Sandbox) launch(ctx context.Context, spec *launchSpec, stdin []byte, timeout time.Duration, stdout, stderr io.Writer) *ExecResult {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	raw, err := json.Marshal(spec)
	if err != nil {
		return &ExecResult{Outcome: OutcomeRunnerFailure, ExitCode: -1, Error: err.Error()}
	}
	cmd := exec.CommandContext(ctx, s.cfg.Launcher, "sandbox-exec")
	cmd.Env = []string{specEnv + "=" + string(raw)}
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var lease *cgroupLease
	if s.cgroups != nil {
		lease, err = s.cgroups.lease(spec)
		if err != nil {
			return &ExecResult{Outcome: OutcomeRunnerFailure, ExitCode: -1, Error: "cgroup: " + err.Error()}
		}
		defer lease.close()
		lease.attach(cmd.SysProcAttr)
	}
	cmd.Cancel = func() error {
		if lease != nil {
			// Every process, including any that left the process group.
			_ = lease.kill()
		}
		// The whole group: the command may have forked.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err = cmd.Run()
	res := &ExecResult{Duration: time.Since(start)}
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			res.CPU = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
			res.MaxRSS = ru.Maxrss * 1024
		}
	}
	var exit *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.Outcome, res.ExitCode = OutcomeTimeout, -1
		res.Error = "timed out after " + timeout.String()
	case err == nil:
		res.Outcome = OutcomeExit
	case errors.As(err, &exit):
		res.Outcome, res.ExitCode = OutcomeExit, exit.ExitCode()
		if st, ok := exit.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			res.ExitCode = 128 + int(st.Signal())
		}
	default:
		res.Outcome, res.ExitCode = OutcomeRunnerFailure, -1
		res.Error = err.Error()
	}
	if lease != nil && lease.oomKilled() {
		res.Denials = append(res.Denials, memoryLimitDenial)
	}
	return res
}

// memoryLimitDenial is reported when the kernel killed the command for
// exceeding its cgroup's memory.max.
const memoryLimitDenial = "memory limit exceeded (killed by the kernel)"

// capped passes the first limit bytes on to w (when set) and notes whether
// more arrived. With keep, it also keeps them, for classification.
type capped struct {
	w         io.Writer
	limit     int
	n         int
	keep      bool
	buf       bytes.Buffer
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit - c.n
	if room <= 0 {
		c.truncated = c.truncated || len(p) > 0
		return len(p), nil
	}
	chunk := p
	if len(chunk) > room {
		chunk, c.truncated = chunk[:room], true
	}
	c.n += len(chunk)
	if c.keep {
		c.buf.Write(chunk)
	}
	if c.w != nil {
		if _, err := c.w.Write(chunk); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

// Denial signatures: the policy refused something the command tried. These
// are the command's behavior, distinct from runner failures.
var denialSignatures = []string{
	"Read-only file system",
	"Permission denied",
	"Operation not permitted",
	"Network is unreachable",
	"Temporary failure in name resolution",
	"evalsi egress denied",
}

// bwrapHint names the fallback for the bubblewrap failures seen on real
// clusters: Ubuntu 24.04 restricts unprivileged user namespaces from
// configuring the network (RTM_NEWADDR), and a pod user namespace needs an
// overlayfs that supports idmapped mounts.
func bwrapHint(msg string) string {
	switch {
	case strings.Contains(msg, "RTM_NEWADDR"), strings.Contains(msg, "loopback"):
		return " (the host restricts unprivileged user namespaces, as Ubuntu 24.04 does; run the pool with mode: privileged, or use the pod rung, ladder: [pod])"
	case strings.Contains(msg, "idmap"), strings.Contains(msg, "mount_setattr"), strings.Contains(msg, "overlay"):
		return " (the node's overlayfs lacks idmapped mounts, which pod user namespaces need; run the pool with mode: privileged, or use the pod rung, ladder: [pod])"
	}
	return ""
}

// classify separates runner failures (infrastructure) from the command's
// own outcomes, and recognizes policy denials.
func classify(driver string, res *ExecResult, stderr string) {
	if res.Outcome != OutcomeExit {
		return
	}
	first := firstLine(stderr)
	switch {
	case res.ExitCode == exitLauncherFailure && strings.HasPrefix(first, launcherPrefix):
		res.Outcome, res.Error = OutcomeRunnerFailure, strings.TrimPrefix(first, launcherPrefix)
		return
	case driver == "bwrap" && res.ExitCode == 1 && strings.HasPrefix(first, "bwrap: execvp "):
		// bwrap could not find or run the command itself: the command's problem.
		res.ExitCode = exitNotFound
		return
	case driver == "bwrap" && res.ExitCode == 1 && strings.HasPrefix(first, "bwrap: "):
		res.Outcome, res.Error = OutcomeRunnerFailure, first+bwrapHint(first)
		return
	}
	if res.ExitCode == 0 {
		return
	}
	for _, sig := range denialSignatures {
		if strings.Contains(stderr, sig) {
			res.Denials = append(res.Denials, sig)
		}
	}
	if len(res.Denials) > 0 {
		res.Outcome = OutcomeDenied
	}
}
