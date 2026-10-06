// Package sandbox runs untrusted commands under the strongest isolation
// available on the host, and fails closed when nothing meets the minimum a
// request asks for (design §13, decision 0005).
//
// The rungs, strongest first:
//
//   - firecracker (level vm): a microVM per sandbox with its own guest
//     kernel, an image root, a guest agent on vsock, deny-by-default egress
//     and snapshots. It needs /dev/kvm.
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
// Kata and hardened pods are Kubernetes rungs on the same interface (Phase 4).
//
// A Session is a sandbox that persists across commands: its workspace (and,
// with an image, its writable root) survives until it is closed, so an agent
// can work in it step by step. Every Exec is confined afresh.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// Request is one sandboxed execution, the JSON of `evalsid sandbox run`.
// Files are written into a fresh workspace, which is the working directory
// and is deleted afterwards. It is a one-shot Session.
type Request struct {
	Command []string          `json:"command"`
	Files   map[string]string `json:"files,omitempty"`
	Stdin   string            `json:"stdin,omitempty"`
	// Environment for the command; nothing is inherited.
	Env  map[string]string `json:"env,omitempty"`
	Mode Mode              `json:"mode,omitempty"`
	// "deny" (default), "allowlist" (with AllowHosts) or "allow".
	Network      string   `json:"network,omitempty"`
	AllowHosts   []string `json:"allow_hosts,omitempty"`
	Image        string   `json:"image,omitempty"`
	TimeoutS     float64  `json:"timeout_s,omitempty"`
	MemoryMB     int      `json:"memory_mb,omitempty"`
	CPUSeconds   int      `json:"cpu_s,omitempty"`
	MaxProcs     int      `json:"max_procs,omitempty"`
	MaxFileMB    int      `json:"max_file_mb,omitempty"`
	OutputLimit  int      `json:"output_limit_bytes,omitempty"`
	MinIsolation string   `json:"min_isolation,omitempty"`
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
	// Policy denials recognized in stderr (for example "Read-only file system")
	// or in the egress log (for example "egress denied: example.com:443").
	Denials []string `json:"denials,omitempty"`
	Error   string   `json:"error,omitempty"`
	// Connections attempted through the egress proxy (network: allowlist).
	Egress []EgressEvent `json:"egress,omitempty"`
}

// ErrUnavailable means no rung on this host meets the requested minimum.
var ErrUnavailable = errors.New("sandbox unavailable")

// Config is the sandbox section of evalsi.yaml.
type Config struct {
	// Rungs to try, strongest first. Default: firecracker, bwrap, landlock.
	Ladder []string `json:"ladder,omitempty"`
	// The weakest isolation any request may get, whatever it asks for.
	MinIsolation string `json:"min_isolation,omitempty"`
	// bubblewrap binary; default: a bwrap beside evalsid (the release's
	// static build), else bwrap on PATH.
	BwrapPath string `json:"bwrap_path,omitempty"`
	// An unpacked root filesystem used as / by bwrap when a request names no
	// image, instead of the host's system directories.
	Rootfs string `json:"rootfs,omitempty"`
	// Extra host paths made readable inside sandboxes (interpreters, toolchains).
	ReadOnlyPaths []string `json:"read_only_paths,omitempty"`
	// Where sandboxes and snapshots live; default: the system temp directory.
	WorkDir string `json:"work_dir,omitempty"`
	// Unpacked image roots, cached by digest; default: the user cache directory.
	CacheDir string `json:"cache_dir,omitempty"`
	// The evalsid binary that re-executes itself as the launcher; default: this executable.
	Launcher string `json:"launcher,omitempty"`
	// Sandboxes alive at once; default 64.
	MaxSandboxes int `json:"max_sandboxes,omitempty"`
	// Destroy sandboxes idle this long; default 30 minutes.
	IdleTimeoutS float64 `json:"idle_timeout_s,omitempty"`
	// The microVM rung (design §13).
	Firecracker *FirecrackerConfig `json:"firecracker,omitempty"`
	// The pod rung (in a Kubernetes sandbox pool).
	Pod *PodConfig `json:"pod,omitempty"`
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
	// supports reports why this rung cannot give a spec what it asks for.
	supports(sp *Spec) error
	// open starts a sandbox in dir (which exists and is private). With from,
	// the sandbox's state is restored from a snapshot directory instead of
	// being built from the spec's image and files.
	open(ctx context.Context, sp *Spec, dir, from string) (backend, error)
}

// backend is one live sandbox of some rung.
type backend interface {
	isolation() Isolation
	// imageDigest is the digest of the image root, if any.
	imageDigest() string
	exec(ctx context.Context, e *Exec, stdout, stderr io.Writer) *ExecResult
	writeFiles(files []File) error
	readFiles(paths []string, maxBytes int64) (files []File, missing []string, truncated bool, err error)
	// snapshot saves the sandbox's state into dir, for open(..., from=dir).
	snapshot(ctx context.Context, dir string) error
	egress() []EgressEvent
	close() error
}

// Sandbox picks a rung per request and runs commands under it.
type Sandbox struct {
	cfg     Config
	drivers []driver
	min     Level
	images  *imageStore

	mu     sync.Mutex
	probes map[string]error
}

var rungNames = []string{"firecracker", "bwrap", "landlock"}

// Validate checks rung names and the minimum level.
func (c Config) Validate() error {
	for _, name := range c.Ladder {
		switch name {
		case "firecracker", "bwrap", "landlock":
		case "pod":
			if c.Pod == nil {
				return errors.New("sandbox rung pod needs a sandbox.pod section")
			}
		case "kata", "gvisor":
			return fmt.Errorf("sandbox rung %q: use the pod rung with sandbox.pod.runtime_class_name", name)
		default:
			return fmt.Errorf("unknown sandbox rung %q (use %s)", name, strings.Join(rungNames, ", "))
		}
	}
	if c.Firecracker != nil {
		if err := c.Firecracker.validate(); err != nil {
			return err
		}
	}
	if c.Pod != nil {
		if err := c.Pod.validate(); err != nil {
			return err
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
		cfg.Ladder = rungNames
		if cfg.Pod != nil {
			// Last, as on Kubernetes: a pod per sandbox when nothing in this
			// process can confine the work.
			cfg.Ladder = append(append([]string{}, rungNames...), "pod")
		}
	}
	min, _ := ParseLevel(cfg.MinIsolation)
	if cfg.Launcher == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locating the sandbox launcher: %w", err)
		}
		cfg.Launcher = exe
	}
	if cfg.CacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		cfg.CacheDir = filepath.Join(base, "evalsi", "sandbox")
	}
	// Sandboxes and probes are made under the work directory, which a
	// fresh volume does not have yet.
	if cfg.WorkDir != "" {
		if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
			return nil, fmt.Errorf("sandbox.work_dir: %w", err)
		}
	}
	s := &Sandbox{cfg: cfg, min: min, probes: map[string]error{}, images: newImageStore(filepath.Join(cfg.CacheDir, "images"))}
	for _, name := range cfg.Ladder {
		switch name {
		case "firecracker":
			s.drivers = append(s.drivers, newFirecracker(s))
		case "bwrap":
			s.drivers = append(s.drivers, &processRung{sb: s, kind: &bwrapDriver{cfg: &s.cfg}})
		case "landlock":
			s.drivers = append(s.drivers, &processRung{sb: s, kind: &landlockDriver{cfg: &s.cfg}})
		case "pod":
			s.drivers = append(s.drivers, newPodDriver(s))
		}
	}
	return s, nil
}

// Close releases what the rungs hold between sandboxes (warm VMs).
func (s *Sandbox) Close() {
	for _, d := range s.drivers {
		if c, ok := d.(interface{ shutdown() }); ok {
			c.shutdown()
		}
	}
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
		err = s.probeRun(ctx, d)
	}
	s.probes[d.name()] = err
	return err
}

func (s *Sandbox) probeRun(ctx context.Context, d driver) error {
	sp := &Spec{}
	sp.defaults()
	dir, err := os.MkdirTemp(s.cfg.WorkDir, "evalsi-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	b, err := d.open(ctx, sp, dir, "")
	if err != nil {
		return err
	}
	defer b.close()
	var stderr bytes.Buffer
	res := b.exec(ctx, &Exec{Command: []string{"true"}, Timeout: 30 * time.Second}, io.Discard, &stderr)
	if res.Outcome != OutcomeExit || res.ExitCode != 0 {
		return fmt.Errorf("probe failed (%s, exit %d): %s", res.Outcome, res.ExitCode, firstLine(stderr.String()+res.Error))
	}
	return nil
}

// pick returns the strongest working rung that meets both the spec's and the
// server's minimum and can serve the spec. It never falls back to running
// unconfined: with no qualifying rung it returns ErrUnavailable.
func (s *Sandbox) pick(ctx context.Context, sp *Spec) (driver, error) {
	want, err := ParseLevel(sp.MinIsolation)
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
		if err := d.supports(sp); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", d.name(), err))
			continue
		}
		if err := s.probe(ctx, d); err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", d.name(), err))
			continue
		}
		return d, nil
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "no rungs configured")
	}
	return nil, fmt.Errorf("%w: nothing meets %s isolation (%s)", ErrUnavailable, want, strings.Join(reasons, "; "))
}

// Run executes a one-shot request: a sandbox made for it and destroyed after.
func (s *Sandbox) Run(ctx context.Context, req *Request) (*Result, error) {
	if len(req.Command) == 0 {
		return nil, errors.New("sandbox request needs a command")
	}
	sp := req.spec()
	if err := sp.Validate(); err != nil {
		return nil, err
	}
	sess, err := s.Open(ctx, sp)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	limit := req.OutputLimit
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	var stdout, stderr bytes.Buffer
	ex := &Exec{Command: req.Command, Stdin: []byte(req.Stdin), OutputLimit: limit}
	if req.TimeoutS > 0 {
		ex.Timeout = time.Duration(req.TimeoutS * float64(time.Second))
	}
	er := sess.Exec(ctx, ex, &stdout, &stderr)
	return &Result{
		Outcome: er.Outcome, ExitCode: er.ExitCode, Stdout: stdout.String(), Stderr: stderr.String(),
		Truncated: er.Truncated, DurationMS: float64(er.Duration.Microseconds()) / 1000,
		Isolation: sess.Isolation(), Denials: er.Denials, Error: er.Error, Egress: sess.Egress(),
	}, nil
}

func (req *Request) spec() *Spec {
	files := make(map[string][]byte, len(req.Files))
	for k, v := range req.Files {
		files[k] = []byte(v)
	}
	sp := &Spec{
		Image: req.Image, MinIsolation: req.MinIsolation, Mode: req.Mode,
		Network:   Network{Mode: req.Network, Allow: req.AllowHosts},
		Resources: Resources{MemoryMB: req.MemoryMB, MaxProcs: req.MaxProcs, MaxFileMB: req.MaxFileMB, CPUSeconds: req.CPUSeconds},
		Env:       req.Env, Files: files,
	}
	sp.defaults()
	return sp
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
