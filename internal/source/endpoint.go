package source

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
)

// CheckEndpoint decides whether the server may dial a source's endpoint. The
// server makes the request from where it runs, so an endpoint chosen by a
// project editor must not reach the cluster's own services or a cloud
// metadata address unless the operator listed the host (sources.allow_hosts):
// it must be https to a public name.
func CheckEndpoint(endpoint string, allow []string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("source.endpoint must be an http(s) URL, got %q", endpoint)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("source.endpoint must be an http(s) URL, got scheme %q", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("source.endpoint must not carry credentials; name them under source.credentials")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if slices.ContainsFunc(allow, func(p string) bool { return matchHost(p, host) }) {
		return nil
	}
	if internalHost(host) {
		return fmt.Errorf("source.endpoint host %q is a private, loopback or cluster-internal address; "+
			"list it under sources.allow_hosts in the server config to allow it", host)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("source.endpoint must be https (or list %q under sources.allow_hosts for a plain HTTP store on a trusted network)", host)
	}
	return nil
}

func matchHost(pattern, host string) bool {
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+rest) && net.ParseIP(host) == nil
	}
	return pattern == host
}

// internalHost is true for a literal private, loopback, link-local or
// unspecified address, and for names that only resolve inside a cluster or on
// the machine. Names are not resolved: an allow-list is the control, this is
// the guard rail against the obvious.
func internalHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
	}
	switch {
	case host == "localhost", strings.HasSuffix(host, ".localhost"),
		host == "metadata.google.internal", strings.HasSuffix(host, ".internal"),
		strings.HasSuffix(host, ".local"), strings.HasSuffix(host, ".svc"),
		strings.HasSuffix(host, ".cluster.local"), strings.Contains(host, ".svc."):
		return true
	case !strings.Contains(host, "."):
		// A bare name ("mlflow") only resolves through a search domain.
		return true
	}
	return false
}
