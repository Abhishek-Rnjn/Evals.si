package sandbox

// Background processes: a command started in a sandbox that outlives the call
// that started it, so a server or a watcher started in one agent step is still
// there in the next. Each runs exactly as an Exec does (the same rung, the
// same confinement and resource limits, its own cgroup where there is one),
// with a wall-clock limit of its own, and its output goes into bounded ring
// buffers that callers read by offset. Its stdin can stay open for writes,
// which is what an interactive agent protocol over stdio (ACP) needs.
//
// Processes die with their sandbox: Close (and so Destroy and the idle
// reaper) kills every one first. A running process is not activity for the
// idle timeout; reading its output or writing to it is.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sync"
	"time"
)

const (
	// Running at once in one sandbox.
	maxProcesses = 16
	// Ended processes kept for their output and exit status; the oldest go first.
	maxKeptProcesses      = 64
	defaultProcessTimeout = time.Hour
	maxProcessTimeout     = 24 * time.Hour
	// Per stream: the newest bytes are kept.
	defaultProcessBuffer = 1 << 20
	maxProcessBuffer     = 16 << 20
	// The longest a read waits for new output.
	maxProcessWait = 30 * time.Second
	// A write to stdin fails when the process has not read it by then.
	stdinWriteTimeout = 10 * time.Second
	// How long Kill and Close wait for a killed process to be reaped.
	processKillWait = 10 * time.Second
)

// ProcessSpec starts a background process.
type ProcessSpec struct {
	Command []string
	// Added to the sandbox's environment.
	Env map[string]string
	// Relative to the workdir.
	Cwd string
	// Wall-clock limit; default one hour, at most 24.
	Timeout time.Duration
	// Bytes of each stream kept; default 1 MiB, at most 16 MiB.
	BufferBytes int
	// Keep stdin open for WriteProcessStdin; otherwise the process reads EOF.
	Stdin bool
}

// ProcessStatus describes a background process.
type ProcessStatus struct {
	ID      string
	Command []string
	Started time.Time
	Running bool
	// Ended because KillProcess (or the sandbox's end) killed it.
	Killed bool
	// How it ended; nil while it runs.
	Result *ExecResult
	// Bytes written to each stream so far (kept or not).
	StdoutBytes, StderrBytes int64
}

// ProcessOutput is what ProcessOutput read.
type ProcessOutput struct {
	Stdout, Stderr []byte
	// Pass these to the next read to continue where this one stopped.
	StdoutOffset, StderrOffset int64
	// Bytes between the requested offset and the oldest byte still kept:
	// the ring buffer wrapped before they were read.
	StdoutDropped, StderrDropped int64
	Status                       ProcessStatus
}

// ring keeps the newest size bytes of a stream.
type ring struct {
	buf   []byte
	size  int
	total int64
}

func (r *ring) write(p []byte) {
	r.total += int64(len(p))
	if len(p) >= r.size {
		r.buf = append(r.buf[:0], p[len(p)-r.size:]...)
		return
	}
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.size; over > 0 {
		n := copy(r.buf, r.buf[over:])
		r.buf = r.buf[:n]
	}
}

// read returns up to limit bytes from offset from, the offset after them, and
// how many bytes before the oldest kept one were asked for and are gone.
func (r *ring) read(from int64, limit int) (data []byte, next, dropped int64) {
	start := r.total - int64(len(r.buf))
	if from < start {
		dropped, from = start-from, start
	}
	from = min(from, r.total)
	chunk := r.buf[from-start:]
	if len(chunk) > limit {
		chunk = chunk[:limit]
	}
	return slices.Clone(chunk), from + int64(len(chunk)), dropped
}

type process struct {
	id      string
	command []string
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}

	mu             sync.Mutex
	stdin          *os.File // the write end; nil when not kept or closed
	stdout, stderr ring
	// Closed and replaced whenever output arrives or the process ends.
	changed chan struct{}
	result  *ExecResult
	killed  bool
}

func (p *process) notify() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *process) status() ProcessStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.statusLocked()
}

func (p *process) statusLocked() ProcessStatus {
	st := ProcessStatus{
		ID: p.id, Command: slices.Clone(p.command), Started: p.started, Running: p.result == nil,
		Killed: p.killed, StdoutBytes: p.stdout.total, StderrBytes: p.stderr.total,
	}
	if p.result != nil {
		res := *p.result
		st.Result = &res
	}
	return st
}

type ringWriter struct {
	p      *process
	stderr bool
}

func (w ringWriter) Write(b []byte) (int, error) {
	w.p.mu.Lock()
	defer w.p.mu.Unlock()
	if w.stderr {
		w.p.stderr.write(b)
	} else {
		w.p.stdout.write(b)
	}
	w.p.notify()
	return len(b), nil
}

// procTable is a session's background processes.
type procTable struct {
	mu    sync.Mutex
	byID  map[string]*process
	order []string
	next  int
}

func (t *procTable) get(id string) (*process, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.byID[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("process %q: %w", id, ErrNotFound)
}

func (t *procTable) all() []*process {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*process, 0, len(t.order))
	for _, id := range t.order {
		out = append(out, t.byID[id])
	}
	return out
}

// running counts the processes that have not ended.
func (t *procTable) running() int {
	n := 0
	for _, p := range t.all() {
		if p.status().Running {
			n++
		}
	}
	return n
}

// prune drops the oldest ended processes past maxKeptProcesses. t.mu is held.
func (t *procTable) prune() {
	for len(t.order) > maxKeptProcesses {
		dropped := false
		for i, id := range t.order {
			if !t.byID[id].status().Running {
				delete(t.byID, id)
				t.order = slices.Delete(t.order, i, i+1)
				dropped = true
				break
			}
		}
		if !dropped {
			return
		}
	}
}

// StartProcess starts a background process and returns at once.
func (ss *Session) StartProcess(sp *ProcessSpec) (ProcessStatus, error) {
	if len(sp.Command) == 0 {
		return ProcessStatus{}, errors.New("empty command")
	}
	if sp.Cwd != "" {
		if _, err := relPath(sp.Cwd); err != nil {
			return ProcessStatus{}, err
		}
	}
	timeout := sp.Timeout
	switch {
	case timeout < 0 || timeout > maxProcessTimeout:
		return ProcessStatus{}, fmt.Errorf("a background process's timeout must be from 0 to %s", maxProcessTimeout)
	case timeout == 0:
		timeout = defaultProcessTimeout
	}
	size := sp.BufferBytes
	switch {
	case size < 0 || size > maxProcessBuffer:
		return ProcessStatus{}, fmt.Errorf("buffer_bytes must be from 0 to %d", maxProcessBuffer)
	case size == 0:
		size = defaultProcessBuffer
	}
	e := &Exec{
		Command: slices.Clone(sp.Command), Env: sp.Env, Cwd: sp.Cwd, Timeout: timeout,
		// The ring buffers bound what is kept; the stream itself is not cut.
		OutputLimit: math.MaxInt32,
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &process{
		command: e.Command, started: time.Now(), cancel: cancel, done: make(chan struct{}),
		stdout: ring{size: size}, stderr: ring{size: size}, changed: make(chan struct{}),
	}
	if sp.Stdin {
		r, w, err := os.Pipe()
		if err != nil {
			cancel()
			return ProcessStatus{}, err
		}
		e.StdinStream, p.stdin = r, w
	}
	fail := func(err error) (ProcessStatus, error) {
		cancel()
		if e.StdinStream != nil {
			e.StdinStream.Close()
			p.stdin.Close()
		}
		return ProcessStatus{}, err
	}
	// Under ss.mu, so Close either sees this process or refuses it.
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return fail(errors.New("sandbox is closed"))
	}
	t := &ss.procs
	if t.running() >= maxProcesses {
		ss.mu.Unlock()
		return fail(fmt.Errorf("at most %d background processes may run in a sandbox at once; kill one first", maxProcesses))
	}
	t.mu.Lock()
	if t.byID == nil {
		t.byID = map[string]*process{}
	}
	t.next++
	p.id = fmt.Sprintf("proc_%d", t.next)
	t.byID[p.id] = p
	t.order = append(t.order, p.id)
	t.prune()
	t.mu.Unlock()
	ss.lastUsed = time.Now()
	ss.mu.Unlock()

	go func() {
		defer close(p.done)
		res := ss.exec(ctx, e, ringWriter{p: p}, ringWriter{p: p, stderr: true}, true)
		cancel()
		if e.StdinStream != nil {
			e.StdinStream.Close()
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.stdin != nil {
			p.stdin.Close()
			p.stdin = nil
		}
		if p.killed && res.Outcome == OutcomeRunnerFailure {
			// A guest-backed rung reports the cancelled call, not an exit status.
			res = &ExecResult{Outcome: OutcomeExit, ExitCode: 128 + 9, Duration: time.Since(p.started), Error: "killed"}
		}
		p.result = res
		p.notify()
	}()
	return p.status(), nil
}

// Processes lists the sandbox's background processes, oldest first.
func (ss *Session) Processes() []ProcessStatus {
	var out []ProcessStatus
	for _, p := range ss.procs.all() {
		out = append(out, p.status())
	}
	return out
}

// RunningProcesses counts the background processes still running.
func (ss *Session) RunningProcesses() int { return ss.procs.running() }

func (ss *Session) touch() {
	ss.mu.Lock()
	ss.lastUsed = time.Now()
	ss.mu.Unlock()
}

// ProcessOutput reads a process's output from the given offsets, at most
// limit bytes per stream (default and cap: its buffer size). With wait, it
// blocks until there is output past the offsets, the process ends, or wait
// passes (at most 30 seconds), whichever is first.
func (ss *Session) ProcessOutput(ctx context.Context, id string, stdoutOffset, stderrOffset int64, limit int, wait time.Duration) (ProcessOutput, error) {
	if err := ss.alive(); err != nil {
		return ProcessOutput{}, err
	}
	p, err := ss.procs.get(id)
	if err != nil {
		return ProcessOutput{}, err
	}
	ss.touch()
	wait = min(wait, maxProcessWait)
	deadline := time.NewTimer(max(wait, 0))
	defer deadline.Stop()
	for {
		p.mu.Lock()
		fresh := p.stdout.total > stdoutOffset || p.stderr.total > stderrOffset || p.result != nil
		if fresh || wait <= 0 {
			break
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
			continue
		case <-deadline.C:
		case <-ctx.Done():
		}
		p.mu.Lock()
		break
	}
	defer p.mu.Unlock()
	if limit <= 0 || limit > p.stdout.size {
		limit = p.stdout.size
	}
	out := ProcessOutput{Status: p.statusLocked()}
	out.Stdout, out.StdoutOffset, out.StdoutDropped = p.stdout.read(stdoutOffset, limit)
	out.Stderr, out.StderrOffset, out.StderrDropped = p.stderr.read(stderrOffset, limit)
	return out, nil
}

// WriteProcessStdin writes to a process's stdin and, with closeAfter, closes it.
func (ss *Session) WriteProcessStdin(id string, data []byte, closeAfter bool) error {
	if err := ss.alive(); err != nil {
		return err
	}
	p, err := ss.procs.get(id)
	if err != nil {
		return err
	}
	ss.touch()
	p.mu.Lock()
	w := p.stdin
	if closeAfter {
		p.stdin = nil
	}
	ended := p.result != nil
	p.mu.Unlock()
	if w == nil {
		if ended {
			return fmt.Errorf("process %s has ended", id)
		}
		if len(data) == 0 && closeAfter {
			return nil
		}
		return fmt.Errorf("process %s has no open stdin (start it with stdin, and do not close it before writing)", id)
	}
	if len(data) > 0 {
		_ = w.SetWriteDeadline(time.Now().Add(stdinWriteTimeout))
		if _, err := w.Write(data); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				err = fmt.Errorf("process %s did not read its stdin within %s", id, stdinWriteTimeout)
			}
			if closeAfter {
				w.Close()
			}
			return err
		}
	}
	if closeAfter {
		return w.Close()
	}
	return nil
}

// KillProcess kills a process (its whole process group, and its cgroup where
// there is one) and waits for it to be reaped.
func (ss *Session) KillProcess(id string) (ProcessStatus, error) {
	p, err := ss.procs.get(id)
	if err != nil {
		return ProcessStatus{}, err
	}
	ss.touch()
	kill(p)
	return p.status(), nil
}

func kill(p *process) {
	p.mu.Lock()
	if p.result == nil {
		p.killed = true
	}
	p.mu.Unlock()
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(processKillWait):
	}
}

// killProcesses kills every background process, for Close.
func (ss *Session) killProcesses() {
	var wg sync.WaitGroup
	for _, p := range ss.procs.all() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kill(p)
		}()
	}
	wg.Wait()
}
