package pluginhost

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	pluginv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/plugin/v1alpha1"
	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
)

// WithObjects wraps a worker so dataset sources on object storage (a path
// s3://bucket/key, or an importer URI scheme://s3://bucket/key?options) are
// fetched into cacheDir first, and the worker reads local files as always.
// It runs next to the Python worker: in evalsid itself, or in each worker
// pod.
func WithObjects(w Worker, objects *objstore.Client, cacheDir string) Worker {
	if objects == nil {
		return w
	}
	return &objectWorker{Worker: w, objects: objects, cacheDir: cacheDir}
}

type objectWorker struct {
	Worker
	objects  *objstore.Client
	cacheDir string
}

func (o *objectWorker) fetch(ctx context.Context, u string) (string, error) {
	loc, ok := objstore.Parse(u)
	if !ok {
		return u, nil
	}
	local, err := o.objects.Fetch(ctx, loc, o.cacheDir)
	if err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("dataset %s: %w", loc, err))
	}
	return local, nil
}

func (o *objectWorker) LoadDataset(ctx context.Context, req *pluginv1alpha1.LoadDatasetRequest) ([]*evalsiv1alpha1.Record, error) {
	src := req.GetSource()
	switch s := src.GetSource().(type) {
	case *evalsiv1alpha1.DatasetSource_Path:
		local, err := o.fetch(ctx, s.Path)
		if err != nil {
			return nil, err
		}
		if local != s.Path {
			req = proto.Clone(req).(*pluginv1alpha1.LoadDatasetRequest)
			req.Source.Source = &evalsiv1alpha1.DatasetSource_Path{Path: local}
		}
	case *evalsiv1alpha1.DatasetSource_Uri:
		scheme, rest, ok := strings.Cut(s.Uri, "://")
		if ok && strings.HasPrefix(rest, "s3://") {
			target, query, hasQuery := strings.Cut(rest, "?")
			local, err := o.fetch(ctx, target)
			if err != nil {
				return nil, err
			}
			uri := scheme + "://" + local
			if hasQuery {
				uri += "?" + query
			}
			req = proto.Clone(req).(*pluginv1alpha1.LoadDatasetRequest)
			req.Source.Source = &evalsiv1alpha1.DatasetSource_Uri{Uri: uri}
		}
	}
	return o.Worker.LoadDataset(ctx, req)
}
