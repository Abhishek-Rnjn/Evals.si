package store

import (
	"context"
	"database/sql"
	"errors"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"google.golang.org/protobuf/proto"
)

// RunRecord reads one output and its evaluations through their primary keys.
// The legacy snapshot schema has no record-ID index: walk it one row at a
// time to locate the index, without loading the run's other outputs/results.
// This also works for snapshots written by the previous release.
func (s *Store) RunRecord(ctx context.Context, runID, recordID string, trial int) (*pb.GetRecordEvidenceResponse, error) {
	idx, record, err := s.findRunRecord(ctx, runID, recordID)
	if err != nil {
		return nil, err
	}
	e := &pb.RunRecordEvidence{RecordId: recordID, Trial: int32(trial), Record: record}
	var blob []byte
	err = s.db.QueryRowContext(ctx, `SELECT record, error FROM run_outputs WHERE run_id = ? AND record_idx = ? AND trial = ?`, runID, idx, trial).Scan(&blob, &e.OutputError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	e.OutputStored = err == nil
	if len(blob) > 0 {
		e.Record = &pb.Record{}
		if err := proto.Unmarshal(blob, e.Record); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT result FROM run_results WHERE run_id = ? AND record_idx = ? AND trial = ? ORDER BY eval_idx`, runID, idx, trial)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &pb.GetRecordEvidenceResponse{Evidence: e}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		r := &pb.EvaluationResult{}
		if err := proto.Unmarshal(b, r); err != nil {
			return nil, err
		}
		out.Results = append(out.Results, r)
	}
	return out, rows.Err()
}

func (s *Store) findRunRecord(ctx context.Context, runID, recordID string) (int, *pb.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT idx, record FROM run_records WHERE run_id = ? ORDER BY idx`, runID)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var idx int
		var blob []byte
		if err := rows.Scan(&idx, &blob); err != nil {
			return 0, nil, err
		}
		r := &pb.Record{}
		if err := proto.Unmarshal(blob, r); err != nil {
			return 0, nil, err
		}
		if r.GetId() == recordID {
			return idx, r, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return 0, nil, ErrNotFound
}
