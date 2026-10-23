package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/abhishek-rnjn/evals.si/internal/config"
)

// The Python SDK's evalsi.log() retries a failed batch with one
// Idempotency-Key: OTLP/HTTP (JSON) accepts it, replays a retried batch and
// refuses the same key with another body.
func TestOTLPHTTPAcceptsTheSDKsIdempotencyKey(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) { c.OTLP.Grace = "10ms" })
	batch := func(name string) string {
		return `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"sdk"}}]},` +
			`"scopeSpans":[{"scope":{"name":"evalsi.sdk"},"spans":[{"traceId":"0af7651916cd43dd8448eb211c80319c",` +
			`"spanId":"b7ad6b7169203331","name":"` + name + `","kind":1,"startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}`
	}
	post := func(body string) (*http.Response, string) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, s.url+"/v1/traces", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.keys["owner"])
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "evalsi-log-0123456789abcdef")
		resp, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(raw)
	}
	if resp, body := post(batch("evalsi.log")); resp.StatusCode != http.StatusOK || resp.Header.Get("Idempotency-Replayed") != "" {
		t.Fatalf("first send: %d %s", resp.StatusCode, body)
	}
	if resp, body := post(batch("evalsi.log")); resp.StatusCode != http.StatusOK || resp.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("retry: %d %s replayed=%q", resp.StatusCode, body, resp.Header.Get("Idempotency-Replayed"))
	}
	if resp, body := post(batch("other")); resp.StatusCode == http.StatusOK {
		t.Fatalf("same key, other body accepted: %s", body)
	}
}
