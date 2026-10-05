// Package sandbox runs untrusted commands under the strongest isolation
// available on the host, and fails closed when nothing meets the minimum a
// request asks for (design §13, decision 0005).
//
// Phase 1 ships the process-confinement rungs:
//
//   - bwrap (level namespaced): bubblewrap with separate user, mount, PID,
//     network, IPC and UTS namespaces, a minimal read-only root, a cleared
//     environment and a seccomp filter.
//   - landlock (level confined): the same world, with Landlock rules on file
//     access and TCP, seccomp (including a socket filter when the network is
//     denied) and resource limits, applied by the evalsid sandbox-exec launcher.
//
// The design follows the deepseek-harness process sandbox (MIT): fail closed,
// policy per call, enforcement reported as full or partial, functional probes
// of each runner, and separate dialects for denials and runner failures.
// Firecracker, Kata and hardened pods are later rungs on the same interface.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Level orders isolation strength.
type Level int

const (
	LevelNone Level = iota
	LevelConfined
	LevelNamespaced
	LevelKernel
	LevelVM
)

var levelNames = []string{"none", "confined", "namespaced", "kernel", "vm"}

func (l Level) String() string {
	if int(l) < len(levelNames) {
		return levelNames[l]
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// ParseLevel accepts none, confined, namespaced, kernel and vm.
func ParseLevel(s string) (Level, error) {
	if s == "" {
		return LevelConfined, nil
	}
	if i := slices.Index(levelNames, s); i >= 0 {
		return Level(i), nil
	}
	return 0, fmt.Errorf("unknown isolation level %q (use one of %s)", s, strings.Join(levelNames, ", "))
}

// Mode is the file-access policy, set per call.
type Mode string

const (
	// ModeReadOnly: the workspace is readable, nothing is writable.
	ModeReadOnly Mode = "read-only"
	// ModeWorkspaceWrite: the workspace is writable, nothing else is.
	ModeWorkspaceWrite Mode = "workspace-write"
)

// Request is one sandboxed execution. Files are written into a fresh
// workspace, which is the working directory and is deleted afterwards.
type Request struct {
	Command []string          `json:"command"`
	Files   map[string]string `json:"files,omitempty"`
	Stdin   string            `json:"stdin,omitempty"`
	// Environment for the command; nothing is inherited.
	Env  map[string]string `json:"env,omitempty"`
	Mode Mode              `json:"mode,omitempty"`
	// "deny" (default) or "allow".
	Network      string  `json:"network,omitempty"`
	TimeoutS     float64 `json:"timeout_s,omitempty"`
	MemoryMB     int     `json:"memory_mb,omitempty"`
	CPUSeconds   int     `json:"cpu_s,omitempty"`
	MaxProcs     int     `json:"max_procs,omitempty"`
	MaxFileMB    int     `json:"max_file_mb,omitempty"`
	OutputLimit  int     `json:"output_limit_bytes,omitempty"`
	MinIsolation string  `json:"min_isolation,omitempty"`
}

// Isolation is what actually confined a command; it is recorded with every result.
type Isolation struct {
	Driver string `json:"driver"`
	Level  string `json:"level"`
	// "full" or "partial" (some requested restriction could not be enforced).
	Enforcement string   `json:"enforcement"`
	Notes       []string `json:"notes,omitempty"`
}

// Outcome kinds. Denied, exit and timeout are the command's own behavior and
// count; runner_failure and unavailable are infrastructure errors and never do.
const (
	OutcomeExit          = "exit"
	OutcomeDenied        = "denied"
	OutcomeTimeout       = "timeout"
	OutcomeRunnerFailure = "runner_failure"
	OutcomeUnavailable   = "unavailable"
)

// Result of one execution.
type Result struct {
	Outcome    string    `json:"outcome"`
	ExitCode   int       `json:"exit_code"`
	Stdout     string    `json:"stdout"`
	Stderr     string    `json:"stderr"`
	Truncated  bool      `json:"truncated,omitempty"`
	DurationMS float64   `json:"duration_ms"`
	Isolation  Isolation `json:"isolation"`
	// Policy denials recognized in stderr (for example "Read-only file system").
	Denials []string `json:"denials,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// ErrUnavailable means no rung on this host meets the requested minimum.
var ErrUnavailable = errors.New("sandbox unavailable")

// Config is the sandbox section of evalsi.yaml.
type Config struct {
	// Rungs to try, strongest first. Default: bwrap, landlock.
	Ladder []string `json:"ladder,omitempty"`
	// The weakest isolation any request may get, whatever it asks for.
	MinIsolation string `json:"min_isolation,omitempty"`
	// bubblewrap binary; default: bwrap on PATH.
	BwrapPath string `json:"bwrap_path,omitempty"`
	// An unpacked root filesystem (for example an OCI image) used as / by bwrap
	// instead of the host's system directories.
	Rootfs string `json:"rootfs,omitempty"`
	// Extra host paths made readable inside sandboxes (interpreters, toolchains).
	ReadOnlyPaths []string `json:"read_only_paths,omitempty"`
	// Where workspaces are created; default: the system temp directory.
	WorkDir string `json:"work_dir,omitempty"`
	// The evalsid binary that re-executes itself as the launcher; default: this executable.
	Launcher string `json:"launcher,omitempty"`
}

// Defaults for requests that leave limits unset.
const (
	defaultTimeout     = 30 * time.Second
	defaultMemoryMB    = 1024
	defaultMaxProcs    = 256
	defaultMaxFileMB   = 64
	defaultOutputLimit = 1 << 20
	sandboxPath        = "/usr/local/bin:/usr/bin:/bin"
	workspaceInside    = "/workspace"
)

// driver is one rung of the ladder.
type driver interface {
	name() string
	level() Level
	// available reports why the driver cannot run here, cheaply (no probe).
	available() error
	// command builds the launcher spec for req in workspace ws.
	spec(req *Request, ws string) (*launchSpec, Isolation, error)
}

// Sandbox picks a rung per request and runs commands under it.
type Sandbox struct {
	cfg     Config
	drivers []driver
	min     Level

	mu     sync.Mutex
	probes map[string]error
}

// Validate checks rung names and the minimum level.
func (c Config) Validate() error {
	for _, name := range c.Ladder {
		switch name {
		case "bwrap", "landlock":
		case "firecracker", "kata", "pod":
			return fmt.Errorf("sandbox rung %q is not available in this version (Phase 1 ships bwrap and landlock)", name)
		default:
			return fmt.Errorf("unknown sandbox rung %q", name)
		}
	}
	_, err := ParseLevel(c.MinIsolation)
	return err
}

// New builds the ladder from cfg. Unknown rung names are an error; rungs that
// cannot run on this host are not, they are skipped (and reported by Probe).
func New(cfg Config) (*Sandbox, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(cfg.Ladder) == 0 {
		cfg.Ladder = []string{"bwrap", "landlock"}
	}
	min, _ := ParseLevel(cfg.MinIsolation)
	if cfg.Launcher == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locating the sandbox launcher: %w", err)
		}
		cfg.Launcher = exe
	}
	s := &Sandbox{cfg: cfg, min: min, probes: map[string]error{}}
	for _, name := range cfg.Ladder {
		switch name {
		case "bwrap":
			s.drivers = append(s.drivers, &bwrapDriver{cfg: &s.cfg})
		case "landlock":
			s.drivers = append(s.drivers, &landlockDriver{cfg: &s.cfg})
		}
	}
	return s, nil
}

// RungStatus is one line of Probe's report.
type RungStatus struct {
	Driver    string `json:"driver"`
	Level     string `json:"level"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Probe reports which rungs work here, running each one's functional probe.
func (s *Sandbox) Probe(ctx context.Context) []RungStatus {
	out := make([]RungStatus, 0, len(s.drivers))
	for _, d := range s.drivers {
		err := s.probe(ctx, d)
		st := RungStatus{Driver: d.name(), Level: d.level().String(), Available: err == nil}
		if err != nil {
			st.Reason = err.Error()
		}
		out = append(out, st)
	}
	return out
}

// probe runs `true` under the real profile once and caches the verdict: an
// installed binary is not proof that it works (user namespaces may be off).
func (s *Sandbox) probe(ctx context.Context, d driver) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.probes[d.name()]; ok {
		return err
	}
	err := d.available()
	if err == nil {
		var res *Result
		res, err = s.runWith(ctx, d, &Request{Command: []string{"true"}, TimeoutS: 10})
		switch {
		case err != nil:
		case res.Outcome != OutcomeExit || res.ExitCode != 0:
			err = fmt.Errorf("probe failed (%s, exit %d): %s", res.Outcome, res.ExitCode, firstLine(res.Stderr+res.Error))
		}
	}
	s.probes[d.name()] = err
	return err
}

// Run executes req under the strongest working rung that meets both the
// request's and the server's minimum. It never runs a command unconfined:
// with no qualifying rung it returns ErrUnavailable.
func (s *Sandbox) Run(ctx context.Context, req *Request) (*Result, error) {
	if len(req.Command) == 0 {
		return nil, errors.New("sandbox request needs a command")
	}
	want, err := ParseLevel(req.MinIsolation)
	if err != nil {
		return nil, err
	}
	want = max(want, s.min)
	var reasons []string
	for _, d := range s.drivers {
		if d.level() < want {
			reasons = append(reasons, fmt.Sprintf("%s: level %s is below %s", d.name(), d.level(), want))
			continue
		}
		if err := s.probe(ctx, d); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", d.name(), err))
			continue
		}
		return s.runWith(ctx, d, req)
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "no rungs configured")
	}
	return nil, fmt.Errorf("%w: nothing meets %s isolation (%s)", ErrUnavailable, want, strings.Join(reasons, "; "))
}

func (s *Sandbox) runWith(ctx context.Context, d driver, req *Request) (*Result, error) {
	ws, err := s.workspace(req)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(ws)
	spec, iso, err := d.spec(req, ws)
	if err != nil {
		return nil, err
	}
	res := s.launch(ctx, spec, req)
	res.Isolation = iso
	classify(d.name(), res)
	return res, nil
}

// workspace creates a private directory holding the request's files.
func (s *Sandbox) workspace(req *Request) (string, error) {
	ws, err := os.MkdirTemp(s.cfg.WorkDir, "evalsi-sandbox-")
	if err != nil {
		return "", fmt.Errorf("creating workspace: %w", err)
	}
	// The sandboxed user may be mapped to another uid; the directory itself is private by its parent.
	if err := os.Chmod(ws, 0o777); err != nil {
		return "", err
	}
	for name, content := range req.Files {
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			os.RemoveAll(ws)
			return "", fmt.Errorf("file %q must be a relative path inside the workspace", name)
		}
		path := filepath.Join(ws, clean)
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			os.RemoveAll(ws)
			return "", err
		}
		if err := os.WriteFile(path, []byte(content), 0o666); err != nil {
			os.RemoveAll(ws)
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Join(ws, ".tmp"), 0o777); err != nil {
		os.RemoveAll(ws)
		return "", err
	}
	return ws, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// env is the command's whole environment: fixed basics plus the request's.
func env(req *Request, home string) []string {
	vars := map[string]string{
		"PATH":   sandboxPath,
		"HOME":   home,
		"TMPDIR": filepath.Join(home, ".tmp"),
		"LANG":   "C.UTF-8",
	}
	for k, v := range req.Env {
		vars[k] = v
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}
