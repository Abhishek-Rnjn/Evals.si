package mlflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// The fixtures under testdata/derived are not recorded from SageMaker or
// Databricks; testdata/derived/README.md says where each comes from.
func derived(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/derived/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const (
	awsKey    = `{"access_key_id":"AKIDEXAMPLE","secret_access_key":"wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY","session_token":"FQoGZXIvYXdzEXAMPLE"}`
	serverARN = "arn:aws:sagemaker:us-east-1:123456789012:mlflow-tracking-server/studio"
	appARN    = "arn:aws:sagemaker:eu-west-1:123456789012:mlflow-app/app-1"
	ucTraceID = "main.agents/f864e522377586ed0a722b8a01c9a611"
)

// The signature is checked against what botocore's SigV4Auth, the signer the
// SageMaker MLflow plugin uses, produced for the same requests and time.
func TestSigV4MatchesBotocore(t *testing.T) {
	var vectors []struct {
		Name, Method, URL, Body, Region, Service string
		Token                                    *string
		Headers                                  map[string]string
		Authorization                            string `json:"authorization"`
		AmzDate                                  string `json:"amz_date"`
	}
	if err := json.Unmarshal(derived(t, "sigv4_vectors"), &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			at, err := time.Parse("20060102T150405Z", v.AmzDate)
			if err != nil {
				t.Fatal(err)
			}
			creds := awsCredentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
			if v.Token != nil {
				creds.SessionToken = *v.Token
			}
			req, _ := http.NewRequest(v.Method, v.URL, strings.NewReader(v.Body))
			for k, val := range v.Headers {
				req.Header.Set(k, val)
			}
			s := &sigV4{creds: creds, region: v.Region, service: v.Service, now: func() time.Time { return at }}
			if err := s.Authorize(context.Background(), req, []byte(v.Body)); err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get("Authorization"); got != v.Authorization {
				t.Fatalf("authorization\n got %s\nwant %s", got, v.Authorization)
			}
		})
	}
}

// rewrite sends every request to the fake server whatever its host, so a test
// can use the AWS hostnames the factory insists on.
func rewrite(srv *httptest.Server) *http.Client {
	target, _ := url.Parse(srv.URL)
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.Header.Set("X-Original-Host", r.URL.Host)
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return srv.Client().Transport.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sagemakerSource(arn, endpoint string) *evalsiv1alpha1.TraceSource {
	return &evalsiv1alpha1.TraceSource{
		Name: "studio", Project: "p", Connector: "mlflow", Variant: "sagemaker", Endpoint: endpoint, Locations: []string{"1"},
		Auth: &evalsiv1alpha1.SourceAuth{Sigv4: &evalsiv1alpha1.SourceAuth_SigV4{Arn: arn}},
	}
}

func TestSageMakerFactoryChecks(t *testing.T) {
	const host = "https://us-east-1.experiments.sagemaker.aws"
	ok := func(src *evalsiv1alpha1.TraceSource, token string) error {
		_, err := Factory(src, token, http.DefaultClient)
		return err
	}
	if err := ok(sagemakerSource(serverARN, host), awsKey); err != nil {
		t.Fatalf("a tracking server: %v", err)
	}
	if err := ok(sagemakerSource(appARN, "https://mlflow.sagemaker.eu-west-1.app.aws"), awsKey); err != nil {
		t.Fatalf("an app: %v", err)
	}
	// Validation on apply happens before the credential is read.
	if err := ok(sagemakerSource(serverARN, host), source.ValidationToken); err != nil {
		t.Fatalf("validation: %v", err)
	}
	for name, c := range map[string]struct {
		src   *evalsiv1alpha1.TraceSource
		token string
		want  string
	}{
		"no arn":         {sagemakerSource("", host), awsKey, "auth.sigv4.arn"},
		"not sagemaker":  {sagemakerSource("arn:aws:s3:::bucket", host), awsKey, "auth.sigv4.arn"},
		"wrong resource": {sagemakerSource("arn:aws:sagemaker:us-east-1:123456789012:endpoint/x", host), awsKey, "only mlflow-tracking-server"},
		"no name":        {sagemakerSource("arn:aws:sagemaker:us-east-1:123456789012:mlflow-tracking-server/", host), awsKey, "no account or resource name"},
		"foreign host":   {sagemakerSource(serverARN, "https://mlflow.example.com"), awsKey, "https AWS host"},
		"lookalike host": {sagemakerSource(serverARN, "https://sagemaker.aws.evil.example"), awsKey, "https AWS host"},
		"plain http":     {sagemakerSource(serverARN, "http://us-east-1.experiments.sagemaker.aws"), awsKey, "https AWS host"},
		"no credential":  {sagemakerSource(serverARN, host), "", "access_key_id"},
		"bare key":       {sagemakerSource(serverARN, host), "AKIDEXAMPLE", "access_key_id"},
		"unknown field":  {sagemakerSource(serverARN, host), `{"access_key_id":"a","secret_access_key":"b","profile":"c"}`, "access_key_id"},
	} {
		if err := ok(c.src, c.token); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := sagemakerSource(serverARN, host)
	other.Auth.Sigv4.Region, other.Auth.Sigv4.Service = "us-west-2", ""
	if err := ok(other, awsKey); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("region mismatch: %v", err)
	}
	other.Auth.Sigv4.Region, other.Auth.Sigv4.Service = "", "sagemaker"
	if err := ok(other, awsKey); err == nil || !strings.Contains(err.Error(), "service") {
		t.Errorf("service mismatch: %v", err)
	}
}

// Every call of a SageMaker source is signed, routed by the ARN header, and
// carries the session token; the rest of the loop is the open-source API.
func TestSageMakerSourceIsSignedAndRouted(t *testing.T) {
	var mu sync.Mutex
	var seen []*http.Request
	f := &fakeMLflow{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(context.Background()))
		mu.Unlock()
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := Factory(sagemakerSource(serverARN, "https://us-east-1.experiments.sagemaker.aws"), awsKey, rewrite(srv))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.List(context.Background(), source.ListRequest{Since: time.Now().Add(-time.Hour), Limit: 10})
	if err != nil || len(res.Infos) != 1 || res.Infos[0].ID != traceID {
		t.Fatalf("list: %+v %v", res, err)
	}
	if traces, err := c.Fetch(context.Background(), res.Infos); err != nil || len(traces) != 1 {
		t.Fatalf("fetch: %v %v", traces, err)
	}
	if _, err := c.WriteBack(context.Background(), []source.Score{{TraceID: traceID, Policy: "studio-agents", Metric: "task-success", Value: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) < 4 {
		t.Fatalf("%d requests", len(seen))
	}
	for _, r := range seen {
		auth := r.Header.Get("Authorization")
		for _, want := range []string{"AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/", "/us-east-1/sagemaker-mlflow/aws4_request", "x-mlflow-sm-tracking-server-arn", "x-amz-security-token"} {
			if !strings.Contains(auth, want) {
				t.Errorf("%s %s: authorization lacks %q: %s", r.Method, r.URL.Path, want, auth)
			}
		}
		if r.Header.Get("x-mlflow-sm-tracking-server-arn") != serverARN || r.Header.Get("X-Amz-Security-Token") != "FQoGZXIvYXdzEXAMPLE" {
			t.Errorf("%s %s: headers %v", r.Method, r.URL.Path, r.Header)
		}
		if got := r.Header.Get("X-Original-Host"); got != "us-east-1.experiments.sagemaker.aws" {
			t.Errorf("signed for host %q", got)
		}
		if strings.Contains(auth, "wJalrXUtnFEMI") {
			t.Error("the secret key is in the Authorization header")
		}
	}
}

// ---- Databricks: OAuth service principal ----

type fakeWorkspace struct {
	t         *testing.T
	mu        sync.Mutex
	tokens    int
	issued    string
	expires   int
	reject    string // an access token the API answers 401 to
	rejectAll bool
	tokenErr  bool
	requests  []string
	// Unity Catalog state.
	pending      int // polls before the search is done
	searchFile   string
	existing     []map[string]any // assessments batchGet shows on the trace
	bodies       []map[string]any
	lastForm     url.Values
	lastUser     string
	lastPassword string
}

func (f *fakeWorkspace) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/oidc/v1/token" {
		f.tokens++
		f.lastForm, _ = url.ParseQuery(string(raw))
		f.lastUser, f.lastPassword, _ = r.BasicAuth()
		if f.tokenErr {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"Client authentication failed"}`))
			return
		}
		f.issued = "access-" + string(rune('0'+f.tokens))
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": f.issued, "token_type": "Bearer", "expires_in": f.expires})
		return
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.bodies = append(f.bodies, body)
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI()+" auth="+r.Header.Get("Authorization"))
	if f.rejectAll || f.reject != "" && r.Header.Get("Authorization") == "Bearer "+f.reject {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error_code":"UNAUTHENTICATED","message":"token expired"}`))
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/3.0/mlflow/traces/search":
		_, _ = w.Write(fixture(f.t, "search"))
	case r.Method == http.MethodPost && r.URL.Path == "/api/4.0/mlflow/traces/search-long-running":
		if f.searchFile == "" {
			f.searchFile = "uc_search_done"
		}
		if f.pending > 0 {
			_, _ = w.Write(derived(f.t, "uc_search_pending"))
			return
		}
		_, _ = w.Write(derived(f.t, f.searchFile))
	case r.Method == http.MethodGet && r.URL.Path == "/api/4.0/mlflow/traces/search/operations/operations/search-1":
		f.pending--
		if f.pending > 0 {
			_, _ = w.Write(derived(f.t, "uc_search_pending"))
			return
		}
		_, _ = w.Write(derived(f.t, f.searchFile))
	case r.Method == http.MethodGet && r.URL.Path == "/api/4.0/mlflow/traces/main.agents/batchGet":
		var resp struct {
			Traces []map[string]any `json:"traces"`
		}
		_ = json.Unmarshal(derived(f.t, "uc_batchget"), &resp)
		if len(f.existing) > 0 {
			resp.Traces[0]["trace_info"].(map[string]any)["assessments"] = f.existing
		}
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/assessments"):
		_, _ = w.Write(derived(f.t, "uc_assess_create"))
	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/assessments/"):
		_, _ = w.Write(derived(f.t, "uc_assess_create"))
	default:
		http.NotFound(w, r)
	}
}

func workspace(t *testing.T, f *fakeWorkspace, mutate func(*evalsiv1alpha1.TraceSource)) (*connector, *httptest.Server) {
	t.Helper()
	f.t = t
	if f.expires == 0 {
		f.expires = 3600
	}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	src := &evalsiv1alpha1.TraceSource{
		Name: "studio", Project: "p", Connector: "mlflow", Variant: "databricks", Endpoint: srv.URL, Locations: []string{"1"},
		Auth: &evalsiv1alpha1.SourceAuth{Databricks: &evalsiv1alpha1.SourceAuth_Databricks{ClientId: "sp-client"}},
	}
	if mutate != nil {
		mutate(src)
	}
	c, err := Factory(src, "sp-secret/&=", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c.(*connector), srv
}

func TestDatabricksServicePrincipalTokenIsExchangedCachedAndRefreshed(t *testing.T) {
	f := &fakeWorkspace{expires: 3600}
	c, _ := workspace(t, f, nil)
	clock := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	oauth := c.auth.(*oauthClient)
	oauth.now = func() time.Time { return clock }
	list := func() {
		t.Helper()
		if _, err := c.List(context.Background(), source.ListRequest{Since: clock, Limit: 10}); err != nil {
			t.Fatal(err)
		}
	}
	list()
	list()
	if f.tokens != 1 {
		t.Fatalf("%d token requests for two calls", f.tokens)
	}
	// The exchange: client credentials, the workspace's own endpoint, the
	// secret in Basic auth (form-encoded first, as RFC 6749 asks) and not in the body.
	if f.lastForm.Get("grant_type") != "client_credentials" || f.lastForm.Get("scope") != "all-apis" || f.lastForm.Get("client_secret") != "" {
		t.Fatalf("form %v", f.lastForm)
	}
	if f.lastUser != "sp-client" || f.lastPassword != "sp-secret%2F%26%3D" {
		t.Fatalf("basic auth %q %q", f.lastUser, f.lastPassword)
	}
	if !strings.HasSuffix(f.requests[0], "auth=Bearer access-1") {
		t.Fatalf("api call %q", f.requests[0])
	}
	// Within a minute of expiry the token is replaced.
	clock = clock.Add(3600*time.Second - 30*time.Second)
	list()
	if f.tokens != 2 || !strings.HasSuffix(f.requests[len(f.requests)-1], "auth=Bearer access-2") {
		t.Fatalf("tokens %d, last call %q", f.tokens, f.requests[len(f.requests)-1])
	}
}

func TestDatabricksRejectedTokenIsReplacedOnce(t *testing.T) {
	f := &fakeWorkspace{reject: "access-1"}
	c, _ := workspace(t, f, nil)
	if _, err := c.List(context.Background(), source.ListRequest{Since: time.Now(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 2 || len(f.requests) != 2 || !strings.HasSuffix(f.requests[1], "auth=Bearer access-2") {
		t.Fatalf("tokens %d, requests %v", f.tokens, f.requests)
	}
}

// A token that is refused again is an error, not a loop.
func TestDatabricksRefusedAgainIsAnError(t *testing.T) {
	f := &fakeWorkspace{rejectAll: true}
	c, _ := workspace(t, f, nil)
	_, err := c.List(context.Background(), source.ListRequest{Since: time.Now(), Limit: 10})
	var ae *apiError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized || f.tokens != 2 || len(f.requests) != 2 {
		t.Fatalf("err %v after %d tokens and %d requests", err, f.tokens, len(f.requests))
	}
}

func TestDatabricksTokenFailureNamesNoSecret(t *testing.T) {
	f := &fakeWorkspace{tokenErr: true}
	c, _ := workspace(t, f, nil)
	_, err := c.List(context.Background(), source.ListRequest{Since: time.Now(), Limit: 10})
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || strings.Contains(err.Error(), "sp-secret") {
		t.Fatalf("got %v", err)
	}
}

func TestDatabricksPersonalAccessTokenIsSentAsIs(t *testing.T) {
	f := &fakeWorkspace{}
	c, _ := workspace(t, f, func(s *evalsiv1alpha1.TraceSource) { s.Auth = nil })
	if _, err := c.List(context.Background(), source.ListRequest{Since: time.Now(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 0 || !strings.HasSuffix(f.requests[0], "auth=Bearer sp-secret/&=") {
		t.Fatalf("tokens %d, requests %v", f.tokens, f.requests)
	}
}

// ---- Databricks: Unity Catalog ----

func ucWorkspace(t *testing.T, f *fakeWorkspace) *connector {
	t.Helper()
	c, _ := workspace(t, f, func(s *evalsiv1alpha1.TraceSource) {
		s.Locations = []string{"main.agents"}
		s.Auth.Databricks.SqlWarehouseId = "wh-1"
	})
	c.pollEvery = time.Millisecond
	return c
}

func TestUnityCatalogListPollsTheSearchOperation(t *testing.T) {
	f := &fakeWorkspace{pending: 3}
	c := ucWorkspace(t, f)
	since := time.Date(2026, 10, 9, 17, 0, 0, 0, time.UTC)
	res, err := c.List(context.Background(), source.ListRequest{Since: since, Cursor: "page2", Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Infos) != 1 || res.Infos[0].ID != ucTraceID || res.Infos[0].InProgress || res.Infos[0].Digest == "" {
		t.Fatalf("infos %+v", res.Infos)
	}
	if want := time.Date(2026, 10, 9, 17, 40, 31, 612_000_000, time.UTC); !res.Infos[0].Started.Equal(want) {
		t.Fatalf("started %v", res.Infos[0].Started)
	}
	body := f.bodies[0]
	loc := body["locations"].([]any)[0].(map[string]any)
	uc := loc["uc_schema"].(map[string]any)
	if loc["type"] != "UC_SCHEMA" || uc["catalog_name"] != "main" || uc["schema_name"] != "agents" {
		t.Fatalf("locations %v", body["locations"])
	}
	if body["sql_warehouse_id"] != "wh-1" || body["page_token"] != "page2" || body["max_results"] != float64(500) ||
		body["filter"] != "trace.timestamp_ms >= "+itoa(since.UnixMilli()) {
		t.Fatalf("body %v", body)
	}
	// One POST, then a poll until the operation is done (3 pending -> 3 polls).
	if len(f.requests) != 4 || !strings.HasPrefix(f.requests[1], "GET /api/4.0/mlflow/traces/search/operations/operations/search-1") {
		t.Fatalf("requests %v", f.requests)
	}
}

func TestUnityCatalogSearchFailureIsAnError(t *testing.T) {
	f := &fakeWorkspace{searchFile: "uc_search_failed"}
	c := ucWorkspace(t, f)
	_, err := c.List(context.Background(), source.ListRequest{Since: time.Now(), Limit: 10})
	if err == nil || !strings.Contains(err.Error(), "SQL warehouse") {
		t.Fatalf("got %v", err)
	}
}

func TestUnityCatalogSearchStopsWithItsContext(t *testing.T) {
	f := &fakeWorkspace{pending: 1 << 30}
	c := ucWorkspace(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.List(ctx, source.ListRequest{Since: time.Now(), Limit: 10}); err == nil {
		t.Fatal("a search that never finishes returned")
	}
}

func TestUnityCatalogTraceBecomesARecordAndIsScoredBack(t *testing.T) {
	f := &fakeWorkspace{}
	c := ucWorkspace(t, f)
	traces, err := c.Fetch(context.Background(), []source.Info{{ID: ucTraceID}})
	if err != nil || len(traces) != 1 || len(traces[0].Spans) != 3 {
		t.Fatalf("fetch: %v %v", traces, err)
	}
	if got := traces[0].Spans[0].Resource[source.AttrSourceTraceID]; got != ucTraceID {
		t.Fatalf("source trace id %v", got)
	}
	req := f.requests[len(f.requests)-1]
	for _, want := range []string{"GET /api/4.0/mlflow/traces/main.agents/batchGet?", "sql_warehouse_id=wh-1", "trace_ids=f864e522377586ed0a722b8a01c9a611"} {
		if !strings.Contains(req, want) {
			t.Fatalf("batchGet request %q lacks %q", req, want)
		}
	}
	rec, _ := ingest.ToRecord(traces[0])
	if rec.GetOutput().GetText() != "4" || len(rec.GetTrajectory().GetSteps()) != 3 {
		t.Fatalf("record %v", rec)
	}

	score := source.Score{TraceID: ucTraceID, Policy: "studio-agents", Metric: "task-success", Value: 1, Rationale: "all tests pass", Evaluator: "tests"}
	f.requests, f.bodies = nil, nil
	done, err := c.WriteBack(context.Background(), []source.Score{score}, nil)
	if err != nil || len(done) != 1 || done[0].RemoteID != "a-uc-1" {
		t.Fatalf("create: %+v %v", done, err)
	}
	// The create is preceded by the look for an assessment already there.
	create := f.requests[len(f.requests)-1]
	if !strings.HasPrefix(create, "POST /api/4.0/mlflow/traces/main.agents/f864e522377586ed0a722b8a01c9a611/assessments?sql_warehouse_id=wh-1") {
		t.Fatalf("create request %q", create)
	}
	a := f.bodies[len(f.bodies)-1]
	if a["trace_id"] != "f864e522377586ed0a722b8a01c9a611" || a["assessment_name"] != "task-success" ||
		a["trace_location"].(map[string]any)["uc_schema"].(map[string]any)["schema_name"] != "agents" ||
		a["source"].(map[string]any)["source_id"] != "evalsi/studio-agents" {
		t.Fatalf("assessment %v", a)
	}

	// With the bookkeeping, a changed score updates the assessment in place.
	f.requests, f.bodies = nil, nil
	prior := map[store.WriteKey]store.SourceWrite{score.Key(): {RemoteID: "a-uc-1"}}
	score.Value = 0
	if _, err := c.WriteBack(context.Background(), []source.Score{score}, prior); err != nil {
		t.Fatal(err)
	}
	patch := f.requests[len(f.requests)-1]
	if len(f.requests) != 1 || !strings.HasPrefix(patch, "PATCH /api/4.0/mlflow/traces/main.agents/f864e522377586ed0a722b8a01c9a611/assessments/a-uc-1?") ||
		!strings.Contains(patch, "update_mask=feedback%2Crationale") || !strings.Contains(patch, "sql_warehouse_id=wh-1") {
		t.Fatalf("update requests %v", f.requests)
	}

	// With the bookkeeping lost, the trace's own assessments are searched.
	f.requests, f.bodies = nil, nil
	f.existing = []map[string]any{{"assessment_id": "a-found", "assessment_name": "task-success", "source": map[string]any{"source_id": "evalsi/studio-agents"}}}
	done, err = c.WriteBack(context.Background(), []source.Score{score}, nil)
	if err != nil || len(done) != 1 || done[0].RemoteID != "a-found" || !strings.HasPrefix(f.requests[len(f.requests)-1], "PATCH ") {
		t.Fatalf("recover: %+v %v %v", done, err, f.requests)
	}
}

func TestDatabricksLocationsAreChecked(t *testing.T) {
	src := func(auth *evalsiv1alpha1.SourceAuth_Databricks, locations ...string) *evalsiv1alpha1.TraceSource {
		return &evalsiv1alpha1.TraceSource{Variant: "databricks", Endpoint: "https://w.cloud.databricks.com", Locations: locations, Auth: &evalsiv1alpha1.SourceAuth{Databricks: auth}}
	}
	wh := &evalsiv1alpha1.SourceAuth_Databricks{SqlWarehouseId: "wh-1"}
	for name, c := range map[string]struct {
		src  *evalsiv1alpha1.TraceSource
		want string
	}{
		"no warehouse":    {src(nil, "main.agents"), "sql_warehouse_id"},
		"mixed":           {src(wh, "main.agents", "12"), "not both"},
		"table prefix":    {src(wh, "main.agents.prefix"), "table prefixes are not built"},
		"experiment name": {src(nil, "studio"), "numeric experiment ID"},
		"warehouse alone": {src(wh, "12"), "Unity Catalog schema locations"},
	} {
		if _, err := Factory(c.src, "token", http.DefaultClient); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Factory(src(wh, "main.agents", "main.other"), "token", http.DefaultClient); err != nil {
		t.Errorf("two schemas: %v", err)
	}
	// Apply validates before the credential is read.
	if _, err := Factory(src(wh, "main.agents"), source.ValidationToken, http.DefaultClient); err != nil {
		t.Errorf("validation: %v", err)
	}
}

// The manager builds a source's connector each cycle, so the token must
// outlive the connector. A rotated secret must not reuse the old secret's token.
func TestDatabricksTokenOutlivesTheConnector(t *testing.T) {
	f := &fakeWorkspace{}
	first, srv := workspace(t, f, nil)
	if _, err := first.List(context.Background(), source.ListRequest{Since: time.Now(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	build := func(secret string) *connector {
		c, err := Factory(first.src, secret, srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		return c.(*connector)
	}
	again := build("sp-secret/&=")
	if _, err := again.List(context.Background(), source.ListRequest{Since: time.Now(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 1 {
		t.Fatalf("%d token requests for a rebuilt connector", f.tokens)
	}
	rotated := build("a-new-secret")
	if _, err := rotated.List(context.Background(), source.ListRequest{Since: time.Now(), Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if f.tokens != 2 {
		t.Fatalf("a rotated secret used the old secret's token (%d token requests)", f.tokens)
	}
}

// A space in a query goes as %20, which is how the signature covers it.
func TestSignedQueryEncodesSpacesAsPercent20(t *testing.T) {
	var raw string
	f := &fakeMLflow{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "get-by-name") {
			raw = r.URL.RawQuery
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	src := sagemakerSource(serverARN, "https://us-east-1.experiments.sagemaker.aws")
	src.Locations = []string{"studio agents+v2"}
	c, err := Factory(src, awsKey, rewrite(srv))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.List(context.Background(), source.ListRequest{Since: time.Now(), Limit: 1}) // the name is unknown; the request is what matters
	if raw != "experiment_name=studio%20agents%2Bv2" {
		t.Fatalf("query on the wire %q", raw)
	}
}
