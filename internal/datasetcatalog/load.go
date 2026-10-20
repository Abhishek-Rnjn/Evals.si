package datasetcatalog

import (
	"context"
	"errors"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Load verifies both immutable content and the external approved pin before a
// run is admitted. The run manager stores the returned records independently.
func Load(ctx context.Context, st *store.Store, ref *pb.DatasetVersionRef, project string, limit int) ([]*pb.Record, error) {
	if ref.GetProject() != "" {
		project = ref.GetProject()
	}
	if err := validatePin(ref.GetSha256()); err != nil {
		return nil, err
	}
	d, err := New(st).get(ctx, project, ref.GetName(), ref.GetVersion())
	if err != nil {
		return nil, err
	}
	if d.GetSha256() != ref.GetSha256() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("dataset catalog SHA-256 mismatch"))
	}
	if d.GetState() != pb.DatasetVersionState_DATASET_VERSION_STATE_APPROVED {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("dataset version must be approved for new runs"))
	}
	c := d.GetContent()
	count := len(c.GetRecords())
	if limit > 0 && limit < count {
		count = limit
	}
	identity, err := structpb.NewValue(map[string]any{"schema": "evalsi.dataset-catalog/v1", "project": c.GetProject(), "name": c.GetName(), "version": c.GetVersion(), "sha256": d.GetSha256(), "owner": c.GetOwner(), "split": c.GetSplit(), "reviewed_by": d.GetReviewedBy(), "reviewed_at": d.GetReviewedAt().AsTime().Format(time.RFC3339Nano), "revision": strconv.FormatInt(d.GetRevision(), 10)})
	if err != nil {
		return nil, err
	}
	out := make([]*pb.Record, 0, count)
	for _, original := range c.GetRecords()[:count] {
		record := proto.Clone(original).(*pb.Record)
		if record.Metadata == nil {
			record.Metadata = map[string]*structpb.Value{}
		}
		record.Metadata[IdentityKey] = proto.Clone(identity).(*structpb.Value)
		out = append(out, record)
	}
	return out, nil
}
