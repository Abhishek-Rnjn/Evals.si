package authz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/genproto/googleapis/rpc/status"
)

func TestAuthZEN(t *testing.T) {
	var calls atomic.Int64
	var slow atomic.Bool
	pdp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if slow.Load() {
			time.Sleep(300 * time.Millisecond)
		}
		var body struct {
			Subject  struct{ ID string } `json:"subject"`
			Action   struct{ Name string }
			Resource struct {
				ID string `json:"id"`
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("Authorization") != "Bearer pdp-secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		// Only reads in project support.
		ok := strings.HasSuffix(body.Action.Name, ".read") && body.Resource.ID == "support"
		_ = json.NewEncoder(w).Encode(map[string]bool{"decision": ok})
	}))
	defer pdp.Close()
	t.Setenv("PDP_TOKEN", "pdp-secret")
	e, err := NewEngine(context.Background(), Options{Enabled: true, Authorization: AuthorizationConfig{ExtAuthz: &ExtAuthzConfig{
		AuthZEN: &AuthZENConfig{URL: pdp.URL, TokenEnv: "PDP_TOKEN"}, Mode: "decide", Timeout: "100ms", CacheTTL: "1m",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u := user("u")
	if d := e.Decide(ctx, u, Request{Action: "runs.read", Project: "support"}); !d.Allowed || !strings.Contains(d.Reason, "authzen") {
		t.Errorf("read: %+v", d)
	}
	if e.Allowed(ctx, u, Request{Action: "runs.create", Project: "support"}) || e.Allowed(ctx, u, Request{Action: "runs.read", Project: "other"}) {
		t.Error("PDP denial ignored")
	}
	before := calls.Load()
	if d := e.Decide(ctx, u, Request{Action: "runs.read", Project: "support"}); !strings.Contains(d.Reason, "cached") || calls.Load() != before {
		t.Errorf("not cached: %+v", d)
	}
	slow.Store(true)
	if d := e.Decide(ctx, u, Request{Action: "traces.read", Project: "support"}); d.Allowed || !strings.Contains(d.Reason, "failing closed") {
		t.Errorf("timeout: %+v", d)
	}
}

type envoyServer struct {
	allow func(*authv3.CheckRequest) bool
}

func (s envoyServer) check(_ context.Context, req *connect.Request[authv3.CheckRequest]) (*connect.Response[authv3.CheckResponse], error) {
	code := int32(7) // PERMISSION_DENIED
	if s.allow(req.Msg) {
		code = 0
	}
	return connect.NewResponse(&authv3.CheckResponse{Status: &status.Status{Code: code}}), nil
}

func TestEnvoyExtAuthz(t *testing.T) {
	var seen atomic.Value
	srv := envoyServer{allow: func(r *authv3.CheckRequest) bool {
		ext := r.GetAttributes().GetContextExtensions()
		seen.Store(ext)
		return ext["evalsi.project"] == "support" && strings.Contains(ext["evalsi.principal"], "user:corp/u")
	}}
	mux := http.NewServeMux()
	mux.Handle("/envoy.service.auth.v3.Authorization/Check", connect.NewUnaryHandler("/envoy.service.auth.v3.Authorization/Check", srv.check))
	h := httptest.NewUnstartedServer(mux)
	h.EnableHTTP2 = true
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	p.SetHTTP1(true)
	h.Config.Protocols = p
	h.Start()
	defer h.Close()
	// Mode require: a local role must allow too.
	e, err := NewEngine(context.Background(), Options{Enabled: true,
		RBAC: RBACConfig{Projects: map[string]map[string][]string{"support": {"viewer": {"user:corp/u"}}, "other": {"viewer": {"user:corp/u"}}}},
		Authorization: AuthorizationConfig{ExtAuthz: &ExtAuthzConfig{
			Envoy: &EnvoyConfig{Address: strings.TrimPrefix(h.URL, "http://")}, Timeout: "2s", CacheTTL: "0s",
		}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u := user("u")
	req := Request{Action: "runs.read", Project: "support", Headers: http.Header{"Authorization": {"Bearer secret"}, "X-Team": {"a"}}}
	if d := e.Decide(ctx, u, req); !d.Allowed || !strings.Contains(d.Reason, "role viewer") {
		t.Errorf("allowed by both: %+v", d)
	}
	if e.Allowed(ctx, u, Request{Action: "runs.read", Project: "other"}) {
		t.Error("ext_authz denial ignored")
	}
	if e.Allowed(ctx, u, Request{Action: "runs.create", Project: "support"}) {
		t.Error("ext_authz allowed what no local role grants in require mode")
	}
	ext := seen.Load().(map[string]string)
	if ext["evalsi.action"] != "runs.read" || strings.Contains(ext["evalsi.principal"], "secret") {
		t.Errorf("context extensions = %v", ext)
	}
	// Down: fails closed.
	h.Close()
	if d := e.Decide(ctx, u, req); d.Allowed {
		t.Errorf("unreachable ext_authz allowed: %+v", d)
	}
}
