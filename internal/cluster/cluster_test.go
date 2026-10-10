package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/nats-io/nats.go/jetstream"

	harnessv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/harness/v1alpha1"
	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// embedded starts a cluster with an embedded NATS server, and returns a
// connector for more clients of the same server.
func embedded(t *testing.T, ackWait string) (*Cluster, func(name string) *Cluster) {
	t.Helper()
	ctx := context.Background()
	c, err := Connect(ctx, Config{Embedded: &Embedded{Listen: freePort(t), StoreDir: t.TempDir()}, AckWait: ackWait}, "test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, func(name string) *Cluster {
		other, err := Connect(ctx, Config{URL: c.ClientURL(), AckWait: ackWait}, name, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(other.Close)
		return other
	}
}

// fakeWorker answers like a Python worker, and counts what it is asked.
type fakeWorker struct {
	name  string
	mu    sync.Mutex
	calls []string
	block chan struct{} // Evaluate waits on it, when set
}

func (f *fakeWorker) note(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeWorker) Describe(context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	f.note("Describe")
	return []*evalsiv1alpha1.EvaluatorManifest{
		{Name: "builtin/exact-match"},
		{Name: "builtin/llm-judge", Requires: &evalsiv1alpha1.Requirements{Judge: true}},
		{Name: "builtin/unit-tests", Requires: &evalsiv1alpha1.Requirements{Isolation: evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_NAMESPACED}},
	}, nil
}

func (f *fakeWorker) Evaluate(ctx context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	f.note("Evaluate " + req.GetEvaluator())
	if f.block != nil {
		<-f.block
	}
	if req.GetEvaluator() == "broken" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("bad params"))
	}
	var out []*evalsiv1alpha1.EvaluationResult
	for _, r := range req.GetRecords() {
		out = append(out, &evalsiv1alpha1.EvaluationResult{RecordId: r.GetId(), Evaluator: req.GetEvaluator() + "@" + f.name})
	}
	return &pluginv1alpha1.EvaluateResponse{Results: out}, nil
}

func (f *fakeWorker) Reduce(context.Context, *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	return &pluginv1alpha1.ReduceResponse{}, nil
}

func (f *fakeWorker) Generate(context.Context, *pluginv1alpha1.GenerateRequest) (*pluginv1alpha1.GenerateResponse, error) {
	f.note("Generate")
	return &pluginv1alpha1.GenerateResponse{}, nil
}

func (f *fakeWorker) LoadDataset(_ context.Context, req *pluginv1alpha1.LoadDatasetRequest) ([]*evalsiv1alpha1.Record, error) {
	f.note("LoadDataset")
	// Big enough to travel through the payload store both ways.
	var out []*evalsiv1alpha1.Record
	for i := range 2000 {
		out = append(out, &evalsiv1alpha1.Record{Id: fmt.Sprint(i), Input: &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: strings.Repeat("x", 500)}}})
	}
	return out, nil
}

func (f *fakeWorker) RunTask(_ context.Context, req *pluginv1alpha1.RunTaskRequest, onEvent func(*harnessv1alpha1.TrajectoryEvent)) (*pluginv1alpha1.TaskResult, error) {
	f.note("RunTask")
	for range 3 {
		onEvent(&harnessv1alpha1.TrajectoryEvent{})
	}
	return &pluginv1alpha1.TaskResult{Error: "done " + req.GetRecord().GetId()}, nil
}

func serve(t *testing.T, c *Cluster, pools []string, w *fakeWorker) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Serve(ctx, pools, w, 4, quiet) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func TestWorkQueue(t *testing.T) {
	ctrl, connect2 := embedded(t, "")
	cpu, judge, harness := &fakeWorker{name: "cpu"}, &fakeWorker{name: "judge"}, &fakeWorker{name: "harness"}
	serve(t, connect2("cpu"), []string{"cpu", "sandbox"}, cpu)
	serve(t, connect2("judge"), []string{"judge"}, judge)
	serve(t, connect2("harness"), []string{"harness"}, harness)
	w := NewWorker(ctrl)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	manifests, err := w.Describe(ctx)
	if err != nil || len(manifests) != 3 {
		t.Fatalf("Describe = %v %v", manifests, err)
	}
	// Routing by manifest: judge evaluators to the judge pool, sandboxed ones
	// to sandbox (served here by the cpu worker), the rest to cpu.
	for evaluator, want := range map[string]string{"exact-match": "cpu", "llm-judge": "judge", "builtin/unit-tests": "cpu"} {
		resp, err := w.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{Evaluator: evaluator, Records: []*evalsiv1alpha1.Record{{Id: "r"}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.GetResults()[0].GetEvaluator(); got != evaluator+"@"+want {
			t.Errorf("%s answered by %s", evaluator, got)
		}
	}
	if pool := w.route("unit-tests"); pool != "sandbox" {
		t.Errorf("unit-tests routed to %s", pool)
	}

	// A large response goes through the payload store.
	records, err := w.LoadDataset(ctx, &pluginv1alpha1.LoadDatasetRequest{})
	if err != nil || len(records) != 2000 {
		t.Fatalf("LoadDataset = %d records, %v", len(records), err)
	}
	// Streaming: RunTask events arrive before the result.
	var events int
	res, err := w.RunTask(ctx, &pluginv1alpha1.RunTaskRequest{Record: &evalsiv1alpha1.Record{Id: "t1"}}, func(*harnessv1alpha1.TrajectoryEvent) { events++ })
	if err != nil || events != 3 || res.GetError() != "done t1" {
		t.Fatalf("RunTask = %v %v, %d events", res, err, events)
	}
	// Errors keep their code.
	_, err = w.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{Evaluator: "broken"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "bad params") {
		t.Errorf("error = %v", err)
	}
	if n, err := ctrl.Backlog(ctx, "cpu"); err != nil || n != 0 {
		t.Errorf("cpu backlog %d %v", n, err)
	}
}

// A worker that dies holding a task does not lose it: after the ack wait
// it goes to another worker.
func TestATaskOutlivesItsWorker(t *testing.T) {
	ctrl, connect2 := embedded(t, "1s")
	w := NewWorker(ctrl)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The first worker takes the task and is never heard from again: it
	// fetches the message and neither answers nor acknowledges it.
	dead := connect2("dead")
	cons, err := dead.js.Consumer(ctx, workStream, "pool-cpu")
	if err != nil {
		t.Fatal(err)
	}
	var answered atomic.Value
	go func() {
		resp, err := w.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{Evaluator: "exact-match", Records: []*evalsiv1alpha1.Record{{Id: "r"}}})
		if err == nil {
			answered.Store(resp.GetResults()[0].GetEvaluator())
		} else {
			answered.Store(err.Error())
		}
	}()
	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for range batch.Messages() {
		got++
	}
	if got != 1 {
		t.Fatalf("the dead worker fetched %d tasks", got)
	}
	dead.Close()

	serve(t, connect2("alive"), []string{"cpu"}, &fakeWorker{name: "alive"})
	deadline := time.Now().Add(20 * time.Second)
	for answered.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the task was not redelivered")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if a := answered.Load(); a != "exact-match@alive" {
		t.Fatalf("answered %v", a)
	}
}

// A long task heartbeats, so it is not redelivered while it runs.
func TestLongTasksAreNotRedelivered(t *testing.T) {
	ctrl, connect2 := embedded(t, "1s")
	slow := &fakeWorker{name: "slow", block: make(chan struct{})}
	serve(t, connect2("slow"), []string{"cpu"}, slow)
	other := &fakeWorker{name: "other"}
	serve(t, connect2("other"), []string{"cpu"}, other)
	w := NewWorker(ctrl)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := w.Evaluate(ctx, &pluginv1alpha1.EvaluateRequest{Evaluator: "e"})
		done <- err
	}()
	// Wait for one of the two to take it, then let 3 ack waits pass.
	time.Sleep(3500 * time.Millisecond)
	close(slow.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	slow.mu.Lock()
	other.mu.Lock()
	total := len(slow.calls) + len(other.calls)
	slow.mu.Unlock()
	other.mu.Unlock()
	if total != 1 {
		t.Errorf("the task ran %d times", total)
	}
}

func TestAuthzChangesReachEveryReplica(t *testing.T) {
	a, connect := embedded(t, "")
	b := connect("b")
	got := make(chan struct{}, 1)
	stop, err := b.OnAuthzChanged(func() { got <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := b.nc.Flush(); err != nil {
		t.Fatal(err)
	}
	a.AuthzChanged()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("replica b never heard of the change")
	}
}
