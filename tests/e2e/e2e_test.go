// Package e2e runs evalsid against a real Python evaluator worker.
//
// It is skipped unless EVALSI_E2E_WORKER holds the command that runs the
// evalsi CLI, for example:
//
//	EVALSI_E2E_WORKER="$PWD/python/.venv/bin/python -m evalsi" go test ./tests/e2e/
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/server"
)

// fakeJudge speaks the OpenAI chat-completions protocol and always gives 4/5.
func fakeJudge(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "fake-judge",
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": `{"reasoning": "fine", "score": 4}`},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func startServer(t *testing.T) string {
	t.Helper()
	worker := os.Getenv("EVALSI_E2E_WORKER")
	if worker == "" {
		t.Skip("set EVALSI_E2E_WORKER to run end-to-end tests")
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.Worker.Command = strings.Fields(worker)
	cfg.Worker.NoCache = true
	cfg.Judges = map[string]config.Judge{
		"local": {Provider: "openai-compatible", Model: "fake-judge", BaseURL: fakeJudge(t).URL + "/v1"},
	}
	cfg.DefaultJudge = "local"
	cfg.Evaluate.BatchSize = 2

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), os.Stderr, ready)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server.Run: %v", err)
		}
	})
	select {
	case addr := <-ready:
		return "http://" + addr
	case err := <-done:
		t.Fatalf("server exited: %v", err)
	case <-time.After(90 * time.Second):
		t.Fatal("server did not start")
	}
	return ""
}

func h2cClient() *http.Client {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: p, DialContext: (&net.Dialer{}).DialContext}}
}

func text(s string) *evalsiv1alpha1.Content {
	return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: s}}
}

// The quickstart's capital-city questions, as in examples/quickstart/qa.jsonl.
func records() []*evalsiv1alpha1.Record {
	pairs := [][3]string{
		{"capital-fr", "Paris", "Paris"}, {"capital-jp", "Kyoto", "Tokyo"}, {"capital-au", "canberra", "Canberra"},
		{"apples", "3 x 12 = 36, minus 4 leaves 32 apples.", "32"}, {"train", "The average speed is 80 km/h.", "80"},
		{"budget", "That comes to $14,000 per year.", "14400"},
	}
	var out []*evalsiv1alpha1.Record
	for _, p := range pairs {
		out = append(out, &evalsiv1alpha1.Record{Id: p[0], Input: text("q"), Output: text(p[1]), Reference: text(p[2])})
	}
	return out
}

func TestEndToEnd(t *testing.T) {
	base := startServer(t)
	ctx := context.Background()

	t.Run("catalog", func(t *testing.T) {
		client := evalsiv1alpha1connect.NewCatalogServiceClient(h2cClient(), base, connect.WithGRPC())
		resp, err := client.ListEvaluators(ctx, connect.NewRequest(&evalsiv1alpha1.ListEvaluatorsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, m := range resp.Msg.GetEvaluators() {
			names[m.GetName()] = true
		}
		if !names["builtin/exact-match"] || !names["builtin/llm-judge"] || resp.Msg.GetDefaultJudge() != "local" {
			t.Fatalf("unexpected catalog: %v", resp.Msg)
		}
	})

	t.Run("evaluate over gRPC", func(t *testing.T) {
		client := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), base, connect.WithGRPC())
		params, _ := structpb.NewStruct(map[string]any{"unit": "chars"})
		resp, err := client.Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
			Records: records(),
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{
				{Ref: "exact-match"}, {Ref: "numeric-match"}, {Ref: "llm-judge"},
				{Ref: "length", Name: "chars", Params: params},
			},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if got := len(resp.Msg.GetResults()); got != 24 {
			t.Fatalf("got %d results, want 24", got)
		}
		sums := map[string]*evalsiv1alpha1.MetricSummary{}
		for _, s := range resp.Msg.GetSummaries() {
			sums[s.GetMetric()] = s
		}
		// Same numbers the embedded Python runner reports for this data.
		em := sums["exact-match"]
		if math.Abs(em.GetMean()-1.0/3) > 1e-12 || math.Abs(em.GetCi().GetLow()-0.0968) > 1e-3 || math.Abs(em.GetCi().GetHigh()-0.7000) > 1e-3 {
			t.Errorf("exact-match = %v", em)
		}
		if nm := sums["numeric-match"]; nm.GetN() != 3 || nm.GetSkipped() != 3 {
			t.Errorf("numeric-match = %v", nm)
		}
		if j := sums["llm-judge"]; j.GetMean() != 0.75 || j.GetN() != 6 {
			t.Errorf("llm-judge = %v", j)
		}
		if c := sums["chars"]; c.GetN() != 6 {
			t.Errorf("chars = %v", c)
		}
	})

	t.Run("evaluate over plain HTTP JSON", func(t *testing.T) {
		body := `{"records": [{"id": "1", "output": {"text": "Paris"}, "reference": {"text": "paris"}}],
		          "evaluators": [{"ref": "exact-match"}]}`
		resp, err := http.Post(base+"/evalsi.v1alpha1.EvaluationService/Evaluate", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Results []struct {
				Outcome string
				Scores  []struct{ Passed bool }
			}
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || out.Results[0].Outcome != "OUTCOME_SCORED" || !out.Results[0].Scores[0].Passed {
			t.Fatalf("status %d, body %+v", resp.StatusCode, out)
		}
	})

	t.Run("bad request is InvalidArgument", func(t *testing.T) {
		client := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), base, connect.WithGRPC())
		_, err := client.Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "regex-match"}},
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("stream", func(t *testing.T) {
		client := evalsiv1alpha1connect.NewEvaluationServiceClient(h2cClient(), base, connect.WithGRPC())
		stream := client.EvaluateStream(ctx)
		_ = stream.Send(&evalsiv1alpha1.EvaluateStreamRequest{Message: &evalsiv1alpha1.EvaluateStreamRequest_Config{
			Config: &evalsiv1alpha1.EvaluateStreamConfig{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "fuzzy-match"}}},
		}})
		for _, r := range records() {
			_ = stream.Send(&evalsiv1alpha1.EvaluateStreamRequest{Message: &evalsiv1alpha1.EvaluateStreamRequest_Record{Record: r}})
		}
		_ = stream.CloseRequest()
		results, summaries := 0, 0
		for {
			msg, err := stream.Receive()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if msg.GetResult() != nil {
				results++
			}
			if msg.GetSummaries() != nil {
				summaries++
			}
		}
		if results != 6 || summaries != 1 {
			t.Fatalf("results=%d summaries=%d", results, summaries)
		}
	})

	t.Run("healthz", func(t *testing.T) {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})
}
