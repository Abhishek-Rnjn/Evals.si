package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/harness/v1alpha1"
	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
)

// TestActionTableCoversEveryRPC walks the API's service descriptors: a new
// RPC cannot ship without saying which permission it needs.
func TestActionTableCoversEveryRPC(t *testing.T) {
	rules := (&gate{}).accessRules()
	seen := map[string]bool{}
	n := 0
	walk := func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			svc := fd.Services().Get(i)
			for j := range svc.Methods().Len() {
				proc := "/" + string(svc.FullName()) + "/" + string(svc.Methods().Get(j).Name())
				n++
				seen[proc] = true
				if _, ok := rules[proc]; !ok {
					t.Errorf("%s has no entry in the action table", proc)
				}
			}
		}
		return true
	}
	protoregistry.GlobalFiles.RangeFilesByPackage("evalsi.v1alpha1", walk)
	// agentgateway's guardrail processor protocol, served for MCP traffic.
	protoregistry.GlobalFiles.RangeFilesByPackage("agentgateway.dev.ext_mcp", walk)
	if n < 30 {
		t.Fatalf("found only %d RPCs; is the registry populated?", n)
	}
	for _, proc := range []string{ingest.TraceExportProcedure, reflectV1, reflectV1Alpha} {
		seen[proc] = true
		if _, ok := rules[proc]; !ok {
			t.Errorf("%s has no entry in the action table", proc)
		}
	}
	for proc, r := range rules {
		if !seen[proc] {
			t.Errorf("action table names %s, which is not an RPC", proc)
		}
		if _, ok := authz.Lookup(r.action); !ok {
			t.Errorf("%s needs unknown permission %q", proc, r.action)
		}
	}
}

// fakeWorker scores exact-match and generates "Paris".
type fakeWorker struct{}

func (fakeWorker) Describe(context.Context) ([]*evalsiv1alpha1.EvaluatorManifest, error) {
	schema, _ := structpb.NewStruct(map[string]any{"type": "object", "properties": map[string]any{}})
	return []*evalsiv1alpha1.EvaluatorManifest{
		{Name: "builtin/exact-match", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD, ParamsSchema: schema,
			Outputs: []*evalsiv1alpha1.MetricSpec{{Name: "exact-match", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED}}},
		{Name: "builtin/unit-tests", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD, ParamsSchema: schema,
			Requires:   &evalsiv1alpha1.Requirements{Isolation: evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_CONFINED},
			Scheduling: &evalsiv1alpha1.Scheduling{Pool: "sandbox"},
			Outputs:    []*evalsiv1alpha1.MetricSpec{{Name: "unit-tests", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED}}},
	}, nil
}

func (fakeWorker) Evaluate(_ context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	resp := &pluginv1alpha1.EvaluateResponse{BatchId: req.GetBatchId()}
	for _, r := range req.GetRecords() {
		resp.Results = append(resp.Results, &evalsiv1alpha1.EvaluationResult{
			RecordId: r.GetId(), Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED,
			Scores: []*evalsiv1alpha1.Score{{Value: &evalsiv1alpha1.Score_Passed{Passed: true}}},
		})
	}
	return resp, nil
}

func (fakeWorker) Reduce(context.Context, *pluginv1alpha1.ReduceRequest) (*pluginv1alpha1.ReduceResponse, error) {
	return &pluginv1alpha1.ReduceResponse{}, nil
}

func (fakeWorker) Generate(_ context.Context, req *pluginv1alpha1.GenerateRequest) (*pluginv1alpha1.GenerateResponse, error) {
	resp := &pluginv1alpha1.GenerateResponse{}
	for _, r := range req.GetRecords() {
		resp.Results = append(resp.Results, &pluginv1alpha1.GenerateResult{RecordId: r.GetId(), Output: text("Paris")})
	}
	return resp, nil
}

func (fakeWorker) RunTask(context.Context, *pluginv1alpha1.RunTaskRequest, func(*harnessv1alpha1.TrajectoryEvent)) (*pluginv1alpha1.TaskResult, error) {
	return &pluginv1alpha1.TaskResult{Error: "no agents in this test"}, nil
}

func (fakeWorker) LoadDataset(context.Context, *pluginv1alpha1.LoadDatasetRequest) ([]*evalsiv1alpha1.Record, error) {
	return nil, errors.New("no datasets")
}

func text(s string) *evalsiv1alpha1.Content {
	return &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: s}}
}

type testServer struct {
	url    string
	keys   map[string]string // role name -> plaintext API key
	signer jose.Signer
	http   *http.Client
}

func (s *testServer) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	base := map[string]any{"iss": "https://token.actions.githubusercontent.com", "aud": "https://evals.example.com",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
	for k, v := range claims {
		base[k] = v
	}
	tok, err := jwt.Signed(s.signer).Claims(base).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func startAuthServer(t *testing.T, mutate func(*config.Config)) *testServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: key, KeyID: "k1", Algorithm: "RS256", Use: "sig"}
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jwk}, nil)
	inline, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk.Public()}})

	ts := &testServer{keys: map[string]string{}, signer: signer}
	roles := map[string]map[string][]string{
		"owner":           {"*": {"owner"}},
		"viewer":          {"support": {"viewer"}},
		"runner":          {"support": {"runner"}},
		"editor":          {"support": {"editor"}},
		"admin":           {"support": {"admin"}},
		"ingest":          {"support": {"ingest"}},
		"checkout-viewer": {"checkout": {"viewer"}},
		"prompt-engineer": {"support": {"prompt-engineer"}},
		"key-admin":       {"support": {"key-admin"}},
		"blind-runner":    {"support": {"blind-runner"}},
		"checkout-runner": {"checkout": {"runner"}},
	}
	var keys []auth.ConfigKey
	for name, r := range roles {
		plain, hash := auth.NewAPIKey()
		ts.keys[name] = plain
		keys = append(keys, auth.ConfigKey{Name: name, Key: "sha256:" + hash, Roles: r})
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.DataDir = t.TempDir()
	cfg.Auth = &auth.Config{
		JWT: &auth.JWTConfig{Providers: []auth.Provider{{
			Name: "github", Issuer: "https://token.actions.githubusercontent.com",
			Audiences: []string{"https://evals.example.com"}, JWKS: auth.JWKS{Inline: inline},
		}}},
		APIKeys:  &auth.APIKeysConfig{Keys: keys},
		CLILogin: &auth.CLILogin{Issuer: "https://sso.example.com", ClientID: "evalsi-cli"},
	}
	cfg.RBAC = authz.RBACConfig{
		Roles: []authz.Role{
			{Name: "prompt-engineer", Inherits: []string{"viewer"}, Permissions: []string{"evaluations.run", "runs.create"},
				Condition: `!has(resource.target) || resource.target.model in ["qwen3"]`},
			{Name: "key-admin", Permissions: []string{"access.manage", "runs.read"}},
			// Starts runs but may not read production traces.
			{Name: "blind-runner", Permissions: []string{"runs.create", "runs.read", "evaluations.run"}},
		},
		Projects: map[string]map[string][]string{
			"support":  {"runner": {`cel:jwt.repository == "acme/agent" && jwt.ref == "refs/heads/main"`}},
			"checkout": {},
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, fakeWorker{}, nil, slog.New(slog.DiscardHandler), ready) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	select {
	case addr := <-ready:
		ts.url = "http://" + addr
	case err := <-done:
		t.Fatal(err)
	}
	ts.http = &http.Client{Transport: &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, n, a string, _ *tls.Config) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, a)
	}}}
	return ts
}

// as returns client options that authenticate with an API key or token.
func as(cred string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if cred != "" {
				req.Header().Set("Authorization", "Bearer "+cred)
			}
			return next(ctx, req)
		}
	}))
}

func codeOf(err error) connect.Code {
	if err == nil {
		return 0
	}
	return connect.CodeOf(err)
}

func runSpec(model string, evaluators ...string) *evalsiv1alpha1.RunSpec {
	if len(evaluators) == 0 {
		evaluators = []string{"exact-match"}
	}
	var refs []*evalsiv1alpha1.EvaluatorRef
	for _, e := range evaluators {
		refs = append(refs, &evalsiv1alpha1.EvaluatorRef{Ref: e})
	}
	return &evalsiv1alpha1.RunSpec{
		Target: &evalsiv1alpha1.Target{Connector: "openai-compatible", Model: model, BaseUrl: "http://127.0.0.1:1/v1"},
		Dataset: &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{
			Records: []*evalsiv1alpha1.Record{{Input: text("Capital of France?"), Reference: text("Paris")}},
		}}},
		Evaluators: refs,
	}
}

func TestUnauthenticatedSurfaces(t *testing.T) {
	s := startAuthServer(t, nil)
	ctx := context.Background()
	for _, opts := range [][]connect.ClientOption{{connect.WithGRPC()}, {connect.WithGRPCWeb()}, {}} {
		runs := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, opts...)
		if _, err := runs.ListRuns(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{})); codeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("ListRuns %v: err = %v", opts, err)
		}
		cat := evalsiv1alpha1connect.NewCatalogServiceClient(s.http, s.url, append(opts, as("evk_bogus"))...)
		if _, err := cat.ListEvaluators(ctx, connect.NewRequest(&evalsiv1alpha1.ListEvaluatorsRequest{})); codeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("ListEvaluators with a bad key %v: err = %v", opts, err)
		}
	}
	otlp := connect.NewClient[collectortracepb.ExportTraceServiceRequest, collectortracepb.ExportTraceServiceResponse](
		s.http, s.url+ingest.TraceExportProcedure, connect.WithGRPC())
	if _, err := otlp.CallUnary(ctx, connect.NewRequest(&collectortracepb.ExportTraceServiceRequest{})); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("OTLP gRPC: err = %v", err)
	}
	stream := evalsiv1alpha1connect.NewEvaluationServiceClient(s.http, s.url, connect.WithGRPC()).EvaluateStream(ctx)
	_ = stream.Send(&evalsiv1alpha1.EvaluateStreamRequest{Message: &evalsiv1alpha1.EvaluateStreamRequest_Config{Config: &evalsiv1alpha1.EvaluateStreamConfig{}}})
	if _, err := stream.Receive(); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("EvaluateStream: err = %v", err)
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/v1alpha1/runs", http.StatusUnauthorized},
		{http.MethodGet, "/v1alpha1/whoami", http.StatusUnauthorized},
		{http.MethodPost, "/v1/traces", http.StatusUnauthorized},
		{http.MethodGet, "/metrics", http.StatusUnauthorized},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/.well-known/evalsi-auth", http.StatusOK},
	} {
		var body io.Reader
		if tc.method == http.MethodPost {
			body = strings.NewReader("{}")
		}
		req, _ := http.NewRequest(tc.method, s.url+tc.path, body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s = %d, want %d (%s)", tc.method, tc.path, resp.StatusCode, tc.want, respBody)
		}
		if tc.want == http.StatusUnauthorized && !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: no WWW-Authenticate challenge", tc.path)
		}
		if tc.path == "/.well-known/evalsi-auth" && !strings.Contains(string(respBody), "evalsi-cli") {
			t.Errorf("login discovery = %s", respBody)
		}
	}
	// Reflection is catalog.read: refused without a credential.
	refl := connect.NewClient[emptyMsg, emptyMsg](s.http, s.url+reflectV1, connect.WithGRPC())
	rs := refl.CallBidiStream(ctx)
	_ = rs.Send(nil)
	if _, err := rs.Receive(); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("reflection: err = %v", err)
	}
}

type emptyMsg = structpb.Struct

func TestPermissionMatrix(t *testing.T) {
	s := startAuthServer(t, nil)
	ctx := context.Background()
	runsAs := func(who string) evalsiv1alpha1connect.RunServiceClient {
		return evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys[who]))
	}
	// Runners create runs in their project, not elsewhere; viewers cannot.
	created, err := runsAs("runner").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("qwen3"), Labels: map[string]string{"app": "x"}}))
	if err != nil {
		t.Fatal(err)
	}
	run := created.Msg.GetRun()
	if run.GetCreatedBy() != "key:runner" || run.GetProject() != "support" || run.GetLabels()["app"] != "x" {
		t.Errorf("run = %+v", run)
	}
	for who, project := range map[string]string{"viewer": "support", "runner": "checkout", "checkout-viewer": "checkout", "ingest": "support"} {
		_, err := runsAs(who).CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: project, Spec: runSpec("qwen3")}))
		if codeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s creating a run in %s: err = %v", who, project, err)
		}
	}
	if _, err := runsAs("runner").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "nope", Spec: runSpec("qwen3")})); codeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown project: %v", err)
	}
	// Reads: the project's viewers yes, other projects' no (and a forbidden run looks missing).
	if _, err := runsAs("viewer").GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: run.GetId()})); err != nil {
		t.Errorf("viewer GetRun: %v", err)
	}
	if _, err := runsAs("checkout-viewer").GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: run.GetId()})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("checkout viewer reading a support run: %v", err)
	}
	if _, err := runsAs("viewer").CancelRun(ctx, connect.NewRequest(&evalsiv1alpha1.CancelRunRequest{Id: run.GetId()})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("viewer cancelling: %v", err)
	}
	for who, want := range map[string]int{"viewer": 1, "checkout-viewer": 0, "owner": 1, "ingest": 0} {
		list, err := runsAs(who).ListRuns(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{}))
		if err != nil || len(list.Msg.GetRuns()) != want {
			t.Errorf("%s ListRuns = %d runs, %v; want %d", who, len(list.Msg.GetRuns()), err, want)
		}
	}
	// A custom role with a model condition starts runs only on its models.
	if _, err := runsAs("prompt-engineer").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("qwen3")})); err != nil {
		t.Errorf("prompt engineer on qwen3: %v", err)
	}
	if _, err := runsAs("prompt-engineer").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("gpt-5")})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("prompt engineer on gpt-5: %v", err)
	}
	// A GitHub Actions job on main runs a gated evaluation with its own OIDC token; a branch cannot.
	main := s.token(t, map[string]any{"sub": "repo:acme/agent:ref:refs/heads/main", "repository": "acme/agent", "ref": "refs/heads/main"})
	if _, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(main)).CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("qwen3")})); err != nil {
		t.Errorf("GitHub Actions on main: %v", err)
	}
	branch := s.token(t, map[string]any{"sub": "repo:acme/agent:ref:refs/heads/x", "repository": "acme/agent", "ref": "refs/heads/x"})
	if _, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(branch)).CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("qwen3")})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("GitHub Actions on a branch: %v", err)
	}
	// Evaluate: runner yes, viewer no; EvaluateStream checks its config message.
	eval := func(who string) error {
		_, err := evalsiv1alpha1connect.NewEvaluationServiceClient(s.http, s.url, as(s.keys[who])).Evaluate(ctx, connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{
			Project: "support", Records: []*evalsiv1alpha1.Record{{Output: text("a"), Reference: text("a")}},
			Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
		}))
		return err
	}
	if err := eval("runner"); err != nil {
		t.Errorf("runner Evaluate: %v", err)
	}
	if err := eval("viewer"); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("viewer Evaluate: %v", err)
	}
	stream := evalsiv1alpha1connect.NewEvaluationServiceClient(s.http, s.url, connect.WithGRPC(), as(s.keys["viewer"])).EvaluateStream(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+s.keys["viewer"])
	_ = stream.Send(&evalsiv1alpha1.EvaluateStreamRequest{Message: &evalsiv1alpha1.EvaluateStreamRequest_Config{Config: &evalsiv1alpha1.EvaluateStreamConfig{
		Project: "support", Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
	}}})
	_ = stream.CloseRequest()
	if _, err := stream.Receive(); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("viewer EvaluateStream: %v", err)
	}
	// Policies: editor writes, runner cannot.
	policy := &evalsiv1alpha1.OnlineEvalPolicy{Name: "p1", Project: "support", Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}}}}}
	mon := func(who string) evalsiv1alpha1connect.MonitorServiceClient {
		return evalsiv1alpha1connect.NewMonitorServiceClient(s.http, s.url, as(s.keys[who]))
	}
	if _, err := mon("runner").ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("runner ApplyPolicy: %v", err)
	}
	if _, err := mon("editor").ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy})); err != nil {
		t.Errorf("editor ApplyPolicy: %v", err)
	}
	if list, _ := mon("checkout-viewer").ListPolicies(ctx, connect.NewRequest(&evalsiv1alpha1.ListPoliciesRequest{})); len(list.Msg.GetPolicies()) != 0 {
		t.Error("checkout viewer sees support policies")
	}
	// Metrics are owner-only on the main port.
	for who, want := range map[string]int{"owner": 200, "admin": 403} {
		req, _ := http.NewRequest(http.MethodGet, s.url+"/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+s.keys[who])
		resp, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s /metrics = %d, want %d", who, resp.StatusCode, want)
		}
	}
	// REST goes through the same gate.
	req, _ := http.NewRequest(http.MethodGet, s.url+"/v1alpha1/runs/"+run.GetId(), nil)
	req.Header.Set("Authorization", "Bearer "+s.keys["checkout-viewer"])
	resp, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("REST GetRun by another project's viewer = %d", resp.StatusCode)
	}
}

func otlpRequest(service string, attrs map[string]string) []byte {
	res := &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}}}}}
	for k, v := range attrs {
		res.Attributes = append(res.Attributes, &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}})
	}
	now := uint64(time.Now().UnixNano())
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	b, _ := proto.Marshal(&collectortracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: res,
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{{
			TraceId: id, SpanId: id[:8], Name: "root", StartTimeUnixNano: now - 1e6, EndTimeUnixNano: now,
		}}}},
	}}})
	return b
}

func TestIngestAssignsProjectFromCredential(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) { c.OTLP.Grace = "10ms" })
	post := func(who string, attrs map[string]string) int {
		req, _ := http.NewRequest(http.MethodPost, s.url+"/v1/traces", bytes.NewReader(otlpRequest("agent", attrs)))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", "Bearer "+s.keys[who])
		resp, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("ingest", map[string]string{"evalsi.label.env": "prod"}); got != 200 {
		t.Fatalf("ingest key = %d", got)
	}
	// An ingest key cannot push into another project's policies, and viewers cannot ingest.
	if got := post("ingest", map[string]string{"evalsi.project": "checkout"}); got != http.StatusForbidden {
		t.Errorf("ingest into checkout = %d", got)
	}
	if got := post("viewer", nil); got != http.StatusForbidden {
		t.Errorf("viewer ingest = %d", got)
	}
	traces := func(who string) []*evalsiv1alpha1.TraceSummary {
		c := evalsiv1alpha1connect.NewTraceServiceClient(s.http, s.url, as(s.keys[who]))
		var out []*evalsiv1alpha1.TraceSummary
		for range 100 {
			list, err := c.ListTraces(context.Background(), connect.NewRequest(&evalsiv1alpha1.ListTracesRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if out = list.Msg.GetTraces(); len(out) > 0 || who != "viewer" {
				return out
			}
			time.Sleep(50 * time.Millisecond)
		}
		return out
	}
	got := traces("viewer")
	if len(got) != 1 || got[0].GetProject() != "support" || got[0].GetLabels()["env"] != "prod" {
		t.Fatalf("support viewer traces = %v", got)
	}
	if other := traces("checkout-viewer"); len(other) != 0 {
		t.Errorf("checkout viewer sees %d support traces", len(other))
	}
	c := evalsiv1alpha1connect.NewTraceServiceClient(s.http, s.url, as(s.keys["checkout-viewer"]))
	if _, err := c.GetTrace(context.Background(), connect.NewRequest(&evalsiv1alpha1.GetTraceRequest{TraceId: got[0].GetTraceId()})); codeOf(err) != connect.CodeNotFound {
		t.Errorf("checkout viewer GetTrace: %v", err)
	}
}

func TestAuthServiceAndAudit(t *testing.T) {
	s := startAuthServer(t, nil)
	ctx := context.Background()
	authAs := func(cred string) evalsiv1alpha1connect.AuthServiceClient {
		return evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(cred))
	}
	me, err := authAs(s.keys["prompt-engineer"]).WhoAmI(ctx, connect.NewRequest(&evalsiv1alpha1.WhoAmIRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if p := me.Msg.GetPrincipal(); p.GetId() != "key:prompt-engineer" || me.Msg.GetOwner() || len(me.Msg.GetAccess()) != 1 ||
		me.Msg.GetAccess()[0].GetProject() != "support" || !strings.Contains(strings.Join(me.Msg.GetAccess()[0].GetPermissions(), ","), "runs.read") {
		t.Errorf("whoami = %v", me.Msg)
	}
	owner := authAs(s.keys["owner"])
	if _, err := owner.CreateProject(ctx, connect.NewRequest(&evalsiv1alpha1.CreateProjectRequest{Name: "labs"})); err != nil {
		t.Fatal(err)
	}
	if _, err := authAs(s.keys["admin"]).CreateProject(ctx, connect.NewRequest(&evalsiv1alpha1.CreateProjectRequest{Name: "x"})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("admin creating a project: %v", err)
	}
	// The key admin holds access.manage and runs.read, nothing else.
	ka := authAs(s.keys["key-admin"])
	if _, err := ka.CreateRole(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRoleRequest{Role: &evalsiv1alpha1.Role{
		Name: "reader", Project: "support", Permissions: []string{"runs.read"},
	}})); err != nil {
		t.Errorf("key admin creating a role it can grant: %v", err)
	}
	_, err = ka.CreateRole(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRoleRequest{Role: &evalsiv1alpha1.Role{
		Name: "starter", Project: "support", Permissions: []string{"runs.create"},
	}}))
	if codeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "escalation") {
		t.Errorf("key admin granting runs.create: %v", err)
	}
	if _, err := ka.CreateBinding(ctx, connect.NewRequest(&evalsiv1alpha1.CreateBindingRequest{Binding: &evalsiv1alpha1.RoleBinding{
		Project: "support", Role: "editor", Subject: "group:corp/x",
	}})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("key admin binding editor: %v", err)
	}
	// Admin issues an API key; it works at once, and stops working when revoked.
	admin := authAs(s.keys["admin"])
	created, err := admin.CreateAPIKey(ctx, connect.NewRequest(&evalsiv1alpha1.CreateAPIKeyRequest{
		Name: "ci", Roles: map[string]*evalsiv1alpha1.RoleList{"support": {Roles: []string{"runner"}}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	secret := created.Msg.GetSecret()
	if !strings.HasPrefix(secret, "evk_") || !strings.HasPrefix(secret, created.Msg.GetKey().GetPrefix()) {
		t.Errorf("secret %q, prefix %q", secret, created.Msg.GetKey().GetPrefix())
	}
	if _, err := admin.CreateAPIKey(ctx, connect.NewRequest(&evalsiv1alpha1.CreateAPIKeyRequest{
		Name: "everywhere", Roles: map[string]*evalsiv1alpha1.RoleList{"*": {Roles: []string{"viewer"}}},
	})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("project admin issuing an install-wide key: %v", err)
	}
	ci := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(secret))
	if _, err := ci.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("qwen3")})); err != nil {
		t.Errorf("new key: %v", err)
	}
	keys, _ := admin.ListAPIKeys(ctx, connect.NewRequest(&evalsiv1alpha1.ListAPIKeysRequest{Project: "support"}))
	found := false
	for _, k := range keys.Msg.GetKeys() {
		found = found || k.GetName() == "ci"
		if k.GetName() == "owner" {
			t.Error("project admin can see the owner key")
		}
	}
	if !found {
		t.Error("ci key not listed")
	}
	if _, err := admin.RevokeAPIKey(ctx, connect.NewRequest(&evalsiv1alpha1.RevokeAPIKeyRequest{Name: "ci"})); err != nil {
		t.Fatal(err)
	}
	if _, err := ci.ListRuns(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunsRequest{})); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("revoked key: %v", err)
	}
	// A stored binding takes effect on the next request.
	if _, err := owner.CreateBinding(ctx, connect.NewRequest(&evalsiv1alpha1.CreateBindingRequest{Binding: &evalsiv1alpha1.RoleBinding{
		Project: "labs", Role: "viewer", Subject: "key:checkout-viewer",
	}})); err != nil {
		t.Fatal(err)
	}
	projects, _ := authAs(s.keys["checkout-viewer"]).ListProjects(ctx, connect.NewRequest(&evalsiv1alpha1.ListProjectsRequest{}))
	if len(projects.Msg.GetProjects()) != 2 {
		t.Errorf("checkout viewer projects = %v", projects.Msg.GetProjects())
	}
	// Every denied call is in the audit log, with what decided it; admins see their project's events only.
	events, err := owner.ListAuditEvents(ctx, connect.NewRequest(&evalsiv1alpha1.ListAuditEventsRequest{DeniedOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, ev := range events.Msg.GetEvents() {
		if ev.GetAllowed() || ev.GetReason() == "" || ev.GetPrincipal() == "" || ev.GetRequestId() == "" {
			t.Errorf("bad denied event %v", ev)
		}
		reasons = append(reasons, ev.GetPrincipal()+" "+ev.GetAction())
	}
	if !strings.Contains(strings.Join(reasons, "\n"), "key:admin projects.manage") {
		t.Errorf("denied events = %v", reasons)
	}
	all, _ := owner.ListAuditEvents(ctx, connect.NewRequest(&evalsiv1alpha1.ListAuditEventsRequest{Action: "access.manage"}))
	var details []string
	for _, ev := range all.Msg.GetEvents() {
		details = append(details, ev.GetResource()+" "+ev.GetDetail())
	}
	if !strings.Contains(strings.Join(details, "\n"), `role/support/reader {"new":`) {
		t.Errorf("role change not audited with its definition: %v", details)
	}
	adminEvents, _ := admin.ListAuditEvents(ctx, connect.NewRequest(&evalsiv1alpha1.ListAuditEventsRequest{}))
	for _, ev := range adminEvents.Msg.GetEvents() {
		if ev.GetProject() != "support" {
			t.Errorf("admin of support sees event in %q", ev.GetProject())
		}
	}
	if _, err := authAs(s.keys["viewer"]).ListAuditEvents(ctx, connect.NewRequest(&evalsiv1alpha1.ListAuditEventsRequest{Project: "support"})); err != nil {
		t.Error(err)
	}
}

func TestStartupRefusesNonLoopbackWithoutAuth(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = "0.0.0.0:0"
	cfg.DataDir = filepath.Join(t.TempDir(), "d")
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "without authentication") {
		t.Errorf("Validate = %v", err)
	}
}

// Datasets drawn from traces, earlier runs or other projects' promotions are
// authorized as reads of those resources; promotion needs datasets.write.
func TestFlywheelAccess(t *testing.T) {
	s := startAuthServer(t, func(c *config.Config) { c.DatasetsDir = t.TempDir() })
	ctx := context.Background()
	runsAs := func(who string) evalsiv1alpha1connect.RunServiceClient {
		return evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys[who]))
	}
	created, err := runsAs("runner").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("qwen3")}))
	if err != nil {
		t.Fatal(err)
	}
	supportRun := created.Msg.GetRun().GetId()
	promote := func(who string) error {
		_, err := runsAs(who).PromoteResults(ctx, connect.NewRequest(&evalsiv1alpha1.PromoteResultsRequest{RunId: supportRun, Dataset: "regressions", When: "true"}))
		return err
	}
	if err := promote("runner"); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("runner promoting: %v", err)
	}
	if err := promote("editor"); err != nil {
		t.Errorf("editor promoting: %v", err)
	}
	withDataset := func(src *evalsiv1alpha1.DatasetSource) *evalsiv1alpha1.RunSpec {
		spec := runSpec("qwen3")
		spec.Target, spec.Dataset = nil, src
		return spec
	}
	traces := &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Traces{Traces: &evalsiv1alpha1.TraceQuery{}}}
	if _, err := runsAs("blind-runner").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: withDataset(traces)})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a runner without traces.read replaying traces: %v", err)
	}
	if _, err := runsAs("runner").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: withDataset(traces)})); codeOf(err) == connect.CodePermissionDenied {
		t.Errorf("runner replaying its project's traces: %v", err)
	}
	fromRun := &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Run{Run: &evalsiv1alpha1.RunOutputs{RunId: supportRun}}}
	if _, err := runsAs("checkout-runner").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "checkout", Spec: withDataset(fromRun)})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("re-scoring another project's run: %v", err)
	}
	promoted := &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Path{Path: "promoted/support/regressions.jsonl"}}
	if _, err := runsAs("checkout-runner").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "checkout", Spec: withDataset(promoted)})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reading another project's promoted dataset: %v", err)
	}
	shadow := func(who string) error {
		spec := runSpec("qwen3")
		_, err := runsAs(who).CreateShadowReplay(ctx, connect.NewRequest(&evalsiv1alpha1.CreateShadowReplayRequest{Project: "support", Candidate: spec}))
		return err
	}
	if err := shadow("viewer"); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("viewer shadow replay: %v", err)
	}
	if err := shadow("runner"); codeOf(err) == connect.CodePermissionDenied {
		t.Errorf("runner shadow replay: %v", err)
	}
}
