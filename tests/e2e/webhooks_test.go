package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/webhooks"
	"google.golang.org/protobuf/proto"
)

// A run whose gate fails delivers run.finished and run.gate_failed to a
// webhook applied over REST, signed with the secret the apply returned.
func TestWebhooks(t *testing.T) {
	e := start(t)
	ctx := context.Background()

	var mu sync.Mutex
	type delivery struct {
		event, id, signature string
		body                 []byte
	}
	var got []delivery
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, delivery{r.Header.Get(webhooks.HeaderEvent), r.Header.Get(webhooks.HeaderDelivery), r.Header.Get(webhooks.HeaderSignature), body})
		n := len(got)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // the first attempt fails and is retried
		}
	}))
	t.Cleanup(receiver.Close)

	body, _ := json.Marshal(map[string]any{"name": "ci", "url": receiver.URL})
	resp, err := http.Post(e.base+"/v1alpha1/webhooks", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var reply struct{ Webhook struct{ Secret string } }
	_ = json.NewDecoder(resp.Body).Decode(&reply)
	applied := reply.Webhook
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || applied.Secret == "" {
		t.Fatalf("apply: %d, secret %q", resp.StatusCode, applied.Secret)
	}

	client := evalsiv1alpha1connect.NewRunServiceClient(h2cClient(), e.base, connect.WithGRPC())
	created, err := client.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Name: "gated", Spec: &evalsiv1alpha1.RunSpec{
		Dataset: &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: []*evalsiv1alpha1.Record{
			{Id: "a", Output: text("Paris"), Reference: text("Paris")},
			{Id: "b", Output: text("Lyon"), Reference: text("Paris")},
		}}}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
		Gates:      []*evalsiv1alpha1.Gate{{Metric: "exact-match", Min: proto.Float64(0.9)}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	runID := created.Msg.GetRun().GetId()

	// run.finished and run.gate_failed, one of them retried: three requests.
	deadline := time.Now().Add(60 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests after 60s, want 3", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	events, ids := map[string]bool{}, map[string]bool{}
	for _, d := range got {
		events[d.event], ids[d.id] = true, true
		if err := webhooks.Verify(applied.Secret, d.signature, d.body, time.Minute, time.Now()); err != nil {
			t.Errorf("%s: %v", d.event, err)
		}
		var p struct {
			Run struct{ ID, Status string }
		}
		_ = json.Unmarshal(d.body, &p)
		if p.Run.ID != runID || p.Run.Status != "RUN_STATUS_FAILED" {
			t.Errorf("%s payload: %s", d.event, d.body)
		}
	}
	// Two events, one of them sent twice (its first attempt got a 503), under the same delivery ID.
	if !events[webhooks.EventRunFinished] || !events[webhooks.EventRunGateFailed] || len(ids) != 2 || len(got) != 3 {
		t.Errorf("events %v, delivery IDs %v, %d requests", events, ids, len(got))
	}
}
