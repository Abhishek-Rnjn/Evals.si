package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRESTRejectsUnknownQueryParamsFieldsAndEnums(t *testing.T) {
	s := startAuthServer(t, nil)
	admin := s.keys["admin"]
	hook := func(extra string) string {
		return `{"project":"support","name":"typo","url":"https://example.com"` + extra + `}`
	}
	for name, tc := range map[string]struct{ method, path, body string }{
		"unknown query param":   {"GET", "/v1alpha1/runs?project=support&bogus=1", ""},
		"unknown enum value":    {"POST", "/v1alpha1/webhooks", hook(`,"events":["WEBHOOK_EVENT_RUN_FINISHD"]`)},
		"unknown body field":    {"POST", "/v1alpha1/webhooks", hook(`,"evnts":["WEBHOOK_EVENT_RUN_FINISHED"]`)},
		"misspelled role field": {"POST", "/v1alpha1/roles", `{"name":"r1","project":"support","permisions":["runs.read"]}`},
	} {
		code, body, _ := s.rest(t, s.keys["owner"], tc.method, tc.path, tc.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, code, body)
		}
	}

	// The same through Connect's JSON codec.
	req, _ := http.NewRequest("POST", s.url+"/evalsi.v1alpha1.WebhookService/ApplyWebhook",
		strings.NewReader(`{"webhook":`+hook(`,"events":["WEBHOOK_EVENT_RUN_FINISHD"]`)+`}`))
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("connect JSON unknown enum: %d %s, want 400", resp.StatusCode, b)
	}

	if code, body, _ := s.rest(t, admin, "POST", "/v1alpha1/webhooks", hook(`,"events":["WEBHOOK_EVENT_RUN_FINISHED"]`)); code != http.StatusOK {
		t.Errorf("valid webhook: %d %s", code, body)
	}
}
