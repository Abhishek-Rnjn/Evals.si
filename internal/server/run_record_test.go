package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
)

func TestRunRecordUsesRunPermissionsAcrossTransportsAndRevocation(t *testing.T) {
	s := startAuthServer(t, nil)
	spec := runSpec("unused")
	spec.Target = nil
	spec.Dataset.GetInline().Records[0].Id = "private/case"
	spec.Dataset.GetInline().Records[0].Output = text("Paris")
	runner := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, as(s.keys["runner"]))
	created, err := runner.CreateRun(t.Context(), connect.NewRequest(&pb.CreateRunRequest{Project: "support", Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.Run.Id
	admin := evalsiv1alpha1connect.NewAuthServiceClient(s.http, s.url, as(s.keys["owner"]))
	key, err := admin.CreateAPIKey(t.Context(), connect.NewRequest(&pb.CreateAPIKeyRequest{Name: "case-reader", Roles: map[string]*pb.RoleList{"support": {Roles: []string{"viewer"}}}}))
	if err != nil {
		t.Fatal(err)
	}
	viewerKey := key.Msg.Secret
	for _, opts := range [][]connect.ClientOption{nil, {connect.WithGRPC()}} {
		viewer := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, append(opts, as(viewerKey))...)
		got, err := viewer.GetRecordEvidence(t.Context(), connect.NewRequest(&pb.GetRecordEvidenceRequest{RunId: id, RecordId: "private/case"}))
		if err != nil || got.Msg.Evidence.Record.Id != "private/case" {
			t.Fatalf("allowed: %v %v", got, err)
		}
		other := evalsiv1alpha1connect.NewRunServiceClient(s.http, s.url, append(opts, as(s.keys["checkout-viewer"]))...)
		if _, err := other.GetRecordEvidence(t.Context(), connect.NewRequest(&pb.GetRecordEvidenceRequest{RunId: id, RecordId: "private/case"})); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("cross-project: %v", err)
		}
	}
	rest := func(key string) int {
		req, _ := http.NewRequestWithContext(t.Context(), "GET", s.url+"/v1alpha1/runs/"+id+"/record?record_id=private%2Fcase", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		response, err := s.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != 200 && body["evidence"] != nil {
			t.Fatal("denied response disclosed evidence")
		}
		return response.StatusCode
	}
	if rest(viewerKey) != 200 || rest(s.keys["checkout-viewer"]) != 403 {
		t.Fatal("REST permissions")
	}
	if _, err := admin.RevokeAPIKey(t.Context(), connect.NewRequest(&pb.RevokeAPIKeyRequest{Name: "case-reader"})); err != nil {
		t.Fatal(err)
	}
	if rest(viewerKey) != 401 {
		t.Fatal("revoked viewer can still read evidence")
	}
}
