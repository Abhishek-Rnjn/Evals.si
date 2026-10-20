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
		evalsiv1alpha1connect.DatasetServiceName,
		evalsiv1alpha1connect.EvaluationServiceName, evalsiv1alpha1connect.CatalogServiceName,
		evalsiv1alpha1connect.RunServiceName, evalsiv1alpha1connect.MonitorServiceName,
		evalsiv1alpha1connect.TraceServiceName,
		evalsiv1alpha1connect.FailureServiceName,
		evalsiv1alpha1connect.DatasetCatalogServiceName,
		evalsiv1alpha1connect.ReleaseServiceName,
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
		{"POST", "/v1alpha1/release-policies:inspect", `{"policy":{"name":"release"}}`, "ReleaseService/InspectReleasePolicy", "", ""},
		{"GET", "/v1alpha1/runs/run-a/release-evidence", "", "ReleaseService/GetReleaseRunEvidence", "runId", "run-a"},
		{"POST", "/v1alpha1/release-decisions", `{"id":"v1"}`, "ReleaseService/CreateReleaseDecision", "id", "v1"},
		{"GET", "/v1alpha1/release-decisions/v1?project=p", "", "ReleaseService/GetReleaseDecision", "id", "v1"},
		{"GET", "/v1alpha1/release-decisions?project=p", "", "ReleaseService/ListReleaseDecisions", "project", "p"},
		{"POST", "/v1alpha1/dataset-versions", `{"content":{"name":"refunds"}}`, "DatasetCatalogService/PublishDatasetVersion", "", ""},
		{"GET", "/v1alpha1/dataset-versions?project=p", "", "DatasetCatalogService/ListDatasetVersions", "project", "p"},
		{"GET", "/v1alpha1/dataset-versions/refunds/v1", "", "DatasetCatalogService/GetDatasetVersion", "version", "v1"},
		{"POST", "/v1alpha1/dataset-versions/refunds/v1:review", `{"sha256":"pin"}`, "DatasetCatalogService/ReviewDatasetVersion", "name", "refunds"},
		{"GET", "/v1alpha1/dataset-versions/refunds/v1/events", "", "DatasetCatalogService/ListDatasetVersionEvents", "version", "v1"},
		{"POST", "/v1alpha1/dataset-versions/refunds:diff", `{"before_version":"v1"}`, "DatasetCatalogService/DiffDatasetVersions", "name", "refunds"},
		{"POST", "/v1alpha1/failures", `{"trace_id":"abc","title":"Incident"}`, "FailureService/CaptureFailure", "trace_id", "abc"},
		{"GET", "/v1alpha1/failures?project=p", "", "FailureService/ListFailures", "project", "p"},
		{"GET", "/v1alpha1/failures/case-id", "", "FailureService/GetFailure", "id", "case-id"},
		{"PUT", "/v1alpha1/failures/case-id", `{"expected_revision":"2"}`, "FailureService/UpdateFailure", "id", "case-id"},
		{"POST", "/v1alpha1/failures/case-id:review", `{"decision":"FAILURE_STATUS_APPROVED"}`, "FailureService/ReviewFailure", "id", "case-id"},
		{"GET", "/v1alpha1/failures/case-id/events", "", "FailureService/ListFailureEvents", "id", "case-id"},
		{"POST", "/v1alpha1/failures:export", `{"project":"p"}`, "FailureService/ExportFailures", "project", "p"},
		{"DELETE", "/v1alpha1/failures/case-id?expected_revision=2", "", "FailureService/DeleteFailure", "id", "case-id"},
		{"POST", "/v1alpha1/datasets:import", `{"config":{"project":"p","name":"data","format":"jsonl"},"uri":"s3://bucket/incoming/p/data.jsonl"}`, "DatasetService/ImportDataset", "", ""},
		{"POST", "/v1alpha1/evaluate", `{"records": []}`, "EvaluationService/Evaluate", "", ""},
		{"GET", "/v1alpha1/evaluators", "", "CatalogService/ListEvaluators", "", ""},
		{"GET", "/v1alpha1/credentials?project=p", "", "CatalogService/ListCredentials", "project", "p"},
		{"POST", "/v1alpha1/runs", `{"spec": {"name": "x"}}`, "RunService/CreateRun", "", ""},
		{"GET", "/v1alpha1/runs?project=p&page_size=5", "", "RunService/ListRuns", "project", "p"},
		{"GET", "/v1alpha1/runs/run-1", "", "RunService/GetRun", "id", "run-1"},
		{"DELETE", "/v1alpha1/runs/run-1", "", "RunService/DeleteRun", "id", "run-1"},
		{"POST", "/v1alpha1/runs/run-1:archive", "{}", "RunService/ArchiveRun", "id", "run-1"},
		{"POST", "/v1alpha1/runs/run-1:cancel", "{}", "RunService/CancelRun", "id", "run-1"},
		{"POST", "/v1alpha1/runs/run-1:resume", "{}", "RunService/ResumeRun", "id", "run-1"},
		{"GET", "/v1alpha1/runs/run-1/results?evaluator=em", "", "RunService/ListRunResults", "runId", "run-1"},
		{"POST", "/v1alpha1/runs:compare", `{"baseline_run_id": "a"}`, "RunService/CompareRuns", "baseline_run_id", "a"},
		{"POST", "/v1alpha1/runs:slice", `{"metric": "em", "by": "topic"}`, "RunService/SliceRuns", "metric", "em"},
		{"POST", "/v1alpha1/runs:import", `{"name": "local"}`, "RunService/ImportRun", "name", "local"},
		{"POST", "/v1alpha1/policies", `{"name": "prod"}`, "MonitorService/ApplyPolicy", "", ""},
		{"GET", "/v1alpha1/policies", "", "MonitorService/ListPolicies", "", ""},
		{"DELETE", "/v1alpha1/policies/prod", "", "MonitorService/DeletePolicy", "name", "prod"},
		{"GET", "/v1alpha1/policies/prod/stats", "", "MonitorService/GetPolicyStats", "name", "prod"},
		{"GET", "/v1alpha1/traces?service=bot", "", "TraceService/ListTraces", "service", "bot"},
		{"GET", "/v1alpha1/traces/0af7", "", "TraceService/GetTrace", "traceId", "0af7"},
		{"GET", "/v1alpha1/traces/0af7/scores?policy=prod", "", "TraceService/ListScores", "traceId", "0af7"},
		{"GET", "/v1alpha1/scores?project=p&labels=workflow=w1&since=2026-10-01T00:00:00Z", "", "TraceService/ListScores", "since", "2026-10-01T00:00:00Z"},
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
