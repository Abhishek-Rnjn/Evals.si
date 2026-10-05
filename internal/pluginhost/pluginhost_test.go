package pluginhost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1/pluginv1alpha1connect"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// fakeWorker is served by the test binary itself when re-executed as a helper.
type fakeWorker struct {
	pluginv1alpha1connect.UnimplementedEvaluatorPluginServiceHandler
	judges string
}

func (f *fakeWorker) Describe(context.Context, *connect.Request[pluginv1alpha1.DescribeRequest]) (*connect.Response[pluginv1alpha1.DescribeResponse], error) {
	return connect.NewResponse(&pluginv1alpha1.DescribeResponse{Evaluators: []*evalsiv1alpha1.EvaluatorManifest{
		{Name: "fake/echo", Version: "1.0.0", Description: f.judges},
	}}), nil
}

func (f *fakeWorker) Evaluate(_ context.Context, stream *connect.BidiStream[pluginv1alpha1.EvaluateRequest, pluginv1alpha1.EvaluateResponse]) error {
	for {
		req, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if req.GetEvaluator() == "crash" {
			os.Exit(3)
		}
		var results []*evalsiv1alpha1.EvaluationResult
		for _, r := range req.GetRecords() {
			results = append(results, &evalsiv1alpha1.EvaluationResult{RecordId: r.GetId(), Evaluator: req.GetEvaluator()})
		}
		if err := stream.Send(&pluginv1alpha1.EvaluateResponse{BatchId: req.GetBatchId(), Results: results}); err != nil {
			return err
		}
	}
}

// TestHelperWorker is not a real test: Start re-executes the test binary with
// EVALSI_HELPER_WORKER=1 to get a fake worker process.
func TestHelperWorker(t *testing.T) {
	if os.Getenv("EVALSI_HELPER_WORKER") != "1" {
		t.Skip("helper process")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	var listen, judgesPath string
	for i := 0; i < len(args)-1; i++ {
		switch args[i] {
		case "--listen":
			listen = strings.TrimPrefix(args[i+1], "unix://")
		case "--judges":
			judgesPath = args[i+1]
		}
	}
	judges, _ := os.ReadFile(judgesPath)
	ln, err := net.Listen("unix", listen)
	if err != nil {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.Handle(pluginv1alpha1connect.NewEvaluatorPluginServiceHandler(&fakeWorker{judges: string(judges)}))
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()
	_ = srv.Serve(ln)
	os.Exit(0)
}

func startFake(t *testing.T) *Process {
	t.Helper()
	p, err := Start(context.Background(), Options{
		Command:      []string{os.Args[0], "-test.run=^TestHelperWorker$", "--"},
		Env:          []string{"EVALSI_HELPER_WORKER=1"},
		Judges:       map[string]map[string]string{"j": {"provider": "anthropic", "model": "m"}},
		StartTimeout: 10 * time.Second,
		Output:       io.Discard,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p
}

func TestStartDescribeEvaluate(t *testing.T) {
	p := startFake(t)
	ctx := context.Background()
	manifests, err := p.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 1 || !strings.Contains(manifests[0].GetDescription(), `"model":"m"`) {
		t.Fatalf("judges were not passed to the worker: %v", manifests)
	}
	resp, err := p.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{
		BatchId:   "b1",
		Evaluator: "fake/echo",
		Records:   []*evalsiv1alpha1.Record{{Id: "a"}, {Id: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetBatchId() != "b1" || len(resp.GetResults()) != 2 {
		t.Fatalf("unexpected response %v", resp)
	}
}

func TestRestartsAfterCrash(t *testing.T) {
	p := startFake(t)
	ctx := context.Background()
	if _, err := p.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{Evaluator: "crash"}); err == nil {
		t.Fatal("expected the crashing call to fail")
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := p.Describe(ctx); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("worker was not restarted")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestStartFailsWhenWorkerExits(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Command:      []string{"false"},
		StartTimeout: 5 * time.Second,
		Output:       io.Discard,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("err = %v", err)
	}
}
