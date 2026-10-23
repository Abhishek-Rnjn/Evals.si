package sandbox

// guestConn is a sandbox driven through evalsi-guest's GuestAgentService:
// a Firecracker VM (over vsock) or a sandbox pod (over the pod network).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	guestv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1/guestv1alpha1connect"
)

type guestConn struct {
	driver string
	sp     *Spec
	guest  guestv1alpha1connect.GuestAgentServiceClient
	// The image's ENV, under the sandbox's variables (a VM). In a pod the
	// agent supplies the container's own environment instead (agentEnv).
	imageEnv []string
	agentEnv bool
	proxy    *egressProxy
	// Where the guest's egress forwarder listens, inside the guest.
	proxyAddr string
}

func (vm *guestConn) mkdir(ctx context.Context, p string) error {
	if _, err := vm.guest.WriteFiles(ctx, connect.NewRequest(&guestv1alpha1.WriteFilesRequest{Dirs: []string{p}})); err != nil {
		if vm.driver == "pod" && strings.Contains(strings.ToLower(err.Error()), "permission denied") {
			// The image runs as a user that cannot create the workdir. With
			// run_as_user set, the workdir is a volume that user owns.
			return fmt.Errorf("creating the workdir %s: %w (the image's user cannot create it: set the pod rung's run_as_user, or a SandboxClass's pod.runAsUser, to a non-zero user with capabilities [], or use an image where it exists and is writable)", p, err)
		}
		return fmt.Errorf("creating the workdir %s: %w", p, err)
	}
	return nil
}

func (vm *guestConn) env(e *Exec) []string {
	vars := map[string]string{}
	if !vm.agentEnv {
		vars["PATH"], vars["LANG"] = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "C.UTF-8"
	}
	for _, kv := range vm.imageEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	vars["HOME"], vars["TMPDIR"], vars["EVALSI_WORKDIR"] = vm.sp.Workdir, "/tmp", vm.sp.Workdir
	if vm.proxy != nil {
		url := "http://" + vm.proxyAddr
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
			vars[k] = url
		}
		vars["NO_PROXY"], vars["no_proxy"] = "localhost,127.0.0.1,::1", "localhost,127.0.0.1,::1"
	}
	for k, v := range vm.sp.Env {
		vars[k] = v
	}
	for k, v := range e.Env {
		vars[k] = v
	}
	if vm.proxy != nil && len(vm.sp.Services) > 0 {
		// Private services resolve to this pod's loopback, so they must not
		// travel through the pool's external egress proxy. Keep caller exclusions.
		for _, key := range []string{"NO_PROXY", "no_proxy"} {
			hosts := []string{}
			if vars[key] != "" {
				hosts = append(hosts, vars[key])
			}
			for _, service := range vm.sp.Services {
				hosts = append(hosts, service.Name)
			}
			vars[key] = strings.Join(hosts, ",")
		}
	}
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

func (vm *guestConn) exec(ctx context.Context, e *Exec, stdout, stderr io.Writer) *ExecResult {
	cwd := vm.sp.Workdir
	if e.Cwd != "" {
		cwd = path.Join(cwd, e.Cwd)
	}
	sniff := &capped{limit: 64 << 10, keep: true}
	res := vm.run(ctx, e, stdout, io.MultiWriter(stderr, sniff), cwd)
	classify(vm.driver, res, sniff.String())
	return res
}

func (vm *guestConn) run(ctx context.Context, e *Exec, stdout, stderr io.Writer, cwd string) *ExecResult {
	r := vm.sp.Resources
	limits := &guestv1alpha1.Limits{MemoryMb: int32(r.MemoryMB), MaxProcs: int32(r.MaxProcs), MaxFileMb: int32(r.MaxFileMB), CpuSeconds: int32(r.CPUSeconds)}
	if limits.MemoryMb == 0 {
		limits.MemoryMb = defaultMemoryMB
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	req := &guestv1alpha1.ExecRequest{
		Command: e.Command, Stdin: e.Stdin, Env: vm.env(e), Cwd: cwd, Timeout: durationpb.New(timeout),
		OutputLimitBytes: int32(e.OutputLimit), Limits: limits,
	}
	var result *guestv1alpha1.ExecResult
	var streamErr error
	if e.StdinStream != nil {
		result, streamErr = vm.runInteractive(ctx, req, e.StdinStream, stdout, stderr)
	} else {
		stream, err := vm.guest.Exec(ctx, connect.NewRequest(req))
		if err != nil {
			return &ExecResult{Outcome: OutcomeRunnerFailure, ExitCode: -1, Error: "guest agent: " + err.Error()}
		}
		defer stream.Close()
		for stream.Receive() {
			switch ev := stream.Msg().GetEvent().(type) {
			case *guestv1alpha1.ExecResponse_Stdout:
				_, _ = stdout.Write(ev.Stdout)
			case *guestv1alpha1.ExecResponse_Stderr:
				_, _ = stderr.Write(ev.Stderr)
			case *guestv1alpha1.ExecResponse_Result:
				result = ev.Result
			}
		}
		streamErr = stream.Err()
	}
	if result == nil {
		msg := "the guest agent ended the command without a result"
		if streamErr != nil {
			msg = "guest agent: " + streamErr.Error()
		}
		return &ExecResult{Outcome: OutcomeRunnerFailure, ExitCode: -1, Error: msg}
	}
	res := &ExecResult{
		Outcome: OutcomeExit, ExitCode: int(result.GetExitCode()), Duration: result.GetDuration().AsDuration(),
		Truncated: result.GetTruncated(), CPU: result.GetCpuTime().AsDuration(), MaxRSS: result.GetMaxRssBytes(),
	}
	if result.GetTimedOut() {
		res.Outcome, res.ExitCode, res.Error = OutcomeTimeout, -1, "timed out after "+timeout.String()
	}
	return res
}

// runInteractive runs a command through ExecInteractive, copying stdin (the
// read end of a pipe) to it until the pipe closes, which closes the command's
// stdin. Cancelling ctx ends the call, and the guest kills the command.
func (vm *guestConn) runInteractive(ctx context.Context, req *guestv1alpha1.ExecRequest, stdin io.Reader, stdout, stderr io.Writer) (*guestv1alpha1.ExecResult, error) {
	stream := vm.guest.ExecInteractive(ctx)
	defer stream.CloseResponse()
	if err := stream.Send(&guestv1alpha1.ExecInteractiveRequest{Event: &guestv1alpha1.ExecInteractiveRequest_Start{Start: req}}); err != nil {
		return nil, err
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				if stream.Send(&guestv1alpha1.ExecInteractiveRequest{Event: &guestv1alpha1.ExecInteractiveRequest_Stdin{Stdin: chunk}}) != nil {
					return
				}
			}
			if err != nil {
				_ = stream.Send(&guestv1alpha1.ExecInteractiveRequest{Event: &guestv1alpha1.ExecInteractiveRequest_CloseStdin{CloseStdin: true}})
				_ = stream.CloseRequest()
				return
			}
		}
	}()
	// A cancelled call must not wait for the guest: closing both ends ends it.
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = stream.CloseRequest()
			_ = stream.CloseResponse()
		case <-finished:
		}
	}()
	var result *guestv1alpha1.ExecResult
	for {
		msg, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return result, err
		}
		switch ev := msg.GetEvent().(type) {
		case *guestv1alpha1.ExecInteractiveResponse_Stdout:
			_, _ = stdout.Write(ev.Stdout)
		case *guestv1alpha1.ExecInteractiveResponse_Stderr:
			_, _ = stderr.Write(ev.Stderr)
		case *guestv1alpha1.ExecInteractiveResponse_Result:
			result = ev.Result
		}
	}
}

// guestPath maps a sandbox file path to an absolute guest path.
func (vm *guestConn) guestPath(name string) (string, error) {
	if path.IsAbs(name) {
		return path.Clean(name), nil
	}
	r, err := relPath(name)
	if err != nil {
		return "", err
	}
	return path.Join(vm.sp.Workdir, r), nil
}

func (vm *guestConn) writeFiles(files []File) error {
	req := &guestv1alpha1.WriteFilesRequest{}
	for _, f := range files {
		p, err := vm.guestPath(f.Path)
		if err != nil {
			return err
		}
		req.Files = append(req.Files, &guestv1alpha1.File{Path: p, Content: f.Content, Mode: uint32(f.Mode.Perm())})
	}
	_, err := vm.guest.WriteFiles(context.Background(), connect.NewRequest(req))
	return err
}

func (vm *guestConn) readFiles(paths []string, maxBytes int64) ([]File, []string, bool, error) {
	req := &guestv1alpha1.ReadFilesRequest{MaxBytes: maxBytes}
	names := map[string]string{}
	for _, p := range paths {
		g, err := vm.guestPath(p)
		if err != nil {
			return nil, nil, false, err
		}
		req.Paths = append(req.Paths, g)
		names[g] = p
	}
	resp, err := vm.guest.ReadFiles(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, nil, false, err
	}
	var files []File
	for _, f := range resp.Msg.GetFiles() {
		// Name files as the caller did: relative to its request path.
		name := f.GetPath()
		for g, orig := range names {
			if name == g {
				name = orig
			} else if rest, ok := strings.CutPrefix(name, strings.TrimSuffix(g, "/")+"/"); ok {
				name = path.Join(orig, rest)
			}
		}
		files = append(files, File{Path: name, Content: f.GetContent(), Mode: os.FileMode(f.GetMode()).Perm()})
	}
	var missing []string
	for _, m := range resp.Msg.GetMissing() {
		missing = append(missing, names[m])
	}
	return files, missing, resp.Msg.GetTruncated(), nil
}

func (vm *guestConn) egress() []EgressEvent {
	if vm.proxy == nil {
		return nil
	}
	return vm.proxy.Events()
}
