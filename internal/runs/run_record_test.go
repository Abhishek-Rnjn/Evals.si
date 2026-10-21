package runs

import (
	"testing"

	"connectrpc.com/connect"
	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func TestRunRecordAndResultPagesKeepTrialIdentity(t *testing.T) {
	h := newHarness(t)
	run := h.create(t, &pb.RunSpec{Dataset: inline("flaky"), Target: target(), Evaluators: refs("exact-match"), Trials: 2})
	h.wait(t, run.Id)
	page, err := h.m.ListRunResults(t.Context(), connect.NewRequest(&pb.ListRunResultsRequest{RunId: run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Msg.RecordsByTrial) != 2 {
		t.Fatal(page.Msg)
	}
	for trial, want := range []string{"right", "wrong"} {
		got, err := h.m.GetRecordEvidence(t.Context(), connect.NewRequest(&pb.GetRecordEvidenceRequest{RunId: run.Id, RecordId: "r0", Trial: int32(trial)}))
		if err != nil || got.Msg.Evidence.Record.Output.GetText() != want || page.Msg.RecordsByTrial[trial].Record.Output.GetText() != want || page.Msg.RecordsByTrial[trial].Trial != int32(trial) {
			t.Fatalf("trial %d: %v %v", trial, got, err)
		}
	}
	for _, trial := range []int32{-1, 2} {
		_, err := h.m.GetRecordEvidence(t.Context(), connect.NewRequest(&pb.GetRecordEvidenceRequest{RunId: run.Id, RecordId: "r0", Trial: trial}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatal(err)
		}
	}
	_, err = h.m.GetRecordEvidence(t.Context(), connect.NewRequest(&pb.GetRecordEvidenceRequest{RunId: run.Id, RecordId: "missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatal(err)
	}
	if _, err := h.m.ArchiveRun(t.Context(), connect.NewRequest(&pb.ArchiveRunRequest{Id: run.Id})); err != nil {
		t.Fatal(err)
	}
	_, err = h.m.GetRecordEvidence(t.Context(), connect.NewRequest(&pb.GetRecordEvidenceRequest{RunId: run.Id, RecordId: "r0"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal(err)
	}
}
