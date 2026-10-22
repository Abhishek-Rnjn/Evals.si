package server

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

func (s *testServer) rest(t *testing.T, cred, method, path, body string) (int, string, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, s.url+path, rd)
	req.Header.Set("Authorization", "Bearer "+cred)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestRequiredClaimsStillRequireExp(t *testing.T) {
	s := startAuthServer(t, func(cfg *config.Config) { cfg.Auth.JWT.Providers[0].RequiredClaims = []string{"sub"} })
	noExp, err := jwt.Signed(s.signer).Claims(map[string]any{
		"iss": "https://token.actions.githubusercontent.com", "aud": "https://evals.example.com", "sub": "bob",
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	if code, body, _ := s.rest(t, noExp, "GET", "/v1alpha1/whoami", ""); code != http.StatusUnauthorized {
		t.Fatalf("token without exp = %d %s, want 401", code, body)
	}
	if code, body, _ := s.rest(t, s.token(t, map[string]any{"sub": "bob"}), "GET", "/v1alpha1/whoami", ""); code != http.StatusOK {
		t.Fatalf("token with exp = %d %s, want 200", code, body)
	}
}

func TestPermissiveForgedTokenGetsNoCELRoles(t *testing.T) {
	s := startAuthServer(t, func(cfg *config.Config) { cfg.Auth.JWT.Mode = auth.ModePermissive })
	evil, _ := rsa.GenerateKey(rand.Reader, 2048)
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: evil}, nil)
	tok, _ := jwt.Signed(signer).Claims(map[string]any{
		"iss": "https://token.actions.githubusercontent.com", "aud": "x", "exp": time.Now().Add(time.Hour).Unix(),
		"repository": "acme/agent", "ref": "refs/heads/main",
	}).Serialize()
	created, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["owner"])).
		CreateRun(t.Context(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Name: "r", Spec: runSpec("qwen3")}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetRun().GetId()
	code, body, _ := s.rest(t, tok, "GET", "/v1alpha1/runs/"+id, "")
	if code == http.StatusOK {
		t.Fatalf("forged token read a run through a cel: binding: %s", body)
	}
}

func TestReleaseDecisionMissingRunIndistinguishableFromForbidden(t *testing.T) {
	s := startAuthServer(t, nil)
	created, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["owner"])).
		CreateRun(t.Context(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Name: "r", Spec: runSpec("qwen3")}))
	if err != nil {
		t.Fatal(err)
	}
	rel := evalsiv1alpha1connect.NewReleaseServiceClient(s.http, s.url, as(s.keys["checkout-viewer"]))
	for _, id := range []string{created.Msg.GetRun().GetId(), "run-doesnotexist"} {
		_, err := rel.CreateReleaseDecision(t.Context(), connect.NewRequest(&evalsiv1alpha1.CreateReleaseDecisionRequest{
			Candidate: &evalsiv1alpha1.ReleaseRunEvidenceRef{RunId: id}, Baseline: &evalsiv1alpha1.ReleaseRunEvidenceRef{RunId: id},
		}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("CreateReleaseDecision(%q) = %v, want PermissionDenied", id, err)
		}
	}
}

func TestUnknownProjectIsPermissionDenied(t *testing.T) {
	s := startAuthServer(t, func(cfg *config.Config) { cfg.DatasetsDir = t.TempDir() })
	for _, who := range []string{"runner", "owner"} {
		_, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys[who])).
			CreateRun(t.Context(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "ghost", Spec: runSpec("qwen3")}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s CreateRun in unknown project = %v, want PermissionDenied", who, err)
		}
	}
	code, body, _ := s.rest(t, s.keys["owner"], "POST", "/v1alpha1/datasets/test/upload?project=ghost&format=jsonl", "{}\n")
	if code != http.StatusForbidden {
		t.Errorf("upload to unknown project = %d %s, want 403", code, body)
	}
}
