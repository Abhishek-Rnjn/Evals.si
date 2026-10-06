package guest

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	guestv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1/guestv1alpha1connect"
)

type fakeSystem struct {
	entropy []byte
	clock   time.Time
}

func (f *fakeSystem) AddEntropy(b []byte) error { f.entropy = b; return nil }
func (f *fakeSystem) SetTime(t time.Time) error { f.clock = t; return nil }

func serve(t *testing.T, a *Agent) guestv1alpha1connect.GuestAgentServiceClient {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "guest.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = a.Serve(ln) }()
	t.Cleanup(func() { ln.Close(); a.Close() })
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{Protocols: p, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	return guestv1alpha1connect.NewGuestAgentServiceClient(hc, "http://guest", connect.WithGRPC())
}

func run(t *testing.T, c guestv1alpha1connect.GuestAgentServiceClient, req *guestv1alpha1.ExecRequest) (string, string, *guestv1alpha1.ExecResult) {
	t.Helper()
	stream, err := c.Exec(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	var res *guestv1alpha1.ExecResult
	for stream.Receive() {
		switch ev := stream.Msg().GetEvent().(type) {
		case *guestv1alpha1.ExecResponse_Stdout:
			out.Write(ev.Stdout)
		case *guestv1alpha1.ExecResponse_Stderr:
			errb.Write(ev.Stderr)
		case *guestv1alpha1.ExecResponse_Result:
			res = ev.Result
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), res
}

func TestAgentRunsCommandsAndFiles(t *testing.T) {
	root := t.TempDir()
	sys := &fakeSystem{}
	c := serve(t, &Agent{Root: root, System: sys})
	ctx := context.Background()
	if h, err := c.Health(ctx, connect.NewRequest(&guestv1alpha1.HealthRequest{})); err != nil || h.Msg.GetVersion() != Version {
		t.Fatalf("health %v %v", h, err)
	}
	if _, err := c.WriteFiles(ctx, connect.NewRequest(&guestv1alpha1.WriteFilesRequest{
		Dirs:  []string{"/work/empty"},
		Files: []*guestv1alpha1.File{{Path: "/work/in.txt", Content: []byte("hi")}},
	})); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "GREETING=hello"}
	out, errOut, res := run(t, c, &guestv1alpha1.ExecRequest{Command: []string{"sh", "-c", "cat in.txt; echo \" $GREETING\"; echo e >&2; ls -d empty; exit 4"}, Env: env, Cwd: "/work"})
	if out != "hi hello\nempty\n" || errOut != "e\n" || res.GetExitCode() != 4 {
		t.Fatalf("exec %q %q %v", out, errOut, res)
	}
	_, _, res = run(t, c, &guestv1alpha1.ExecRequest{Command: []string{"sleep", "5"}, Env: env, Timeout: durationpb.New(100 * time.Millisecond)})
	if !res.GetTimedOut() {
		t.Errorf("timeout %v", res)
	}
	out, _, res = run(t, c, &guestv1alpha1.ExecRequest{Command: []string{"sh", "-c", "yes | head -c 5000"}, Env: env, OutputLimitBytes: 100})
	if len(out) != 100 || !res.GetTruncated() {
		t.Errorf("output cap: %d %v", len(out), res)
	}
	_, _, res = run(t, c, &guestv1alpha1.ExecRequest{Command: []string{"no-such-command-evalsi"}, Env: env})
	if res.GetExitCode() != 127 {
		t.Errorf("missing command %v", res)
	}
	// Files stay inside the root, whatever symlinks the command planted.
	run(t, c, &guestv1alpha1.ExecRequest{Command: []string{"ln", "-s", "/etc", "/proc/self/cwd/escape"}, Env: env, Cwd: "/work"})
	_ = os.Symlink("/etc", filepath.Join(root, "work", "escape"))
	read, err := c.ReadFiles(ctx, connect.NewRequest(&guestv1alpha1.ReadFilesRequest{Paths: []string{"/work", "/work/escape/hostname", "/nope"}}))
	if err == nil {
		for _, f := range read.Msg.GetFiles() {
			if strings.Contains(f.GetPath(), "escape") {
				t.Errorf("followed a symlink out: %s", f.GetPath())
			}
		}
	}
	if _, err := c.WriteFiles(ctx, connect.NewRequest(&guestv1alpha1.WriteFilesRequest{Files: []*guestv1alpha1.File{{Path: "/work/escape/evalsi-test", Content: []byte("x")}}})); err == nil {
		t.Error("wrote through a symlink")
	}
	if _, err := c.WriteFiles(ctx, connect.NewRequest(&guestv1alpha1.WriteFilesRequest{Files: []*guestv1alpha1.File{{Path: "relative", Content: []byte("x")}}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("relative path: %v", err)
	}
	now := time.Now()
	if _, err := c.Refresh(ctx, connect.NewRequest(&guestv1alpha1.RefreshRequest{Entropy: []byte("seed"), UnixNanos: now.UnixNano()})); err != nil {
		t.Fatal(err)
	}
	if string(sys.entropy) != "seed" || !sys.clock.Equal(time.Unix(0, now.UnixNano())) {
		t.Errorf("refresh: %q %v", sys.entropy, sys.clock)
	}
}

func TestAgentEgressRelaysToTheHost(t *testing.T) {
	hostSock := filepath.Join(t.TempDir(), "host_1025")
	ln, err := net.Listen("unix", hostSock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	c := serve(t, &Agent{Root: t.TempDir(), DialHost: func(port uint32) (net.Conn, error) {
		if port != 1025 {
			t.Errorf("port %d", port)
		}
		return net.Dial("unix", hostSock)
	}})
	resp, err := c.StartEgress(context.Background(), connect.NewRequest(&guestv1alpha1.StartEgressRequest{Listen: "127.0.0.1:0", VsockPort: 1025}))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", resp.Msg.GetListen())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("relay: %q %v", buf, err)
	}
}

// In a pod the agent is on the network: calls need its token, and commands
// run with the container's environment under their own variables.
func TestAgentInAPod(t *testing.T) {
	a := &Agent{Root: t.TempDir(), Token: "s3cret", BaseEnv: []string{"PATH=/usr/bin:/bin", "IMAGE_VAR=from-image", "LANG=C"}}
	client := serve(t, a)
	run := func(c guestv1alpha1connect.GuestAgentServiceClient, token string) (string, error) {
		req := connect.NewRequest(&guestv1alpha1.ExecRequest{
			Command: []string{"sh", "-c", "echo $IMAGE_VAR $LANG"}, Env: []string{"LANG=C.UTF-8"},
		})
		if token != "" {
			req.Header().Set("Authorization", "Bearer "+token)
		}
		stream, err := c.Exec(context.Background(), req)
		if err != nil {
			return "", err
		}
		var out string
		for stream.Receive() {
			out += string(stream.Msg().GetStdout())
		}
		return out, stream.Err()
	}
	if _, err := run(client, ""); connect.CodeOf(err) != connect.CodeUnauthenticated && connect.CodeOf(err) != connect.CodeUnknown {
		t.Fatalf("no token: %v", err)
	} else if err == nil {
		t.Fatal("a call without the token succeeded")
	}
	if _, err := run(client, "wrong"); err == nil {
		t.Fatal("a call with a wrong token succeeded")
	}
	out, err := run(client, "s3cret")
	if err != nil || out != "from-image C.UTF-8\n" {
		t.Fatalf("out %q err %v", out, err)
	}
}
