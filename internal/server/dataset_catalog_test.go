package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
)

func TestDatasetCatalogPermissionsIndependentApprovalAndPinnedRuns(t *testing.T) {
	publicKey := ""
	s := startAuthServer(t, func(c *config.Config) {
		c.RBAC.Roles = append(c.RBAC.Roles, authz.Role{Name: "public-dataset-reader", Permissions: []string{"datasets.read"}, Condition: `resource.labels.classification == "public"`})
		plain, hash := auth.NewAPIKey()
		publicKey = plain
		c.Auth.APIKeys.Keys = append(c.Auth.APIKeys.Keys, auth.ConfigKey{Name: "public-dataset-reader", Key: "sha256:" + hash, Roles: map[string][]string{"support": {"public-dataset-reader"}}})
	})
	ctx := t.Context()
	client := func(key string) evalsiv1alpha1connect.DatasetCatalogServiceClient {
		return evalsiv1alpha1connect.NewDatasetCatalogServiceClient(s.http, s.url, as(key))
	}
	content := func(version, classification string) *pb.DatasetVersionContent {
		return &pb.DatasetVersionContent{Project: "support", Name: "refunds", Version: version, Owner: "support-team", Split: "held-out", Labels: map[string]string{"classification": classification}, Records: []*pb.Record{{Id: "one", Input: text("request"), Output: text("reviewed"), Reference: text("reviewed")}}}
	}
	if _, err := client(s.keys["viewer"]).PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: content("v1", "public")})); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer publication: %v", err)
	}
	published, err := client(s.keys["editor"]).PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: content("v1", "public")}))
	if err != nil {
		t.Fatal(err)
	}
	d := published.Msg.GetDataset()
	ref := published.Msg.GetReference()
	if d.GetPublishedBy() != "key:editor" || d.GetState() != pb.DatasetVersionState_DATASET_VERSION_STATE_DRAFT {
		t.Fatal(d)
	}
	if _, err := client(s.keys["editor"]).PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: content("v1", "public")})); codeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("version overwrite: %v", err)
	}
	runs := func(key string) evalsiv1alpha1connect.RunServiceClient {
		return evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(key))
	}
	spec := &pb.RunSpec{Dataset: &pb.DatasetSource{Source: &pb.DatasetSource_Catalog{Catalog: ref}}, Evaluators: []*pb.EvaluatorRef{{Ref: "exact-match"}}, Gates: []*pb.Gate{{Metric: "exact-match", Min: proto.Float64(1)}}}
	request := func(project string) *connect.Request[pb.CreateRunRequest] {
		return connect.NewRequest(&pb.CreateRunRequest{Project: project, Spec: proto.Clone(spec).(*pb.RunSpec)})
	}
	if _, err := runs(s.keys["runner"]).CreateRun(ctx, request("support")); codeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("draft run: %v", err)
	}
	review := func(dataset *pb.DatasetVersion, state pb.DatasetVersionState) *connect.Request[pb.ReviewDatasetVersionRequest] {
		c := dataset.GetContent()
		return connect.NewRequest(&pb.ReviewDatasetVersionRequest{Project: c.GetProject(), Name: c.GetName(), Version: c.GetVersion(), Sha256: dataset.GetSha256(), ExpectedRevision: dataset.GetRevision(), Decision: state, Note: "References and source checked"})
	}
	if _, err := client(s.keys["editor"]).ReviewDatasetVersion(ctx, review(d, pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED)); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("editor review: %v", err)
	}
	approved, err := client(s.keys["admin"]).ReviewDatasetVersion(ctx, review(d, pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED))
	if err != nil {
		t.Fatal(err)
	}
	d = approved.Msg.GetDataset()
	if _, err := runs(s.keys["checkout-runner"]).CreateRun(ctx, request("checkout")); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-project catalog read: %v", err)
	}
	if _, err := runs(s.keys["blind-runner"]).CreateRun(ctx, request("support")); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("run without datasets.read: %v", err)
	}
	wrong := request("support")
	wrong.Msg.Spec.Dataset.GetCatalog().Sha256 = strings.Repeat("0", 64)
	if _, err := runs(s.keys["runner"]).CreateRun(ctx, wrong); codeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("wrong pin: %v", err)
	}
	created, err := runs(s.keys["runner"]).CreateRun(ctx, request("support"))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.GetRun().GetId()
	var finished *pb.Run
	for range 100 {
		result, err := runs(s.keys["viewer"]).GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		finished = result.Msg.GetRun()
		if finished.GetStatus() == pb.RunStatus_RUN_STATUS_SUCCEEDED {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if finished.GetStatus() != pb.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatal(finished)
	}
	page, err := runs(s.keys["viewer"]).ListRunResults(ctx, connect.NewRequest(&pb.ListRunResultsRequest{RunId: id}))
	if err != nil {
		t.Fatal(err)
	}
	identity := page.Msg.GetRecords()[0].GetMetadata()["evalsi.dataset-version"].GetStructValue().AsMap()
	if identity["sha256"] != d.GetSha256() || identity["reviewed_by"] != "key:admin" {
		t.Fatal(identity)
	}
	if _, err := client(s.keys["admin"]).ReviewDatasetVersion(ctx, review(d, pb.DatasetVersionState_DATASET_VERSION_STATE_RETIRED)); err != nil {
		t.Fatal(err)
	}
	if _, err := runs(s.keys["runner"]).CreateRun(ctx, request("support")); codeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("retired run: %v", err)
	}
	if _, err := client(s.keys["editor"]).PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: content("private", "private")})); err != nil {
		t.Fatal(err)
	}
	listed, err := client(publicKey).ListDatasetVersions(ctx, connect.NewRequest(&pb.ListDatasetVersionsRequest{Project: "support"}))
	if err != nil || len(listed.Msg.GetDatasets()) != 1 || len(listed.Msg.GetDatasets()[0].GetContent().GetRecords()) != 0 {
		t.Fatalf("label-filtered list: %v %v", listed, err)
	}
	if _, err := client(publicKey).GetDatasetVersion(ctx, connect.NewRequest(&pb.GetDatasetVersionRequest{Project: "support", Name: "refunds", Version: "private"})); codeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("private version read: %v", err)
	}
	self, err := client(s.keys["owner"]).PublishDatasetVersion(ctx, connect.NewRequest(&pb.PublishDatasetVersionRequest{Content: content("self", "public")}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client(s.keys["owner"]).ReviewDatasetVersion(ctx, review(self.Msg.GetDataset(), pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED)); codeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("owner self approval: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, s.url+"/v1alpha1/dataset-versions/refunds/v1?project=support", nil)
	req.Header.Set("Authorization", "Bearer "+s.keys["viewer"])
	response, err := s.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(string(raw), d.GetSha256()) {
		t.Fatalf("REST catalog: %d %s", response.StatusCode, raw)
	}
}
