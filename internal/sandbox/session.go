package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// Network modes.
const (
	NetworkDeny      = "deny"
	NetworkAllowlist = "allowlist"
	NetworkAllow     = "allow"
)

// Network is a sandbox's egress policy.
type Network struct {
	// "deny" (default: loopback only), "allowlist" or "allow".
	Mode string `json:"mode,omitempty"`
	// For allowlist: "host" (ports 80 and 443), "host:port" or "*.suffix".
	Allow []string `json:"allow,omitempty"`
}

// Resources caps each command run in a sandbox.
type Resources struct {
	MemoryMB   int `json:"memory_mb,omitempty"`
	MaxProcs   int `json:"max_procs,omitempty"`
	MaxFileMB  int `json:"max_file_mb,omitempty"`
	CPUSeconds int `json:"cpu_s,omitempty"`
}

// Spec describes a sandbox.
type Spec struct {
	// An OCI image reference that becomes the root filesystem (see imageStore).
	// Empty: the host's system directories (or Config.Rootfs), read-only.
	Image string `json:"image,omitempty"`
	// Bind the image root read-only instead of giving the sandbox a private
	// writable copy. Faster; only the workdir is writable.
	ReadOnlyRoot bool              `json:"read_only_root,omitempty"`
	MinIsolation string            `json:"min_isolation,omitempty"`
	Mode         Mode              `json:"mode,omitempty"`
	Network      Network           `json:"network"`
	Resources    Resources         `json:"resources"`
	Env          map[string]string `json:"env,omitempty"`
	// Written into the workdir at creation.
	Files map[string][]byte `json:"files,omitempty"`
	// The working directory inside the sandbox; default /workspace. The
	// landlock rung cannot remap paths, so there it is a host directory and
	// EVALSI_WORKDIR tells commands where it is.
	Workdir     string        `json:"workdir,omitempty"`
	IdleTimeout time.Duration `json:"idle_timeout,omitempty"`
}

func (sp *Spec) defaults() {
	if sp.Mode == "" {
		sp.Mode = ModeWorkspaceWrite
	}
	if sp.Network.Mode == "" {
		sp.Network.Mode = NetworkDeny
	}
	if sp.Workdir == "" {
		sp.Workdir = workspaceInside
	}
}

// Validate checks a spec after defaults.
func (sp *Spec) Validate() error {
	sp.defaults()
	if _, err := ParseLevel(sp.MinIsolation); err != nil {
		return err
	}
	switch sp.Mode {
	case ModeReadOnly, ModeWorkspaceWrite:
	default:
		return fmt.Errorf("unknown file mode %q (use read-only or workspace-write)", sp.Mode)
	}
	switch sp.Network.Mode {
	case NetworkDeny, NetworkAllow:
		if len(sp.Network.Allow) > 0 {
			return fmt.Errorf("network.allow needs network mode allowlist, not %s", sp.Network.Mode)
		}
	case NetworkAllowlist:
		if len(sp.Network.Allow) == 0 {
			return errors.New("network mode allowlist needs at least one allowed host")
		}
		if _, err := parseAllowlist(sp.Network.Allow); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown network mode %q (use deny, allowlist or allow)", sp.Network.Mode)
	}
	if !path.IsAbs(sp.Workdir) || path.Clean(sp.Workdir) != sp.Workdir || sp.Workdir == "/" {
		return fmt.Errorf("workdir %q must be a clean absolute path below /", sp.Workdir)
	}
	for _, reserved := range []string{"/proc", "/dev", "/tmp", "/sys"} {
		if sp.Workdir == reserved || strings.HasPrefix(sp.Workdir, reserved+"/") {
			return fmt.Errorf("workdir %q is inside %s", sp.Workdir, reserved)
		}
	}
	for name := range sp.Files {
		if _, err := relPath(name); err != nil {
			return err
		}
	}
	for k := range sp.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("invalid environment variable name %q", k)
		}
	}
	return nil
}

// relPath checks that name stays inside the workdir and returns it cleaned.
func relPath(name string) (string, error) {
	clean := path.Clean(name)
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", fmt.Errorf("file %q must be a relative path inside the workspace", name)
	}
	return clean, nil
}

// Exec is one command run in a sandbox.
type Exec struct {
	Command []string
	Stdin   []byte
	// Added to the sandbox's environment.
	Env     map[string]string
	Timeout time.Duration
	// Per stream; output past it is dropped and reported as truncated.
	OutputLimit int
	// Relative to the workdir.
	Cwd string
}

// ExecResult is how one command ended.
type ExecResult struct {
	Outcome   string
	ExitCode  int
	Duration  time.Duration
	Truncated bool
	Denials   []string
	Error     string
	// Resource use of the command.
	CPU    time.Duration
	MaxRSS int64
}

// File is a file read from or written to a sandbox.
type File struct {
	// Relative to the workdir, or absolute inside a writable image root.
	Path    string
	Content []byte
	Mode    os.FileMode
}

// Session is a live sandbox.
type Session struct {
	ID      string
	Spec    Spec
	Driver  string
	Created time.Time

	b   backend
	dir string

	mu       sync.Mutex
	lastUsed time.Time
	running  int
	execs    int64
	cpu      time.Duration
	maxRSS   int64
	closed   bool
}

// Open creates a session on the strongest rung that can serve sp.
func (s *Sandbox) Open(ctx context.Context, sp *Spec) (*Session, error) {
	if err := sp.Validate(); err != nil {
		return nil, err
	}
	d, err := s.pick(ctx, sp)
	if err != nil {
		return nil, err
	}
	return s.openWith(ctx, d, sp, "")
}

func (s *Sandbox) openWith(ctx context.Context, d driver, sp *Spec, from string) (*Session, error) {
	dir, err := os.MkdirTemp(s.cfg.WorkDir, "evalsi-sbx-")
	if err != nil {
		return nil, fmt.Errorf("creating sandbox directory: %w", err)
	}
	b, err := d.open(ctx, sp, dir, from)
	if err != nil {
		_ = removeAll(dir)
		return nil, err
	}
	now := time.Now()
	return &Session{ID: newSessionID(), Spec: *sp, Driver: d.name(), Created: now, b: b, dir: dir, lastUsed: now}, nil
}

// ImageDigest is the digest of the image root, if the sandbox has one.
func (ss *Session) ImageDigest() string { return ss.b.imageDigest() }

// Isolation is what confines this session's commands.
func (ss *Session) Isolation() Isolation { return ss.b.isolation() }

// Exec runs a command, streaming its output to stdout and stderr (each
// capped at e.OutputLimit) and reporting how it ended.
func (ss *Session) Exec(ctx context.Context, e *Exec, stdout, stderr io.Writer) *ExecResult {
	if len(e.Command) == 0 {
		return &ExecResult{Outcome: OutcomeRunnerFailure, ExitCode: -1, Error: "empty command"}
	}
	if e.Cwd != "" {
		if _, err := relPath(e.Cwd); err != nil {
			return &ExecResult{Outcome: OutcomeRunnerFailure, ExitCode: -1, Error: err.Error()}
		}
	}
	if e.Timeout <= 0 {
		e.Timeout = defaultTimeout
	}
	if e.OutputLimit <= 0 {
		e.OutputLimit = defaultOutputLimit
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return &ExecResult{Outcome: OutcomeRunnerFailure, ExitCode: -1, Error: "sandbox is closed"}
	}
	ss.running++
	ss.mu.Unlock()
	before := len(ss.b.egress())
	out, errw := &capped{w: stdout, limit: e.OutputLimit}, &capped{w: stderr, limit: e.OutputLimit}
	res := ss.b.exec(ctx, e, out, errw)
	res.Truncated = res.Truncated || out.truncated || errw.truncated
	for _, ev := range ss.b.egress()[before:] {
		if !ev.Allowed {
			res.Denials = append(res.Denials, fmt.Sprintf("egress denied: %s:%d", ev.Host, ev.Port))
		}
	}
	if len(res.Denials) > 0 && res.Outcome == OutcomeExit && res.ExitCode != 0 {
		res.Outcome = OutcomeDenied
	}
	ss.mu.Lock()
	ss.running--
	ss.execs++
	ss.cpu += res.CPU
	ss.maxRSS = max(ss.maxRSS, res.MaxRSS)
	ss.lastUsed = time.Now()
	ss.mu.Unlock()
	return res
}

// WriteFiles writes files into the sandbox.
func (ss *Session) WriteFiles(files []File) error {
	if err := ss.alive(); err != nil {
		return err
	}
	return ss.b.writeFiles(files)
}

// ReadFiles reads files (directories recursively) from the sandbox.
func (ss *Session) ReadFiles(paths []string, maxBytes int64) ([]File, []string, bool, error) {
	if err := ss.alive(); err != nil {
		return nil, nil, false, err
	}
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	return ss.b.readFiles(paths, maxBytes)
}

// Egress is every connection attempted through the egress proxy.
func (ss *Session) Egress() []EgressEvent { return ss.b.egress() }

// Stats is the session's resource use.
func (ss *Session) Stats() (execs int64, cpu time.Duration, maxRSS int64) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.execs, ss.cpu, ss.maxRSS
}

func (ss *Session) idleSince() (time.Time, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.lastUsed, ss.running == 0
}

func (ss *Session) alive() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return errors.New("sandbox is closed")
	}
	return nil
}

// Close destroys the sandbox and everything in it.
func (ss *Session) Close() error {
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil
	}
	ss.closed = true
	ss.mu.Unlock()
	err := ss.b.close()
	if rmErr := removeAll(ss.dir); err == nil {
		err = rmErr
	}
	return err
}

// removeAll deletes a tree even where the sandboxed command removed write
// permission from directories inside it.
func removeAll(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = walkChmod(dir)
	return os.RemoveAll(dir)
}
