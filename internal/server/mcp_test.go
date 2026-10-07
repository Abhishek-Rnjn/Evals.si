package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

// mcpCall POSTs one JSON-RPC request to /mcp and decodes the response.
func (s *testServer) mcpCall(t *testing.T, cred string, header http.Header, method string, params any) (int, http.Header, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, s.url+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"raw": string(raw)}
	}
	return resp.StatusCode, resp.Header, out
}

func toolResult(t *testing.T, out map[string]any) (string, bool, map[string]any) {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	structured, _ := res["structuredContent"].(map[string]any)
	return text, res["isError"] == true, structured
}

func toolNames(t *testing.T, out map[string]any) []string {
	t.Helper()
	var names []string
	for _, tool := range out["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

func TestMCPAuthorizationSpec(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) {
		c.MCP.Resource = "https://evals.example.com/mcp"
	})

	// No credential: 401 with a challenge naming the resource metadata.
	code, h, _ := s.mcpCall(t, "", nil, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if code != http.StatusUnauthorized {
		t.Fatalf("no credential: %d", code)
	}
	challenge := h.Get("WWW-Authenticate")
	if !strings.Contains(challenge, `resource_metadata="https://evals.example.com/.well-known/oauth-protected-resource/mcp"`) {
		t.Errorf("challenge %q", challenge)
	}

	// RFC 9728 metadata, public.
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		resp, err := s.http.Get(s.url + path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		if doc["resource"] != "https://evals.example.com/mcp" {
			t.Errorf("%s: resource %v", path, doc["resource"])
		}
		servers, _ := doc["authorization_servers"].([]any)
		if len(servers) != 1 || servers[0] != "https://token.actions.githubusercontent.com" {
			t.Errorf("%s: authorization_servers %v", path, servers)
		}
	}

	// RFC 8707: a valid token for the server but not this resource is refused.
	wrongAud := s.token(t, map[string]any{"sub": "repo:acme/agent", "aud": "https://evals.example.com"})
	code, h, _ = s.mcpCall(t, wrongAud, nil, "initialize", map[string]any{})
	if code != http.StatusUnauthorized || !strings.Contains(h.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("token without the MCP audience: %d %q", code, h.Get("WWW-Authenticate"))
	}
	rightAud := s.token(t, map[string]any{"sub": "repo:acme/agent", "aud": []string{"https://evals.example.com", "https://evals.example.com/mcp"}})
	code, h, out := s.mcpCall(t, rightAud, nil, "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if code != http.StatusOK || out["result"] == nil {
		t.Fatalf("token with the MCP audience: %d %v", code, out)
	}
	if h.Get("Mcp-Session-Id") == "" {
		t.Error("no session id")
	}

	// A browser on another origin is refused (DNS rebinding).
	code, _, _ = s.mcpCall(t, s.keys["viewer"], http.Header{"Origin": {"https://evil.example"}}, "ping", nil)
	if code != http.StatusForbidden {
		t.Errorf("foreign origin: %d", code)
	}
	// An unknown protocol version header is a 400.
	code, _, _ = s.mcpCall(t, s.keys["viewer"], http.Header{"Mcp-Protocol-Version": {"1999-01-01"}}, "ping", nil)
	if code != http.StatusBadRequest {
		t.Errorf("bad protocol version: %d", code)
	}
}

func TestMCPToolsGoThroughTheGate(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) {
		// agentgateway-style per-tool rule: nobody may start runs over MCP.
		c.Authorization.Rules = append(c.Authorization.Rules, authz.Rule{
			Deny: `request.action == "mcp.tools.call" && mcp.tool.name == "run"`,
		})
	})
	_, _, out := s.mcpCall(t, s.keys["runner"], nil, "tools/list", map[string]any{})
	names := toolNames(t, out)
	if slices.Contains(names, "run") || len(names) != 4 {
		t.Errorf("tools for runner: %v", names)
	}
	_, _, out = s.mcpCall(t, s.keys["runner"], nil, "tools/call", map[string]any{"name": "run", "arguments": map[string]any{"spec": map[string]any{}}})
	if text, isErr, _ := toolResult(t, out); !isErr || !strings.Contains(text, "mcp.tools.call") {
		t.Errorf("denied tool: %s", text)
	}

	records := []any{map[string]any{"id": "a", "output": "Paris", "reference": "Paris"}}
	args := map[string]any{"project": "support", "records": records, "evaluators": []any{"exact-match"}}
	// The tool is allowed, but evaluating needs evaluations.run, which a viewer lacks.
	_, _, out = s.mcpCall(t, s.keys["viewer"], nil, "tools/call", map[string]any{"name": "evaluate", "arguments": args})
	if text, isErr, _ := toolResult(t, out); !isErr || !strings.Contains(text, "permission_denied") {
		t.Errorf("viewer evaluate: %s", text)
	}
	_, _, out = s.mcpCall(t, s.keys["runner"], nil, "tools/call", map[string]any{"name": "evaluate", "arguments": args})
	text, isErr, structured := toolResult(t, out)
	if isErr || !strings.Contains(text, "a exact-match: true") {
		t.Fatalf("runner evaluate: %s", text)
	}
	if structured["summaries"] == nil {
		t.Errorf("no structured summaries: %v", structured)
	}
	// list_evaluators needs catalog.read only.
	_, _, out = s.mcpCall(t, s.keys["viewer"], nil, "tools/call", map[string]any{"name": "list_evaluators", "arguments": map[string]any{}})
	if text, isErr, _ := toolResult(t, out); isErr || !strings.Contains(text, "exact-match") {
		t.Errorf("list_evaluators: %s", text)
	}
	// Protocol errors stay JSON-RPC errors.
	_, _, out = s.mcpCall(t, s.keys["viewer"], nil, "resources/list", nil)
	if e, _ := out["error"].(map[string]any); e == nil || e["code"].(float64) != -32601 {
		t.Errorf("unknown method: %v", out)
	}
}

func TestMCPRunAndCompare(t *testing.T) {
	s := startAuthServer(t, nil)
	spec := `apiVersion: evals.si/v1alpha1
kind: EvalRun
metadata: {name: suite, project: support}
spec:
  target: {connector: openai-compatible, model: m, base_url: "http://127.0.0.1:1/v1"}
  dataset: {inline: {records: [{id: a, input: {text: q}, reference: {text: Paris}}, {id: b, input: {text: q}, reference: {text: Paris}}]}}
  evaluators: [{ref: exact-match}]
  gates: [{metric: exact-match, min: 0.5}]
`
	call := func() (string, map[string]any) {
		_, _, out := s.mcpCall(t, s.keys["owner"], nil, "tools/call", map[string]any{"name": "run", "arguments": map[string]any{"spec": spec}})
		text, isErr, structured := toolResult(t, out)
		if isErr {
			t.Fatalf("run: %s", text)
		}
		return text, structured
	}
	first, run1 := call()
	if !strings.Contains(first, ": succeeded") || !strings.Contains(first, "gate exact-match: passed") {
		t.Fatalf("first run: %s", first)
	}
	second, run2 := call()
	if !strings.Contains(second, "compared with run "+run1["id"].(string)) {
		t.Errorf("second run did not compare with the first: %s", second)
	}
	if _, ok := run2["comparison"]; !ok {
		t.Errorf("no structured comparison: %v", run2)
	}
	_, _, out := s.mcpCall(t, s.keys["owner"], nil, "tools/call", map[string]any{"name": "get_run", "arguments": map[string]any{"run_id": run2["id"]}})
	if text, isErr, _ := toolResult(t, out); isErr || !strings.Contains(text, "succeeded") {
		t.Errorf("get_run: %s", text)
	}
}
