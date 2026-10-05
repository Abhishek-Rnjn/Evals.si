package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
)

// Every REST route reaches the intended RPC, with path variables in the request.
func TestRESTRoutes(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = map[string]any{}
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	})
	handlers := map[string]http.Handler{}
	for _, name := range []string{
		evalsiv1alpha1connect.EvaluationServiceName, evalsiv1alpha1connect.CatalogServiceName,
		evalsiv1alpha1connect.RunServiceName, evalsiv1alpha1connect.MonitorServiceName,
		evalsiv1alpha1connect.TraceServiceName,
	} {
		handlers[name] = echo
	}
	rest, err := restHandler(handlers)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		method, path, body, rpc, field, value string
	}{
		{"POST", "/v1alpha1/evaluate", `{"records": []}`, "EvaluationService/Evaluate", "", ""},
		{"GET", "/v1alpha1/evaluators", "", "CatalogService/ListEvaluators", "", ""},
		{"POST", "/v1alpha1/runs", `{"spec": {"name": "x"}}`, "RunService/CreateRun", "", ""},
		{"GET", "/v1alpha1/runs?project=p&page_size=5", "", "RunService/ListRuns", "project", "p"},
		{"GET", "/v1alpha1/runs/run-1", "", "RunService/GetRun", "id", "run-1"},
		{"POST", "/v1alpha1/runs/run-1:cancel", "{}", "RunService/CancelRun", "id", "run-1"},
		{"POST", "/v1alpha1/runs/run-1:resume", "{}", "RunService/ResumeRun", "id", "run-1"},
		{"GET", "/v1alpha1/runs/run-1/results?evaluator=em", "", "RunService/ListRunResults", "runId", "run-1"},
		{"POST", "/v1alpha1/runs:compare", `{"baseline_run_id": "a"}`, "RunService/CompareRuns", "baseline_run_id", "a"},
		{"POST", "/v1alpha1/policies", `{"name": "prod"}`, "MonitorService/ApplyPolicy", "", ""},
		{"GET", "/v1alpha1/policies", "", "MonitorService/ListPolicies", "", ""},
		{"DELETE", "/v1alpha1/policies/prod", "", "MonitorService/DeletePolicy", "name", "prod"},
		{"GET", "/v1alpha1/policies/prod/stats", "", "MonitorService/GetPolicyStats", "name", "prod"},
		{"GET", "/v1alpha1/traces?service=bot", "", "TraceService/ListTraces", "service", "bot"},
		{"GET", "/v1alpha1/traces/0af7", "", "TraceService/GetTrace", "traceId", "0af7"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		gotPath = ""
		rest.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || gotPath != "/evalsi.v1alpha1."+c.rpc {
			t.Errorf("%s %s: status %d reached %q, want %s (%s)", c.method, c.path, rec.Code, gotPath, c.rpc, rec.Body.String())
			continue
		}
		if c.field != "" && gotBody[c.field] != c.value {
			t.Errorf("%s %s: request %v lacks %s=%s", c.method, c.path, gotBody, c.field, c.value)
		}
	}
}
