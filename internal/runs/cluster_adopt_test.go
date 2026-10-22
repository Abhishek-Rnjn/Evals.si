package runs

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

type stubCluster struct{}

func (stubCluster) Owner() string                                            { return "a" }
func (stubCluster) PublishRunEvent(string, *evalsiv1alpha1.WatchRunResponse) {}
func (stubCluster) SubscribeRunEvents(string) (<-chan *evalsiv1alpha1.WatchRunResponse, func(), error) {
	return make(chan *evalsiv1alpha1.WatchRunResponse), func() {}, nil
}
func (stubCluster) RequestCancel(string) error            { return nil }
func (stubCluster) OnCancel(func(string)) (func(), error) { return func() {}, nil }

// adopt() lists unfinished runs, then takes the lease per run. A run that
// finishes in between must not be executed again.
func TestStartDoesNotReExecuteRunFinishedAfterAdoptListing(t *testing.T) {
	h := newHarness(t)
	run := h.create(t, &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline("easy"), Evaluators: refs("exact-match")})
	final := h.wait(t, run.GetId())
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatal(final.GetStatus())
	}
	h.m.opts.Cluster = stubCluster{}
	finished := 0
	h.m.opts.OnFinished = func(*evalsiv1alpha1.Run) { finished++ }
	stale := proto.Clone(final).(*evalsiv1alpha1.Run)
	stale.Status = evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING // what adopt's earlier listing saw
	h.m.start(stale)
	time.Sleep(500 * time.Millisecond)
	h.m.Wait()
	got, _ := h.st.GetRun(context.Background(), run.GetId())
	if finished != 0 || got.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED {
		t.Fatalf("terminal run was re-executed: %d OnFinished calls, status %v", finished, got.GetStatus())
	}
}
