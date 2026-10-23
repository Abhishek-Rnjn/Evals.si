package guest

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	guestv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/guest/v1alpha1"
)

func TestExecMissingCwdIsNotCommandNotFound(t *testing.T) {
	c := serve(t, &Agent{Root: t.TempDir()})
	_, e, res := run(t, c, &guestv1alpha1.ExecRequest{Command: []string{"/bin/echo", "hi"}, Cwd: "/nope"})
	if res.ExitCode != 126 || !strings.Contains(res.Error, "working directory not found: /nope") || strings.Contains(e, "command not found") {
		t.Fatalf("exit=%d err=%q stderr=%q", res.ExitCode, res.Error, e)
	}
}

func TestReadFilesReportsSymlinkAsMissing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	c := serve(t, &Agent{Root: root})
	r, err := c.ReadFiles(context.Background(), connect.NewRequest(&guestv1alpha1.ReadFilesRequest{Paths: []string{"/link"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Msg.Files) != 0 || len(r.Msg.Missing) != 1 || r.Msg.Missing[0] != "/link" {
		t.Fatalf("files=%d missing=%v", len(r.Msg.Files), r.Msg.Missing)
	}
}

func TestEgressKeepsReplyAfterClientHalfClose(t *testing.T) {
	hostLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hostLn.Close()
	go func() {
		for {
			c, err := hostLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(io.Discard, c)
				time.Sleep(200 * time.Millisecond)
				_, _ = c.Write([]byte("reply"))
			}()
		}
	}()
	a := &Agent{Root: t.TempDir(), DialHost: func(uint32) (net.Conn, error) { return net.Dial("tcp", hostLn.Addr().String()) }}
	c := serve(t, a)
	r, err := c.StartEgress(context.Background(), connect.NewRequest(&guestv1alpha1.StartEgressRequest{Listen: "127.0.0.1:0", VsockPort: 1}))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", r.Msg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	_ = conn.(*net.TCPConn).CloseWrite()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, err := io.ReadAll(conn)
	if string(b) != "reply" {
		t.Fatalf("reply lost after client half-close: %q (%v)", b, err)
	}
}

func TestExecInteractiveKeepsStdinOpen(t *testing.T) {
	root := t.TempDir()
	c := serve(t, &Agent{Root: root})
	stream := c.ExecInteractive(context.Background())
	start := &guestv1alpha1.ExecRequest{Command: []string{"sh", "-c", "while read l; do echo got:$l; done; echo end"}, Cwd: "/", Timeout: durationpb.New(10 * time.Second)}
	if err := stream.Send(&guestv1alpha1.ExecInteractiveRequest{Event: &guestv1alpha1.ExecInteractiveRequest_Start{Start: start}}); err != nil {
		t.Fatal(err)
	}
	send := func(r *guestv1alpha1.ExecInteractiveRequest) {
		if err := stream.Send(r); err != nil {
			t.Fatal(err)
		}
	}
	send(&guestv1alpha1.ExecInteractiveRequest{Event: &guestv1alpha1.ExecInteractiveRequest_Stdin{Stdin: []byte("a\n")}})
	var out strings.Builder
	// read consumes events until out has want, or the result arrives.
	read := func(want string) *guestv1alpha1.ExecResult {
		for !strings.Contains(out.String(), want) {
			msg, err := stream.Receive()
			if err != nil {
				t.Fatalf("waiting for %q (have %q): %v", want, out.String(), err)
			}
			switch ev := msg.GetEvent().(type) {
			case *guestv1alpha1.ExecInteractiveResponse_Stdout:
				out.Write(ev.Stdout)
			case *guestv1alpha1.ExecInteractiveResponse_Result:
				return ev.Result
			}
		}
		return nil
	}
	read("got:a")
	send(&guestv1alpha1.ExecInteractiveRequest{Event: &guestv1alpha1.ExecInteractiveRequest_Stdin{Stdin: []byte("b\n")}})
	read("got:b")
	send(&guestv1alpha1.ExecInteractiveRequest{Event: &guestv1alpha1.ExecInteractiveRequest_CloseStdin{CloseStdin: true}})
	res := read("never-appears")
	if res == nil || res.GetExitCode() != 0 || !strings.Contains(out.String(), "end") {
		t.Fatalf("result %v out %q", res, out.String())
	}
}

func TestExecInteractiveCancelEndsTheCommand(t *testing.T) {
	root := t.TempDir()
	c := serve(t, &Agent{Root: root})
	ctx, cancel := context.WithCancel(context.Background())
	stream := c.ExecInteractive(ctx)
	start := &guestv1alpha1.ExecRequest{Command: []string{"sh", "-c", "echo $$ > pid; sleep 300"}, Cwd: "/", Timeout: durationpb.New(time.Minute)}
	if err := stream.Send(&guestv1alpha1.ExecInteractiveRequest{Event: &guestv1alpha1.ExecInteractiveRequest_Start{Start: start}}); err != nil {
		t.Fatal(err)
	}
	var pid []byte
	for range 100 {
		pid, _ = os.ReadFile(filepath.Join(root, "pid"))
		if len(pid) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(pid) == 0 {
		t.Fatal("command never started")
	}
	cancel()
	proc := "/proc/" + strings.TrimSpace(string(pid))
	for range 100 {
		if _, err := os.Stat(proc); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the command outlived the cancelled call")
}
