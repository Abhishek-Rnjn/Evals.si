package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	extmcp "github.com/abhishek-rnjn/evals.si/gen/go/agentgateway/dev/ext_mcp"
	"github.com/abhishek-rnjn/evals.si/gen/go/agentgateway/dev/ext_mcp/ext_mcpconnect"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

func TestGuardrails(t *testing.T) {
	var gatewayKey string
	s := startAuthServer(t, func(c *config.Config) {
		plain, hash := auth.NewAPIKey()
		gatewayKey = plain
		c.Auth.APIKeys.Keys = append(c.Auth.APIKeys.Keys, auth.ConfigKey{
			Name: "gateway", Key: "sha256:" + hash, Roles: map[string][]string{"support": {"guard", "ingest"}},
		})
	})
	ctx := context.Background()
	client := func(cred string) evalsiv1alpha1connect.GuardrailServiceClient {
		return evalsiv1alpha1connect.NewGuardrailServiceClient(s.http, s.url, as(cred))
	}
	g := &evalsiv1alpha1.Guardrail{
		Name: "chat", Project: "support", Message: "Not here.",
		Redact:     []*evalsiv1alpha1.RedactRule{{Builtin: "email"}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
		BlockWhen:  `content.contains("launch codes")`,
	}
	apply := &evalsiv1alpha1.ApplyGuardrailRequest{Guardrail: g}
	if _, err := client(s.keys["runner"]).ApplyGuardrail(ctx, connect.NewRequest(apply)); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("runner applying: %v", err)
	}
	applied, err := client(s.keys["editor"]).ApplyGuardrail(ctx, connect.NewRequest(apply))
	if err != nil {
		t.Fatal(err)
	}
	if applied.Msg.GetGuardrail().GetUpdatedBy() != "key:editor" {
		t.Errorf("updated_by: %v", applied.Msg.GetGuardrail())
	}
	if got, err := client(s.keys["viewer"]).GetGuardrail(ctx, connect.NewRequest(&evalsiv1alpha1.GetGuardrailRequest{Project: "support", Name: "chat"})); err != nil || got.Msg.GetGuardrail().GetBlockWhen() == "" {
		t.Errorf("viewer reading: %v %v", got, err)
	}
	if _, err := client(s.keys["checkout-viewer"]).GetGuardrail(ctx, connect.NewRequest(&evalsiv1alpha1.GetGuardrailRequest{Project: "support", Name: "chat"})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("other project reading: %v", err)
	}

	// The prompt-guard webhook, as agentgateway calls it.
	webhook := func(cred, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, s.url+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cred != "" {
			req.Header.Set("Authorization", "Bearer "+cred)
		}
		resp, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	prompt := `{"body":{"messages":[{"role":"user","content":"reach me at a@b.co"}]}}`
	code, body := webhook(gatewayKey, "/guardrails/support/chat/request", prompt)
	if code != 200 || !strings.Contains(body, `"content":"reach me at <EMAIL>"`) {
		t.Errorf("mask: %d %s", code, body)
	}
	code, body = webhook(gatewayKey, "/guardrails/support/chat/response", `{"body":{"choices":[{"message":{"role":"assistant","content":"the launch codes are 0000"}}]}}`)
	if code != 200 || !strings.Contains(body, `"body":"Not here."`) || !strings.Contains(body, `"status_code":403`) {
		t.Errorf("reject: %d %s", code, body)
	}
	for name, cred := range map[string]string{"anonymous": "", "viewer": s.keys["viewer"], "ingest": s.keys["ingest"], "checkout-viewer": s.keys["checkout-viewer"]} {
		code, body := webhook(cred, "/guardrails/support/chat/request", prompt)
		want := http.StatusForbidden
		if cred == "" {
			want = http.StatusUnauthorized
		}
		// Plain text: a JSON error object would read as a pass to the gateway.
		if code != want || json.Valid([]byte(body)) {
			t.Errorf("%s: %d %q", name, code, body)
		}
	}

	// ExtMcp over gRPC, as agentgateway's mcpGuardrails remote processor calls it.
	mcp := func(cred string) ext_mcpconnect.ExtMcpClient {
		return ext_mcpconnect.NewExtMcpClient(s.http, s.url, connect.WithGRPC(), as(cred))
	}
	md, _ := structpb.NewStruct(map[string]any{"project": "support", "guardrail": "chat"})
	req := func(args string) *connect.Request[extmcp.McpRequest] {
		return connect.NewRequest(&extmcp.McpRequest{Method: "tools/call", MetadataContext: md, ServiceNames: []string{"crm"},
			McpRequest: []byte(`{"name":"email","arguments":` + args + `}`)})
	}
	res, err := mcp(gatewayKey).CheckRequest(ctx, req(`{"to":"x@y.io"}`))
	if err != nil || !strings.Contains(string(res.Msg.GetMutated()), `"to":"<EMAIL>"`) {
		t.Errorf("mcp mask: %v %v", res, err)
	}
	res, err = mcp(gatewayKey).CheckRequest(ctx, req(`{"body":"send the launch codes"}`))
	if err != nil || res.Msg.GetError().GetCode() != extmcp.AuthorizationError_PERMISSION_DENIED {
		t.Errorf("mcp block: %v %v", res, err)
	}
	if _, err := mcp("").CheckRequest(ctx, req(`{}`)); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("mcp anonymous: %v", err)
	}
	if _, err := mcp(s.keys["viewer"]).CheckRequest(ctx, req(`{}`)); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("mcp viewer: %v", err)
	}
	other, _ := structpb.NewStruct(map[string]any{"project": "checkout", "guardrail": "chat"})
	if _, err := mcp(gatewayKey).CheckRequest(ctx, connect.NewRequest(&extmcp.McpRequest{Method: "tools/call", MetadataContext: other, McpRequest: []byte(`{}`)})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("mcp other project: %v", err)
	}

	// A dry run with an inline guardrail scores like Evaluate, so it needs evaluations.run.
	check := &evalsiv1alpha1.CheckRequest{Project: "support", Content: "hi", Inline: &evalsiv1alpha1.Guardrail{Name: "dry", Redact: g.GetRedact()}}
	if _, err := client(gatewayKey).Check(ctx, connect.NewRequest(check)); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("guard dry run: %v", err)
	}
	if got, err := client(s.keys["runner"]).Check(ctx, connect.NewRequest(check)); err != nil || got.Msg.GetDecision() != evalsiv1alpha1.GuardrailDecision_GUARDRAIL_DECISION_PASS {
		t.Errorf("runner dry run: %v %v", got, err)
	}

	// REST and metrics.
	get := func(cred, path string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, s.url+path, nil)
		req.Header.Set("Authorization", "Bearer "+cred)
		resp, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(s.keys["viewer"], "/v1alpha1/guardrails?project=support"); code != 200 || !strings.Contains(body, `"name":"chat"`) {
		t.Errorf("REST list: %d %s", code, body)
	}
	if code, body := get(s.keys["owner"], "/metrics"); code != 200 ||
		!strings.Contains(body, `evalsi_guardrail_blocked_total{project="support",guardrail="chat",phase="response",verdict="block"} 1`) {
		t.Errorf("metrics: %d\n%s", code, body)
	}
}
