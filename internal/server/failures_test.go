package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

func TestFailureInboxPermissionsAndLiveApproval(t *testing.T) {
	custom := map[string]string{}
	s := startAuthServer(t, func(c *config.Config) {
		c.OTLP.Grace = "10ms"
		for _, role := range []authz.Role{
			{Name: "triager", Permissions: []string{"failures.read", "failures.write", "traces.read"}},
			{Name: "reviewer", Permissions: []string{"failures.read", "failures.review", "traces.read"}},
			{Name: "public-triager", Permissions: []string{"failures.*", "traces.read", "datasets.write"}, Condition: `resource.service == "public"`},
		} {
			c.RBAC.Roles = append(c.RBAC.Roles, role)
			plain, hash := auth.NewAPIKey()
			custom[role.Name] = plain
			c.Auth.APIKeys.Keys = append(c.Auth.APIKeys.Keys, auth.ConfigKey{Name: role.Name, Key: "sha256:" + hash, Roles: map[string][]string{"support": {role.Name}}})
		}
	})
	ctx := t.Context()
	client := func(key string) evalsiv1alpha1connect.FailureServiceClient {
		return evalsiv1alpha1connect.NewFailureServiceClient(s.http, s.url, as(key))
	}
	for _, service := range []string{"public", "private"} {
		req, _ := http.NewRequest(http.MethodPost, s.url+"/v1/traces", bytes.NewReader(otlpRequest(service, map[string]string{"evalsi.label.env": "prod"})))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Authorization", "Bearer "+s.keys["ingest"])
		response, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("ingest: %d", response.StatusCode)
		}
	}
	traces := evalsiv1alpha1connect.NewTraceServiceClient(s.http, s.url, as(s.keys["viewer"]))
	ids := map[string]string{}
	for range 100 {
		out, err := traces.ListTraces(ctx, connect.NewRequest(&pb.ListTracesRequest{Project: "support"}))
		if err != nil {
			t.Fatal(err)
		}
		for _, trace := range out.Msg.GetTraces() {
			ids[trace.GetService()] = trace.GetTraceId()
		}
		if len(ids) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(ids) != 2 {
		t.Fatalf("traces not ingested: %v", ids)
	}
	capture := func(id string) *connect.Request[pb.CaptureFailureRequest] {
		return connect.NewRequest(&pb.CaptureFailureRequest{Project: "support", TraceId: id, Title: "Wrong refund", Reason: "Missing approval"})
	}
	if _, err := client(s.keys["viewer"]).CaptureFailure(ctx, capture(ids["public"])); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer capture: %v", err)
	}
	if _, err := client(custom["public-triager"]).CaptureFailure(ctx, capture(ids["private"])); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("source rule bypass: %v", err)
	}
	editor := client(s.keys["editor"])
	created, err := editor.CaptureFailure(ctx, capture(ids["public"]))
	if err != nil {
		t.Fatal(err)
	}
	f := created.Msg.GetFailure()
	private, err := editor.CaptureFailure(ctx, capture(ids["private"]))
	if err != nil {
		t.Fatal(err)
	}
	if private.Msg.GetFailure().GetId() == f.GetId() {
		t.Fatal("different traces deduplicated")
	}
	for _, key := range []string{s.keys["checkout-viewer"], custom["public-triager"]} {
		id := f.GetId()
		if key == custom["public-triager"] {
			id = private.Msg.GetFailure().GetId()
		}
		if _, err := client(key).GetFailure(ctx, connect.NewRequest(&pb.GetFailureRequest{Project: "support", Id: id})); codeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("hidden get: %v", err)
		}
	}
	listed, err := client(custom["public-triager"]).ListFailures(ctx, connect.NewRequest(&pb.ListFailuresRequest{Project: "support"}))
	if err != nil || len(listed.Msg.GetFailures()) != 1 || listed.Msg.GetFailures()[0].GetRecord() != nil {
		t.Fatalf("filtered list: %v %v", listed, err)
	}
	update := &pb.UpdateFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 1, Title: "Wrong refund", Reason: "Require human approval", Assignee: "support-oncall", Record: &pb.Record{Input: text("Refund order"), Output: text("Refunded"), Reference: text("Ask for approval")}, Status: pb.FailureStatus_FAILURE_STATUS_IN_REVIEW}
	if _, err := client(s.keys["viewer"]).UpdateFailure(ctx, connect.NewRequest(update)); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer edit: %v", err)
	}
	edited, err := client(custom["triager"]).UpdateFailure(ctx, connect.NewRequest(update))
	if err != nil {
		t.Fatal(err)
	}
	f = edited.Msg.GetFailure()
	review := &pb.ReviewFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 2, Decision: pb.FailureStatus_FAILURE_STATUS_APPROVED, Note: "Checked against support policy"}
	if _, err := client(custom["triager"]).ReviewFailure(ctx, connect.NewRequest(review)); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("triager approved: %v", err)
	}
	reviewed, err := client(custom["reviewer"]).ReviewFailure(ctx, connect.NewRequest(review))
	if err != nil {
		t.Fatal(err)
	}
	f = reviewed.Msg.GetFailure()
	if f.GetReviewedBy() != "key:reviewer" {
		t.Fatal(f.GetReviewedBy())
	}
	export := func() *connect.Request[pb.ExportFailuresRequest] {
		r := connect.NewRequest(&pb.ExportFailuresRequest{Project: "support", Selections: []*pb.FailureSelection{{Id: f.GetId(), Revision: 3}}})
		r.Header().Set("Idempotency-Key", "always-check-live-approval")
		return r
	}
	if _, err := client(s.keys["viewer"]).ExportFailures(ctx, export()); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer export: %v", err)
	}
	if _, err := client(custom["reviewer"]).ExportFailures(ctx, export()); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("reviewer exported without datasets.write: %v", err)
	}
	exported, err := editor.ExportFailures(ctx, export())
	if err != nil {
		t.Fatal(err)
	}
	if exported.Msg.GetRecords()[0].GetMetadata()["evalsi.failure"].GetStructValue().AsMap()["revision"] != "3" {
		t.Fatal("missing export revision")
	}
	update.ExpectedRevision = 3
	update.Reason = "new findings require another review"
	if _, err := client(custom["triager"]).UpdateFailure(ctx, connect.NewRequest(update)); err != nil {
		t.Fatal(err)
	}
	if _, err := editor.ExportFailures(ctx, export()); codeOf(err) != connect.CodeAborted {
		t.Fatalf("cached export bypassed revoked approval: %v", err)
	}
	// Retrying capture cannot overwrite or reopen the human-edited revision.
	retry, err := editor.CaptureFailure(ctx, capture(ids["public"]))
	if err != nil || !retry.Msg.GetDeduplicated() || retry.Msg.GetFailure().GetRevision() != 4 {
		t.Fatalf("capture retry: %v %v", retry, err)
	}
	// REST reaches the same real service and revision guard.
	req, _ := http.NewRequest(http.MethodGet, s.url+"/v1alpha1/failures/"+f.GetId()+"?project=support", nil)
	req.Header.Set("Authorization", "Bearer "+s.keys["viewer"])
	response, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(body), "new findings") {
		t.Fatalf("REST get: %d %s", response.StatusCode, body)
	}
	if _, err := editor.DeleteFailure(ctx, connect.NewRequest(&pb.DeleteFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 4})); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("editor delete: %v", err)
	}
	if _, err := client(s.keys["admin"]).DeleteFailure(ctx, connect.NewRequest(&pb.DeleteFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 4})); err != nil {
		t.Fatal(err)
	}
	audit := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["admin"]))
	events, err := audit.ListAuditEvents(ctx, connect.NewRequest(&pb.ListAuditEventsRequest{Project: "support", PageSize: 100}))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(events.Msg)
	if !strings.Contains(string(raw), "failures.review") || !strings.Contains(string(raw), "failures.delete") || strings.Contains(string(raw), "Checked against support policy") {
		t.Fatalf("audit actions/payloads: %s", raw)
	}
}
