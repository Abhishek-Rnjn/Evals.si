package runs

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/datasetcatalog"
)

func TestCatalogDatasetPinAndReviewFailBeforeExecution(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	service := datasetcatalog.New(h.st)
	created, err := service.PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: &pb.DatasetVersionContent{Project: "support", Name: "suite", Version: "v1", Owner: "support", Split: "held-out", Records: []*pb.Record{{Id: "one", Input: text("easy"), Reference: text("right")}}}}))
	if err != nil {
		t.Fatal(err)
	}
	d := created.Msg.GetDataset()
	ref := created.Msg.GetReference()
	spec := &pb.RunSpec{Target: target(), Dataset: &pb.DatasetSource{Source: &pb.DatasetSource_Catalog{Catalog: ref}}, Evaluators: refs("exact-match")}
	start := func(s *pb.RunSpec) (*connect.Response[pb.CreateRunResponse], error) {
		return h.m.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{Project: "support", Spec: s}))
	}
	if _, err := start(spec); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("draft execution: %v", err)
	}
	if h.worker.generateCalls.Load() != 0 || h.worker.evaluateCalls.Load() != 0 {
		t.Fatal("draft reached target/evaluator")
	}
	if _, err := service.ReviewDatasetVersion(ctx, connect.NewRequest(&pb.ReviewDatasetVersionRequest{Project: "support", Name: "suite", Version: "v1", Sha256: d.GetSha256(), ExpectedRevision: 1, Decision: pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED, Note: "reviewed"})); err != nil {
		t.Fatal(err)
	}
	bad := proto.Clone(spec).(*pb.RunSpec)
	bad.Dataset.GetCatalog().Sha256 = strings.Repeat("0", 64)
	if _, err := start(bad); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("wrong digest execution: %v", err)
	}
	bad = proto.Clone(spec).(*pb.RunSpec)
	bad.Dataset.Mapping = map[string]string{"metadata": "attributes"}
	if _, err := start(bad); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("mapping execution: %v", err)
	}
	if h.worker.generateCalls.Load() != 0 || h.worker.evaluateCalls.Load() != 0 {
		t.Fatal("invalid catalog reference reached execution")
	}
	run, err := start(spec)
	if err != nil {
		t.Fatal(err)
	}
	finished := h.wait(t, run.Msg.GetRun().GetId())
	if finished.GetStatus() != pb.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatal(finished)
	}
	if _, err := service.ReviewDatasetVersion(ctx, connect.NewRequest(&pb.ReviewDatasetVersionRequest{Project: "support", Name: "suite", Version: "v1", Sha256: d.GetSha256(), ExpectedRevision: 2, Decision: pb.DatasetVersionState_DATASET_VERSION_STATE_RETIRED, Note: "retire after execution"})); err != nil {
		t.Fatal(err)
	}
	if _, err := start(spec); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("retired execution: %v", err)
	}
	page, err := h.m.ListRunResults(ctx, connect.NewRequest(&pb.ListRunResultsRequest{RunId: finished.GetId()}))
	if err != nil || len(page.Msg.GetRecords()) != 1 {
		t.Fatalf("admitted record snapshot lost: %v %v", page, err)
	}
	identity := page.Msg.GetRecords()[0].GetMetadata()[datasetcatalog.IdentityKey].GetStructValue().AsMap()
	if identity["sha256"] != d.GetSha256() {
		t.Fatal("stored run lost approved dataset identity")
	}
	if _, err := h.m.ImportRun(ctx, connect.NewRequest(&pb.ImportRunRequest{Project: "support", Spec: spec})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("import claimed catalog execution: %v", err)
	}
}
