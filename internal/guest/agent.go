// Package guest is evalsi-guest's agent: the service inside each Firecracker
// microVM that runs commands, moves files and refreshes entropy and the
// clock after a snapshot restore. The host reaches it over vsock.
//
// The VM is the isolation boundary; inside it the agent runs commands with
// resource limits, a wall-clock timeout that kills the process group, and
// capped output. In tests the agent runs on the host with Root set to a
// directory, which maps every guest path into it.
package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	guestv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1/guestv1alpha1connect"
)

// Version identifies the agent; the host checks it after boot.
const Version = "evalsi-guest/1"

// VsockPort is where the agent listens inside the VM.
const VsockPort = 1024

// Agent implements GuestAgentService.
type Agent struct {
	// Root maps guest paths: "/" in a VM, a directory in tests.
	Root string
	// Limiter re-executes a command under resource limits; nil runs it as is.
	Limiter func(limits *guestv1alpha1.Limits, argv []string) []string
	// DialHost opens a connection to the host's vsock port (egress).
	DialHost func(port uint32) (net.Conn, error)
	// System applies entropy and the clock (in a VM); nil records only.
	System System

	mu        sync.Mutex
	egress    []net.Listener
	refreshed int
}

// System is what Refresh changes in a real guest.
type System interface {
	AddEntropy(b []byte) error
	SetTime(t time.Time) error
}

// Handler serves the agent.
func (a *Agent) Handler() (string, http.Handler) {
	return guestv1alpha1connect.NewGuestAgentServiceHandler(a)
}

// Serve serves the agent on a listener until it closes (h2c, gRPC protocol).
func (a *Agent) Serve(ln net.Listener) error {
	mux := http.NewServeMux()
	mux.Handle(a.Handler())
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: p, ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

// Refreshed reports how many times Refresh ran (tests).
func (a *Agent) Refreshed() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.refreshed
}

func (a *Agent) Health(context.Context, *connect.Request[guestv1alpha1.HealthRequest]) (*connect.Response[guestv1alpha1.HealthResponse], error) {
	return connect.NewResponse(&guestv1alpha1.HealthResponse{Version: Version}), nil
}

// host maps an absolute guest path to the agent's filesystem.
func (a *Agent) host(p string) (string, error) {
	if !path.IsAbs(p) {
		return "", fmt.Errorf("guest path %q must be absolute", p)
	}
	root := a.Root
	if root == "" {
		root = "/"
	}
	return filepath.Join(root, filepath.FromSlash(path.Clean(p))), nil
}

// rel maps an absolute guest path into os.Root form.
func rel(p string) (string, error) {
	if !path.IsAbs(p) {
		return "", fmt.Errorf("guest path %q must be absolute", p)
	}
	r := strings.TrimPrefix(path.Clean(p), "/")
	if r == "" {
		r = "."
	}
	return r, nil
}

func (a *Agent) openRoot() (*os.Root, error) {
	root := a.Root
	if root == "" {
		root = "/"
	}
	return os.OpenRoot(root)
}

type capWriter struct {
	mu        *sync.Mutex
	stream    *connect.ServerStream[guestv1alpha1.ExecResponse]
	stderr    bool
	limit, n  int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.limit - w.n
	if room <= 0 {
		w.truncated = w.truncated || len(p) > 0
		return len(p), nil
	}
	chunk := p
	if len(chunk) > room {
		chunk, w.truncated = chunk[:room], true
	}
	w.n += len(chunk)
	resp := &guestv1alpha1.ExecResponse{Event: &guestv1alpha1.ExecResponse_Stdout{Stdout: append([]byte(nil), chunk...)}}
	if w.stderr {
		resp.Event = &guestv1alpha1.ExecResponse_Stderr{Stderr: append([]byte(nil), chunk...)}
	}
	w.mu.Lock()
	err := w.stream.Send(resp)
	w.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (a *Agent) Exec(ctx context.Context, req *connect.Request[guestv1alpha1.ExecRequest], stream *connect.ServerStream[guestv1alpha1.ExecResponse]) error {
	m := req.Msg
	if len(m.GetCommand()) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("empty command"))
	}
	cwd := m.GetCwd()
	if cwd == "" {
		cwd = "/"
	}
	dir, err := a.host(cwd)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	timeout := m.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	limit := int(m.GetOutputLimitBytes())
	if limit <= 0 {
		limit = 1 << 20
	}
	argv := m.GetCommand()
	if a.Limiter != nil && m.GetLimits() != nil {
		argv = a.Limiter(m.GetLimits(), argv)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = m.GetEnv()
	if p := lookPath(argv[0], m.GetEnv()); p != "" {
		cmd.Path = p
	}
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(string(m.GetStdin()))
	mu := &sync.Mutex{}
	out := &capWriter{mu: mu, stream: stream, limit: limit}
	errw := &capWriter{mu: mu, stream: stream, stderr: true, limit: limit}
	cmd.Stdout, cmd.Stderr = out, errw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	start := time.Now()
	runErr := cmd.Run()
	res := &guestv1alpha1.ExecResult{Duration: durationpb.New(time.Since(start))}
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
			res.CpuTime = durationpb.New(time.Duration(ru.Utime.Nano() + ru.Stime.Nano()))
			res.MaxRssBytes = ru.Maxrss * 1024
		}
	}
	var exit *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.TimedOut, res.ExitCode = true, -1
	case runErr == nil:
	case errors.As(runErr, &exit):
		res.ExitCode = int32(exit.ExitCode())
		if st, ok := exit.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			res.ExitCode = 128 + int32(st.Signal())
		}
	case errors.Is(runErr, exec.ErrNotFound), errors.Is(runErr, fs.ErrNotExist):
		res.ExitCode, res.Error = 127, fmt.Sprintf("command not found: %s", argv[0])
		_, _ = errw.Write([]byte("sandbox: command not found: " + argv[0] + "\n"))
	default:
		res.ExitCode, res.Error = 126, runErr.Error()
	}
	res.Truncated = out.truncated || errw.truncated
	mu.Lock()
	defer mu.Unlock()
	return stream.Send(&guestv1alpha1.ExecResponse{Event: &guestv1alpha1.ExecResponse_Result{Result: res}})
}

// lookPath resolves a command against the PATH in the command's environment.
func lookPath(name string, env []string) string {
	if strings.Contains(name, "/") {
		return ""
	}
	dirs := "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			dirs = v
		}
	}
	for _, d := range filepath.SplitList(dirs) {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

func (a *Agent) WriteFiles(_ context.Context, req *connect.Request[guestv1alpha1.WriteFilesRequest]) (*connect.Response[guestv1alpha1.WriteFilesResponse], error) {
	root, err := a.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	for _, d := range req.Msg.GetDirs() {
		r, err := rel(d)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if err := root.MkdirAll(r, 0o755); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	for _, f := range req.Msg.GetFiles() {
		r, err := rel(f.GetPath())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if dir := path.Dir(r); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
		}
		mode := os.FileMode(f.GetMode()).Perm()
		if mode == 0 {
			mode = 0o644
		}
		fh, err := root.OpenFile(r, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		_, werr := fh.Write(f.GetContent())
		cerr := fh.Chmod(mode)
		fh.Close()
		if werr != nil || cerr != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.Join(werr, cerr))
		}
	}
	return connect.NewResponse(&guestv1alpha1.WriteFilesResponse{}), nil
}

func (a *Agent) ReadFiles(_ context.Context, req *connect.Request[guestv1alpha1.ReadFilesRequest]) (*connect.Response[guestv1alpha1.ReadFilesResponse], error) {
	root, err := a.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	max := req.Msg.GetMaxBytes()
	if max <= 0 {
		max = 16 << 20
	}
	out := &guestv1alpha1.ReadFilesResponse{}
	var total int64
	add := func(r, name string) (bool, error) {
		fh, err := root.OpenFile(r, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false, err
		}
		defer fh.Close()
		fi, err := fh.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			return false, nil
		}
		if total+fi.Size() > max {
			return true, nil
		}
		data, err := io.ReadAll(io.LimitReader(fh, max-total))
		if err != nil {
			return false, err
		}
		total += int64(len(data))
		out.Files = append(out.Files, &guestv1alpha1.File{Path: name, Content: data, Mode: uint32(fi.Mode().Perm())})
		return false, nil
	}
	for _, p := range req.Msg.GetPaths() {
		r, err := rel(p)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		fi, err := root.Lstat(r)
		if errors.Is(err, fs.ErrNotExist) {
			out.Missing = append(out.Missing, p)
			continue
		}
		if err != nil {
			return nil, err
		}
		var full bool
		if fi.IsDir() {
			err = fs.WalkDir(root.FS(), r, func(sub string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return err
				}
				if full, err = add(sub, "/"+sub); full {
					return fs.SkipAll
				}
				return err
			})
		} else if fi.Mode().IsRegular() {
			full, err = add(r, p)
		}
		if err != nil {
			return nil, err
		}
		if full {
			out.Truncated = true
			break
		}
	}
	return connect.NewResponse(out), nil
}

func (a *Agent) Refresh(_ context.Context, req *connect.Request[guestv1alpha1.RefreshRequest]) (*connect.Response[guestv1alpha1.RefreshResponse], error) {
	if a.System != nil {
		if err := a.System.AddEntropy(req.Msg.GetEntropy()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("entropy: %w", err))
		}
		if err := a.System.SetTime(time.Unix(0, req.Msg.GetUnixNanos())); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("clock: %w", err))
		}
	}
	a.mu.Lock()
	a.refreshed++
	n := a.refreshed
	a.mu.Unlock()
	if a.System == nil && a.Root != "" {
		// Tests: leave evidence where the host can see it.
		_ = os.WriteFile(filepath.Join(a.Root, ".evalsi-refreshed"), []byte(strconv.Itoa(n)), 0o644)
	}
	return connect.NewResponse(&guestv1alpha1.RefreshResponse{}), nil
}

func (a *Agent) StartEgress(_ context.Context, req *connect.Request[guestv1alpha1.StartEgressRequest]) (*connect.Response[guestv1alpha1.StartEgressResponse], error) {
	if a.DialHost == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no host channel for egress"))
	}
	ln, err := net.Listen("tcp", req.Msg.GetListen())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	a.mu.Lock()
	a.egress = append(a.egress, ln)
	a.mu.Unlock()
	port := req.Msg.GetVsockPort()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := a.DialHost(port)
				if err != nil {
					return
				}
				defer up.Close()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return connect.NewResponse(&guestv1alpha1.StartEgressResponse{Listen: ln.Addr().String()}), nil
}

// Close stops the egress forwarders.
func (a *Agent) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ln := range a.egress {
		ln.Close()
	}
	a.egress = nil
}
