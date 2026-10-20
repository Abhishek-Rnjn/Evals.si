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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	"google.golang.org/protobuf/types/known/timestamppb"

	harnessv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/harness/v1alpha1"
	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func testIdempotencyInterceptor(g *gate) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			return g.idempotent(ctx, req, next)
		}
	})
}

type testStreamIdempotencyInterceptor struct{ gate *gate }

func (interceptor testStreamIdempotencyInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return next
}
func (interceptor testStreamIdempotencyInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (interceptor testStreamIdempotencyInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return interceptor.gate.idempotentStream(ctx, conn, func(any) error { return nil }, next)
	}
}

func TestIdempotencyStreamReplayAndConflict(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	procedure := evalsiv1alpha1connect.DatasetServiceUploadDatasetProcedure
	var calls atomic.Int64
	handler := connect.NewClientStreamHandler(procedure, func(ctx context.Context, stream *connect.ClientStream[evalsiv1alpha1.UploadDatasetRequest]) (*connect.Response[evalsiv1alpha1.UploadDatasetResponse], error) {
		calls.Add(1)
		for stream.Receive() {
		}
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return connect.NewResponse(&evalsiv1alpha1.UploadDatasetResponse{Path: "stored-once", Records: 1}), nil
	}, connect.WithInterceptors(testStreamIdempotencyInterceptor{gate: &gate{store: st}}))
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client := connect.NewClient[evalsiv1alpha1.UploadDatasetRequest, evalsiv1alpha1.UploadDatasetResponse](server.Client(), server.URL+procedure, connect.WithGRPC())
	call := func(payload string) (*connect.Response[evalsiv1alpha1.UploadDatasetResponse], error) {
		stream := client.CallClientStream(t.Context())
		stream.RequestHeader().Set("Idempotency-Key", "upload-once")
		if err := stream.Send(&evalsiv1alpha1.UploadDatasetRequest{Part: &evalsiv1alpha1.UploadDatasetRequest_Data{Data: []byte(payload)}}); err != nil {
			return nil, err
		}
		return stream.CloseAndReceive()
	}
	if _, err := call("same"); err != nil {
		t.Fatal(err)
	}
	replay, err := call("same")
	if err != nil || replay.Msg.GetPath() != "stored-once" || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
		t.Fatalf("stream retry: %+v, %v, calls=%d", replay, err, calls.Load())
	}
	if _, err := call("different"); connect.CodeOf(err) != connect.CodeAlreadyExists || calls.Load() != 1 {
		t.Fatalf("changed stream: %v", err)
	}
}

func TestIdempotencyUnaryReplayAndConflict(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	procedure := evalsiv1alpha1connect.EvaluationServiceEvaluateProcedure
	g := &gate{store: st, rules: map[string]accessRule{procedure: {action: "evaluations.run"}}}
	var calls atomic.Int64
	handler := connect.NewUnaryHandler(procedure, func(ctx context.Context, req *connect.Request[evalsiv1alpha1.EvaluateRequest]) (*connect.Response[evalsiv1alpha1.EvaluateResponse], error) {
		calls.Add(1)
		return connect.NewResponse(&evalsiv1alpha1.EvaluateResponse{Results: []*evalsiv1alpha1.EvaluationResult{{RecordId: "once"}}}), nil
	}, connect.WithInterceptors(testIdempotencyInterceptor(g)))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := connect.NewClient[evalsiv1alpha1.EvaluateRequest, evalsiv1alpha1.EvaluateResponse](server.Client(), server.URL+procedure)
	request := func(project string) *connect.Request[evalsiv1alpha1.EvaluateRequest] {
		req := connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{Project: project})
		req.Header().Set("Idempotency-Key", "retry-once")
		return req
	}
	if _, err := client.CallUnary(t.Context(), request("project")); err != nil {
		t.Fatal(err)
	}
	replayed, err := client.CallUnary(t.Context(), request("project"))
	if err != nil || replayed.Msg.GetResults()[0].GetRecordId() != "once" || replayed.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
		t.Fatal("retry was not replayed without evaluating twice")
	}
	changed := request("project")
	changed.Msg.Judge = "changed"
	if _, err := client.CallUnary(t.Context(), changed); connect.CodeOf(err) != connect.CodeAlreadyExists || calls.Load() != 1 {
		t.Fatal("different payload did not conflict before execution")
	}
}

func TestIdempotencyFailedCallIsRetriedWithTheSameKey(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	procedure := evalsiv1alpha1connect.EvaluationServiceEvaluateProcedure
	g := &gate{store: st, rules: map[string]accessRule{procedure: {action: "evaluations.run"}}}
	var calls atomic.Int64
	handler := connect.NewUnaryHandler(procedure, func(ctx context.Context, req *connect.Request[evalsiv1alpha1.EvaluateRequest]) (*connect.Response[evalsiv1alpha1.EvaluateResponse], error) {
		if calls.Add(1) == 1 {
			return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("quota"))
		}
		return connect.NewResponse(&evalsiv1alpha1.EvaluateResponse{Results: []*evalsiv1alpha1.EvaluationResult{{RecordId: "second"}}}), nil
	}, connect.WithInterceptors(testIdempotencyInterceptor(g)))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := connect.NewClient[evalsiv1alpha1.EvaluateRequest, evalsiv1alpha1.EvaluateResponse](server.Client(), server.URL+procedure)
	request := func() *connect.Request[evalsiv1alpha1.EvaluateRequest] {
		req := connect.NewRequest(&evalsiv1alpha1.EvaluateRequest{Project: "project"})
		req.Header().Set("Idempotency-Key", "retry-after-failure")
		return req
	}
	if _, err := client.CallUnary(t.Context(), request()); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("first call: %v", err)
	}
	retry, err := client.CallUnary(t.Context(), request())
	if err != nil || retry.Msg.GetResults()[0].GetRecordId() != "second" || retry.Header().Get("Idempotency-Replayed") != "" || calls.Load() != 2 {
		t.Fatalf("a failed call was replayed instead of retried: %+v, %v, calls=%d", retry, err, calls.Load())
	}
	replay, err := client.CallUnary(t.Context(), request())
	if err != nil || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 2 {
		t.Fatalf("a success was not replayed: %v, calls=%d", err, calls.Load())
	}
}

func TestIdempotencyCredentialResponseDoesNotPersistSecret(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	procedure := evalsiv1alpha1connect.AuthServiceCreateAPIKeyProcedure
	g := &gate{store: st, rules: map[string]accessRule{procedure: {action: "access.manage"}}}
	var calls atomic.Int64
	handler := connect.NewUnaryHandler(procedure, func(ctx context.Context, req *connect.Request[evalsiv1alpha1.CreateAPIKeyRequest]) (*connect.Response[evalsiv1alpha1.CreateAPIKeyResponse], error) {
		calls.Add(1)
		return connect.NewResponse(&evalsiv1alpha1.CreateAPIKeyResponse{Key: &evalsiv1alpha1.APIKey{Name: "created"}, Secret: "one-time-secret"}), nil
	}, connect.WithInterceptors(testIdempotencyInterceptor(g)))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := connect.NewClient[evalsiv1alpha1.CreateAPIKeyRequest, evalsiv1alpha1.CreateAPIKeyResponse](server.Client(), server.URL+procedure)
	request := func() *connect.Request[evalsiv1alpha1.CreateAPIKeyRequest] {
		req := connect.NewRequest(&evalsiv1alpha1.CreateAPIKeyRequest{Name: "created"})
		req.Header().Set("Idempotency-Key", "create-once")
		return req
	}
	first, err := client.CallUnary(t.Context(), request())
	if err != nil || first.Msg.GetSecret() != "one-time-secret" {
		t.Fatal("first response lost its generated credential")
	}
	retry, err := client.CallUnary(t.Context(), request())
	if err != nil || retry.Msg.GetSecret() != "" || retry.Msg.GetKey().GetName() != "created" || calls.Load() != 1 {
		t.Fatal("retry recreated or replayed the credential")
	}
	scope := digest([]byte(auth.PrincipalFrom(t.Context()).ID() + "\x00" + procedure + "\x00"))
	raw, err := proto.Marshal(request().Msg)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.BeginIdempotency(t.Context(), scope, digest([]byte("create-once")), digest(raw), time.Now())
	if err != nil || !stored.Completed || bytes.Contains(stored.Response, []byte("one-time-secret")) {
		t.Fatal("generated credential reached the idempotency store")
	}
}

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
	rewardSchema, _ := structpb.NewStruct(map[string]any{"type": "object", "properties": map[string]any{
		"url": map[string]any{"type": "string"}, "api_key_env": map[string]any{"type": "string"},
	}})
	return []*evalsiv1alpha1.EvaluatorManifest{
		{Name: "builtin/exact-match", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD, ParamsSchema: schema,
			Outputs: []*evalsiv1alpha1.MetricSpec{{Name: "exact-match", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED}}},
		{Name: "builtin/unit-tests", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD, ParamsSchema: schema,
			Requires:   &evalsiv1alpha1.Requirements{Isolation: evalsiv1alpha1.IsolationLevel_ISOLATION_LEVEL_CONFINED},
			Scheduling: &evalsiv1alpha1.Scheduling{Pool: "sandbox"},
			Outputs:    []*evalsiv1alpha1.MetricSpec{{Name: "unit-tests", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_PASSED}}},
		// Needs a judge, for judge scoping.
		{Name: "builtin/judge-score", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD, ParamsSchema: schema,
			Requires: &evalsiv1alpha1.Requirements{Judge: true},
			Outputs:  []*evalsiv1alpha1.MetricSpec{{Name: "judge-score", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER}}},
		// Params naming a worker variable and where its value goes.
		{Name: "builtin/reward-model", Version: "1.0.0", Scope: evalsiv1alpha1.Scope_SCOPE_RECORD, ParamsSchema: rewardSchema,
			Outputs: []*evalsiv1alpha1.MetricSpec{{Name: "reward-model", Type: evalsiv1alpha1.ScoreType_SCORE_TYPE_NUMBER}}},
	}, nil
}

func (fakeWorker) Evaluate(_ context.Context, req *pluginv1alpha1.EvaluateRequest) (*pluginv1alpha1.EvaluateResponse, error) {
	resp := &pluginv1alpha1.EvaluateResponse{BatchId: req.GetBatchId()}
	for _, r := range req.GetRecords() {
		resp.Results = append(resp.Results, &evalsiv1alpha1.EvaluationResult{
			EvaluatorRef: req.GetEvaluator() + "@1.0.0", RecordId: r.GetId(), Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED,
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

type uploadFailureReader struct{ err error }

func (reader uploadFailureReader) Read([]byte) (int, error) { return 0, reader.err }

func TestDatasetHTTPSpoolPrivacyAndReadErrors(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("TMPDIR", directory)
	spool, checksum, err := spoolDatasetUpload(strings.NewReader("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := spool.Stat()
	if err != nil || info.Mode().Perm() != 0o600 || checksum != digest([]byte("{}\n")) {
		t.Fatalf("spool permissions or digest: %v %v", info, err)
	}
	raw, err := io.ReadAll(spool)
	if err != nil || string(raw) != "{}\n" {
		t.Fatalf("spool was not rewound: %q %v", raw, err)
	}
	_ = spool.Close()
	_ = os.Remove(spool.Name())
	for _, test := range []struct {
		err  error
		code int
	}{{errors.New("broken body"), http.StatusBadRequest}, {&http.MaxBytesError{Limit: 64 << 20}, http.StatusRequestEntityTooLarge}} {
		if file, _, err := spoolDatasetUpload(uploadFailureReader{test.err}); file != nil || err == nil {
			t.Fatalf("read failure accepted: %v %v", file, err)
		} else {
			writer := httptest.NewRecorder()
			datasetHTTPError(writer, err)
			if writer.Code != test.code {
				t.Fatalf("read error classified as %d, want %d", writer.Code, test.code)
			}
		}
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("failed spool leaked files: %v %v", entries, err)
	}
}

func TestDatasetHTTPIdempotencyAndAuthorization(t *testing.T) {
	s := startAuthServer(t, func(cfg *config.Config) { cfg.DatasetsDir = t.TempDir() })
	call := func(role, payload string) (*http.Response, []byte) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.url+"/v1alpha1/datasets/test/upload?project=support&format=jsonl", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+s.keys[role])
		request.Header.Set("Idempotency-Key", "raw-upload")
		response, err := s.http.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response, raw
	}
	first, raw := call("admin", "{}\n")
	if first.StatusCode != http.StatusCreated || !bytes.Contains(raw, []byte("uploaded/support/test/")) {
		t.Fatalf("upload: %d %s", first.StatusCode, raw)
	}
	retry, replay := call("admin", "{}\n")
	if retry.StatusCode != http.StatusCreated || retry.Header.Get("Idempotency-Replayed") != "true" || !bytes.Equal(raw, replay) {
		t.Fatalf("retry: %d %s", retry.StatusCode, replay)
	}
	if response, _ := call("admin", "{\"id\":\"changed\"}\n"); response.StatusCode != http.StatusConflict {
		t.Fatalf("conflict: %d", response.StatusCode)
	}
	if response, _ := call("viewer", "{}\n"); response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer upload: %d", response.StatusCode)
	}
	writer := httptest.NewRecorder()
	datasetHTTPError(writer, connect.NewError(connect.CodeInvalidArgument, errors.New("row 2: invalid Record fields")))
	if !strings.Contains(writer.Body.String(), "row 2") {
		t.Fatal("HTTP row error lost its row")
	}
}

func TestDeleteRunAuthorizedRetry(t *testing.T) {
	s := startAuthServer(t, nil)
	client := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["admin"]))
	created, err := client.CreateRun(t.Context(), connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: &evalsiv1alpha1.RunSpec{
		Dataset:    &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Inline{Inline: &evalsiv1alpha1.InlineRecords{Records: []*evalsiv1alpha1.Record{{Id: "one", Output: text("Paris"), Reference: text("Paris")}}}}},
		Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetRun().GetId()
	watchRequest := connect.NewRequest(&evalsiv1alpha1.WatchRunRequest{Id: id})
	watchRequest.Header().Set("Authorization", "Bearer "+s.keys["admin"])
	stream, err := client.WatchRun(t.Context(), watchRequest)
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	request := func() *connect.Request[evalsiv1alpha1.DeleteRunRequest] {
		request := connect.NewRequest(&evalsiv1alpha1.DeleteRunRequest{Id: id})
		request.Header().Set("Idempotency-Key", "delete-once")
		return request
	}
	if _, err := client.DeleteRun(t.Context(), request()); err != nil {
		t.Fatal(err)
	}
	retry, err := client.DeleteRun(t.Context(), request())
	if err != nil || retry.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("project-admin deletion replay: %v", err)
	}
	other := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["checkout-runner"]))
	if _, err := other.DeleteRun(t.Context(), request()); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-project deletion: %v", err)
	}
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
		"app-x-reader":    {"support": {"app-x-reader"}},
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
			// Reads only the runs labelled app=x.
			{Name: "app-x-reader", Permissions: []string{"runs.read"}, Condition: `"app" in resource.labels && resource.labels.app == "x"`},
			// Starts runs but may not read production traces.
			{Name: "blind-runner", Permissions: []string{"runs.create", "runs.read", "evaluations.run"}},
		},
		Projects: map[string]map[string][]string{
			"support":  {"runner": {`cel:jwt.repository == "acme/agent" && jwt.ref == "refs/heads/main"`}},
			"checkout": {},
		},
	}
	// The test targets are model servers on loopback; support and default
	// runs may send the worker's key there.
	cfg.Credentials.Grants = []credentials.Grant{{Env: "OPENAI_API_KEY", Projects: []string{"support", "default"}, Hosts: []string{"127.0.0.1"}}}
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

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Connect reads a unary request whole before the gate's interceptor runs.
// A request whose credential was rejected is cut off after a few KiB, and
// any other at MaxMessageBytes, instead of being buffered whole.
func TestOversizedRequests(t *testing.T) {
	s := startAuthServer(t, nil)
	for name, tc := range map[string]struct {
		cred string
		size int64
	}{
		"no credential":    {"", rejectedBodyBytes * 4},
		"invalid key":      {"evk_bogus", rejectedBodyBytes * 4},
		"valid key":        {s.keys["owner"], MaxMessageBytes + 1<<20},
		"valid key, small": {s.keys["owner"], 0},
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, s.url+evalsiv1alpha1connect.EvaluationServiceEvaluateProcedure, io.LimitReader(zeros{}, tc.size))
			req.Header.Set("Content-Type", "application/proto")
			if tc.cred != "" {
				req.Header.Set("Authorization", "Bearer "+tc.cred)
			}
			resp, err := s.http.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if want := "resource_exhausted"; tc.size > 0 && !strings.Contains(string(body), want) {
				t.Fatalf("status %d: %s, want %s", resp.StatusCode, body, want)
			}
			if tc.size == 0 && strings.Contains(string(body), "resource_exhausted") {
				t.Fatalf("an empty request was refused for its size: %s", body)
			}
		})
	}
}

// Importing a run evaluated elsewhere is creating one: runners in their
// project only, and the run records who imported it.
func TestImportRunPermissions(t *testing.T) {
	s := startAuthServer(t, nil)
	ctx := context.Background()
	runsAs := func(who string) evalsiv1alpha1connect.RunServiceClient {
		return evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys[who]))
	}
	imported := func(project string) *evalsiv1alpha1.ImportRunRequest {
		return &evalsiv1alpha1.ImportRunRequest{
			Project: project, Name: "local", Spec: &evalsiv1alpha1.RunSpec{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}}},
			Records: []*evalsiv1alpha1.Record{{Id: "a"}},
			Results: []*evalsiv1alpha1.EvaluationResult{{RecordId: "a", Evaluator: "exact-match", Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED}},
		}
	}
	if got, err := runsAs("runner").ImportRun(ctx, connect.NewRequest(imported("support"))); err != nil || got.Msg.GetRun().GetCreatedBy() != "key:runner" || !got.Msg.GetRun().GetImported() {
		t.Errorf("runner importing a run: %v, %v", got, err)
	}
	for who, project := range map[string]string{"viewer": "support", "runner": "checkout", "ingest": "support"} {
		if _, err := runsAs(who).ImportRun(ctx, connect.NewRequest(imported(project))); codeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s importing a run into %s: err = %v", who, project, err)
		}
	}
}

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

// Replacing a policy checks the stored policy too, even in the same
// project: relabelling cannot take over a policy the caller may not manage.
func TestPolicyReplacementChecksTheStoredPolicy(t *testing.T) {
	plain, hash := auth.NewAPIKey()
	s := startAuthServer(t, func(c *config.Config) {
		c.RBAC.Roles = append(c.RBAC.Roles, authz.Role{Name: "team-editor", Permissions: []string{"policies.write", "policies.read"},
			Condition: `"team" in resource.labels && resource.labels.team == "mine"`})
		c.Auth.APIKeys.Keys = append(c.Auth.APIKeys.Keys, auth.ConfigKey{Name: "team-editor", Key: "sha256:" + hash,
			Roles: map[string][]string{"support": {"team-editor"}}})
	})
	ctx := context.Background()
	mon := func(cred string) evalsiv1alpha1connect.MonitorServiceClient {
		return evalsiv1alpha1connect.NewMonitorServiceClient(s.http, s.url, as(cred))
	}
	policy := func(team string) *evalsiv1alpha1.OnlineEvalPolicy {
		return &evalsiv1alpha1.OnlineEvalPolicy{Name: "p1", Project: "support", Labels: map[string]string{"team": team},
			Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "exact-match"}}}}}
	}
	if _, err := mon(s.keys["owner"]).ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy("theirs")})); err != nil {
		t.Fatal(err)
	}
	if _, err := mon(plain).ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy("mine")})); codeOf(err) != connect.CodePermissionDenied {
		t.Errorf("relabelling another team's policy: %v, want PermissionDenied", err)
	}
	if _, err := mon(s.keys["owner"]).ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy("mine")})); err != nil {
		t.Fatal(err)
	}
	if _, err := mon(plain).ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: policy("mine")})); err != nil {
		t.Errorf("editing the team's own policy: %v", err)
	}
}

// A trace dataset is read like a trace listing: traces.read scoped by
// service or by label drives a dataset over the traces it may read, and each
// trace is checked as the run loads it.
func TestTraceDatasetsUseTraceReadRules(t *testing.T) {
	keys := map[string]string{}
	s := startAuthServer(t, func(c *config.Config) {
		c.OTLP.Grace = "10ms"
		c.RBAC.Roles = append(c.RBAC.Roles,
			authz.Role{Name: "public-traces", Permissions: []string{"traces.read"},
				Condition: `resource.service == "public" && !("secret" in resource.labels)`},
			authz.Role{Name: "team-traces", Permissions: []string{"traces.read"},
				Condition: `"team" in resource.labels && resource.labels.team == "mine"`})
		for _, role := range []string{"public-traces", "team-traces"} {
			plain, hash := auth.NewAPIKey()
			keys[role] = plain
			c.Auth.APIKeys.Keys = append(c.Auth.APIKeys.Keys, auth.ConfigKey{Name: role, Key: "sha256:" + hash,
				Roles: map[string][]string{"support": {"blind-runner", role}}})
		}
	})
	ctx := context.Background()
	ingest := []struct {
		service string
		labels  map[string]string
	}{
		{"public", map[string]string{"evalsi.label.team": "mine"}},
		{"public", map[string]string{"evalsi.label.secret": "yes", "evalsi.label.team": "theirs"}},
		{"private", map[string]string{"evalsi.label.team": "mine"}},
	}
	for _, tr := range ingest {
		req, _ := http.NewRequest(http.MethodPost, s.url+"/v1/traces", bytes.NewReader(otlpRequest(tr.service, tr.labels)))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", "Bearer "+s.keys["ingest"])
		resp, err := s.http.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("ingest: %v %v", resp, err)
		}
		resp.Body.Close()
	}
	list := evalsiv1alpha1connect.NewTraceServiceClient(s.http, s.url, as(s.keys["owner"]))
	for range 100 {
		l, err := list.ListTraces(ctx, connect.NewRequest(&evalsiv1alpha1.ListTracesRequest{Project: "support"}))
		if err != nil {
			t.Fatal(err)
		}
		if len(l.Msg.GetTraces()) == len(ingest) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	create := func(key, service string, validateOnly bool) (*evalsiv1alpha1.Run, error) {
		spec := runSpec("qwen3")
		spec.Target = nil
		spec.Dataset = &evalsiv1alpha1.DatasetSource{Source: &evalsiv1alpha1.DatasetSource_Traces{Traces: &evalsiv1alpha1.TraceQuery{Service: service}}}
		runs := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(key))
		resp, err := runs.CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: spec, ValidateOnly: validateOnly}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetRun(), nil
	}
	for _, tc := range []struct {
		role, service string
		tasks         int64
	}{
		// Service-scoped: the public trace without the secret label.
		{"public-traces", "public", 1},
		// Label-scoped: the trace labelled team=mine in the service asked for.
		{"team-traces", "public", 1},
		{"team-traces", "private", 1},
	} {
		if _, err := create(keys[tc.role], tc.service, true); err != nil {
			t.Errorf("%s validating a dataset over %s: %v", tc.role, tc.service, err)
		}
		run, err := create(keys[tc.role], tc.service, false)
		if err != nil {
			t.Errorf("%s over %s: %v", tc.role, tc.service, err)
			continue
		}
		if total := run.GetProgress().GetTotal(); total != tc.tasks {
			t.Errorf("%s over %s: %d tasks, want %d (traces it may not read must be left out)", tc.role, tc.service, total, tc.tasks)
		}
		page, err := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["owner"])).ListRunResults(ctx,
			connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: run.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Msg.GetRecords() {
			if r.GetMetadata()["trace"].GetStructValue().GetFields()["service"].GetStringValue() != tc.service {
				t.Errorf("%s over %s: a record from another service: %v", tc.role, tc.service, r)
			}
		}
	}
	// No readable trace in the service: nothing to evaluate, and no run.
	if _, err := create(keys["public-traces"], "private", false); codeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a service none of whose traces the caller may read: %v", err)
	}
}

// SliceRuns reads results the way GetRun does: only runs the caller may read
// contribute, whichever way they are selected, and a forbidden run named by id
// looks like a missing one.
func TestSliceRunsHonoursRunAccess(t *testing.T) {
	s := startAuthServer(t, nil)
	ctx := context.Background()
	runsAs := func(who string) evalsiv1alpha1connect.RunServiceClient {
		return evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys[who]))
	}
	create := func(project, app string) string {
		t.Helper()
		who := map[string]string{"support": "runner", "checkout": "checkout-runner"}[project]
		spec := runSpec("qwen3")
		spec.Target.BaseUrl, spec.Target.ApiKeyEnv = "https://models.example/v1", credentials.None // needs no grant
		resp, err := runsAs(who).CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: project, Spec: spec, Labels: map[string]string{"app": app}}))
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.GetRun().GetId()
	}
	x, y, other := create("support", "x"), create("support", "y"), create("checkout", "x")
	slice := func(who string, req *evalsiv1alpha1.SliceRunsRequest) (*evalsiv1alpha1.SliceRunsResponse, error) {
		req.Metric, req.By = "em", "topic"
		resp, err := runsAs(who).SliceRuns(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	for who, want := range map[string]int32{"owner": 3, "viewer": 2, "app-x-reader": 1, "checkout-viewer": 1, "ingest": 0} {
		resp, err := slice(who, &evalsiv1alpha1.SliceRunsRequest{})
		if err != nil || resp.GetRuns() != want {
			t.Errorf("%s slices %d runs, %v; want %d", who, resp.GetRuns(), err, want)
		}
	}
	// Naming a run does not widen what a caller reads.
	for who, tc := range map[string]struct {
		id string
		ok bool
	}{
		"viewer":          {y, true},
		"app-x-reader":    {x, true},
		"checkout-viewer": {other, true},
	} {
		if _, err := slice(who, &evalsiv1alpha1.SliceRunsRequest{RunIds: []string{tc.id}}); (err == nil) != tc.ok {
			t.Errorf("%s slicing its own run: %v", who, err)
		}
	}
	for _, tc := range []struct{ who, id string }{{"app-x-reader", y}, {"app-x-reader", other}, {"checkout-viewer", x}, {"viewer", other}} {
		_, err := slice(tc.who, &evalsiv1alpha1.SliceRunsRequest{RunIds: []string{tc.id}})
		_, missing := slice(tc.who, &evalsiv1alpha1.SliceRunsRequest{RunIds: []string{"no-such-run"}})
		if codeOf(err) != connect.CodeNotFound || codeOf(missing) != connect.CodeNotFound {
			t.Errorf("%s slicing %s: %v (a missing run: %v), want NotFound for both", tc.who, tc.id, err, missing)
		}
	}
	// Narrowing to a project the caller cannot read finds nothing.
	if resp, err := slice("checkout-viewer", &evalsiv1alpha1.SliceRunsRequest{Project: "support"}); err != nil || resp.GetRuns() != 0 {
		t.Errorf("a project the caller cannot read: %d runs, %v", resp.GetRuns(), err)
	}
	// Without a credential nothing is read.
	if _, err := slice("", &evalsiv1alpha1.SliceRunsRequest{}); codeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("anonymous: %v", err)
	}
}

// Archiving needs the permission deleting does (runs.delete, held by admins,
// not by runners or viewers), and an archived run stays readable while its
// results are gone.
func TestArchiveRunPermission(t *testing.T) {
	s := startAuthServer(t, nil)
	ctx := context.Background()
	runsAs := func(who string) evalsiv1alpha1connect.RunServiceClient {
		return evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys[who]))
	}
	created, err := runsAs("runner").CreateRun(ctx, connect.NewRequest(&evalsiv1alpha1.CreateRunRequest{Project: "support", Spec: runSpec("qwen3")}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetRun().GetId()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := runsAs("admin").GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		if st := got.Msg.GetRun().GetStatus(); st == evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED || st == evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR || st == evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %v", got.Msg.GetRun())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, who := range []string{"viewer", "runner", "checkout-viewer"} {
		if _, err := runsAs(who).ArchiveRun(ctx, connect.NewRequest(&evalsiv1alpha1.ArchiveRunRequest{Id: id})); codeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s archiving: %v", who, err)
		}
	}
	resp, err := runsAs("admin").ArchiveRun(ctx, connect.NewRequest(&evalsiv1alpha1.ArchiveRunRequest{Id: id}))
	if err != nil || resp.Msg.GetRun().GetArchivedAt() == nil {
		t.Fatalf("admin archiving: %v, %v", resp, err)
	}
	if got, err := runsAs("viewer").GetRun(ctx, connect.NewRequest(&evalsiv1alpha1.GetRunRequest{Id: id})); err != nil || got.Msg.GetRun().GetArchivedAt() == nil {
		t.Errorf("an archived run is still readable: %v, %v", got, err)
	}
	if _, err := runsAs("viewer").ListRunResults(ctx, connect.NewRequest(&evalsiv1alpha1.ListRunResultsRequest{RunId: id})); codeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("results of an archived run: %v", err)
	}
	// The archive is in the audit log.
	events, err := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["owner"])).ListAuditEvents(ctx, connect.NewRequest(&evalsiv1alpha1.ListAuditEventsRequest{Action: "runs.delete"}))
	if err != nil {
		t.Fatal(err)
	}
	var allowed, denied int
	for _, ev := range events.Msg.GetEvents() {
		if ev.GetResource() == "run/"+id {
			if ev.GetAllowed() {
				allowed++
			} else {
				denied++
			}
		}
	}
	if allowed != 1 || denied != 3 {
		t.Errorf("audit: %d allowed, %d denied; want 1 and 3", allowed, denied)
	}
}

// The server's retention janitor audits what it archives and deletes: the run
// manager is given the auditor.
func TestRetentionAuditsAutomaticArchiving(t *testing.T) {
	old := time.Now().Add(-72 * time.Hour)
	s := startAuthServer(t, func(cfg *config.Config) {
		cfg.Runs.ArchiveAfter = "24h"
		st, err := store.Open(filepath.Join(cfg.DataDir, "evalsi.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		run := &evalsiv1alpha1.Run{
			Id: "old-run", Project: "support", Status: evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED,
			CreatedAt: timestamppb.New(old), FinishedAt: timestamppb.New(old.Add(time.Hour)),
		}
		if err := st.CreateRun(context.Background(), run, []*evalsiv1alpha1.Record{{Id: "r"}}); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for {
		events, err := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["owner"])).ListAuditEvents(ctx, connect.NewRequest(&evalsiv1alpha1.ListAuditEventsRequest{Action: "runs.archive"}))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range events.Msg.GetEvents() {
			if ev.GetResource() == "run/old-run" && ev.GetPrincipal() == "system:retention" && ev.GetAllowed() && ev.GetProject() == "support" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no audit event for the archived run: %v", events.Msg.GetEvents())
		}
		time.Sleep(100 * time.Millisecond)
	}
}
