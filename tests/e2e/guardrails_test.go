package e2e

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	extmcp "github.com/abhishek-rnjn/evals.si/gen/go/agentgateway/dev/ext_mcp"
	"github.com/abhishek-rnjn/evals.si/gen/go/agentgateway/dev/ext_mcp/ext_mcpconnect"
)

// TestGuardrails applies the example guardrail with the CLI and checks
// traffic the way agentgateway sends it: the prompt-guard webhook for LLM
// calls and the ExtMcp processor for MCP calls, scored by the real safety
// pack in the Python worker.
func TestGuardrails(t *testing.T) {
	e := start(t)
	evalsi := func(args ...string) (string, int) {
		t.Helper()
		full := append(append([]string{}, e.workerCmd[1:]...), args...)
		out, err := exec.Command(e.workerCmd[0], full...).CombinedOutput()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return string(out), ee.ExitCode()
		}
		if err != nil {
			t.Fatalf("evalsi %v: %v\n%s", args, err, out)
		}
		return string(out), 0
	}
	if out, code := evalsi("guardrails", "apply", "-f", "../../examples/guardrails/support.yaml", "--server", e.base); code != 0 {
		t.Fatalf("apply: %d %s", code, out)
	}

	webhook := func(phase, body string) string {
		t.Helper()
		resp, err := http.Post(e.base+"/guardrails/support/support-chat/"+phase, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", phase, resp.StatusCode, b)
		}
		return string(b)
	}
	body := webhook("request", `{"body":{"messages":[{"role":"system","content":"You help customers."},`+
		`{"role":"user","content":"I'm jo@example.com, SSN 123-45-6789, account ACCT-0012345. Why was I charged twice?"}]}}`)
	if !strings.Contains(body, `I'm <EMAIL>, SSN <US_SSN>, account <ACCOUNT>. Why was I charged twice?`) {
		t.Errorf("mask: %s", body)
	}
	body = webhook("response", `{"body":{"choices":[{"message":{"role":"assistant","content":"Use key AKIAIOSFODNN7EXAMPLE to log in."}}]}}`)
	if !strings.Contains(body, `"status_code":403`) || !strings.Contains(body, "secret-leak") {
		t.Errorf("reject: %s", body)
	}
	body = webhook("response", `{"body":{"choices":[{"message":{"role":"assistant","content":"You were charged once; the second line is a hold."}}]}}`)
	if strings.Contains(body, `"body"`) {
		t.Errorf("pass: %s", body)
	}

	// ExtMcp, over gRPC.
	md, _ := structpb.NewStruct(map[string]any{"project": "support", "guardrail": "support-chat"})
	client := ext_mcpconnect.NewExtMcpClient(h2cClient(), e.base, connect.WithGRPC())
	res, err := client.CheckRequest(context.Background(), connect.NewRequest(&extmcp.McpRequest{
		Method: "tools/call", MetadataContext: md, ServiceNames: []string{"billing"},
		McpRequest: []byte(`{"name":"refund","arguments":{"account":"ACCT-0012345","note":"customer jo@example.com"}}`),
	}))
	if err != nil || !strings.Contains(string(res.Msg.GetMutated()), `"account":"<ACCOUNT>"`) {
		t.Errorf("mcp: %v %v", res, err)
	}

	// The CLI's dry run exits 3 when content would be blocked.
	if out, code := evalsi("guardrails", "check", "support-chat", "--project", "support", "--server", e.base,
		"--text", "ghp_"+strings.Repeat("a", 36)); code != 3 || !strings.HasPrefix(out, "block: ") {
		t.Errorf("check: %d %s", code, out)
	}
}
