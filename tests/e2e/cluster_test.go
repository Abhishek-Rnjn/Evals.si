package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/cluster"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/server"
)

// slowModel answers "Paris" after a delay, so runs last long enough to move
// between replicas.
func slowModel(t *testing.T, delay time.Duration) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "slow",
			"choices": []any{map[string]any{"message": map[string]any{"content": "Paris"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

type replica struct {
	base string
	stop func()
}

// clusterConfig is a replica's or worker's config: Postgres in its own
// schema, the test's NATS server, short leases.
func clusterConfig(t *testing.T, dsn, natsURL string) config.Config {
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.DataDir = t.TempDir()
	cfg.Worker.Command = strings.Fields(os.Getenv("EVALSI_E2E_WORKER"))
	cfg.Worker.NoCache = true
	cfg.Evaluate.BatchSize = 2
	cfg.OTLP.Grace = "100ms"
	cfg.Storage.Postgres = &config.Postgres{DSN: dsn}
	cfg.Cluster = &cluster.Config{URL: natsURL, AckWait: "5s"}
	cfg.Runs.LeaseTTL, cfg.Runs.AdoptInterval = "2s", "500ms"
	return cfg
}

func startReplica(t *testing.T, cfg config.Config) replica {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), os.Stderr, ready) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("replica: %v", err)
		}
	}
	t.Cleanup(stop)
	select {
	case addr := <-ready:
		return replica{base: "http://" + addr, stop: stop}
	case err := <-done:
		t.Fatalf("replica exited: %v", err)
	case <-time.After(90 * time.Second):
		t.Fatal("replica did not start")
	}
	return replica{}
}

// TestCluster runs the Kubernetes topology in one process: two API replicas
// sharing PostgreSQL, a worker process with the Python worker, and a NATS
// server. Work goes through the pools' queues; a run created on one replica
// is watched and cancelled from the other; a replica that shuts down mid-run
// hands it off and the other finishes it; traces sent to either replica are
// evaluated by the elected policy engine.
func TestCluster(t *testing.T) {
	dsn := os.Getenv("EVALSI_E2E_POSTGRES_DSN")
	if os.Getenv("EVALSI_E2E_WORKER") == "" || dsn == "" {
		t.Skip("set EVALSI_E2E_WORKER and EVALSI_E2E_POSTGRES_DSN")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("cluster_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "search_path=" + schema

	// NATS outlives the replicas.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	natsAddr := l.Addr().String()
	l.Close()
	nats, err := cluster.Connect(ctx, cluster.Config{Embedded: &cluster.Embedded{Listen: natsAddr, StoreDir: t.TempDir()}}, "test-nats", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nats.Close)
	natsURL := nats.ClientURL()

	// The worker process: every pool.
	wctx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- server.RunWorker(wctx, clusterConfig(t, dsn, natsURL), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), os.Stderr)
	}()
	t.Cleanup(func() {
		stopWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker: %v", err)
		}
	})

	a := startReplica(t, clusterConfig(t, dsn, natsURL))
	b := startReplica(t, clusterConfig(t, dsn, natsURL))
	runsA := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), a.base, connect.WithGRPC())
	runsB := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), b.base, connect.WithGRPC())

	model := slowModel(t, 300*time.Millisecond)
	var recs []*evalsiv1alpha1.Record
	for i := range 60 {
		recs = append(recs, &evalsiv1alpha1.Record{Id: fmt.Sprintf("q%d", i), Input: text("Capital of France?"), Reference: text("Paris")})
	}
	spec := &evalsiv1alpha1.RunSpec{
		Target:     &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: "slow", BaseUrl: model},
		Dataset:    &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: recs}}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
	}

	// 1. Created on A, watched from B to the end.
	created, err := runsA.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	final := watch(t, runsB, created.Msg.GetRun().GetId())
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED || mean(final, "exact-match") != 1 {
		t.Fatalf("run watched from B: %v %s", final.GetStatus(), final.GetError())
	}

	// 2. Created on A, cancelled from B.
	created, err = runsA.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	waitProgress(t, runsB, created.Msg.GetRun().GetId())
	cancelled, err := runsB.CancelRun(ctx, connect.NewRequest(&evalsiv1alpha1.CancelRunRequest{Id: created.Msg.GetRun().GetId()}))
	if err != nil || cancelled.Msg.GetRun().GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED {
		t.Fatalf("cancel from B: %v %v", cancelled, err)
	}

	// 3. Created on A, which shuts down mid-run: B adopts and finishes it,
	// keeping the results A stored.
	created, err = runsA.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetRun().GetId()
	waitProgress(t, runsB, id)
	a.stop()
	// A handed the run off unfinished (rather than finishing or failing it).
	mid, err := runsB.GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if st := mid.Msg.GetRun().GetStatus(); st != evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING && st != evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING {
		t.Fatalf("after A stopped the run is %v; the takeover was not exercised", st)
	}
	doneBefore := mid.Msg.GetRun().GetProgress().GetDone()
	final = watch(t, runsB, id)
	if doneBefore == 0 || doneBefore >= final.GetProgress().GetTotal() {
		t.Errorf("A did %d of %d tasks before stopping", doneBefore, final.GetProgress().GetTotal())
	}
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED || mean(final, "exact-match") != 1 {
		t.Fatalf("adopted run: %v %s", final.GetStatus(), final.GetError())
	}
	results, err := runsB.ListRunResults(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: id, PageSize: 1000}))
	if err != nil || len(results.Msg.GetResults()) != len(recs) {
		t.Fatalf("adopted run has %d results (want %d, none twice): %v", len(results.Msg.GetResults()), len(recs), err)
	}

	// 4. Online evaluation: a policy applied on B; traces sent to B reach
	// the elected policy engine (B, now the only replica).
	policies := evalsiv1alpha1connect.NewMonitorServiceClient(h2cClient(), b.base, connect.WithGRPC())
	policy := &evalsiv1alpha1.OnlineEvalPolicy{
		Name: "answers", Selector: `service == "bot"`,
		Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: []*evalsiv1alpha1.EvaluatorRef{
			{Ref: "contains", Params: mustStruct(map[string]any{"substring": "Paris"})},
		}}},
	}
	if _, err := policies.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy})); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal([]any{map[string]any{"role": "assistant", "parts": []any{map[string]any{"type": "text", "content": "Paris"}}}})
	span, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{map[string]any{"key": "service.name", "value": map[string]any{"stringValue": "bot"}}}},
		"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{
			"traceId": fmt.Sprintf("%032x", 7), "spanId": fmt.Sprintf("%016x", 7), "name": "chat",
			"startTimeUnixNano": fmt.Sprint(time.Now().UnixNano()), "endTimeUnixNano": fmt.Sprint(time.Now().UnixNano() + 5e6),
			"attributes": []any{
				map[string]any{"key": "gen_ai.operation.name", "value": map[string]any{"stringValue": "chat"}},
				map[string]any{"key": "gen_ai.output.messages", "value": map[string]any{"stringValue": string(out)}},
			},
		}}}},
	}}})
	resp, err := http.Post(b.base+"/v1/traces", "application/json", bytes.NewReader(span))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("OTLP export: %v %v", resp, err)
	}
	resp.Body.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := policies.GetPolicyStats(ctx, connect.NewRequest(&evalsiv1alpha1.GetPolicyStatsRequest{Name: "answers"}))
		if err == nil && st.Msg.GetStats().GetTracesEvaluated() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the trace was not evaluated: %v %v", st, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func watch(t *testing.T, c evalsiv1alpha1connect.RunServiceClient, id string) *evalsiv1alpha1.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	stream, err := c.WatchRun(ctx, connect.NewRequest(&evalsiv1alpha1.WatchRunRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	var last *evalsiv1alpha1.Run
	for stream.Receive() {
		if r := stream.Msg().GetRun(); r != nil {
			last = r
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("watching %s: %v", id, err)
	}
	return last
}

func waitProgress(t *testing.T, c evalsiv1alpha1connect.RunServiceClient, id string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		r, err := c.GetRun(context.Background(), connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: id}))
		if err == nil && r.Msg.GetRun().GetProgress().GetDone() > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s made no progress: %v %v", id, r, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mean(r *evalsiv1alpha1.Run, metric string) float64 {
	for _, s := range r.GetSummaries() {
		if s.GetMetric() == metric {
			return s.GetMean()
		}
	}
	return -1
}
