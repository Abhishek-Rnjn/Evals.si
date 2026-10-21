package runs

import (
	"connectrpc.com/connect"
	"context"
	"errors"
	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func (m *Manager) GetRecordEvidence(ctx context.Context, req *connect.Request[pb.GetRecordEvidenceRequest]) (*connect.Response[pb.GetRecordEvidenceResponse], error) {
	run, err := m.get(ctx, req.Msg.GetRunId())
	if err != nil {
		return nil, err
	}
	if err := errArchived(run); err != nil {
		return nil, err
	}
	trials := max(1, int(run.GetSpec().GetTrials()))
	if req.Msg.GetRecordId() == "" || req.Msg.GetTrial() < 0 || int(req.Msg.GetTrial()) >= trials {
		return nil, invalid("record_id is required and trial must be within the run's trials")
	}
	out, err := m.store.RunRecord(ctx, run.GetId(), req.Msg.GetRecordId(), int(req.Msg.GetTrial()))
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}
