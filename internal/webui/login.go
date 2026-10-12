package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Login lets the UI sign a person in through the OIDC provider that
// auth.cli_login names: the authorization code flow with PKCE, run by the
// browser. The browser cannot read the provider's metadata or call its token
// endpoint itself (the Content-Security-Policy allows only this origin, and
// providers rarely allow cross-origin token requests), so evalsid does both,
// for one client and one provider fixed in its configuration. It holds no
// secret: the client is public, and the code verifier stays in the browser
// until the exchange.
type Login struct {
	Issuer   string
	ClientID string
	Audience string
	Scopes   []string
	// HTTP reaches the provider; nil means a client with a 10 second timeout
	// that follows no redirects.
	HTTP *http.Client

	mu      sync.Mutex
	meta    *metadata
	fetched time.Time
	slots   chan struct{}
}

type metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

// metadataTTL is how long the provider's endpoints are remembered.
const metadataTTL = time.Hour

// maxExchanges bounds the token requests in flight at once.
const maxExchanges = 8

var (
	verifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
	codePattern     = regexp.MustCompile(`^[\x21-\x7e]+$`)
)

// maxCode bounds an authorization code, which providers keep short.
const maxCode = 2048

// RegisterLogin mounts the sign-in endpoints beside the UI:
//
//	GET  /ui/login/config  the authorization endpoint, client ID, scopes and audience
//	POST /ui/login/token   exchanges an authorization code (with its PKCE verifier) for tokens
func RegisterLogin(mux *http.ServeMux, l *Login) {
	l.slots = make(chan struct{}, maxExchanges)
	if l.HTTP == nil {
		l.HTTP = &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	mux.HandleFunc("GET "+Prefix+"login/config", l.config)
	mux.HandleFunc("POST "+Prefix+"login/token", l.token)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"message": fmt.Sprintf(format, args...)})
}

func (l *Login) config(w http.ResponseWriter, r *http.Request) {
	m, err := l.discover(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "the identity provider's settings could not be read: %v", err)
		return
	}
	scopes := make([]string, 0, len(l.Scopes))
	for _, s := range l.Scopes {
		// A browser tab asks for no refresh token: it signs in again instead.
		if s != "offline_access" {
			scopes = append(scopes, s)
		}
	}
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authorization_endpoint": m.AuthorizationEndpoint,
		"client_id":              l.ClientID,
		"scopes":                 scopes,
		"audience":               l.Audience,
	})
}

func (l *Login) token(w http.ResponseWriter, r *http.Request) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "send application/json")
		return
	}
	var in struct {
		Code        string `json:"code"`
		Verifier    string `json:"code_verifier"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, "the request is not the expected JSON")
		return
	}
	if len(in.Code) > maxCode || !codePattern.MatchString(in.Code) || !verifierPattern.MatchString(in.Verifier) {
		fail(w, http.StatusBadRequest, "the code or its verifier is malformed")
		return
	}
	// The provider checks the redirect URI against the client's registered
	// ones; this only keeps the endpoint from relaying other requests.
	if u, err := url.Parse(in.RedirectURI); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" ||
		u.Path != Prefix || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		fail(w, http.StatusBadRequest, "redirect_uri must be this server's %s", Prefix)
		return
	}
	select {
	case l.slots <- struct{}{}:
		defer func() { <-l.slots }()
	default:
		fail(w, http.StatusTooManyRequests, "too many sign-ins at once; try again")
		return
	}
	m, err := l.discover(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, "the identity provider's settings could not be read: %v", err)
		return
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {in.Code}, "code_verifier": {in.Verifier},
		"redirect_uri": {in.RedirectURI}, "client_id": {l.ClientID},
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		fail(w, http.StatusBadGateway, "the token request could not be built")
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		fail(w, http.StatusBadGateway, "the identity provider could not be reached: %v", err)
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		fail(w, http.StatusBadGateway, "the identity provider's answer could not be read")
		return
	}
	var out struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK || out.Error != "" || (out.IDToken == "" && out.AccessToken == "") {
		msg := strings.TrimSpace(out.Error + " " + out.Description)
		if msg == "" {
			msg = fmt.Sprintf("the provider answered HTTP %d", resp.StatusCode)
		}
		fail(w, http.StatusBadRequest, "%s", msg)
		return
	}
	// No refresh token goes to the browser.
	writeJSON(w, http.StatusOK, map[string]any{
		"id_token": out.IDToken, "access_token": out.AccessToken, "token_type": out.TokenType, "expires_in": out.ExpiresIn,
	})
}

// discover reads the provider's metadata, at most once an hour.
func (l *Login) discover(ctx context.Context) (*metadata, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.meta != nil && time.Since(l.fetched) < metadataTTL {
		return l.meta, nil
	}
	issuer := strings.TrimRight(l.Issuer, "/")
	if err := checkEndpoint(issuer); err != nil {
		return nil, fmt.Errorf("issuer: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var m metadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("not OIDC metadata: %w", err)
	}
	// RFC 8414 / OIDC Discovery: the document must name the issuer it was read for.
	if strings.TrimRight(m.Issuer, "/") != issuer {
		return nil, fmt.Errorf("the metadata names issuer %q, not %q", m.Issuer, issuer)
	}
	for name, endpoint := range map[string]string{"authorization_endpoint": m.AuthorizationEndpoint, "token_endpoint": m.TokenEndpoint} {
		if err := checkEndpoint(endpoint); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	l.meta, l.fetched = &m, time.Now()
	return l.meta, nil
}

// checkEndpoint accepts an https URL, or http only to a loopback address (a
// provider run on the developer's own machine).
func checkEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return fmt.Errorf("%q is not a usable URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("%q must be https", raw)
}
