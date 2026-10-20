package authz

import (
	"reflect"
	"strings"
	"testing"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestRunAccessSnapshotPreservesAuthorizationWithoutPayload(t *testing.T) {
	params, _ := structpb.NewStruct(map[string]any{"rubric": "private-payload-528"})
	for _, agent := range []*pb.AgentTarget{nil, {Kind: &pb.AgentTarget_Http{Http: &pb.HTTPAgent{Url: "https://private-payload-528/agent"}}}, {Kind: &pb.AgentTarget_Cli{Cli: &pb.CLIAgent{Command: []string{"private-payload-528"}}}}} {
		run := &pb.Run{Id: "run-1", Name: "release-run", Project: "support", Status: pb.RunStatus_RUN_STATUS_SUCCEEDED, CreatedBy: "key:ci", Labels: map[string]string{"team": "support"}, Spec: &pb.RunSpec{
			Dataset:    &pb.DatasetSource{Source: &pb.DatasetSource_Catalog{Catalog: &pb.DatasetVersionRef{Project: "support", Name: "suite", Version: "v1", Sha256: strings.Repeat("a", 64)}}},
			Target:     &pb.Target{Connector: "openai-compatible", Model: "m", BaseUrl: "https://agent.example.com", SystemPrompt: "private-payload-528", Agent: agent},
			Evaluators: []*pb.EvaluatorRef{{Ref: "judge", Params: params}}, Judge: "shared-judge", Trials: 3, Budget: &pb.Budget{MaxTargetTokens: 123, MaxJudgeTokens: 456},
			Environment: &pb.Environment{Image: "agent:1", Files: map[string]string{"secret.txt": "private-payload-528"}, Env: map[string]string{"SECRET": "private-payload-528"}, Sandbox: &pb.SandboxPolicy{Network: "allowlist", MinIsolation: "namespaced"}},
		}}
		for _, harness := range []*pb.Harness{nil, {Kind: &pb.Harness_External{External: &pb.ExternalHarness{Kind: &pb.ExternalHarness_Command{Command: &pb.CommandHarness{Argv: []string{"private-payload-528"}}}}}}} {
			run.Spec.Harness = harness
			snapshot := RunAccessSnapshot(run)
			runsCode := func([]*pb.EvaluatorRef) bool { return true }
			if !reflect.DeepEqual(RunResource(snapshot, runsCode), RunResource(run, runsCode)) {
				t.Fatalf("snapshot changed resource: %v", snapshot)
			}
			data, err := proto.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "private-payload-528") {
				t.Fatal("snapshot retained private configuration")
			}
			snapshot.Labels["team"] = "changed"
			if run.Labels["team"] != "support" {
				t.Fatal("snapshot aliases source labels")
			}
		}
	}
}

func TestDatasetAccessSnapshotPreservesTraceBoundariesWithoutRecords(t *testing.T) {
	content := &pb.DatasetVersionContent{Project: "support", Name: "suite", Version: "v1", Owner: "team", Split: "held-out", Tags: []string{"refunds"}, Labels: map[string]string{"classification": "private"}, Description: "private-payload-528", Records: []*pb.Record{{Id: "case", Input: &pb.Content{Kind: &pb.Content_Text{Text: "private-payload-528"}}}}, AccessSources: []*pb.TraceSummary{{Project: "support", TraceId: "trace", Service: "refund-agent", Name: "refund", Labels: map[string]string{"tenant": "enterprise"}}}}
	snapshot := DatasetAccessSnapshot(content)
	if !reflect.DeepEqual(DatasetVersionResource(snapshot), DatasetVersionResource(content)) || !reflect.DeepEqual(TraceResource(snapshot.AccessSources[0]), TraceResource(content.AccessSources[0])) {
		t.Fatal("snapshot changed dataset/source permissions")
	}
	data, err := proto.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-payload-528") {
		t.Fatal("snapshot retained dataset payload")
	}
}
