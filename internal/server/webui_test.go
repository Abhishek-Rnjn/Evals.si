package server

import (
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestWebUI: the UI's files are public (they hold no data) and locked down
// with headers; the API they call is not.
func TestWebUI(t *testing.T) {
	s := startAuthServer(t, nil)
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := s.http.Get(s.url + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get("/ui/")
	if resp.StatusCode != 200 || !strings.Contains(body, `<script src="app.js" defer></script>`) {
		t.Fatalf("index: %d %s", resp.StatusCode, body)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers: %v", resp.Header)
	}
	if resp, _ := get("/ui/app.js"); resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Errorf("app.js: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	noRedirect := *s.http
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if resp, err := noRedirect.Get(s.url + "/ui"); err != nil || resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("/ui: %v %v", resp, err)
	}
	// The data is not public.
	if resp, _ := get("/v1alpha1/runs"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("API without a credential: %d", resp.StatusCode)
	}
}

// TestWebUIRendersText keeps the UI from building HTML out of strings, so
// API data (record inputs, outputs, explanations) can never become markup.
func TestWebUIRendersText(t *testing.T) {
	src, err := os.ReadFile("../webui/static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{`innerHTML`, `outerHTML`, `insertAdjacentHTML`, `document\.write`, `\beval\(`, `new Function`} {
		if regexp.MustCompile(re).Match(src) {
			t.Errorf("app.js uses %s", re)
		}
	}
	html, _ := os.ReadFile("../webui/static/index.html")
	if regexp.MustCompile(`<script>|\son[a-z]+=`).Match(html) {
		t.Error("index.html has inline script, which the CSP forbids")
	}
}
