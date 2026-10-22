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
