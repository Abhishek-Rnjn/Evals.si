package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeProvider is an OIDC provider on a loopback address.
type fakeProvider struct {
	srv       *httptest.Server
	discovery atomic.Int32
	form      url.Values
	status    int
	body      string
	issuer    string // what the metadata claims; the server's URL if empty
}

func newProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{status: http.StatusOK, body: `{"id_token":"idt","access_token":"act","refresh_token":"secret-refresh","token_type":"Bearer","expires_in":3600}`}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			p.discovery.Add(1)
			issuer := p.issuer
			if issuer == "" {
				issuer = p.srv.URL
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer": issuer, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token",
			})
		case "/token":
			raw, _ := io.ReadAll(r.Body)
			p.form, _ = url.ParseQuery(string(raw))
			w.WriteHeader(p.status)
			_, _ = io.WriteString(w, p.body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func loginServer(t *testing.T, p *fakeProvider) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	RegisterLogin(mux, &Login{Issuer: p.srv.URL, ClientID: "evalsi-ui", Audience: "evals.si", Scopes: []string{"openid", "email", "offline_access"}})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const (
	goodVerifier = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ-._~0123456789"
	goodRedirect = "https://evals.example.com/ui/"
)

func exchange(t *testing.T, srv *httptest.Server, contentType string, body map[string]string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/ui/login/token", contentType, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestLoginConfigNamesTheProviderEndpointsAndDropsRefreshScope(t *testing.T) {
	p := newProvider(t)
	srv := loginServer(t, p)
	for i := 0; i < 2; i++ {
		resp, err := http.Get(srv.URL + "/ui/login/config")
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Authorization string   `json:"authorization_endpoint"`
			ClientID      string   `json:"client_id"`
			Scopes        []string `json:"scopes"`
			Audience      string   `json:"audience"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&cfg)
		resp.Body.Close()
		if cfg.Authorization != p.srv.URL+"/authorize" || cfg.ClientID != "evalsi-ui" || cfg.Audience != "evals.si" ||
			strings.Join(cfg.Scopes, " ") != "openid email" {
			t.Fatalf("config %+v", cfg)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("cache-control %q", resp.Header.Get("Cache-Control"))
		}
	}
	if p.discovery.Load() != 1 {
		t.Errorf("metadata read %d times for two calls", p.discovery.Load())
	}
}

func TestLoginTokenExchangesTheCodeAndKeepsTheRefreshTokenBack(t *testing.T) {
	p := newProvider(t)
	srv := loginServer(t, p)
	status, out := exchange(t, srv, "application/json", map[string]string{"code": "the-code", "code_verifier": goodVerifier, "redirect_uri": goodRedirect})
	if status != http.StatusOK || out["id_token"] != "idt" || out["access_token"] != "act" {
		t.Fatalf("%d %v", status, out)
	}
	if _, leaked := out["refresh_token"]; leaked {
		t.Fatal("the refresh token was sent to the browser")
	}
	want := map[string]string{
		"grant_type": "authorization_code", "code": "the-code", "code_verifier": goodVerifier,
		"redirect_uri": goodRedirect, "client_id": "evalsi-ui",
	}
	for k, v := range want {
		if p.form.Get(k) != v {
			t.Errorf("provider got %s=%q, want %q", k, p.form.Get(k), v)
		}
	}
	if p.form.Get("client_secret") != "" {
		t.Error("a client secret was sent")
	}
}

func TestLoginTokenReportsTheProvidersRefusal(t *testing.T) {
	p := newProvider(t)
	p.status, p.body = http.StatusBadRequest, `{"error":"invalid_grant","error_description":"code expired"}`
	srv := loginServer(t, p)
	status, out := exchange(t, srv, "application/json", map[string]string{"code": "c", "code_verifier": goodVerifier, "redirect_uri": goodRedirect})
	if status != http.StatusBadRequest || out["message"] != "invalid_grant code expired" {
		t.Fatalf("%d %v", status, out)
	}
}

func TestLoginTokenRefusesWhatItShouldNotRelay(t *testing.T) {
	p := newProvider(t)
	srv := loginServer(t, p)
	ok := map[string]string{"code": "c", "code_verifier": goodVerifier, "redirect_uri": goodRedirect}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range ok {
			m[a] = b
		}
		m[k] = v
		return m
	}
	if status, _ := exchange(t, srv, "text/plain", ok); status != http.StatusUnsupportedMediaType {
		t.Errorf("a form post: %d", status)
	}
	for name, body := range map[string]map[string]string{
		"no code":            with("code", ""),
		"spaces in code":     with("code", "a b"),
		"short verifier":     with("code_verifier", "short"),
		"other path":         with("redirect_uri", "https://evals.example.com/other"),
		"query":              with("redirect_uri", "https://evals.example.com/ui/?x=1"),
		"ftp":                with("redirect_uri", "ftp://evals.example.com/ui/"),
		"credentials in uri": with("redirect_uri", "https://u:p@evals.example.com/ui/"),
		"no host":            with("redirect_uri", "/ui/"),
	} {
		if status, _ := exchange(t, srv, "application/json", body); status != http.StatusBadRequest {
			t.Errorf("%s: %d", name, status)
		}
	}
	if p.form != nil {
		t.Errorf("a refused request reached the provider: %v", p.form)
	}
}

func TestLoginRefusesAProviderThatIsNotWhatWasConfigured(t *testing.T) {
	p := newProvider(t)
	p.issuer = "https://elsewhere.example.com"
	srv := loginServer(t, p)
	resp, err := http.Get(srv.URL + "/ui/login/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(out["message"], "names issuer") {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	for _, bad := range []string{"http://idp.example.com", "ftp://idp.example.com", "https://u:p@idp.example.com"} {
		if err := checkEndpoint(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	for _, good := range []string{"https://idp.example.com/auth", "http://localhost:8081/x", "http://127.0.0.1:9000", "http://[::1]:9000"} {
		if err := checkEndpoint(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
}
