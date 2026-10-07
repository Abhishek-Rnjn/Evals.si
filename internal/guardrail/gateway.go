package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	extmcp "github.com/abhishek-rnjn/evals.si/gen/go/agentgateway/dev/ext_mcp"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// This file speaks agentgateway's two inline guardrail protocols:
//
//   - The prompt-guard webhook (ai.promptGuard.request/response[].webhook)
//     for LLM traffic: POST {"body":{"messages":[...]}} to the request path
//     and {"body":{"choices":[{"message":...}]}} to the response path; the
//     reply is {"action":{...}}, a pass ({reason}), a mask ({body, reason})
//     or a reject ({body, status_code, reason}). agentgateway tells them
//     apart by shape and ignores the HTTP status, so errors here are plain
//     text: any JSON object it cannot read as a mask or a reject is a pass.
//     Errors then follow the webhook's failureMode on the gateway.
//   - ExtMcp (mcpGuardrails processors of kind remote), a gRPC service, for
//     MCP traffic. The processor's metadata names the guardrail
//     (metadata: {project: '"support"', guardrail: '"tools"'}).

// LabelHeaderPrefix marks request headers whose values become labels for
// block_when (X-Evalsi-Label-Tenant: acme is labels["tenant"]). The webhook's
// headers setting adds them with CEL: x-evalsi-label-user: jwt.sub.
const LabelHeaderPrefix = "X-Evalsi-Label-"

type whMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type whChoice struct {
	Message whMessage `json:"message"`
}

type whBody struct {
	Messages []whMessage `json:"messages,omitempty"`
	Choices  []whChoice  `json:"choices,omitempty"`
}

// Webhook serves POST /guardrails/{project}/{guardrail}/{phase}, phase
// "request" or "response". authorize decides whether the caller may check
// content against the guardrail.
func (s *Service) Webhook(authorize func(r *http.Request, project, name string) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proj, name, phaseName := project(r.PathValue("project")), r.PathValue("guardrail"), r.PathValue("phase")
		var phase evalsiv1alpha1.GuardrailPhase
		switch phaseName {
		case "request":
			phase = evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_REQUEST
		case "response":
			phase = evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_RESPONSE
		default:
			http.Error(w, "the phase is request or response", http.StatusNotFound)
			return
		}
		if err := authorize(r, proj, name); err != nil {
			http.Error(w, err.Error(), httpStatus(err))
			return
		}
		var req struct {
			Body whBody `json:"body"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxContent+1<<16)).Decode(&req); err != nil {
			http.Error(w, "invalid guardrail webhook request: "+err.Error(), http.StatusBadRequest)
			return
		}
		in := Input{Phase: phase, Source: "llm", Labels: headerLabels(r.Header)}
		if phase == evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_REQUEST {
			msgs := req.Body.Messages
			for _, m := range msgs {
				in.Parts = append(in.Parts, m.Content)
			}
			// Evaluators judge the newest message; earlier ones were checked
			// when they were new and are its context.
			in.Focus = max(len(msgs)-1, 0)
			in.MakeContext = func(redacted []string) *evalsiv1alpha1.Content {
				if len(msgs) < 2 {
					return nil
				}
				var out []*evalsiv1alpha1.Message
				for i, m := range msgs[:len(msgs)-1] {
					out = append(out, &evalsiv1alpha1.Message{Role: m.Role, Content: redacted[i]})
				}
				return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Messages{Messages: &evalsiv1alpha1.Messages{Messages: out}}}
			}
		} else {
			for _, c := range req.Body.Choices {
				in.Parts = append(in.Parts, c.Message.Content)
			}
		}
		if len(in.Parts) == 0 {
			writeJSON(w, map[string]any{"action": map[string]any{"reason": "nothing to check"}})
			return
		}
		res, err := s.Run(r.Context(), proj, name, in)
		if err != nil {
			http.Error(w, err.Error(), httpStatus(err))
			return
		}
		reason := res.Response.GetReason()
		switch {
		case !res.Pass():
			writeJSON(w, map[string]any{"action": map[string]any{"body": res.Message, "status_code": http.StatusForbidden, "reason": reason}})
		case res.Masked():
			var body whBody
			if phase == evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_REQUEST {
				for i, m := range req.Body.Messages {
					body.Messages = append(body.Messages, whMessage{Role: m.Role, Content: res.Parts[i]})
				}
			} else {
				for i, c := range req.Body.Choices {
					body.Choices = append(body.Choices, whChoice{Message: whMessage{Role: c.Message.Role, Content: res.Parts[i]}})
				}
			}
			writeJSON(w, map[string]any{"action": map[string]any{"body": body, "reason": reason}})
		default:
			writeJSON(w, map[string]any{"action": map[string]any{"reason": reason}})
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// marshal is json.Marshal without HTML escaping, so a payload handed back
// to the gateway keeps its characters (<EMAIL>, not \u003cEMAIL\u003e).
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func httpStatus(err error) int {
	var ce *connect.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case connect.CodeUnauthenticated:
			return http.StatusUnauthorized
		case connect.CodePermissionDenied:
			return http.StatusForbidden
		case connect.CodeNotFound:
			return http.StatusNotFound
		case connect.CodeInvalidArgument, connect.CodeFailedPrecondition:
			return http.StatusBadRequest
		}
	}
	return http.StatusInternalServerError
}

func headerLabels(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vs := range h {
		if len(vs) > 0 && len(k) > len(LabelHeaderPrefix) && strings.EqualFold(k[:len(LabelHeaderPrefix)], LabelHeaderPrefix) {
			out[strings.ToLower(k[len(LabelHeaderPrefix):])] = vs[0]
		}
	}
	return out
}

// MCPTarget is the guardrail an ExtMcp call names in its metadata context.
func MCPTarget(md *structpb.Struct) (proj, name string, err error) {
	f := md.GetFields()
	name = f["guardrail"].GetStringValue()
	if name == "" {
		return "", "", connect.NewError(connect.CodeInvalidArgument,
			errors.New(`the processor's metadata must name the guardrail (metadata: {guardrail: '"name"', project: '"project"'})`))
	}
	return project(f["project"].GetStringValue()), name, nil
}

func mcpLabels(md *structpb.Struct, services []string) map[string]string {
	out := map[string]string{}
	for k, v := range md.GetFields() {
		if s, ok := v.GetKind().(*structpb.Value_StringValue); ok && k != "project" && k != "guardrail" {
			out[k] = s.StringValue
		}
	}
	if _, ok := out["service"]; !ok && len(services) == 1 {
		out["service"] = services[0]
	}
	return out
}

// structural keys of MCP payloads whose string values are not content.
var structural = map[string]bool{"type": true, "mimeType": true}

// mapStrings visits the content strings of a JSON value in a stable order
// (object keys sorted), replacing each with f's result.
func mapStrings(v any, f func(string) string) any {
	switch t := v.(type) {
	case string:
		return f(t)
	case []any:
		for i := range t {
			t[i] = mapStrings(t[i], f)
		}
		return t
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !structural[k] {
				t[k] = mapStrings(t[k], f)
			}
		}
		return t
	}
	return v
}

func decodeJSON(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// checkJSON checks the content strings of a JSON payload; mutated is set
// when the guardrail masked some of them.
func (s *Service) checkJSON(ctx context.Context, proj, name string, in Input, payload any) (*Result, []byte, error) {
	mapStrings(payload, func(str string) string { in.Parts = append(in.Parts, str); return str })
	if len(in.Parts) == 0 {
		return nil, nil, nil
	}
	res, err := s.Run(ctx, proj, name, in)
	if err != nil || !res.Masked() {
		return res, nil, err
	}
	i := 0
	mapStrings(payload, func(string) string { i++; return res.Parts[i-1] })
	out, err := marshal(payload)
	return res, out, err
}

func denied(res *Result) *extmcp.AuthorizationError {
	reason := res.Message
	if r := res.Response.GetReason(); r != "" {
		reason += ": " + r
	}
	return &extmcp.AuthorizationError{Code: extmcp.AuthorizationError_PERMISSION_DENIED, Reason: reason}
}

// ExtMcp implements agentgateway's ExtMcp processor service.
type ExtMcp struct{ s *Service }

// ExtMcp returns the service's ExtMcp handler.
func (s *Service) ExtMcp() *ExtMcp { return &ExtMcp{s: s} }

// CheckRequest checks an MCP call's params (for tools/call, its arguments).
func (x *ExtMcp) CheckRequest(ctx context.Context, req *connect.Request[extmcp.McpRequest]) (*connect.Response[extmcp.McpRequestResult], error) {
	m := req.Msg
	proj, name, err := MCPTarget(m.GetMetadataContext())
	if err != nil {
		return nil, err
	}
	pass := connect.NewResponse(&extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Pass{Pass: &extmcp.Pass{}}})
	if len(m.GetMcpRequest()) == 0 {
		return pass, nil
	}
	params, err := decodeJSON(m.GetMcpRequest())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("mcp_request: %w", err))
	}
	in := Input{Phase: evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_REQUEST, Source: "mcp", Method: m.GetMethod(),
		Labels: mcpLabels(m.GetMetadataContext(), m.GetServiceNames())}
	checked := params
	obj, isObj := params.(map[string]any)
	if m.GetMethod() == "tools/call" && isObj {
		in.Tool, _ = obj["name"].(string)
		checked = obj["arguments"]
	}
	res, mutated, err := x.s.checkJSON(ctx, proj, name, in, checked)
	switch {
	case err != nil:
		return nil, err
	case res == nil:
		return pass, nil
	case !res.Pass():
		return connect.NewResponse(&extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Error{Error: denied(res)}}), nil
	case mutated != nil:
		if m.GetMethod() == "tools/call" && isObj {
			obj["arguments"] = checked
			if mutated, err = marshal(obj); err != nil {
				return nil, err
			}
		}
		return connect.NewResponse(&extmcp.McpRequestResult{Result: &extmcp.McpRequestResult_Mutated{Mutated: mutated}}), nil
	}
	return pass, nil
}

// CheckResponse checks an MCP result: a tool's output, or the tools a
// server lists (their descriptions are where tool poisoning hides).
func (x *ExtMcp) CheckResponse(ctx context.Context, req *connect.Request[extmcp.McpResponse]) (*connect.Response[extmcp.McpResponseResult], error) {
	m := req.Msg
	proj, name, err := MCPTarget(m.GetMetadataContext())
	if err != nil {
		return nil, err
	}
	pass := connect.NewResponse(&extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Pass{Pass: &extmcp.Pass{}}})
	if len(m.GetMcpResponse()) == 0 {
		return pass, nil
	}
	result, err := decodeJSON(m.GetMcpResponse())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("mcp_response: %w", err))
	}
	in := Input{Phase: evalsiv1alpha1.GuardrailPhase_GUARDRAIL_PHASE_RESPONSE, Source: "mcp", Method: m.GetMethod(),
		Labels: mcpLabels(m.GetMetadataContext(), m.GetServiceNames())}
	res, mutated, err := x.s.checkJSON(ctx, proj, name, in, result)
	switch {
	case err != nil:
		return nil, err
	case res == nil:
		return pass, nil
	case !res.Pass():
		return connect.NewResponse(&extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Error{Error: denied(res)}}), nil
	case mutated != nil:
		return connect.NewResponse(&extmcp.McpResponseResult{Result: &extmcp.McpResponseResult_Mutated{Mutated: mutated}}), nil
	}
	return pass, nil
}
