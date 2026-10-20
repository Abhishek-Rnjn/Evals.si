package server

import (
	"net/http"
	"testing"

	"connectrpc.com/connect"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
)

// surfaces reads one run through every API surface a credential can use and
// returns each one's outcome: "ok", "unauthenticated" or "denied".
func surfaces(t *testing.T, s *testServer, cred, runID string) map[string]string {
	t.Helper()
	ctx := t.Context()
	outcome := func(err error) string {
		switch connect.CodeOf(err) {
		case connect.CodeUnauthenticated:
			return "unauthenticated"
		case connect.CodePermissionDenied, connect.CodeNotFound:
			return "denied"
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return "ok"
	}
	get := connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: runID})
	out := map[string]string{}
	_, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(cred)).GetRun(ctx, get)
	out["connect"] = outcome(err)
	_, err = evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(cred), connect.WithGRPC()).GetRun(ctx, get)
	out["grpc"] = outcome(err)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"/v1alpha1/runs/"+runID, nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	out["rest"] = map[int]string{200: "ok", 401: "unauthenticated", 403: "denied", 404: "denied"}[resp.StatusCode]

	status, _, body := s.mcpCall(t, cred, nil, "tools/call", map[string]any{"name": "get_run", "arguments": map[string]any{"run_id": runID}})
	switch {
	case status == http.StatusUnauthorized:
		out["mcp"] = "unauthenticated"
	case body["result"] == nil:
		t.Fatalf("mcp get_run: %d %v", status, body)
	default:
		if _, isErr, _ := toolResult(t, body); isErr {
			out["mcp"] = "denied"
		} else {
			out["mcp"] = "ok"
		}
	}
	return out
}

func expectEverywhere(t *testing.T, got map[string]string, want, when string) {
	t.Helper()
	for surface, outcome := range got {
		if outcome != want {
			t.Errorf("%s: %s gave %q, want %q (all surfaces: %v)", when, surface, outcome, want, got)
		}
	}
}

// ENT-02: one identity gets the same decision on every API surface, and
// revoking it ends access on all of them at once.
func TestAnIdentityIsDecidedAlikeOnEverySurfaceAndRevokedEverywhere(t *testing.T) {
	s := startAuthServer(t, nil)
	ctx := t.Context()
	owner := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["owner"]))
	run, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["owner"])).CreateRun(ctx,
		connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("qwen3")}))
	if err != nil {
		t.Fatal(err)
	}
	id := run.Msg.GetRun().GetId()

	created, err := owner.CreateAPIKey(ctx, connect.NewRequest(&evalsiv1alpha1.CreateAPIKeyRequest{
		Name: "matrix", Roles: map[string]*evalsiv1alpha1.RoleList{"support": {Roles: []string{"viewer"}}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	key := created.Msg.GetSecret()
	expectEverywhere(t, surfaces(t, s, key, id), "ok", "a support viewer reading a support run")

	// Project isolation: a viewer of another project is refused on every surface.
	expectEverywhere(t, surfaces(t, s, s.keys["checkout-viewer"], id), "denied", "a checkout viewer reading a support run")

	if _, err := owner.RevokeAPIKey(ctx, connect.NewRequest(&evalsiv1alpha1.RevokeAPIKeyRequest{Name: "matrix"})); err != nil {
		t.Fatal(err)
	}
	expectEverywhere(t, surfaces(t, s, key, id), "unauthenticated", "after revoking the key")

	// A JWT identity's access comes from a binding; deleting the binding ends
	// it on every surface by the next request, while the token stays valid.
	tok := s.token(t, map[string]any{"sub": "alice", "repository": "acme/other"})
	binding := &evalsiv1alpha1.RoleBinding{Project: "support", Role: "viewer", Subject: "user:github/alice"}
	if _, err := owner.CreateBinding(ctx, connect.NewRequest(&evalsiv1alpha1.CreateBindingRequest{Binding: binding})); err != nil {
		t.Fatal(err)
	}
	expectEverywhere(t, surfaces(t, s, tok, id), "ok", "a bound JWT user")
	if _, err := owner.DeleteBinding(ctx, connect.NewRequest(&evalsiv1alpha1.DeleteBindingRequest{Binding: binding})); err != nil {
		t.Fatal(err)
	}
	expectEverywhere(t, surfaces(t, s, tok, id), "denied", "after deleting the JWT user's binding")
}
