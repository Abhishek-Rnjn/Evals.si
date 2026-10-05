package sandboxsvc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	sandboxv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/sandbox/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/sandbox/v1alpha1/sandboxv1alpha1connect"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/sandbox"
)

// The test binary doubles as the sandbox launcher, as evalsid does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sandbox-exec" {
		os.Exit(sandbox.Launch(os.Stderr))
	}
	os.Exit(m.Run())
}

func client(t *testing.T, ladder ...string) sandboxv1alpha1connect.SandboxServiceClient {
	t.Helper()
	sb, err := sandbox.New(sandbox.Config{Ladder: ladder, MinIsolation: "none"})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range sb.Probe(context.Background()) {
		if !st.Available {
			t.Skipf("%s unavailable: %s", st.Driver, st.Reason)
		}
	}
	m := sandbox.NewManager(sb)
	t.Cleanup(m.Close)
	socket := filepath.Join(t.TempDir(), "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := Serve(ctx, m, socket); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(socket); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	hc := &http.Client{Transport: &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	return sandboxv1alpha1connect.NewSandboxServiceClient(hc, "http://sandbox", connect.WithGRPC())
}

func exec(t *testing.T, c sandboxv1alpha1connect.SandboxServiceClient, id string, cmd ...string) (string, string, *sandboxv1alpha1.ExecResult) {
	t.Helper()
	stream, err := c.Exec(context.Background(), connect.NewRequest(&sandboxv1alpha1.ExecRequest{SandboxId: id, Command: cmd, Timeout: durationpb.New(20 * time.Second)}))
	if err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	var res *sandboxv1alpha1.ExecResult
	for stream.Receive() {
		switch ev := stream.Msg().GetEvent().(type) {
		case *sandboxv1alpha1.ExecResponse_Stdout:
			out.Write(ev.Stdout)
		case *sandboxv1alpha1.ExecResponse_Stderr:
			errb.Write(ev.Stderr)
		case *sandboxv1alpha1.ExecResponse_Result:
			res = ev.Result
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), res
}

func TestServiceLifecycle(t *testing.T) {
	c := client(t, "bwrap", "landlock")
	ctx := context.Background()
	probe, err := c.Probe(ctx, connect.NewRequest(&sandboxv1alpha1.ProbeRequest{}))
	if err != nil || len(probe.Msg.GetRungs()) != 2 {
		t.Fatalf("probe: %v %v", probe, err)
	}
	created, err := c.Create(ctx, connect.NewRequest(&sandboxv1alpha1.CreateRequest{Spec: &sandboxv1alpha1.SandboxSpec{
		Files: map[string][]byte{"in.txt": []byte("hi")}, Env: map[string]string{"GREETING": "hello"},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetSandboxId()
	iso := created.Msg.GetIsolation()
	if iso.GetDriver() != "bwrap" || iso.GetLevel() != evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_NAMESPACED || len(iso.GetNotes()) == 0 {
		t.Errorf("isolation %v", iso)
	}
	out, errOut, res := exec(t, c, id, "sh", "-c", "cat in.txt; echo \" $GREETING\"; echo oops >&2; echo made > out.txt; exit 3")
	if out != "hi hello\n" || errOut != "oops\n" || res.GetOutcome() != sandboxv1alpha1.Outcome_OUTCOME_EXIT || res.GetExitCode() != 3 {
		t.Fatalf("exec: %q %q %v", out, errOut, res)
	}
	if _, err := c.WriteFiles(ctx, connect.NewRequest(&sandboxv1alpha1.WriteFilesRequest{SandboxId: id, Files: []*sandboxv1alpha1.File{{Path: "w/x.txt", Content: []byte("x")}}})); err != nil {
		t.Fatal(err)
	}
	read, err := c.ReadFiles(ctx, connect.NewRequest(&sandboxv1alpha1.ReadFilesRequest{SandboxId: id, Paths: []string{"out.txt", "w", "nope"}}))
	if err != nil || len(read.Msg.GetFiles()) != 2 || read.Msg.GetMissing()[0] != "nope" {
		t.Fatalf("read: %v %v", read, err)
	}
	snap, err := c.Snapshot(ctx, connect.NewRequest(&sandboxv1alpha1.SnapshotRequest{SandboxId: id}))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := c.Restore(ctx, connect.NewRequest(&sandboxv1alpha1.RestoreRequest{SnapshotId: snap.Msg.GetSnapshotId()}))
	if err != nil {
		t.Fatal(err)
	}
	if out, _, _ := exec(t, c, restored.Msg.GetSandboxId(), "cat", "out.txt"); out != "made\n" {
		t.Errorf("restored: %q", out)
	}
	stats, err := c.Stats(ctx, connect.NewRequest(&sandboxv1alpha1.StatsRequest{SandboxId: id}))
	if err != nil || stats.Msg.GetExecs() != 1 {
		t.Fatalf("stats %v %v", stats, err)
	}
	if _, err := c.Destroy(ctx, connect.NewRequest(&sandboxv1alpha1.DestroyRequest{SandboxId: id})); err != nil {
		t.Fatal(err)
	}
	_, err = c.Destroy(ctx, connect.NewRequest(&sandboxv1alpha1.DestroyRequest{SandboxId: id}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("second destroy: %v", err)
	}
}

func TestServiceFailsClosed(t *testing.T) {
	c := client(t, "landlock")
	_, err := c.Create(context.Background(), connect.NewRequest(&sandboxv1alpha1.CreateRequest{Spec: &sandboxv1alpha1.SandboxSpec{MinIsolation: "namespaced"}}))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition || !strings.HasPrefix(ce.Message(), "SANDBOX_UNAVAILABLE") {
		t.Fatalf("err = %v", err)
	}
	_, err = c.Create(context.Background(), connect.NewRequest(&sandboxv1alpha1.CreateRequest{Spec: &sandboxv1alpha1.SandboxSpec{
		Network: &sandboxv1alpha1.NetworkPolicy{Mode: sandboxv1alpha1.NetworkMode_NETWORK_MODE_ALLOWLIST},
	}}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("allowlist without hosts: %v", err)
	}
}
