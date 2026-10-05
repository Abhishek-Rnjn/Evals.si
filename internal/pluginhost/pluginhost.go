// Package pluginhost starts and supervises evaluator workers and talks to them
// over evalsi.plugin.v1alpha1.EvaluatorPluginService.
//
// A worker is a subprocess (normally `python -m evalsi worker`) listening on a
// Unix socket in a private temporary directory. The host speaks gRPC to it
// over HTTP/2 without TLS. If the worker exits unexpectedly it is restarted
// with exponential backoff; calls made while it is down fail with Unavailable.
package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1/pluginv1alpha1connect"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// Worker is what the rest of the server needs from an evaluator worker.
type Worker interface {
	Describe(ctx context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error)
	Evaluate(ctx context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error)
	Reduce(ctx context.Context, req *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error)
}

// Options configures a supervised worker process.
type Options struct {
	// Command runs the evalsi CLI; "worker --listen unix://... [--judges file]" is appended.
	Command []string
	// Judges are written to a private file and passed to the worker by name.
	Judges any
	// NoCache disables the worker's judge response cache.
	NoCache      bool
	StartTimeout time.Duration
	// Output receives the worker's stdout and stderr; nil means os.Stderr.
	Output io.Writer
	Logger *slog.Logger
	// Env adds environment variables for the worker.
	Env []string
}

// Process is a supervised worker subprocess.
type Process struct {
	opts   Options
	dir    string
	socket string
	args   []string
	client pluginv1alpha1connect.EvaluatorPluginServiceClient
	log    *slog.Logger

	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{}
	stopping bool
	stopped  chan struct{}
}

// Start launches the worker and waits until it answers Describe.
func Start(ctx context.Context, opts Options) (*Process, error) {
	if len(opts.Command) == 0 {
		return nil, errors.New("pluginhost: empty worker command")
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 60 * time.Second
	}
	if opts.Output == nil {
		opts.Output = os.Stderr
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	// Unix socket paths are limited to ~100 bytes, so keep the directory short.
	dir, err := os.MkdirTemp("", "evalsi-")
	if err != nil {
		return nil, err
	}
	p := &Process{
		opts:    opts,
		dir:     dir,
		socket:  filepath.Join(dir, "worker.sock"),
		log:     opts.Logger,
		stopped: make(chan struct{}),
	}
	p.args = append(append([]string{}, opts.Command[1:]...), "worker", "--listen", "unix://"+p.socket)
	if opts.Judges != nil {
		raw, err := json.Marshal(opts.Judges)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("pluginhost: encoding judges: %w", err)
		}
		judges := filepath.Join(dir, "judges.json")
		if err := os.WriteFile(judges, raw, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		p.args = append(p.args, "--judges", judges)
	}
	if opts.NoCache {
		p.args = append(p.args, "--no-cache")
	}
	p.client = pluginv1alpha1connect.NewEvaluatorPluginServiceClient(
		unixHTTPClient(p.socket), "http://worker", connect.WithGRPC(),
	)
	if err := p.launch(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := p.waitReady(ctx); err != nil {
		p.Stop()
		return nil, err
	}
	go p.supervise()
	return p, nil
}

func unixHTTPClient(socket string) *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{
		Protocols: protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

func (p *Process) launch() error {
	_ = os.Remove(p.socket)
	cmd := exec.Command(p.opts.Command[0], p.args...)
	cmd.Stdout = p.opts.Output
	cmd.Stderr = p.opts.Output
	cmd.Env = append(os.Environ(), p.opts.Env...)
	// Own process group, so a terminal Ctrl-C reaches evalsid, which then stops the worker.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("pluginhost: starting worker %q: %w", p.opts.Command[0], err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	p.mu.Lock()
	p.cmd, p.exited = cmd, exited
	p.mu.Unlock()
	p.log.Info("worker started", "pid", cmd.Process.Pid, "socket", p.socket)
	return nil
}

func (p *Process) waitReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.opts.StartTimeout)
	defer cancel()
	p.mu.Lock()
	exited := p.exited
	p.mu.Unlock()
	for {
		callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := p.client.Describe(callCtx, connect.NewRequest(&pluginv1alpha1.DescribeRequest{}))
		callCancel()
		if err == nil {
			return nil
		}
		select {
		case <-exited:
			return errors.New("pluginhost: worker exited during startup; see its output above")
		case <-ctx.Done():
			return fmt.Errorf("pluginhost: worker not ready after %s: %w", p.opts.StartTimeout, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// supervise restarts the worker when it exits unexpectedly.
func (p *Process) supervise() {
	backoff := time.Second
	for {
		p.mu.Lock()
		exited := p.exited
		p.mu.Unlock()
		select {
		case <-p.stopped:
			return
		case <-exited:
		}
		p.mu.Lock()
		stopping := p.stopping
		p.mu.Unlock()
		if stopping {
			return
		}
		p.log.Warn("worker exited; restarting", "backoff", backoff)
		select {
		case <-p.stopped:
			return
		case <-time.After(backoff):
		}
		if err := p.launch(); err != nil {
			p.log.Error("worker restart failed", "err", err)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), p.opts.StartTimeout)
		err := p.waitReady(ctx)
		cancel()
		if err != nil {
			p.log.Error("restarted worker is not ready", "err", err)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
	}
}

// Stop terminates the worker (SIGTERM, then SIGKILL after five seconds) and
// removes its temporary directory. It is safe to call more than once.
func (p *Process) Stop() {
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return
	}
	p.stopping = true
	close(p.stopped)
	cmd, exited := p.cmd, p.exited
	p.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}
	_ = os.RemoveAll(p.dir)
}

// Describe lists the evaluators the worker hosts.
func (p *Process) Describe(ctx context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	resp, err := p.client.Describe(ctx, connect.NewRequest(&pluginv1alpha1.DescribeRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetEvaluators(), nil
}

// Evaluate sends one batch over a fresh stream and waits for its response.
// Concurrency comes from running several of these at once.
func (p *Process) Evaluate(ctx context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	stream := p.client.Evaluate(ctx)
	if err := stream.Send(req); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := stream.CloseRequest(); err != nil {
		return nil, err
	}
	resp, err := stream.Receive()
	if err != nil {
		_ = stream.CloseResponse()
		return nil, err
	}
	return resp, stream.CloseResponse()
}

// Reduce runs a dataset-scope evaluator.
func (p *Process) Reduce(ctx context.Context, req *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	resp, err := p.client.Reduce(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}
