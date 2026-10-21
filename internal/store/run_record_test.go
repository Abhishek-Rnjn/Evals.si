package store

import (
	"errors"
	"fmt"
	"testing"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"google.golang.org/protobuf/proto"
)

func TestRunRecordReadsExactTrialBeyondResultPages(t *testing.T) {
	s, ctx := open(t), t.Context()
	records := make([]*pb.Record, 601)
	for i := range records {
		records[i] = &pb.Record{Id: fmt.Sprintf("case/%d", i)}
	}
	if err := s.CreateRun(ctx, run("run", "support", 1), records); err != nil {
		t.Fatal(err)
	}
	for trial := range 2 {
		r := proto.Clone(records[600]).(*pb.Record)
		r.Output = &pb.Content{Kind: &pb.Content_Text{Text: fmt.Sprintf("trial-%d", trial)}}
		r.Trajectory = &pb.Trajectory{Steps: []*pb.Step{{Name: fmt.Sprintf("tool-%d", trial)}}}
		if err := s.PutOutputs(ctx, "run", []Output{{RecordIdx: 600, Trial: trial, Record: r}}); err != nil {
			t.Fatal(err)
		}
		if err := s.PutResults(ctx, "run", []Result{{RecordIdx: 600, Trial: trial, Result: &pb.EvaluationResult{RecordId: r.Id, Trial: int32(trial), Evaluator: "quality", Outcome: pb.Outcome_OUTCOME_SCORED}}}); err != nil {
			t.Fatal(err)
		}
	}
	// Decoding every output would fail here. A selected case must read only
	// its output and scores, irrespective of their position in list pages.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO run_outputs (run_id, record_idx, trial, record, error) VALUES (?, ?, ?, ?, ?)`, "run", 0, 0, []byte{0xff}, ""); err != nil {
		t.Fatal(err)
	}
	for trial := range 2 {
		got, err := s.RunRecord(ctx, "run", "case/600", trial)
		if err != nil {
			t.Fatal(err)
		}
		if got.Evidence.Record.Output.GetText() != fmt.Sprintf("trial-%d", trial) || got.Evidence.Record.Trajectory.Steps[0].Name != fmt.Sprintf("tool-%d", trial) || !got.Evidence.OutputStored || len(got.Results) != 1 || got.Results[0].Trial != int32(trial) {
			t.Fatal(got)
		}
	}
	got, err := s.RunRecord(ctx, "run", "case/599", 0)
	if err != nil || got.Evidence.OutputStored || len(got.Results) != 0 {
		t.Fatalf("snapshot: %v %v", got, err)
	}
	if _, err := s.RunRecord(ctx, "run", "missing", 0); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
