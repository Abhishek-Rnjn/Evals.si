// Package mcp serves Evals.si over the Model Context Protocol's streamable
// HTTP transport at /mcp, following the MCP authorization specification:
//
//   - OAuth 2.0 Protected Resource Metadata (RFC 9728) at
//     /.well-known/oauth-protected-resource, naming the issuers people sign
//     in with as authorization servers;
//   - 401 responses with a WWW-Authenticate challenge pointing at it;
//   - audience-bound tokens (RFC 8707): with mcp.resource set, a bearer
//     token must name that resource in its audience;
//   - an Origin check against DNS rebinding.
//
// Every tool call is an in-process call to evalsid's own Connect handlers,
// with the caller's credential, TLS state and address, so the same
// authentication, authorization, quotas and audit apply as for any client.
// The tool itself is authorized first as mcp.tools.call, with mcp.tool.name
// available to CEL rules (as in agentgateway's mcpAuthorization); tools the
// caller may not call are left out of tools/list.
package mcp

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"time"

	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/version"
)

// Config is the mcp section of evalsi.yaml.
type Config struct {
	// Turn the endpoint off.
	Disabled bool `json:"disabled,omitempty"`
	// The endpoint's canonical URI (https://evalsi.example.com/mcp). With it,
	// bearer tokens must carry it in their audience (RFC 8707), and the
	// metadata names it; without it, the metadata derives it from the request.
	Resource string `json:"resource,omitempty"`
	// Origins browsers may call from (Origin header); requests from other
	// origins are refused. Requests without an Origin header are not browsers.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	// How long the run tool waits for a run to finish; default 10 minutes.
	// A run still going after that is returned with its id, to poll.
	RunWaitS float64 `json:"run_wait_s,omitempty"`
}

// Validate checks the section.
func (c Config) Validate() error {
	if c.Resource != "" && !strings.HasPrefix(c.Resource, "https://") && !strings.HasPrefix(c.Resource, "http://") {
		return fmt.Errorf("mcp.resource %q must be an http(s) URI", c.Resource)
	}
	if c.RunWaitS < 0 {
		return errors.New("mcp.run_wait_s must not be negative")
	}
	return nil
}

// Authorize decides whether the request's principal may call a tool;
// listing asks for tools/list, which should not audit.
type Authorize func(r *http.Request, tool string, listing bool) error

// Server is the /mcp endpoint.
type Server struct {
	cfg       Config
	backend   http.Handler
	authorize Authorize
	issuers   func() []string
	log       *slog.Logger
	tools     []tool
	now       func() time.Time
}

// Protocol versions this server speaks, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26"}

// New makes the endpoint. backend is evalsid's root handler (authentication
// middleware and all services), which tool calls go through.
func New(cfg Config, backend http.Handler, authorize Authorize, issuers func() []string, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, backend: backend, authorize: authorize, issuers: issuers, log: log, now: time.Now}
	s.tools = s.toolset()
	return s
}

// Register mounts the endpoint and its metadata.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle("/mcp", s)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.metadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.metadata)
}

func (s *Server) resource(r *http.Request) string {
	if s.cfg.Resource != "" {
		return s.cfg.Resource
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/mcp"
}

func (s *Server) metadataURL(r *http.Request) string {
	res := s.resource(r)
	base := strings.TrimSuffix(res, "/mcp")
	return base + "/.well-known/oauth-protected-resource/mcp"
}

// metadata is RFC 9728's protected resource metadata.
func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"resource":                 s.resource(r),
		"resource_name":            "Evals.si",
		"bearer_methods_supported": []string{"header"},
		"authorization_servers":    []string{},
	}
	if issuers := s.issuers(); len(issuers) > 0 {
		doc["authorization_servers"] = issuers
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (s *Server) challenge(w http.ResponseWriter, r *http.Request, code, description string) {
	v := fmt.Sprintf(`Bearer resource_metadata=%q`, s.metadataURL(r))
	if code != "" {
		v += fmt.Sprintf(`, error=%q, error_description=%q`, code, description)
	}
	w.Header().Set("WWW-Authenticate", v)
	http.Error(w, description, http.StatusUnauthorized)
}

// ServeHTTP is the streamable HTTP transport: JSON-RPC messages POSTed, a
// JSON response per request. This server never sends requests of its own,
// so it offers no GET stream.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if origin := r.Header.Get("Origin"); origin != "" && !slices.Contains(s.cfg.AllowedOrigins, origin) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	res := auth.FromContext(r.Context())
	if res.Err != nil {
		s.challenge(w, r, "invalid_token", res.Err.Error())
		return
	}
	if p := res.Principal; p != nil && p.Method == auth.MethodJWT && !p.Anonymous() && s.cfg.Resource != "" && !audienceHas(p.Claims["aud"], s.cfg.Resource) {
		s.challenge(w, r, "invalid_token", "the token's audience does not include "+s.cfg.Resource)
		return
	}
	if p := res.Principal; p != nil && p.Anonymous() && p.Method != auth.MethodNone {
		s.challenge(w, r, "invalid_token", "a valid credential is required")
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK) // sessions hold no state here
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(protocolVersions, v) && v != "2024-11-05" {
		http.Error(w, "unsupported MCP-Protocol-Version "+v, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var raw json.RawMessage = body
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		// JSON-RPC batches were removed in 2025-06-18.
		writeJSON(w, http.StatusOK, rpcError(nil, codeInvalidRequest, "batches are not supported"))
		return
	}
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		writeJSON(w, http.StatusOK, rpcError(nil, codeParse, "invalid JSON"))
		return
	}
	if msg.Method == "" {
		// A response or notification from the client: nothing to answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if msg.ID == nil {
		w.WriteHeader(http.StatusAccepted) // notifications
		return
	}
	resp := s.handle(r, &msg)
	if msg.Method == "initialize" && resp.Error == nil {
		w.Header().Set("Mcp-Session-Id", newSessionID())
	}
	writeJSON(w, http.StatusOK, resp)
}

func audienceHas(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == want {
				return true
			}
		}
	case []string:
		return slices.Contains(v, want)
	}
	return false
}

// --- JSON-RPC -------------------------------------------------------------

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcErr     `json:"error,omitempty"`
}

func rpcError(id json.RawMessage, code int, msg string) response {
	if id == nil {
		id = json.RawMessage("null")
	}
	return response{JSONRPC: "2.0", ID: id, Error: &jsonrpcErr{Code: code, Message: msg}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handle(r *http.Request, msg *message) response {
	if msg.JSONRPC != "2.0" {
		return rpcError(msg.ID, codeInvalidRequest, "not JSON-RPC 2.0")
	}
	ok := func(v any) response { return response{JSONRPC: "2.0", ID: msg.ID, Result: v} }
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		version := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return ok(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "evalsid", "title": "Evals.si", "version": version_()},
			"instructions":    "Evaluate with Evals.si: list_evaluators, evaluate records, start a run from a run spec and compare it with a baseline run.",
		})
	case "ping":
		return ok(map[string]any{})
	case "tools/list":
		var list []map[string]any
		for _, t := range s.tools {
			if s.authorize(r, t.name, true) == nil {
				list = append(list, t.describe())
			}
		}
		if list == nil {
			list = []map[string]any{}
		}
		return ok(map[string]any{"tools": list})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return rpcError(msg.ID, codeInvalidParams, "params: "+err.Error())
		}
		t := s.tool(p.Name)
		if t == nil {
			return rpcError(msg.ID, codeInvalidParams, fmt.Sprintf("unknown tool %q", p.Name))
		}
		if err := s.authorize(r, t.name, false); err != nil {
			return ok(toolError("permission denied: " + err.Error()))
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		out, err := t.call(r.Context(), s.client(r), p.Arguments)
		if err != nil {
			return ok(toolError(err.Error()))
		}
		return ok(out)
	default:
		return rpcError(msg.ID, codeNoMethod, fmt.Sprintf("method %q is not supported", msg.Method))
	}
}

func (s *Server) tool(name string) *tool {
	for i := range s.tools {
		if s.tools[i].name == name {
			return &s.tools[i]
		}
	}
	return nil
}

func toolError(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + text}}, "isError": true}
}

func version_() string { return version.Version }

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b[:])
}

// --- in-process calls --------------------------------------------------------

// inproc sends tool calls through evalsid's own handler, as the MCP caller:
// their headers (credentials and proxy identity), TLS state (client
// certificates) and address.
type inproc struct {
	backend http.Handler
	orig    *http.Request
}

var dropHeaders = map[string]bool{
	"Content-Type": true, "Content-Length": true, "Accept": true, "Accept-Encoding": true,
	"Connection": true, "Te": true, "Transfer-Encoding": true, "Origin": true,
	"Mcp-Session-Id": true, "Mcp-Protocol-Version": true,
}

func (t inproc) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range t.orig.Header {
		k = http.CanonicalHeaderKey(k)
		if dropHeaders[k] || strings.HasPrefix(k, "Connect-") || strings.HasPrefix(k, "Grpc-") {
			continue
		}
		if _, set := req.Header[k]; !set {
			req.Header[k] = v
		}
	}
	req.RemoteAddr = t.orig.RemoteAddr
	req.TLS = t.orig.TLS
	req.RequestURI = req.URL.RequestURI()
	rec := httptest.NewRecorder()
	t.backend.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func (s *Server) client(r *http.Request) *http.Client {
	return &http.Client{Transport: inproc{backend: s.backend, orig: r}}
}

// internalURL is the base URL of in-process calls; it never reaches a network.
const internalURL = "http://evalsid.internal"

// waitFor is how long the run tool waits for a run.
func (s *Server) waitFor() time.Duration {
	if s.cfg.RunWaitS > 0 {
		return time.Duration(s.cfg.RunWaitS * float64(time.Second))
	}
	return 10 * time.Minute
}
