package webhooks

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/source"
)

func TestSSRFGuardChecksStoredWebhooks(t *testing.T) {
	rcv := newReceiver(t)
	legacy, st := setup(t, Options{})
	w := apply(t, legacy, &evalsiv1alpha1.Webhook{Name: "internal", Url: rcv.srv.URL})
	guarded := New(st, Options{SSRFGuard: true})
	if _, err := guarded.ApplyWebhook(context.Background(), connect.NewRequest(&evalsiv1alpha1.ApplyWebhookRequest{Webhook: w})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("private destination accepted: %v", err)
	}
	if _, err := guarded.send(context.Background(), w, "test", EventPing, []byte(`{}`)); err == nil {
		t.Fatal("guard did not protect an existing webhook")
	}
	if len(rcv.requests()) != 0 {
		t.Fatal("blocked destination received a request")
	}
	u, _ := url.Parse(rcv.srv.URL)
	allowed := New(st, Options{SSRFGuard: true, AllowHosts: []string{u.Hostname()}})
	if status, err := allowed.send(context.Background(), w, "test", EventPing, []byte(`{}`)); err != nil || status != 200 {
		t.Fatalf("explicit exception failed: %d, %v", status, err)
	}
}

func TestSSRFGuardValidatesEndpointAndNeverFollowsRedirects(t *testing.T) {
	s, _ := setup(t, Options{SSRFGuard: true})
	for _, endpoint := range []string{"http://example.com/hook", "https://169.254.169.254/latest", "https://[::1]/hook", "https://metadata.google.internal"} {
		if err := s.checkEndpoint(endpoint); err == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
	if err := s.checkEndpoint("https://example.com/hook"); err != nil {
		t.Fatal(err)
	}
	target := newReceiver(t)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	u, _ := url.Parse(redirect.URL)
	s, _ = setup(t, Options{SSRFGuard: true, AllowHosts: []string{u.Hostname()}})
	status, err := s.send(context.Background(), &evalsiv1alpha1.Webhook{Url: redirect.URL}, "id", EventPing, nil)
	if status != http.StatusTemporaryRedirect || err == nil || len(target.requests()) != 0 {
		t.Fatalf("redirect followed: status=%d err=%v requests=%d", status, err, len(target.requests()))
	}
}

// A webhook saved while the guard was off fails at once when it is on, with
// an error naming both settings, instead of being retried for an hour.
func TestBlockedStoredWebhookFailsAtOnceNamingTheSetting(t *testing.T) {
	rcv := newReceiver(t)
	legacy, st := setup(t, Options{})
	apply(t, legacy, &evalsiv1alpha1.Webhook{Name: "ci", Url: rcv.srv.URL})
	guarded := New(st, Options{SSRFGuard: true, MaxAttempts: 6, Backoff: func(int) time.Duration { return 0 }})
	guarded.RunFinished(finishedRun(evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED))
	drain(t, guarded, st)
	if len(rcv.requests()) != 0 {
		t.Fatal("blocked destination received a request")
	}
	ds, _ := guarded.ListWebhookDeliveries(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListWebhookDeliveriesRequest{Name: "ci"}))
	d := ds.Msg.GetDeliveries()
	if len(d) != 1 || d[0].GetState() != evalsiv1alpha1.DeliveryState_DELIVERY_STATE_FAILED || d[0].GetAttempts() != 1 {
		t.Fatalf("deliveries: %v", d)
	}
	if msg := d[0].GetError(); !strings.Contains(msg, "webhooks.allow_hosts") || !strings.Contains(msg, "webhooks.ssrf_guard: false") {
		t.Errorf("error does not name the settings: %q", msg)
	}
}

// A name that resolves to an internal address is refused at dial time; the
// refusal is worded for webhooks and fails the delivery without retries.
func TestDialTimeRefusalIsWordedForWebhooks(t *testing.T) {
	err := guardError(&source.RefusedError{Host: "hooks.example.com", IP: net.ParseIP("10.0.0.7")})
	if !isBlocked(err) {
		t.Fatal("not treated as blocked")
	}
	for _, want := range []string{"webhook delivery to hooks.example.com", "10.0.0.7", "webhooks.allow_hosts", "webhooks.ssrf_guard: false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
	if other := errors.New("connection refused"); guardError(other) != other || isBlocked(other) {
		t.Error("other errors are changed")
	}
}

func TestAllowHostsSuffixPatterns(t *testing.T) {
	s, _ := setup(t, Options{SSRFGuard: true, AllowHosts: []string{"*.svc.cluster.local", "receiver.studio"}})
	for _, ok := range []string{"http://studio.studio.svc.cluster.local/hooks", "http://receiver.studio:8080/x"} {
		if err := s.checkEndpoint(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://svc.cluster.local/x", "http://evil.svc.cluster.local.example.com/x", "http://other.studio/x", "https://10.0.0.1/x"} {
		if err := s.checkEndpoint(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
