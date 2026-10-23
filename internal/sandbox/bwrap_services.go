package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Service groups on the bubblewrap rung (decision 0033).
//
// A sandbox with services is a process group on one private network. The group
// starts a holder process in new user and network namespaces (netns_linux.go);
// every service, and every command the agent runs, joins that network before its
// own bubblewrap sandbox starts, so they reach each other on loopback and nothing
// else. Each service has its own filesystem (a private copy of its image's root),
// its own environment, and none of the agent's files or credentials. Services start
// in dependency order, each after its readiness check passes. Their output is kept
// (the last 16 KiB) for status and logs. A service that exits is an
// infrastructure error at the next command boundary, as on the pod backend. All of
// it is killed when the sandbox is destroyed or its service lifetime ends.
//
// Limits: memory, file size and process count are rlimits, as for the agent's own
// commands; cpu_millis is validated but not enforced here (the pod backend does).
// The network is deny-only: the private network has no route out, and a service's
// own `setup` is the only step that may use the host's network.

const (
	// A service gets this long to become ready, and each setup command this long to run.
	serviceStartTimeout = 180 * time.Second
	serviceLogBytes     = 16 << 10
	readyPoll           = 250 * time.Millisecond
)

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

type serviceProc struct {
	spec ServiceSpec
	// The service's private root (a copy of its image), or "" on the host's system directories.
	root string
	logs *tailBuffer
	cmd  *exec.Cmd
	done chan struct{}
	// Valid once done is closed.
	exit  int
	ready atomic.Bool
}

type fileBind struct{ src, dest string }

type serviceGroup struct {
	b       *hostBackend
	d       *bwrapDriver
	holder  *netnsHolder
	nsenter string
	hosts   string
	dir     string
	procs   []*serviceProc
	// Files that sit inside a mounted volume, bound over it from the host, by service.
	fileBinds map[string][]fileBind
	timer     *time.Timer
	expired   atomic.Bool
	closed    sync.Once
}

func (d *bwrapDriver) supportsServices(sp *Spec) error {
	if runtime.GOOS != "linux" {
		return errors.New("services on bubblewrap need Linux")
	}
	if _, err := exec.LookPath("nsenter"); err != nil {
		return errors.New("services on bubblewrap need nsenter (util-linux) to share a private network")
	}
	if sp.Network.Mode != NetworkDeny {
		return errors.New("services on bubblewrap need network: deny (their private network has no route out)")
	}
	for _, s := range sp.Services {
		if s.Image == "" && len(s.Setup) > 0 {
			return fmt.Errorf("service %s: setup needs an image", s.Name)
		}
		if s.Image == "" {
			for name := range s.Files {
				if !under(path.Clean("/"+name), s.mountPoints()) {
					return fmt.Errorf("service %s: file %q needs an image (only files inside a volume or the workdir mount can be placed without one)", s.Name, name)
				}
			}
		}
	}
	return nil
}

func newServiceGroup(ctx context.Context, b *hostBackend, d *bwrapDriver) (*serviceGroup, error) {
	nsenter, err := exec.LookPath("nsenter")
	if err != nil {
		return nil, err
	}
	ordered, err := orderServices(b.sp.Services)
	if err != nil {
		return nil, err
	}
	holder, err := startNetNS(d.cfg.Launcher)
	if err != nil {
		return nil, err
	}
	g := &serviceGroup{b: b, d: d, holder: holder, nsenter: nsenter, dir: filepath.Join(b.dir, "svc"), fileBinds: map[string][]fileBind{}}
	fail := func(err error) (*serviceGroup, error) {
		g.close()
		return nil, err
	}
	if err := os.Mkdir(g.dir, 0o700); err != nil {
		return fail(err)
	}
	g.hosts = filepath.Join(g.dir, "hosts")
	if err := os.WriteFile(g.hosts, []byte(hostsFile(ordered)), 0o644); err != nil {
		return fail(err)
	}
	if b.root != "" {
		// A mount point for the generated hosts file in the agent's own root.
		if err := ensureFile(b.root, "etc/hosts"); err != nil {
			return fail(err)
		}
	}
	for _, s := range ordered {
		p, err := g.start(ctx, s)
		if err != nil {
			return fail(err)
		}
		g.procs = append(g.procs, p)
	}
	if life := b.sp.ServiceLifetime; life > 0 {
		g.timer = time.AfterFunc(life, func() {
			g.expired.Store(true)
			g.killAll()
		})
	}
	return g, nil
}

// hostsFile maps every service name and alias to loopback, the network they share.
func hostsFile(services []ServiceSpec) string {
	var sb strings.Builder
	sb.WriteString("127.0.0.1 localhost\n::1 localhost\n")
	for _, s := range services {
		names := append([]string{s.Name}, s.Aliases...)
		fmt.Fprintf(&sb, "127.0.0.1 %s\n", strings.Join(names, " "))
	}
	return sb.String()
}

// ensureFile makes rel exist as a regular file in root (an empty one when absent),
// without following links out of it.
func ensureFile(root, rel string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	if fi, err := r.Lstat(rel); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := r.Remove(rel); err != nil {
			return err
		}
	}
	f, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// writeRootFile writes content to rel in root.
func writeRootFile(root, rel string, content []byte) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if dir := path.Dir(rel); dir != "." {
		if err := r.MkdirAll(dir, 0o777); err != nil {
			return err
		}
	}
	f, err := r.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o666)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(content)
	return err
}

func (g *serviceGroup) volumeDir(name string) (string, error) {
	dir := filepath.Join(g.dir, "vol", name)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o777)
}

// env is a service's whole environment: nothing of the agent's.
func (g *serviceGroup) env(s ServiceSpec) ([]string, error) {
	vars := map[string]string{"PATH": sandboxPath, "LANG": "C.UTF-8", "HOME": "/tmp", "TMPDIR": "/tmp"}
	if s.Image != "" {
		img, err := g.b.sb.images.get(context.Background(), s.Image)
		if err != nil {
			return nil, err
		}
		for _, kv := range img.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				vars[k] = v
			}
		}
	}
	for k, v := range s.Env {
		vars[k] = v
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out, nil
}

// prepareRoot gives a service its private root: a copy of its image, with its files.
func (g *serviceGroup) prepareRoot(ctx context.Context, s ServiceSpec) (string, error) {
	if err := os.MkdirAll(filepath.Join(g.dir, s.Name), 0o700); err != nil {
		return "", err
	}
	root := ""
	if s.Image != "" {
		img, err := g.b.sb.images.get(ctx, s.Image)
		if err != nil {
			return "", err
		}
		root = filepath.Join(g.dir, s.Name, "root")
		if err := copyTree(ctx, img.Root, root); err != nil {
			return "", fmt.Errorf("service %s: copying the image root: %w", s.Name, err)
		}
		if err := ensureFile(root, "etc/hosts"); err != nil {
			return "", fmt.Errorf("service %s: %w", s.Name, err)
		}
	}
	mounts := s.mountPoints()
	names := make([]string, 0, len(s.Files))
	for name := range s.Files {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, name := range names {
		abs := path.Clean("/" + name)
		if !path.IsAbs(name) && s.Workdir != "" {
			abs = path.Join(s.Workdir, name)
		}
		content := []byte(s.Files[name])
		if under(abs, mounts) {
			// The path is inside a volume or the mounted workdir, which hides the
			// image's root there: bind the file over the mount from the host, as
			// docker does for a file mounted into a volume.
			dir := filepath.Join(g.dir, s.Name, "files")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			src := filepath.Join(dir, strconv.Itoa(i))
			if err := os.WriteFile(src, content, 0o644); err != nil {
				return "", err
			}
			g.fileBinds[s.Name] = append(g.fileBinds[s.Name], fileBind{src, abs})
			continue
		}
		if err := writeRootFile(root, strings.TrimPrefix(abs, "/"), content); err != nil {
			return "", fmt.Errorf("service %s: file %q: %w", s.Name, name, err)
		}
	}
	return root, nil
}

// mountPoints are the paths where a volume or the agent's workdir is mounted in s.
func (s ServiceSpec) mountPoints() []string {
	var out []string
	if s.WorkdirMount != "" {
		out = append(out, s.WorkdirMount)
	}
	for _, v := range s.Volumes {
		if _, dest, _, err := parseVolume(v); err == nil {
			out = append(out, dest)
		}
	}
	return out
}

func under(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// volumeArgs are the binds of named volumes, from the group's host directories.
func (g *serviceGroup) volumeArgs(volumes []string) ([]string, error) {
	var args []string
	for _, v := range volumes {
		name, dest, ro, err := parseVolume(v)
		if err != nil {
			return nil, err
		}
		dir, err := g.volumeDir(name)
		if err != nil {
			return nil, err
		}
		bind := "--bind"
		if ro {
			bind = "--ro-bind"
		}
		args = append(args, bind, dir, dest)
	}
	return args, nil
}

// bwrapArgs is the bubblewrap command line for one process of service s.
func (g *serviceGroup) bwrapArgs(s ServiceSpec, root string, hostNet bool, command []string) ([]string, error) {
	bin, err := g.d.binary()
	if err != nil {
		return nil, err
	}
	args := []string{bin, "--unshare-all", "--share-net", "--unshare-user", "--uid", "65534", "--gid", "65534",
		"--die-with-parent", "--new-session", "--cap-drop", "ALL"}
	rootArgs, err := g.d.rootArgs(root, "", true)
	if err != nil {
		return nil, err
	}
	args = append(args, rootArgs...)
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	if hostNet {
		if s.Image != "" {
			args = append(args, "--ro-bind-try", "/etc/resolv.conf", "/etc/resolv.conf")
		}
	} else {
		args = append(args, "--ro-bind", g.hosts, "/etc/hosts")
	}
	if s.WorkdirMount != "" {
		args = append(args, "--bind", g.b.work, s.WorkdirMount)
	}
	vol, err := g.volumeArgs(s.Volumes)
	if err != nil {
		return nil, err
	}
	args = append(args, vol...)
	for _, fb := range g.fileBinds[s.Name] {
		args = append(args, "--ro-bind", fb.src, fb.dest)
	}
	wd := s.Workdir
	if wd == "" && s.Image != "" {
		if img, err := g.b.sb.images.get(context.Background(), s.Image); err == nil {
			wd = img.WorkingDir
		}
	}
	if wd == "" {
		wd = "/"
	}
	return append(args, "--chdir", wd, "--seccomp", "3", "--"), nil
}

// launchSpec builds the launcher's spec for a command of service s, inside the
// group's network unless hostNet.
func (g *serviceGroup) launchSpec(s ServiceSpec, root string, hostNet bool, command []string) (*launchSpec, error) {
	args, err := g.bwrapArgs(s, root, hostNet, command)
	if err != nil {
		return nil, err
	}
	env, err := g.env(s)
	if err != nil {
		return nil, err
	}
	argv := append(args, command...)
	if !hostNet {
		argv = append(g.holder.nsenterArgs(g.nsenter), argv...)
	}
	spec := &launchSpec{Argv: argv, Env: env, Dir: "/", Seccomp: "fd"}
	limits(&Resources{MemoryMB: s.MemoryMB}, spec)
	return spec, nil
}

func (g *serviceGroup) start(ctx context.Context, s ServiceSpec) (*serviceProc, error) {
	root, err := g.prepareRoot(ctx, s)
	if err != nil {
		return nil, err
	}
	p := &serviceProc{spec: s, root: root, logs: &tailBuffer{limit: serviceLogBytes}, done: make(chan struct{})}
	for _, line := range s.Setup {
		spec, err := g.launchSpec(s, root, s.SetupNetwork == NetworkAllow, []string{"sh", "-c", line})
		if err != nil {
			return nil, err
		}
		var out capped
		out.limit, out.keep = 8192, true
		res := g.b.sb.launch(ctx, spec, nil, serviceStartTimeout, &out, &out)
		classify("bwrap", res, out.String())
		if res.Outcome != OutcomeExit || res.ExitCode != 0 {
			return nil, fmt.Errorf("service %s: setup %q failed (%s, exit %d): %s", s.Name, line, res.Outcome, res.ExitCode, tail(out.String()+res.Error))
		}
	}
	command, err := g.command(ctx, s)
	if err != nil {
		return nil, err
	}
	spec, err := g.launchSpec(s, root, false, command)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(g.d.cfg.Launcher, "sandbox-exec")
	cmd.Env = []string{specEnv + "=" + string(raw)}
	cmd.Stdout, cmd.Stderr = p.logs, p.logs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("service %s: %w", s.Name, err)
	}
	p.cmd = cmd
	go func() {
		err := cmd.Wait()
		p.exit = cmd.ProcessState.ExitCode()
		if p.exit < 0 && err != nil {
			p.exit = 137
		}
		close(p.done)
	}()
	if err := g.waitReady(ctx, p); err != nil {
		g.kill(p)
		return nil, fmt.Errorf("%w; output: %s", err, tail(p.logs.String()))
	}
	return p, nil
}

// command is the service's argv: its command, or with image_entrypoint the image's
// ENTRYPOINT followed by the command (the image's CMD when there is none).
func (g *serviceGroup) command(ctx context.Context, s ServiceSpec) ([]string, error) {
	if !s.ImageEntrypoint {
		return s.Command, nil
	}
	if s.Image == "" {
		return nil, fmt.Errorf("service %s: image_entrypoint needs an image", s.Name)
	}
	img, err := g.b.sb.images.get(ctx, s.Image)
	if err != nil {
		return nil, err
	}
	args := s.Command
	if len(args) == 0 {
		args = img.Cmd
	}
	argv := append(append([]string{}, img.Entrypoint...), args...)
	if len(argv) == 0 {
		return nil, fmt.Errorf("service %s: the image has no entrypoint or command and none was given", s.Name)
	}
	return argv, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 800 {
		s = "..." + s[len(s)-800:]
	}
	return s
}

func (g *serviceGroup) waitReady(ctx context.Context, p *serviceProc) error {
	s := p.spec
	started := time.Now()
	deadline := time.NewTimer(serviceStartTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(readyPoll)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			if s.Oneshot && p.exit == 0 {
				p.ready.Store(true)
				return nil
			}
			return fmt.Errorf("service %s exited during startup (exit %d)", s.Name, p.exit)
		default:
		}
		if !s.Oneshot && g.probe(ctx, p) {
			p.ready.Store(true)
			return nil
		}
		if !s.Oneshot && s.Port == 0 && len(s.ReadyCommand) == 0 && time.Since(started) > time.Second {
			// Nothing to check: it is ready once it has stayed up for a second.
			p.ready.Store(true)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("service %s was not ready after %s", s.Name, serviceStartTimeout)
		case <-tick.C:
		case <-p.done:
		}
	}
}

// probe is one readiness check: the service's ready_command run in its own
// sandbox, or a TCP connection to its port inside the private network.
func (g *serviceGroup) probe(ctx context.Context, p *serviceProc) bool {
	s := p.spec
	if len(s.ReadyCommand) > 0 {
		spec, err := g.launchSpec(s, p.root, false, s.ReadyCommand)
		if err != nil {
			return false
		}
		res := g.b.sb.launch(ctx, spec, nil, 10*time.Second, nil, nil)
		return res.Outcome == OutcomeExit && res.ExitCode == 0
	}
	if s.Port == 0 {
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	argv := append(g.holder.nsenterArgs(g.nsenter), g.d.cfg.Launcher, "sandbox-probe", "127.0.0.1:"+strconv.Itoa(s.Port))
	return exec.CommandContext(pctx, argv[0], argv[1:]...).Run() == nil
}

func (g *serviceGroup) kill(p *serviceProc) {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	}
}

func (g *serviceGroup) killAll() {
	for _, p := range g.procs {
		g.kill(p)
	}
}

// check is the command-boundary health check: a service that has exited (or whose
// lifetime ended) is an infrastructure failure, never a score.
func (g *serviceGroup) check() error {
	if g.expired.Load() {
		return errors.New("the services' lifetime ended")
	}
	for _, p := range g.procs {
		if p.spec.Oneshot {
			continue
		}
		select {
		case <-p.done:
			return fmt.Errorf("service %s is unhealthy (exited with %d): %s", p.spec.Name, p.exit, tail(p.logs.String()))
		default:
		}
	}
	return nil
}

func (g *serviceGroup) status(logs bool) []ServiceStatus {
	out := make([]ServiceStatus, 0, len(g.procs))
	for _, p := range g.procs {
		st := ServiceStatus{Name: p.spec.Name, Ready: p.ready.Load()}
		select {
		case <-p.done:
			st.Ready = p.spec.Oneshot && p.exit == 0
			if !p.spec.Oneshot || p.exit != 0 {
				st.Reason = fmt.Sprintf("exited (code %d)", p.exit)
			}
		default:
		}
		if g.expired.Load() {
			st.Ready, st.Reason = false, "lifetime ended"
		}
		if logs {
			st.Logs = p.logs.String()
		}
		out = append(out, st)
	}
	return out
}

func (g *serviceGroup) close() {
	g.closed.Do(func() {
		if g.timer != nil {
			g.timer.Stop()
		}
		g.killAll()
		g.holder.close()
	})
}

func (b *hostBackend) services(_ context.Context, logs bool) ([]ServiceStatus, error) {
	if b.group == nil {
		return nil, nil
	}
	return b.group.status(logs), nil
}
