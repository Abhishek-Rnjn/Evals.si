package failures

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func text(value string) *pb.Content { return &pb.Content{Kind: &pb.Content_Text{Text: value}} }

func fixture(t *testing.T) (*store.Store, *Service, *pb.FailureCase) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.PutTrace(t.Context(), &pb.TraceSummary{Project: "support", TraceId: strings.Repeat("a", 32), Service: "refunds", StartTime: timestamppb.Now(), Labels: map[string]string{"environment": "production"}}, &pb.Record{Id: "original", Input: text("refund order"), Output: text("unauthorized refund"), Metadata: map[string]*structpb.Value{"private": structpb.NewStringValue("remove-me-secret")}}); err != nil {
		t.Fatal(err)
	}
	svc := New(st)
	response, err := svc.CaptureFailure(t.Context(), connect.NewRequest(&pb.CaptureFailureRequest{Project: "support", TraceId: strings.Repeat("a", 32), Title: "Unauthorized refund", Reason: "Missing approval"}))
	if err != nil {
		t.Fatal(err)
	}
	return st, svc, response.Msg.GetFailure()
}

func editRequest(f *pb.FailureCase) *pb.UpdateFailureRequest {
	r := proto.Clone(f.GetRecord()).(*pb.Record)
	r.Reference = text("requires approval")
	r.Metadata = nil
	return &pb.UpdateFailureRequest{Project: f.GetProject(), Id: f.GetId(), ExpectedRevision: f.GetRevision(), Record: r, Title: "Reviewed refund incident", Reason: "Require approval before refund", Assignee: "support-owner", Status: pb.FailureStatus_FAILURE_STATUS_IN_REVIEW}
}

func TestReviewedRevisionExportAndEditRevokesApproval(t *testing.T) {
	st, svc, f := fixture(t)
	ctx := t.Context()
	export := func(revision int64) (*connect.Response[pb.ExportFailuresResponse], error) {
		return svc.ExportFailures(ctx, connect.NewRequest(&pb.ExportFailuresRequest{Project: "support", Selections: []*pb.FailureSelection{{Id: f.GetId(), Revision: revision}}}))
	}
	if _, err := export(1); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("draft export: %v", err)
	}
	if _, err := svc.ReviewFailure(ctx, connect.NewRequest(&pb.ReviewFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 1, Decision: pb.FailureStatus_FAILURE_STATUS_APPROVED, Note: "unreviewed"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("approval bypass: %v", err)
	}
	edited, err := svc.UpdateFailure(ctx, connect.NewRequest(editRequest(f)))
	if err != nil {
		t.Fatal(err)
	}
	f = edited.Msg.GetFailure()
	if f.GetRevision() != 2 || f.GetSourceSha256() == store.RecordHash(f.GetRecord()) || f.GetRecord().GetId() != f.GetSource().GetTraceId() {
		t.Fatalf("lost immutable source identity: %v", f)
	}
	reviewed, err := svc.ReviewFailure(ctx, connect.NewRequest(&pb.ReviewFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 2, Decision: pb.FailureStatus_FAILURE_STATUS_APPROVED, Note: "Expected behavior and redaction checked"}))
	if err != nil {
		t.Fatal(err)
	}
	f = reviewed.Msg.GetFailure()
	if f.GetRevision() != 3 || f.GetReviewedBy() != "anonymous" || f.GetReviewedAt() == nil {
		t.Fatal(f)
	}
	if _, err := export(2); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("stale selection: %v", err)
	}
	response, err := export(3)
	if err != nil {
		t.Fatal(err)
	}
	record := response.Msg.GetRecords()[0]
	if record.GetId() != "failure/"+f.GetId() || record.GetReference().GetText() != "requires approval" || record.GetOutput().GetText() != "unauthorized refund" {
		t.Fatal(record)
	}
	provenance := record.GetMetadata()["evalsi.failure"].GetStructValue().AsMap()
	if provenance["revision"] != "3" || provenance["source_sha256"] != f.GetSourceSha256() || provenance["record_sha256"] != store.RecordHash(f.GetRecord()) {
		t.Fatal(provenance)
	}
	if _, err := st.DeleteTracesBefore(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Source retention cannot erase the curated case or its captured permissions.
	if _, err := svc.GetFailure(ctx, connect.NewRequest(&pb.GetFailureRequest{Project: "support", Id: f.GetId()})); err != nil {
		t.Fatal(err)
	}
	edited, err = svc.UpdateFailure(ctx, connect.NewRequest(editRequest(f)))
	if err != nil {
		t.Fatal(err)
	}
	f = edited.Msg.GetFailure()
	if f.GetReviewedBy() != "" || f.GetReviewedAt() != nil || f.GetReviewNote() != "" {
		t.Fatal("approval survived editing")
	}
	if _, err := export(3); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatal(err)
	}
	if _, err := export(4); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal(err)
	}
	events, err := svc.ListFailureEvents(ctx, connect.NewRequest(&pb.ListFailureEventsRequest{Project: "support", Id: f.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(events.Msg)
	if len(events.Msg.GetEvents()) != 4 || strings.Contains(string(encoded), "remove-me-secret") || strings.Contains(string(encoded), "requires approval") {
		t.Fatalf("history retains payloads: %s", encoded)
	}
}

func TestInvalidEditsAndApprovalRequirements(t *testing.T) {
	_, svc, f := fixture(t)
	for _, mutate := range []func(*pb.UpdateFailureRequest){
		func(r *pb.UpdateFailureRequest) { r.ExpectedRevision = 0 },
		func(r *pb.UpdateFailureRequest) { r.ExpectedRevision = 99 },
		func(r *pb.UpdateFailureRequest) { r.Title = " " },
		func(r *pb.UpdateFailureRequest) { r.Reason = "" },
		func(r *pb.UpdateFailureRequest) { r.Assignee = "bad\nowner" },
		func(r *pb.UpdateFailureRequest) { r.Record = nil },
		func(r *pb.UpdateFailureRequest) { r.Status = pb.FailureStatus_FAILURE_STATUS_APPROVED },
		func(r *pb.UpdateFailureRequest) { r.Record.Output = text(strings.Repeat("x", MaxRecordBytes)) },
		func(r *pb.UpdateFailureRequest) {
			r.Record.Metadata = map[string]*structpb.Value{"evalsi.dataset": structpb.NewStringValue("forged")}
		},
		func(r *pb.UpdateFailureRequest) {
			r.Record.Metadata = map[string]*structpb.Value{"evalsi.failure": structpb.NewStringValue("forged")}
		},
	} {
		r := editRequest(f)
		mutate(r)
		if _, err := svc.UpdateFailure(t.Context(), connect.NewRequest(r)); err == nil {
			t.Fatalf("accepted invalid edit: %v", r)
		}
	}
	r := editRequest(f)
	r.Record.Reference = nil
	updated, err := svc.UpdateFailure(t.Context(), connect.NewRequest(r))
	if err != nil {
		t.Fatal(err)
	}
	f = updated.Msg.GetFailure()
	for _, decision := range []pb.FailureStatus{pb.FailureStatus_FAILURE_STATUS_APPROVED, pb.FailureStatus_FAILURE_STATUS_OPEN, pb.FailureStatus(999)} {
		if _, err := svc.ReviewFailure(t.Context(), connect.NewRequest(&pb.ReviewFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 2, Decision: decision, Note: "no reference"})); err == nil {
			t.Fatalf("accepted invalid decision %v", decision)
		}
	}
	dismissed, err := svc.ReviewFailure(t.Context(), connect.NewRequest(&pb.ReviewFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 2, Decision: pb.FailureStatus_FAILURE_STATUS_DISMISSED, Note: "not a regression case"}))
	if err != nil || dismissed.Msg.GetFailure().GetStatus() != pb.FailureStatus_FAILURE_STATUS_DISMISSED {
		t.Fatalf("dismissal: %v %v", dismissed, err)
	}
}

func TestExportSelectionAndPaginationValidation(t *testing.T) {
	_, svc, f := fixture(t)
	for _, selections := range [][]*pb.FailureSelection{nil, {{Id: f.GetId(), Revision: 1}, {Id: f.GetId(), Revision: 1}}, {{Id: "bad", Revision: 1}}, {{Id: f.GetId()}}, {{Id: strings.Repeat("f", 24), Revision: 1}}, make([]*pb.FailureSelection, 101)} {
		if _, err := svc.ExportFailures(t.Context(), connect.NewRequest(&pb.ExportFailuresRequest{Project: "support", Selections: selections})); err == nil {
			t.Fatal("accepted invalid selection")
		}
	}
	for _, request := range []*pb.ListFailuresRequest{{PageSize: -1}, {PageSize: 101}, {PageToken: "invalid"}, {Status: pb.FailureStatus(999)}} {
		if _, err := svc.ListFailures(t.Context(), connect.NewRequest(request)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatal(err)
		}
	}
	if _, err := svc.ListFailureEvents(t.Context(), connect.NewRequest(&pb.ListFailureEventsRequest{Project: "support", Id: f.GetId(), PageToken: "-1"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatal(err)
	}
}

func TestRecaptureCannotExposeCaseThroughChangedSourceLabels(t *testing.T) {
	st, svc, f := fixture(t)
	ctx := t.Context()
	summary, record, err := st.GetTrace(ctx, "support", f.GetSource().GetTraceId())
	if err != nil {
		t.Fatal(err)
	}
	// The original case is bound to service=refunds. Only the trace summary
	// changes; the same source record hash still deduplicates to that case.
	summary.Service = "public"
	if err := st.PutTrace(ctx, summary, record); err != nil {
		t.Fatal(err)
	}
	engine, err := authz.NewEngine(ctx, authz.Options{Enabled: true, RBAC: authz.RBACConfig{
		Roles:    []authz.Role{{Name: "public-only", Permissions: []string{"traces.read", "failures.read", "failures.write"}, Condition: `resource.service == "public"`}},
		Projects: map[string]map[string][]string{"support": {"public-only": {"key:limited"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx = authz.WithChecker(ctx, &authz.Checker{Engine: engine, Principal: &auth.Principal{Kind: auth.KindAPIKey, KeyName: "limited"}})
	if _, err := svc.CaptureFailure(ctx, connect.NewRequest(&pb.CaptureFailureRequest{Project: "support", TraceId: f.GetSource().GetTraceId(), Title: "Recapture", Reason: "changed source labels"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("recapture disclosed hidden curated data: %v", err)
	}
}
