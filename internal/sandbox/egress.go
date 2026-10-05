package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// EgressEvent is one connection a sandbox attempted through the egress proxy.
type EgressEvent struct {
	Time          time.Time `json:"time"`
	Host          string    `json:"host"`
	Port          int       `json:"port"`
	Allowed       bool      `json:"allowed"`
	BytesSent     int64     `json:"bytes_sent,omitempty"`
	BytesReceived int64     `json:"bytes_received,omitempty"`
}

type allowRule struct {
	host string // exact host, or ".suffix" for "*.suffix"
	port int    // 0: 80 and 443
}

func parseAllowlist(entries []string) ([]allowRule, error) {
	rules := make([]allowRule, 0, len(entries))
	for _, raw := range entries {
		e := strings.ToLower(strings.TrimSpace(raw))
		host, port := e, 0
		if h, p, err := net.SplitHostPort(e); err == nil {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("allowed host %q: bad port", raw)
			}
			host, port = h, n
		}
		if s, ok := strings.CutPrefix(host, "*."); ok {
			host = "." + s
		}
		if host == "" || host == "." || strings.ContainsAny(host, "*/@ ") {
			return nil, fmt.Errorf("allowed host %q: use host, host:port or *.suffix", raw)
		}
		rules = append(rules, allowRule{host: host, port: port})
	}
	return rules, nil
}

func allowed(rules []allowRule, host string, port int) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, r := range rules {
		if r.port == 0 && port != 80 && port != 443 || r.port != 0 && r.port != port {
			continue
		}
		if host == r.host || strings.HasPrefix(r.host, ".") && strings.HasSuffix(host, r.host) {
			return true
		}
	}
	return false
}

// egressProxy is a logging HTTP proxy (CONNECT for TLS, absolute-form for
// plain HTTP) that only reaches allowlisted hosts. Names are resolved here,
// on the host, never inside the sandbox. Every attempt is logged; the log is
// evidence for safety evaluators (exfiltration attempts, policy violations).
type egressProxy struct {
	rules []allowRule
	// Any host (network: allow on rungs without a network device).
	anyHost bool
	ln      net.Listener
	network string
	// The socket's real path (it may be listened on through /proc/self/fd).
	path      string
	dialer    net.Dialer
	proxyFunc func(*url.URL) (*url.URL, error)

	mu     sync.Mutex
	events []EgressEvent
	conns  map[net.Conn]bool
	closed bool
}

func newEgressProxy(network, addr string, allow []string) (*egressProxy, error) {
	rules, err := parseAllowlist(allow)
	if err != nil {
		return nil, err
	}
	p := &egressProxy{}
	listen := addr
	if network == "unix" {
		short, release, err := shortSocket(addr)
		if err != nil {
			return nil, err
		}
		defer release()
		listen = short
	}
	ln, err := net.Listen(network, listen)
	if err != nil {
		return nil, fmt.Errorf("egress proxy: %w", err)
	}
	if network == "unix" {
		// The sandboxed user is mapped to another uid inside its namespace.
		if err := os.Chmod(addr, 0o666); err != nil {
			ln.Close()
			return nil, err
		}
		p.path = addr
	}
	p.rules, p.ln, p.network, p.conns = rules, ln, network, map[net.Conn]bool{}
	p.dialer = net.Dialer{Timeout: 15 * time.Second}
	p.proxyFunc = httpproxy.FromEnvironment().ProxyFunc()
	go p.serve()
	return p, nil
}

// Addr is the socket path (unix) or host:port (tcp).
func (p *egressProxy) Addr() string {
	if p.path != "" {
		return p.path
	}
	return p.ln.Addr().String()
}

// Port is the TCP port of a tcp proxy.
func (p *egressProxy) Port() uint16 {
	if a, ok := p.ln.Addr().(*net.TCPAddr); ok {
		return uint16(a.Port)
	}
	return 0
}

func (p *egressProxy) Events() []EgressEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]EgressEvent(nil), p.events...)
}

func (p *egressProxy) Close() {
	p.mu.Lock()
	p.closed = true
	conns := p.conns
	p.conns = map[net.Conn]bool{}
	p.mu.Unlock()
	p.ln.Close()
	for c := range conns {
		c.Close()
	}
}

func (p *egressProxy) track(c net.Conn, on bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if on && p.closed {
		return false
	}
	if on {
		p.conns[c] = true
	} else {
		delete(p.conns, c)
	}
	return true
}

func (p *egressProxy) log(ev EgressEvent) {
	p.mu.Lock()
	p.events = append(p.events, ev)
	p.mu.Unlock()
}

func (p *egressProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *egressProxy) handle(c net.Conn) {
	if !p.track(c, true) {
		c.Close()
		return
	}
	defer p.track(c, false)
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Minute))
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Time{})
		if req.Method == http.MethodConnect {
			p.connect(c, br, req)
			return
		}
		if !p.forward(c, req) {
			return
		}
	}
}

func hostPort(hostport string, defPort int) (string, int, error) {
	host, port := hostport, defPort
	if h, ps, err := net.SplitHostPort(hostport); err == nil {
		n, err := strconv.Atoi(ps)
		if err != nil {
			return "", 0, err
		}
		host, port = h, n
	}
	if host == "" {
		return "", 0, errors.New("no host")
	}
	return host, port, nil
}

func deny(c net.Conn, status int, msg string) {
	body := "evalsi egress denied: " + msg + "\n"
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

func (p *egressProxy) connect(c net.Conn, br *bufio.Reader, req *http.Request) {
	host, port, err := hostPort(req.Host, 443)
	if err != nil {
		deny(c, http.StatusBadRequest, "bad CONNECT target")
		return
	}
	ev := EgressEvent{Time: time.Now().UTC(), Host: host, Port: port}
	if !p.anyHost && !allowed(p.rules, host, port) {
		p.log(ev)
		deny(c, http.StatusForbidden, fmt.Sprintf("%s:%d is not on the allowlist", host, port))
		return
	}
	up, err := p.dial("https", host, port)
	if err != nil {
		ev.Allowed = true
		p.log(ev)
		deny(c, http.StatusBadGateway, err.Error())
		return
	}
	defer up.Close()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	ev.Allowed = true
	ev.BytesSent, ev.BytesReceived = pipe(c, br, up)
	p.log(ev)
}

// forward relays one plain-HTTP request; it reports whether the client
// connection can carry another.
func (p *egressProxy) forward(c net.Conn, req *http.Request) bool {
	if req.URL.Scheme != "http" || req.URL.Host == "" {
		// Some clients (busybox wget) send GET https://... instead of CONNECT;
		// TLS is never intercepted, so those cannot be relayed.
		deny(c, http.StatusBadRequest, "only CONNECT and absolute http:// requests are proxied; use a client that tunnels https with CONNECT")
		return false
	}
	host, port, err := hostPort(req.URL.Host, 80)
	if err != nil {
		deny(c, http.StatusBadRequest, "bad target")
		return false
	}
	ev := EgressEvent{Time: time.Now().UTC(), Host: host, Port: port}
	if !p.anyHost && !allowed(p.rules, host, port) {
		p.log(ev)
		deny(c, http.StatusForbidden, fmt.Sprintf("%s:%d is not on the allowlist", host, port))
		return false
	}
	ev.Allowed = true
	// Never pass the sandbox's own proxy credentials on.
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authorization")
	upstream := p.upstream(&url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port))})
	var up net.Conn
	if upstream != nil {
		// An upstream HTTP proxy takes the absolute-form request as is.
		up, err = p.dialProxy(upstream)
	} else {
		up, err = p.dialer.DialContext(context.Background(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	if err != nil {
		p.log(ev)
		deny(c, http.StatusBadGateway, err.Error())
		return false
	}
	defer up.Close()
	if upstream != nil {
		if auth := proxyAuth(upstream); auth != "" {
			req.Header.Set("Proxy-Authorization", auth)
		}
		req.RequestURI = req.URL.String()
		cw := &countWriter{w: up}
		return p.relay(c, up, cw, req, &ev)
	}
	req.RequestURI = ""
	cw := &countWriter{w: up}
	return p.relay(c, up, cw, req, &ev)
}

func (p *egressProxy) relay(c, up net.Conn, cw *countWriter, req *http.Request, ev *EgressEvent) bool {
	var err error
	if req.RequestURI != "" {
		// Absolute form for an upstream proxy: write the request line ourselves.
		err = req.WriteProxy(cw)
	} else {
		err = req.Write(cw)
	}
	if err != nil {
		p.log(*ev)
		return false
	}
	resp, err := http.ReadResponse(bufio.NewReader(up), req)
	if err != nil {
		p.log(*ev)
		deny(c, http.StatusBadGateway, err.Error())
		return false
	}
	defer resp.Body.Close()
	rw := &countWriter{w: c}
	err = resp.Write(rw)
	ev.BytesSent, ev.BytesReceived = cw.n, rw.n
	p.log(*ev)
	return err == nil && !resp.Close && !req.Close
}

// upstream is the proxy evalsid itself must use to reach u, from its
// HTTPS_PROXY, HTTP_PROXY and NO_PROXY environment (corporate networks).
func (p *egressProxy) upstream(u *url.URL) *url.URL {
	if p.proxyFunc == nil {
		return nil
	}
	pu, err := p.proxyFunc(u)
	if err != nil || pu == nil {
		return nil
	}
	if pu.Port() == "" {
		port := "80"
		if pu.Scheme == "https" {
			port = "443"
		}
		pu.Host = net.JoinHostPort(pu.Hostname(), port)
	}
	return pu
}

func (p *egressProxy) dialProxy(upstream *url.URL) (net.Conn, error) {
	if upstream.Scheme == "https" {
		return (&tls.Dialer{NetDialer: &p.dialer}).DialContext(context.Background(), "tcp", upstream.Host)
	}
	return p.dialer.DialContext(context.Background(), "tcp", upstream.Host)
}

func proxyAuth(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	pw, _ := u.User.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+pw))
}

// dial connects to host:port, tunnelling through the upstream proxy if one applies.
func (p *egressProxy) dial(scheme, host string, port int) (net.Conn, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	upstream := p.upstream(&url.URL{Scheme: scheme, Host: target})
	if upstream == nil {
		return p.dialer.DialContext(context.Background(), "tcp", target)
	}
	conn, err := p.dialProxy(upstream)
	if err != nil {
		return nil, fmt.Errorf("upstream proxy: %w", err)
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if auth := proxyAuth(upstream); auth != "" {
		req.Header.Set("Proxy-Authorization", auth)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("upstream proxy: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("upstream proxy refused %s: %s", target, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// pipe copies both ways until either side closes; it returns the bytes sent
// upstream and received from upstream.
func pipe(client net.Conn, clientR io.Reader, up net.Conn) (sent, received int64) {
	var s, r atomic.Int64
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(up, clientR)
		s.Store(n)
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(client, up)
		r.Store(n)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	return s.Load(), r.Load()
}

// Forward is `evalsid sandbox-forward`, run inside a bwrap sandbox whose
// network namespace has only loopback. It listens on loopback, relays every
// connection to the egress proxy's socket (bound in from the host), runs the
// command as its child and exits with the command's status.
func Forward(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox-forward", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", forwardAddr, "loopback address to listen on")
	socket := fs.String("socket", insideSocket, "the egress proxy's Unix socket")
	if err := fs.Parse(args); err != nil {
		return exitLauncherFailure
	}
	command := fs.Args()
	if len(command) == 0 {
		fmt.Fprintln(stderr, launcherPrefix+"sandbox-forward needs a command")
		return exitLauncherFailure
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(stderr, launcherPrefix+"egress forwarder: %v\n", err)
		return exitLauncherFailure
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := net.Dial("unix", *socket)
				if err != nil {
					return
				}
				defer up.Close()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, c); _ = up.(*net.UnixConn).CloseWrite(); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
				<-done
				<-done
			}()
		}
	}()
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, stderr
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "sandbox: command not found: %s\n", command[0])
			return exitNotFound
		}
		fmt.Fprintf(stderr, "sandbox: %v\n", err)
		return 126
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if st, ok := exit.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			return 128 + int(st.Signal())
		}
		return exit.ExitCode()
	}
	if err != nil {
		return 126
	}
	return 0
}
