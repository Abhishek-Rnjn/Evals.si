package source

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A public-looking name can resolve to an internal address, and an allowed
// endpoint can redirect to one: the guard catches both, where CheckEndpoint
// (the URL string) cannot.
func TestGuardChecksWhereRequestsGo(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secret metadata")
	}))
	t.Cleanup(internal.Close)
	u, _ := url.Parse(internal.URL)
	port := u.Port()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/latest", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	ru, _ := url.Parse(redirector.URL)

	dns := map[string]string{"evil.example.com": "127.0.0.1", "store.example.com": "127.0.0.1", "public.example.com": "203.0.113.7"}
	guard := func(allow ...string) *Guard {
		g := NewGuard(allow)
		g.proxies = nil
		g.lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
			ip, ok := dns[host]
			if !ok {
				return nil, fmt.Errorf("no such host %s", host)
			}
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		}
		return g
	}
	get := func(c *http.Client, target string) (string, error) {
		resp, err := c.Get(target)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		buf := new(strings.Builder)
		_, _ = fmt.Fprint(buf, resp.StatusCode)
		return buf.String(), nil
	}

	// A name that resolves to loopback is refused...
	c := guard().Client(5 * time.Second)
	if _, err := get(c, "http://evil.example.com:"+port+"/"); err == nil || !strings.Contains(err.Error(), "resolves to 127.0.0.1") {
		t.Fatalf("a name resolving to loopback was dialed: %v", err)
	}
	// ...and so is a literal internal address or the metadata address.
	for _, target := range []string{internal.URL, "http://169.254.169.254/latest"} {
		if _, err := get(c, target); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("%s: %v", target, err)
		}
	}
	// Listed, the same host is reachable.
	if code, err := get(guard("evil.example.com").Client(5*time.Second), "http://evil.example.com:"+port+"/"); err != nil || code != "200" {
		t.Fatalf("an allowed host: %s %v", code, err)
	}
	// An allowed endpoint that redirects to an internal address is stopped at the hop.
	c = guard("store.example.com").Client(5 * time.Second)
	if _, err := get(c, "http://store.example.com:"+ru.Port()+"/"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a redirect to an internal address was followed: %v", err)
	}
	// A public address passes the check (the dial itself fails here: no route in tests).
	if _, err := guard().resolve(context.Background(), "public.example.com"); err != nil {
		t.Fatalf("a public address: %v", err)
	}
}
