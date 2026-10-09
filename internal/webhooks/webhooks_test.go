package webhooks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// receiver is an endpoint that records what it is sent and answers with the
// statuses it is told to.
type receiver struct {
	mu       sync.Mutex
	statuses []int // consumed one per request; 200 when empty
	got      []received
	srv      *httptest.Server
}

type received struct {
	header http.Header
	body   []byte
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	r := &receiver{statuses: statuses}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, received{req.Header.Clone(), body})
		status := 200
		if len(r.statuses) > 0 {
			status, r.statuses = r.statuses[0], r.statuses[1:]
		}
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) requests() []received {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]received(nil), r.got...)
}

func setup(t *testing.T, opts Options) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if opts.Backoff == nil {
		opts.Backoff = func(int) time.Duration { return 0 }
	}
	return New(st, opts), st
}

func apply(t *testing.T, s *Service, w *evalsiv1alpha1.Webhook) *evalsiv1alpha1.Webhook {
	t.Helper()
	resp, err := s.ApplyWebhook(context.Background(), connect.NewRequest(&evalsiv1alpha1.ApplyWebhookRequest{Webhook: w}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.GetWebhook()
}

func finishedRun(status evalsiv1alpha1.RunStatus) *evalsiv1alpha1.Run {
	return &evalsiv1alpha1.Run{
		Id: "run-1", Name: "nightly", Project: "default", Status: status,
		Spec:  &evalsiv1alpha1.RunSpec{Judge: "secret-judge-name"},
		Gates: []*evalsiv1alpha1.GateResult{{Passed: status != evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED, Reason: "exact-match below 0.8"}},
	}
}

// drain runs the dispatcher's work until no delivery is pending.
func drain(t *testing.T, s *Service, st *store.Store) {
	t.Helper()
	for range 20 {
		due, err := st.DueDeliveries(context.Background(), time.Now().Add(time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(due) == 0 {
			return
		}
		s.dispatch(context.Background())
	}
	t.Fatal("deliveries are still pending")
}

// The vector the Python helper (evalsi.webhooks) checks too.
func TestSignatureVector(t *testing.T) {
	got := Sign("whsec_0123456789abcdef", time.Unix(1_700_000_000, 0), []byte(`{"type":"run.finished"}`))
	if want := "t=1700000000,v1=600685486bcb4ae8c251487c8adcf7dfc5a0f46112e6dc82ae42273e5983d70f"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestSignAndVerify(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"a":1}`)
	sig := Sign("whsec_0123456789abcdef", now, body)
	if err := Verify("whsec_0123456789abcdef", sig, body, time.Minute, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		secret, header string
		body           []byte
		now            time.Time
	}{
		"wrong secret": {"other-secret-0123456", sig, body, now},
		"tampered":     {"whsec_0123456789abcdef", sig, []byte(`{"a":2}`), now},
		"stale":        {"whsec_0123456789abcdef", sig, body, now.Add(time.Hour)},
		"malformed":    {"whsec_0123456789abcdef", "v1=zz", body, now},
	} {
		if err := Verify(c.secret, c.header, c.body, time.Minute, c.now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRunFinishedIsDeliveredSigned(t *testing.T) {
	rcv := newReceiver(t)
	s, st := setup(t, Options{})
	created := apply(t, s, &evalsiv1alpha1.Webhook{Name: "ci", Url: rcv.srv.URL})
	if created.GetSecret() == "" {
		t.Fatal("no generated secret returned on create")
	}
	s.RunFinished(finishedRun(evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED))
	drain(t, s, st)

	reqs := rcv.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d deliveries, want 1 (a succeeded run has no gate_failed event)", len(reqs))
	}
	r := reqs[0]
	if r.header.Get(HeaderEvent) != EventRunFinished || r.header.Get(HeaderDelivery) == "" {
		t.Errorf("headers: %v", r.header)
	}
	if err := Verify(created.GetSecret(), r.header.Get(HeaderSignature), r.body, time.Minute, time.Now()); err != nil {
		t.Errorf("signature: %v", err)
	}
	var p struct {
		ID, Type, Project string
		Run               map[string]any
	}
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != EventRunFinished || p.ID != r.header.Get(HeaderDelivery) || p.Project != "default" || p.Run["id"] != "run-1" || p.Run["status"] != "RUN_STATUS_SUCCEEDED" {
		t.Errorf("payload: %s", r.body)
	}
	if _, has := p.Run["spec"]; has {
		t.Errorf("the run's spec was sent: %s", r.body)
	}
}

func TestGateFailureSendsBothEvents(t *testing.T) {
	rcv := newReceiver(t)
	s, st := setup(t, Options{})
	apply(t, s, &evalsiv1alpha1.Webhook{Name: "all", Url: rcv.srv.URL})
	only := newReceiver(t)
	apply(t, s, &evalsiv1alpha1.Webhook{Name: "gates", Url: only.srv.URL, Events: []evalsiv1alpha1.WebhookEvent{evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_RUN_GATE_FAILED}})
	s.RunFinished(finishedRun(evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED))
	drain(t, s, st)

	events := func(r *receiver) []string {
		var out []string
		for _, q := range r.requests() {
			out = append(out, q.header.Get(HeaderEvent))
		}
		return out
	}
	if got := events(rcv); len(got) != 2 || got[0] == got[1] {
		t.Errorf("subscribed to everything: %v", got)
	}
	if got := events(only); len(got) != 1 || got[0] != EventRunGateFailed {
		t.Errorf("subscribed to gate_failed: %v", got)
	}
}

func TestFailuresAreRetriedWithTheSameDeliveryID(t *testing.T) {
	rcv := newReceiver(t, 500, 503, 200)
	s, st := setup(t, Options{})
	apply(t, s, &evalsiv1alpha1.Webhook{Name: "ci", Url: rcv.srv.URL})
	s.RunFinished(finishedRun(evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED))
	drain(t, s, st)

	reqs := rcv.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d attempts, want 3", len(reqs))
	}
	for _, r := range reqs {
		if r.header.Get(HeaderDelivery) != reqs[0].header.Get(HeaderDelivery) {
			t.Error("the delivery ID changed between attempts")
		}
	}
	ds, err := s.ListWebhookDeliveries(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListWebhookDeliveriesRequest{Name: "ci"}))
	if err != nil {
		t.Fatal(err)
	}
	if d := ds.Msg.GetDeliveries(); len(d) != 1 || d[0].GetState() != evalsiv1alpha1.DeliveryState_DELIVERY_STATE_DELIVERED || d[0].GetAttempts() != 3 {
		t.Errorf("deliveries: %v", d)
	}
}

func TestDeliveryFailsAfterMaxAttempts(t *testing.T) {
	rcv := newReceiver(t, 500, 500, 500, 500)
	s, st := setup(t, Options{MaxAttempts: 3})
	apply(t, s, &evalsiv1alpha1.Webhook{Name: "ci", Url: rcv.srv.URL})
	s.RunFinished(finishedRun(evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED))
	drain(t, s, st)

	if n := len(rcv.requests()); n != 3 {
		t.Errorf("%d attempts, want 3", n)
	}
	ds, _ := s.ListWebhookDeliveries(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListWebhookDeliveriesRequest{Name: "ci"}))
	d := ds.Msg.GetDeliveries()
	if len(d) != 1 || d[0].GetState() != evalsiv1alpha1.DeliveryState_DELIVERY_STATE_FAILED || d[0].GetStatusCode() != 500 || d[0].GetError() != "HTTP 500" {
		t.Errorf("deliveries: %v", d)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	target := newReceiver(t)
	redirect := httptest.NewServer(http.RedirectHandler(target.srv.URL, http.StatusFound))
	t.Cleanup(redirect.Close)
	s, st := setup(t, Options{MaxAttempts: 1})
	apply(t, s, &evalsiv1alpha1.Webhook{Name: "ci", Url: redirect.URL})
	s.RunFinished(finishedRun(evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED))
	drain(t, s, st)
	if n := len(target.requests()); n != 0 {
		t.Errorf("the redirect was followed (%d requests)", n)
	}
}

func TestDisabledAndOtherProjectsGetNothing(t *testing.T) {
	rcv := newReceiver(t)
	s, st := setup(t, Options{})
	apply(t, s, &evalsiv1alpha1.Webhook{Name: "off", Url: rcv.srv.URL, Disabled: true})
	apply(t, s, &evalsiv1alpha1.Webhook{Name: "elsewhere", Project: "other", Url: rcv.srv.URL})
	s.RunFinished(finishedRun(evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED))
	drain(t, s, st)
	if n := len(rcv.requests()); n != 0 {
		t.Errorf("%d deliveries, want none", n)
	}
}

func TestSecretsAreNeverReturnedAfterCreate(t *testing.T) {
	s, _ := setup(t, Options{})
	ctx := context.Background()
	created := apply(t, s, &evalsiv1alpha1.Webhook{Name: "ci", Url: "https://example.com/hook"})
	if created.GetSecret() == "" {
		t.Fatal("no secret on create")
	}
	// Applying again without a secret keeps the stored one and returns none.
	again := apply(t, s, &evalsiv1alpha1.Webhook{Name: "ci", Url: "https://example.com/other"})
	if again.GetSecret() != "" || !again.GetSecretSet() {
		t.Errorf("re-apply: secret=%q set=%v", again.GetSecret(), again.GetSecretSet())
	}
	stored, _ := s.store.GetWebhook(ctx, "default", "ci")
	if stored.GetSecret() != created.GetSecret() || stored.GetUrl() != "https://example.com/other" {
		t.Errorf("stored: %v", stored)
	}
	got, _ := s.GetWebhook(ctx, connect.NewRequest(&evalsiv1alpha1.GetWebhookRequest{Name: "ci"}))
	list, _ := s.ListWebhooks(ctx, connect.NewRequest(&evalsiv1alpha1.ListWebhooksRequest{}))
	if got.Msg.GetWebhook().GetSecret() != "" || list.Msg.GetWebhooks()[0].GetSecret() != "" {
		t.Error("a secret was returned")
	}
	// A secret the caller sets replaces it and is not echoed.
	set := apply(t, s, &evalsiv1alpha1.Webhook{Name: "ci", Url: "https://example.com/hook", Secret: "caller-chosen-secret"})
	if set.GetSecret() != "" {
		t.Error("a caller's secret was echoed")
	}
}

func TestApplyValidates(t *testing.T) {
	s, _ := setup(t, Options{})
	for name, w := range map[string]*evalsiv1alpha1.Webhook{
		"no name":       {Url: "https://example.com"},
		"bad name":      {Name: "Has Space", Url: "https://example.com"},
		"no url":        {Name: "a"},
		"ftp":           {Name: "a", Url: "ftp://example.com/x"},
		"credentials":   {Name: "a", Url: "https://user:pw@example.com/x"},
		"short secret":  {Name: "a", Url: "https://example.com", Secret: "short"},
		"unknown event": {Name: "a", Url: "https://example.com", Events: []evalsiv1alpha1.WebhookEvent{evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_UNSPECIFIED}},
	} {
		_, err := s.ApplyWebhook(context.Background(), connect.NewRequest(&evalsiv1alpha1.ApplyWebhookRequest{Webhook: w}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTestWebhookReportsTheAnswer(t *testing.T) {
	rcv := newReceiver(t, 200, 410)
	s, _ := setup(t, Options{})
	apply(t, s, &evalsiv1alpha1.Webhook{Name: "ci", Url: rcv.srv.URL})
	ctx := context.Background()
	ok, err := s.TestWebhook(ctx, connect.NewRequest(&evalsiv1alpha1.TestWebhookRequest{Name: "ci"}))
	if err != nil || !ok.Msg.GetDelivered() || ok.Msg.GetStatusCode() != 200 {
		t.Fatalf("%v %v", ok, err)
	}
	bad, _ := s.TestWebhook(ctx, connect.NewRequest(&evalsiv1alpha1.TestWebhookRequest{Name: "ci"}))
	if bad.Msg.GetDelivered() || bad.Msg.GetStatusCode() != 410 || bad.Msg.GetError() == "" {
		t.Errorf("%v", bad.Msg)
	}
	if reqs := rcv.requests(); reqs[0].header.Get(HeaderEvent) != EventPing {
		t.Errorf("event %q", reqs[0].header.Get(HeaderEvent))
	}
	if _, err := s.TestWebhook(ctx, connect.NewRequest(&evalsiv1alpha1.TestWebhookRequest{Name: "missing"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("missing: %v", err)
	}
}

// Two replicas share a database; only the one holding the lease sends.
func TestOnlyTheLeaseHolderSends(t *testing.T) {
	rcv := newReceiver(t)
	a, st := setup(t, Options{Owner: "a"})
	b := New(st, Options{Owner: "b", Backoff: func(int) time.Duration { return 0 }})
	apply(t, a, &evalsiv1alpha1.Webhook{Name: "ci", Url: rcv.srv.URL})
	a.dispatch(context.Background()) // a takes the lease
	a.RunFinished(finishedRun(evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED))
	b.dispatch(context.Background())
	if n := len(rcv.requests()); n != 0 {
		t.Fatalf("a replica without the lease sent %d deliveries", n)
	}
	a.dispatch(context.Background())
	if n := len(rcv.requests()); n != 1 {
		t.Errorf("%d deliveries, want 1", n)
	}
}
