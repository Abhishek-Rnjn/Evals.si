package runs

import (
	"context"
	"strings"
	"testing"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
)

// A grant revoked after a run was stored stops the run before it starts
// more work.
func TestRevokedGrantStopsAStoredRun(t *testing.T) {
	h := newHarness(t)
	granted := credentials.Settings{Credentials: credentials.Config{Grants: []credentials.Grant{
		{Env: "OPENAI_API_KEY", Projects: []string{"p"}, Hosts: []string{"t"}, AllowHTTP: true},
	}}}
	policy := credentials.NewPolicy(granted)
	h.m.engine.UseCredentials(policy)

	run, err := h.tryCreate("p", &evalsiv1alpha1.RunSpec{Target: target(), Dataset: inline("easy", "easy", "easy", "easy", "easy", "easy", "easy", "easy"), Evaluators: refs("test/slow")})
	if err != nil {
		t.Fatal(err)
	}
	// The first batch is with the worker; revoke the grant, then let it finish.
	deadline := time.Now().Add(5 * time.Second)
	for h.worker.evaluateCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	policy.Update(credentials.Settings{Credentials: credentials.Config{Grants: []credentials.Grant{
		{Env: "OPENAI_API_KEY", Projects: []string{"other"}, Hosts: []string{"t"}, AllowHTTP: true},
	}}})
	close(h.worker.release)
	final := h.wait(t, run.GetId())
	if final.GetStatus() != evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR || !strings.Contains(final.GetError(), "OPENAI_API_KEY") {
		t.Fatalf("run after revocation: %v %q", final.GetStatus(), final.GetError())
	}
	if n := h.worker.evaluateCalls.Load(); n >= 8 {
		t.Errorf("the worker graded %d records after the grant was revoked", n)
	}

	// Resuming it is refused the same way, without calling the worker.
	before := h.worker.evaluateCalls.Load()
	if _, err := h.m.prepare(context.Background(), final, &activeRun{}); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("preparing a stored run under a revoked grant: %v", err)
	}
	if h.worker.evaluateCalls.Load() != before {
		t.Error("the worker was called")
	}
}
