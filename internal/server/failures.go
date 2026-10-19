package server

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func (g *gate) failureTargets(ctx context.Context, project, id string) ([]target, error) {
	project, err := g.project(project)
	if err != nil {
		return nil, err
	}
	f, err := g.store.GetFailure(ctx, project, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	resource := authz.TraceResource(f.GetSource())
	return []target{
		{project: project, resource: resource, name: "failure/" + id},
		{project: project, resource: resource, name: "failure/" + id, action: "failures.read"},
		{project: project, resource: resource, name: "trace/" + f.GetSource().GetTraceId(), action: "traces.read"},
	}, nil
}

func (g *gate) addFailureRules(rules map[string]accessRule) {
	rules[evalsiv1alpha1connect.FailureServiceCaptureFailureProcedure] = accessRule{action: "failures.write", resolve: func(ctx context.Context, msg any) ([]target, error) {
		m := msg.(*pb.CaptureFailureRequest)
		project, err := g.project(m.GetProject())
		if err != nil {
			return nil, err
		}
		source, _, err := g.store.GetTrace(ctx, project, strings.ToLower(m.GetTraceId()))
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		resource := authz.TraceResource(source)
		return []target{
			{project: project, resource: resource, name: "failure/new"},
			{project: project, resource: resource, name: "trace/" + m.GetTraceId(), action: "traces.read"},
			{project: project, resource: resource, name: "failure/new", action: "failures.read"},
		}, nil
	}}
	rules[evalsiv1alpha1connect.FailureServiceListFailuresProcedure] = accessRule{action: "failures.read", filtered: true}
	type identified interface {
		GetProject() string
		GetId() string
	}
	for procedure, action := range map[string]string{
		evalsiv1alpha1connect.FailureServiceGetFailureProcedure:        "failures.read",
		evalsiv1alpha1connect.FailureServiceListFailureEventsProcedure: "failures.read",
		evalsiv1alpha1connect.FailureServiceUpdateFailureProcedure:     "failures.write",
		evalsiv1alpha1connect.FailureServiceReviewFailureProcedure:     "failures.review",
		evalsiv1alpha1connect.FailureServiceDeleteFailureProcedure:     "failures.delete",
	} {
		rules[procedure] = accessRule{action: action, resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(identified)
			return g.failureTargets(ctx, m.GetProject(), m.GetId())
		}}
	}
	rules[evalsiv1alpha1connect.FailureServiceExportFailuresProcedure] = accessRule{action: "datasets.write", resolve: func(ctx context.Context, msg any) ([]target, error) {
		m := msg.(*pb.ExportFailuresRequest)
		if len(m.GetSelections()) < 1 || len(m.GetSelections()) > 100 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("export requires 1-100 explicit selections"))
		}
		var targets []target
		for _, selection := range m.GetSelections() {
			ts, err := g.failureTargets(ctx, m.GetProject(), selection.GetId())
			if err != nil {
				return nil, err
			}
			targets = append(targets, ts...)
		}
		return targets, nil
	}}
}
