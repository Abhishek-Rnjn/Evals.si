package datasetcatalog

import (
	"context"
	"slices"
	"sort"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func changedFields(a, b proto.Message, skip string) []string {
	left, right := a.ProtoReflect(), b.ProtoReflect()
	fields := left.Descriptor().Fields()
	var out []string
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if string(field.Name()) == skip {
			continue
		}
		x, y := left.New(), right.New()
		if left.Has(field) {
			x.Set(field, left.Get(field))
		}
		if right.Has(field) {
			y.Set(field, right.Get(field))
		}
		if !proto.Equal(x.Interface(), y.Interface()) {
			out = append(out, string(field.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func (s *Service) DiffDatasetVersions(ctx context.Context, req *connect.Request[pb.DiffDatasetVersionsRequest]) (*connect.Response[pb.DiffDatasetVersionsResponse], error) {
	m := req.Msg
	if err := validatePin(m.GetBeforeSha256()); err != nil {
		return nil, err
	}
	if err := validatePin(m.GetAfterSha256()); err != nil {
		return nil, err
	}
	a, err := s.get(ctx, m.GetProject(), m.GetName(), m.GetBeforeVersion())
	if err != nil {
		return nil, err
	}
	b, err := s.get(ctx, m.GetProject(), m.GetName(), m.GetAfterVersion())
	if err != nil {
		return nil, err
	}
	if a.GetSha256() != m.GetBeforeSha256() || b.GetSha256() != m.GetAfterSha256() {
		return nil, invalid("diff SHA-256 does not match the selected versions")
	}
	before, after := map[string]*pb.Record{}, map[string]*pb.Record{}
	var aOrder, bOrder []string
	for _, r := range a.GetContent().GetRecords() {
		before[r.GetId()] = r
		aOrder = append(aOrder, r.GetId())
	}
	for _, r := range b.GetContent().GetRecords() {
		after[r.GetId()] = r
		bOrder = append(bOrder, r.GetId())
	}
	out := &pb.DiffDatasetVersionsResponse{BeforeSha256: a.GetSha256(), AfterSha256: b.GetSha256(), OrderChanged: !slices.Equal(aOrder, bOrder), MetadataChanged: changedFields(a.GetContent(), b.GetContent(), "records")}
	for id, r := range before {
		other, ok := after[id]
		if !ok {
			out.Removed = append(out.Removed, id)
			continue
		}
		if proto.Equal(r, other) {
			out.Unchanged++
		} else {
			out.Changed = append(out.Changed, &pb.DatasetRecordChange{Id: id, Fields: changedFields(r, other, "")})
		}
	}
	for id := range after {
		if _, ok := before[id]; !ok {
			out.Added = append(out.Added, id)
		}
	}
	sort.Strings(out.Added)
	sort.Strings(out.Removed)
	slices.SortFunc(out.Changed, func(a, b *pb.DatasetRecordChange) int {
		if a.GetId() < b.GetId() {
			return -1
		}
		if a.GetId() > b.GetId() {
			return 1
		}
		return 0
	})
	return connect.NewResponse(out), nil
}
