package datasetcatalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/failures"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func text(value string) *pb.Content { return &pb.Content{Kind: &pb.Content_Text{Text: value}} }
func as(ctx context.Context, name string) context.Context {
	return auth.WithResult(ctx, auth.Result{Principal: &auth.Principal{Kind: auth.KindAPIKey, KeyName: name}})
}

func fixture(t *testing.T) (*store.Store, *Service, *pb.DatasetVersion) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := New(st)
	result, err := svc.PublishDatasetVersion(as(t.Context(), "publisher"), connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: content("v1")}))
	if err != nil {
		t.Fatal(err)
	}
	return st, svc, result.Msg.GetDataset()
}

func content(version string) *pb.DatasetVersionContent {
	return &pb.DatasetVersionContent{Project: "support", Name: "refunds", Version: version, Owner: "support-team", Split: "held-out", Tags: []string{"synthetic", "approved"}, Records: []*pb.Record{{Id: "one", Input: text("Refund order"), Output: text("Approval required"), Reference: text("Approval required")}, {Id: "two", Input: text("Cancel order"), Output: text("Cancelled"), Reference: text("Cancelled")}}}
}

func review(d *pb.DatasetVersion, state pb.DatasetVersionState) *pb.ReviewDatasetVersionRequest {
	c := d.GetContent()
	return &pb.ReviewDatasetVersionRequest{Project: c.GetProject(), Name: c.GetName(), Version: c.GetVersion(), Sha256: d.GetSha256(), ExpectedRevision: d.GetRevision(), Decision: state, Note: "Expected behavior and source reviewed"}
}

func TestDatasetCatalogApprovalPinsRetirementAndStableCopies(t *testing.T) {
	st, svc, d := fixture(t)
	ctx := t.Context()
	ref := Reference(d)
	if _, err := Load(ctx, st, ref, "support", 0); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("draft loaded: %v", err)
	}
	if _, err := svc.ReviewDatasetVersion(as(ctx, "publisher"), connect.NewRequest(review(d, pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED))); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("self approval: %v", err)
	}
	approved, err := svc.ReviewDatasetVersion(as(ctx, "reviewer"), connect.NewRequest(review(d, pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED)))
	if err != nil {
		t.Fatal(err)
	}
	d = approved.Msg.GetDataset()
	if d.GetSha256() != ref.GetSha256() || d.GetRevision() != 2 || d.GetReviewedBy() != "key:reviewer" {
		t.Fatal(d)
	}
	records, err := Load(ctx, st, ref, "support", 1)
	if err != nil || len(records) != 1 {
		t.Fatalf("load: %v %v", records, err)
	}
	identity := records[0].GetMetadata()[IdentityKey].GetStructValue().AsMap()
	if identity["sha256"] != d.GetSha256() || identity["reviewed_by"] != "key:reviewer" {
		t.Fatal(identity)
	}
	records[0].Reference = text("mutated caller copy")
	stored, err := st.GetDatasetVersion(ctx, "support", "refunds", "v1")
	if err != nil || stored.GetContent().GetRecords()[0].GetReference().GetText() != "Approval required" {
		t.Fatalf("load mutated catalog: %v %v", stored, err)
	}
	wrong := proto.Clone(ref).(*pb.DatasetVersionRef)
	wrong.Sha256 = strings.Repeat("0", 64)
	if _, err := Load(ctx, st, wrong, "support", 0); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("wrong pin: %v", err)
	}
	if _, err := Load(ctx, st, &pb.DatasetVersionRef{Name: "refunds", Version: "v1"}, "support", 0); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("missing pin: %v", err)
	}
	if _, err := svc.ReviewDatasetVersion(as(ctx, "reviewer"), connect.NewRequest(review(d, pb.DatasetVersionState_DATASET_VERSION_STATE_RETIRED))); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(ctx, st, ref, "support", 0); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("retired version loaded: %v", err)
	}
	if _, err := svc.PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: content("v1")})); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("retired name reused: %v", err)
	}
	retired, err := svc.get(ctx, "support", "refunds", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReviewDatasetVersion(as(ctx, "reviewer"), connect.NewRequest(review(retired, pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED))); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("retired version reactivated: %v", err)
	}
	events, err := svc.ListDatasetVersionEvents(ctx, connect.NewRequest(&pb.ListDatasetVersionEventsRequest{Project: "support", Name: "refunds", Version: "v1"}))
	if err != nil || len(events.Msg.GetEvents()) != 3 {
		t.Fatalf("history: %v %v", events, err)
	}
}

func TestDatasetCatalogRejectsInvalidContentAndStaleReviews(t *testing.T) {
	_, svc, d := fixture(t)
	for _, mutate := range []func(*pb.DatasetVersionContent){
		func(c *pb.DatasetVersionContent) { c.Name = "../escape" }, func(c *pb.DatasetVersionContent) { c.Version = "" }, func(c *pb.DatasetVersionContent) { c.Owner = " " }, func(c *pb.DatasetVersionContent) { c.Split = "unknown" },
		func(c *pb.DatasetVersionContent) { c.Tags = []string{"duplicate", "duplicate"} }, func(c *pb.DatasetVersionContent) { c.Records = nil }, func(c *pb.DatasetVersionContent) { c.Records[0].Id = "" }, func(c *pb.DatasetVersionContent) { c.Records[0].Id = c.Records[1].GetId() },
		func(c *pb.DatasetVersionContent) {
			c.Records[0].Metadata = map[string]*structpb.Value{IdentityKey: structpb.NewStringValue("forged")}
		}, func(c *pb.DatasetVersionContent) { c.AccessSources = []*pb.TraceSummary{{TraceId: "forged-access"}} },
		func(c *pb.DatasetVersionContent) { c.Records[0].Input = text(strings.Repeat("x", MaxVersionBytes)) },
	} {
		c := content("v2")
		mutate(c)
		if _, err := svc.PublishDatasetVersion(t.Context(), connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: c})); err == nil {
			t.Fatal("accepted invalid publication")
		}
	}
	for _, mutate := range []func(*pb.ReviewDatasetVersionRequest){func(r *pb.ReviewDatasetVersionRequest) { r.Sha256 = "" }, func(r *pb.ReviewDatasetVersionRequest) { r.Sha256 = strings.Repeat("0", 64) }, func(r *pb.ReviewDatasetVersionRequest) { r.ExpectedRevision = 0 }, func(r *pb.ReviewDatasetVersionRequest) { r.ExpectedRevision = 99 }, func(r *pb.ReviewDatasetVersionRequest) { r.Note = "" }, func(r *pb.ReviewDatasetVersionRequest) {
		r.Decision = pb.DatasetVersionState_DATASET_VERSION_STATE_DRAFT
	}} {
		r := review(d, pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED)
		mutate(r)
		if _, err := svc.ReviewDatasetVersion(as(t.Context(), "reviewer"), connect.NewRequest(r)); err == nil {
			t.Fatal("accepted invalid review")
		}
	}
}

func TestDatasetCatalogDiffExcludesPayloadsAndPreservesOrdering(t *testing.T) {
	_, svc, a := fixture(t)
	c := content("v2")
	c.Owner = "another-team"
	c.Records[0].Reference = text("private-expected-849")
	c.Records[1].Id = "new-id"
	c.Records = []*pb.Record{c.Records[1], c.Records[0]}
	created, err := svc.PublishDatasetVersion(t.Context(), connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: c}))
	if err != nil {
		t.Fatal(err)
	}
	b := created.Msg.GetDataset()
	diff, err := svc.DiffDatasetVersions(t.Context(), connect.NewRequest(&pb.DiffDatasetVersionsRequest{Project: "support", Name: "refunds", BeforeVersion: "v1", AfterVersion: "v2", BeforeSha256: a.GetSha256(), AfterSha256: b.GetSha256()}))
	if err != nil {
		t.Fatal(err)
	}
	out := diff.Msg
	if len(out.GetAdded()) != 1 || out.GetAdded()[0] != "new-id" || len(out.GetRemoved()) != 1 || out.GetRemoved()[0] != "two" || len(out.GetChanged()) != 1 || out.GetChanged()[0].GetFields()[0] != "reference" || !out.GetOrderChanged() {
		t.Fatal(out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "private-expected-849") {
		t.Fatal("diff leaked record payload")
	}
}

func TestFailurePublicationRetainsCapturedAccessBoundaries(t *testing.T) {
	st, svc, _ := fixture(t)
	ctx := t.Context()
	traceID := strings.Repeat("a", 32)
	if err := st.PutTrace(ctx, &pb.TraceSummary{Project: "support", TraceId: traceID, Service: "private", StartTime: timestamppb.Now()}, &pb.Record{Id: traceID, Input: text("input"), Output: text("observed")}); err != nil {
		t.Fatal(err)
	}
	inbox := failures.New(st)
	captured, err := inbox.CaptureFailure(ctx, connect.NewRequest(&pb.CaptureFailureRequest{Project: "support", TraceId: traceID, Title: "Incident", Reason: "Expected behavior"}))
	if err != nil {
		t.Fatal(err)
	}
	f := captured.Msg.GetFailure()
	record := proto.Clone(f.GetRecord()).(*pb.Record)
	record.Reference = text("reviewed")
	if _, err := inbox.UpdateFailure(ctx, connect.NewRequest(&pb.UpdateFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 1, Record: record, Title: f.GetTitle(), Reason: f.GetReason(), Status: pb.FailureStatus_FAILURE_STATUS_IN_REVIEW})); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.ReviewFailure(ctx, connect.NewRequest(&pb.ReviewFailureRequest{Project: "support", Id: f.GetId(), ExpectedRevision: 2, Decision: pb.FailureStatus_FAILURE_STATUS_APPROVED, Note: "reviewed"})); err != nil {
		t.Fatal(err)
	}
	c := content("incident")
	c.Records = nil
	created, err := svc.PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: c, Failures: []*pb.FailureSelection{{Id: f.GetId(), Revision: 3}}}))
	if err != nil {
		t.Fatal(err)
	}
	d := created.Msg.GetDataset()
	if len(d.GetContent().GetAccessSources()) != 1 || d.GetContent().GetRecords()[0].GetMetadata()["evalsi.failure"] == nil {
		t.Fatal("lost source access/provenance")
	}
	engine, err := authz.NewEngine(ctx, authz.Options{Enabled: true, RBAC: authz.RBACConfig{Roles: []authz.Role{{Name: "dataset-only", Permissions: []string{"datasets.read", "datasets.write"}}}, Projects: map[string]map[string][]string{"support": {"dataset-only": {"key:restricted"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	restricted := authz.WithChecker(as(ctx, "restricted"), &authz.Checker{Engine: engine, Principal: auth.PrincipalFrom(as(ctx, "restricted"))})
	if _, err := svc.GetDatasetVersion(restricted, connect.NewRequest(&pb.GetDatasetVersionRequest{Project: "support", Name: "refunds", Version: "incident"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("trace boundary bypass: %v", err)
	}
	listed, err := svc.ListDatasetVersions(restricted, connect.NewRequest(&pb.ListDatasetVersionsRequest{Project: "support", Name: "refunds"}))
	if err != nil || len(listed.Msg.GetDatasets()) != 1 {
		t.Fatalf("filtered catalog: %v %v", listed, err)
	}
	if _, err := svc.ReviewDatasetVersion(ctx, connect.NewRequest(review(d, pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED))); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(restricted, st, Reference(d), "support", 0); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("run source boundary bypass: %v", err)
	}
	if _, err := st.DeleteTracesBefore(ctx, timestamppb.Now().AsTime().Add(1e9)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetDatasetVersion(ctx, connect.NewRequest(&pb.GetDatasetVersionRequest{Project: "support", Name: "refunds", Version: "incident"})); err != nil {
		t.Fatal("source retention removed curated dataset", err)
	}
}
