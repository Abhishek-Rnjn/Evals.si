package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func releaseTargets(d *pb.StoredReleaseDecision) []target {
	decision := d.GetDecision()
	out := []target{{project: decision.GetProject(), resource: authz.CapturedRunResource(d.GetCandidateAccess(), d.GetCandidateAttributes()), name: "release/" + decision.GetId()}}
	for i, run := range []*pb.Run{d.GetBaselineAccess(), d.GetCandidateAccess()} {
		attributes := []*pb.ReleaseRunAccessAttributes{d.GetBaselineAttributes(), d.GetCandidateAttributes()}[i]
		out = append(out, target{project: run.GetProject(), resource: authz.CapturedRunResource(run, attributes), name: "run/" + run.GetId(), action: "runs.read"})
	}
	c := d.GetDatasetAccess()
	out = append(out, target{project: c.GetProject(), resource: authz.DatasetVersionResource(c), name: "dataset-version/" + c.GetName() + "/" + c.GetVersion(), action: "datasets.read"})
	for _, source := range c.GetAccessSources() {
		out = append(out, target{project: source.GetProject(), resource: authz.TraceResource(source), name: "trace/" + source.GetTraceId(), action: "traces.read"})
	}
	return out
}
func (g *gate) addReleaseRules(rules map[string]accessRule) {
	rules[evalsiv1alpha1connect.ReleaseServiceInspectReleasePolicyProcedure] = accessRule{action: "datasets.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
		ref := msg.(*pb.InspectReleasePolicyRequest).GetPolicy().GetDataset()
		return g.datasetVersionTargets(ctx, ref.GetProject(), ref.GetName(), ref.GetVersion())
	}}

	rules[evalsiv1alpha1connect.ReleaseServiceGetReleaseRunEvidenceProcedure] = accessRule{action: "runs.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
		id := msg.(*pb.GetReleaseRunEvidenceRequest).GetRunId()
		targets, err := g.runTargets(ctx, id)
		if err != nil {
			return nil, err
		}
		run, err := g.store.GetRun(ctx, id)
		if err != nil {
			return targets, nil
		}
		if ref := run.GetSpec().GetDataset().GetCatalog(); ref != nil {
			project := ref.GetProject()
			if project == "" {
				project = run.GetProject()
			}
			extra, err := g.datasetVersionTargets(ctx, project, ref.GetName(), ref.GetVersion())
			if err != nil {
				return nil, err
			}
			extra[0].action = "datasets.read"
			targets = append(targets, extra...)
		}
		return targets, nil
	}}
	rules[evalsiv1alpha1connect.ReleaseServiceCreateReleaseDecisionProcedure] = accessRule{action: "releases.create", resolve: func(ctx context.Context, msg any) ([]target, error) {
		r := msg.(*pb.CreateReleaseDecisionRequest)
		run, err := g.store.GetRun(ctx, r.GetCandidate().GetRunId())
		if err != nil {
			return nil, err
		}
		primary, err := g.runTargets(ctx, run.GetId())
		if err != nil {
			return nil, err
		}
		out := primary
		readable := primary[0]
		readable.action = "releases.read"
		out = append(out, readable)
		for _, id := range []string{r.GetBaseline().GetRunId(), run.GetId()} {
			targets, err := g.runTargets(ctx, id)
			if err != nil {
				return nil, err
			}
			for i := range targets {
				targets[i].action = "runs.read"
			}
			out = append(out, targets...)
		}
		ref := r.GetPolicy().GetDataset()
		targets, err := g.datasetVersionTargets(ctx, ref.GetProject(), ref.GetName(), ref.GetVersion())
		if err != nil {
			return nil, err
		}
		targets[0].action = "datasets.read"
		return append(out, targets...), nil
	}}
	rules[evalsiv1alpha1connect.ReleaseServiceGetReleaseDecisionProcedure] = accessRule{action: "releases.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
		r := msg.(*pb.GetReleaseDecisionRequest)
		project, err := g.project(r.GetProject())
		if err != nil {
			return nil, err
		}
		d, err := g.store.GetReleaseDecision(ctx, project, r.GetId())
		if errors.Is(err, store.ErrNotFound) {
			return []target{{project: project, name: "release/" + r.GetId()}}, nil
		}
		if errors.Is(err, store.ErrReleaseIntegrity) {
			return nil, connect.NewError(connect.CodeDataLoss, err)
		}
		if err != nil {
			return nil, err
		}
		if d.GetBaselineAttributes() == nil || d.GetCandidateAttributes() == nil {
			return nil, connect.NewError(connect.CodeDataLoss, errors.New("release decision lacks captured authorization attributes"))
		}
		targets := releaseTargets(d)
		return targets, nil
	}}
	rules[evalsiv1alpha1connect.ReleaseServiceListReleaseDecisionsProcedure] = accessRule{action: "releases.read", filtered: true}
}
