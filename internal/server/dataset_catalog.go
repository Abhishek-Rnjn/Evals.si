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

func (g *gate) datasetVersionTargets(ctx context.Context, project, name, version string) ([]target, error) {
	project, err := g.project(project)
	if err != nil {
		return nil, err
	}
	d, err := g.store.GetDatasetVersion(ctx, project, name, version)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		if errors.Is(err, store.ErrDatasetVersionIntegrity) {
			return nil, connect.NewError(connect.CodeDataLoss, err)
		}
		return nil, err
	}
	c := d.GetContent()
	if c == nil {
		c = &pb.DatasetVersionContent{Project: project, Name: name, Version: version}
	}
	out := []target{{project: project, resource: authz.DatasetVersionResource(c), name: "dataset-version/" + name + "/" + version}}
	for _, source := range c.GetAccessSources() {
		out = append(out, target{project: source.GetProject(), resource: authz.TraceResource(source), name: "trace/" + source.GetTraceId(), action: "traces.read"})
	}
	return out, nil
}

func (g *gate) addDatasetCatalogRules(rules map[string]accessRule) {
	rules[evalsiv1alpha1connect.DatasetCatalogServicePublishDatasetVersionProcedure] = accessRule{action: "datasets.write", resolve: func(ctx context.Context, msg any) ([]target, error) {
		m := msg.(*pb.PublishDatasetVersionRequest)
		c := m.GetContent()
		project, err := g.project(c.GetProject())
		if err != nil {
			return nil, err
		}
		resource := authz.DatasetVersionResource(c)
		out := []target{{project: project, resource: resource, name: "dataset-version/" + c.GetName() + "/" + c.GetVersion()}, {project: project, resource: resource, name: "dataset-version/" + c.GetName() + "/" + c.GetVersion(), action: "datasets.read"}}
		if len(m.GetFailures()) > 100 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("at most 100 failure selections may be published"))
		}
		for _, selection := range m.GetFailures() {
			targets, err := g.failureTargets(ctx, project, selection.GetId())
			if err != nil {
				return nil, err
			}
			out = append(out, targets...)
		}
		return out, nil
	}}
	rules[evalsiv1alpha1connect.DatasetCatalogServiceListDatasetVersionsProcedure] = accessRule{action: "datasets.read", filtered: true}
	type identified interface {
		GetProject() string
		GetName() string
		GetVersion() string
	}
	for procedure, action := range map[string]string{
		evalsiv1alpha1connect.DatasetCatalogServiceGetDatasetVersionProcedure:        "datasets.read",
		evalsiv1alpha1connect.DatasetCatalogServiceListDatasetVersionEventsProcedure: "datasets.read",
		evalsiv1alpha1connect.DatasetCatalogServiceReviewDatasetVersionProcedure:     "datasets.review",
	} {
		rules[procedure] = accessRule{action: action, resolve: func(ctx context.Context, msg any) ([]target, error) {
			m := msg.(identified)
			targets, err := g.datasetVersionTargets(ctx, m.GetProject(), m.GetName(), m.GetVersion())
			if err != nil {
				return nil, err
			}
			// Review responses contain records, so review also needs read access.
			read := targets[0]
			read.action = "datasets.read"
			return append(targets, read), nil
		}}
	}
	rules[evalsiv1alpha1connect.DatasetCatalogServiceDiffDatasetVersionsProcedure] = accessRule{action: "datasets.read", resolve: func(ctx context.Context, msg any) ([]target, error) {
		m := msg.(*pb.DiffDatasetVersionsRequest)
		left, err := g.datasetVersionTargets(ctx, m.GetProject(), m.GetName(), m.GetBeforeVersion())
		if err != nil {
			return nil, err
		}
		right, err := g.datasetVersionTargets(ctx, m.GetProject(), m.GetName(), m.GetAfterVersion())
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil
	}}
}
