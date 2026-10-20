package authz

import (
	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"google.golang.org/protobuf/proto"
)

// RunAccessSnapshot retains exactly the data RunResource needs, without source
// prompts, evaluator parameter values, environment files/variables or results.
// Keep this in step with RunResource when adding authorization attributes.
func RunAccessSnapshot(run *pb.Run) *pb.Run {
	spec := run.GetSpec()
	target := spec.GetTarget()
	env := spec.GetEnvironment()
	snapshot := &pb.Run{Id: run.GetId(), Name: run.GetName(), Project: run.GetProject(), Status: run.GetStatus(), CreatedBy: run.GetCreatedBy(), Labels: run.GetLabels(), Spec: &pb.RunSpec{
		Target: &pb.Target{Connector: target.GetConnector(), Model: target.GetModel(), BaseUrl: target.GetBaseUrl()}, Judge: spec.GetJudge(), Trials: spec.GetTrials(),
		Budget:      &pb.Budget{MaxTargetTokens: spec.GetBudget().GetMaxTargetTokens(), MaxJudgeTokens: spec.GetBudget().GetMaxJudgeTokens()},
		Environment: &pb.Environment{Image: env.GetImage(), Sandbox: &pb.SandboxPolicy{Network: env.GetSandbox().GetNetwork(), MinIsolation: env.GetSandbox().GetMinIsolation()}},
	}}
	for _, ref := range spec.GetEvaluators() {
		snapshot.Spec.Evaluators = append(snapshot.Spec.Evaluators, &pb.EvaluatorRef{Ref: ref.GetRef()})
	}
	if agent := target.GetAgent(); agent != nil {
		snapshot.Spec.Target.Agent = &pb.AgentTarget{}
		copyKind(agent, snapshot.Spec.Target.Agent)
	}
	if harness := spec.GetHarness(); harness != nil {
		snapshot.Spec.Harness = &pb.Harness{}
		copyKind(harness, snapshot.Spec.Harness)
	}
	ds := spec.GetDataset()
	snapshot.Spec.Dataset = &pb.DatasetSource{}
	switch source := ds.GetSource().(type) {
	case *pb.DatasetSource_Path:
		snapshot.Spec.Dataset.Source = &pb.DatasetSource_Path{Path: source.Path}
	case *pb.DatasetSource_Uri:
		snapshot.Spec.Dataset.Source = &pb.DatasetSource_Uri{Uri: source.Uri}
	case *pb.DatasetSource_Inline:
		snapshot.Spec.Dataset.Source = &pb.DatasetSource_Inline{Inline: &pb.InlineRecords{}}
	case *pb.DatasetSource_Catalog:
		snapshot.Spec.Dataset.Source = &pb.DatasetSource_Catalog{Catalog: source.Catalog}
	}
	// Deep-copy retained maps/references so subsequent source changes cannot alter
	// the snapshot before it is serialized.
	return proto.Clone(snapshot).(*pb.Run)
}
func copyKind(from, to proto.Message) {
	source := from.ProtoReflect()
	field := source.WhichOneof(source.Descriptor().Oneofs().ByName("kind"))
	if field != nil {
		to.ProtoReflect().Mutable(field)
	} // Preserve the variant, not its configuration.
}

// DatasetAccessSnapshot keeps immutable dataset/source authorization attributes
// without record content or descriptive payloads.
func DatasetAccessSnapshot(content *pb.DatasetVersionContent) *pb.DatasetVersionContent {
	snapshot := &pb.DatasetVersionContent{Project: content.GetProject(), Name: content.GetName(), Version: content.GetVersion(), Owner: content.GetOwner(), Split: content.GetSplit(), Tags: content.GetTags(), Labels: content.GetLabels()}
	for _, source := range content.GetAccessSources() {
		snapshot.AccessSources = append(snapshot.AccessSources, &pb.TraceSummary{Project: source.GetProject(), TraceId: source.GetTraceId(), Name: source.GetName(), Service: source.GetService(), Labels: source.GetLabels()})
	}
	return proto.Clone(snapshot).(*pb.DatasetVersionContent)
}
