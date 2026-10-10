package source

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// Guard keeps a source's requests off internal addresses. CheckEndpoint looks
// at the URL a source names; Guard looks at where each request really goes: it
// resolves the host and refuses a private, loopback, link-local or unspecified
// address unless the host is in sources.allow_hosts, then dials the address it
// checked (so the name cannot resolve differently a moment later). Redirects
// pass through it hop by hop.
type Guard struct {
	allow []string
	// For tests; net.DefaultResolver otherwise.
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
	// The proxies the environment configures (HTTP_PROXY, HTTPS_PROXY): the
	// transport dials them, and they are the operator's choice, not a source's.
	proxies []string
}

// NewGuard returns a guard for the hosts sources.allow_hosts lists.
func NewGuard(allow []string) *Guard {
	g := &Guard{allow: allow, lookup: net.DefaultResolver.LookupIPAddr}
	for _, env := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if u, err := url.Parse(os.Getenv(env)); err == nil && u.Host != "" {
			g.proxies = append(g.proxies, strings.ToLower(u.Host))
		}
	}
	return g
}

func (g *Guard) allowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return slices.ContainsFunc(g.allow, func(p string) bool { return matchHost(p, host) })
}

// resolve returns the addresses a host may be dialed at, or why it may not.
func (g *Guard) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !g.allowed(host) && internalIP(ip) {
			return nil, fmt.Errorf("source request to %s refused: a private, loopback or link-local address (list it under sources.allow_hosts)", host)
		}
		return []net.IP{ip}, nil
	}
	addrs, err := g.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if !g.allowed(host) && internalIP(a.IP) {
			return nil, fmt.Errorf("source request to %s refused: it resolves to %s, a private, loopback or link-local address (list the host under sources.allow_hosts)", host, a.IP)
		}
		ips = append(ips, a.IP)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("source request to %s: no addresses", host)
	}
	return ips, nil
}

func internalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsInterfaceLocalMulticast()
}

// Client is an HTTP client whose every request, redirect hops included, goes
// through the guard.
func (g *Guard) Client(timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if slices.Contains(g.proxies, strings.ToLower(addr)) {
			return dialer.DialContext(ctx, network, addr)
		}
		ips, err := g.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		var last error
		for _, ip := range ips {
			c, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
	return &http.Client{Timeout: timeout, Transport: guarded{g: g, next: tr}}
}

// guarded checks the target of every request, also when it goes through a
// proxy (whose dial the transport makes, not the target's).
type guarded struct {
	g    *Guard
	next http.RoundTripper
}

func (t guarded) RoundTrip(req *http.Request) (*http.Response, error) {
	if _, err := t.g.resolve(req.Context(), req.URL.Hostname()); err != nil {
		return nil, err
	}
	return t.next.RoundTrip(req)
}
