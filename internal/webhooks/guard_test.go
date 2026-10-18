package webhooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func TestSSRFGuardIsOptInAndChecksStoredWebhooks(t *testing.T) {
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
