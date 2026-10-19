package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func TestWatchWritesFenceLeaseRevisionAndRetention(t *testing.T) {
	for backend, traces := range traceBackends(t) {
		t.Run(backend, func(t *testing.T) {
			st := open(t)
			if backend == "clickhouse" {
				st.UseTraces(traces)
			}
			ctx := t.Context()
			sum, record := trace("p", "trace", "studio", time.Now().Add(-time.Hour))
			old := WatchPending{Project: "p", TraceID: "trace", Payload: []byte("old"), StartNS: sum.GetStartTime().AsTime().UnixNano()}
			newer := old
			newer.Payload = []byte("new")
			write := func(p WatchPending, owner PolicyLease) ([]bool, error) {
				return st.PutWatchTraces(ctx, owner, []WatchTraceWrite{{Pending: p, Trace: TraceWrite{Summary: sum, Record: record}}})
			}
			results := []*evalsiv1alpha1.EvaluationResult{{Evaluator: "quality", Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED}}
			if err := st.PutWatchPending(ctx, []WatchPending{old}); err != nil {
				t.Fatal(err)
			}
			a := testPolicyLease(t, st, "a")
			if ok, err := write(old, a); err != nil || len(ok) != 1 || !ok[0] {
				t.Fatalf("current write refused: %v %v", ok, err)
			}
			if err := st.ReleaseLease(ctx, PolicyEngineLease, "a"); err != nil {
				t.Fatal(err)
			}
			b := testPolicyLease(t, st, "b")
			// Keep the very same pending payload: only the lease check can reject a.
			if _, err := write(old, a); !errors.Is(err, ErrWatchStale) {
				t.Fatalf("stale leader stored trace: %v", err)
			}
			if err := st.PutWatchResults(ctx, a, old, "quality", results); !errors.Is(err, ErrWatchStale) {
				t.Fatalf("stale leader stored score: %v", err)
			}
			if err := st.AckWatchPending(ctx, a, old); !errors.Is(err, ErrWatchStale) {
				t.Fatalf("stale leader acknowledged work: %v", err)
			}
			if err := st.PutWatchPending(ctx, []WatchPending{newer}); err != nil {
				t.Fatal(err)
			}
			if ok, err := write(old, b); err != nil || ok[0] {
				t.Fatalf("obsolete revision stored trace: %v %v", ok, err)
			}
			if err := st.PutWatchResults(ctx, b, old, "quality", results); !errors.Is(err, ErrWatchStale) {
				t.Fatalf("obsolete revision stored score: %v", err)
			}
			if err := st.AckWatchPending(ctx, b, old); err != nil {
				t.Fatal(err)
			}
			if ok, err := st.WatchPendingMatches(ctx, newer); err != nil || !ok {
				t.Fatalf("new revision acknowledged: %v %v", ok, err)
			}
			if ok, err := write(newer, b); err != nil || !ok[0] {
				t.Fatalf("new revision refused: %v %v", ok, err)
			}
			if err := st.PutWatchResults(ctx, b, newer, "quality", results); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DeleteTracesBefore(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			if ok, err := write(newer, b); err != nil || ok[0] {
				t.Fatalf("retained trace resurrected: %v %v", ok, err)
			}
			if err := st.PutWatchResults(ctx, b, newer, "quality", results); !errors.Is(err, ErrWatchStale) {
				t.Fatalf("retained score resurrected: %v", err)
			}
			if _, _, err := st.GetTrace(ctx, "p", "trace"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted trace visible: %v", err)
			}
			// Expiration is checked even if no other replica has acquired the lease.
			if ok, err := st.AcquireLease(ctx, PolicyEngineLease, "b", -time.Second); err != nil || !ok {
				t.Fatalf("expire lease: %v %v", ok, err)
			}
			if err := st.PutWatchPending(ctx, []WatchPending{newer}); err != nil {
				t.Fatal(err)
			}
			if _, err := write(newer, b); !errors.Is(err, ErrWatchStale) {
				t.Fatalf("expired leader stored trace: %v", err)
			}
		})
	}
}

// A real external INSERT may outlive its client timeout. Its reserved version
// must never be reused after the metadata transaction rolls back.
func TestClickHouseWatchVersionsSurviveRollbackAndClockSkew(t *testing.T) {
	st := open(t)
	var mu sync.Mutex
	var versions []int64
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var row struct {
			Version int64 `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		mu.Lock()
		versions = append(versions, row.Version)
		shouldFail := fail
		mu.Unlock()
		if shouldFail {
			http.Error(w, "accepted but response failed", 500)
		}
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	st.UseTraces(&clickhouse{base: base, db: "test", ready: true, client: server.Client()})
	// Simulate a previous leader whose clock was far ahead of this replica.
	if _, err := st.db.ExecContext(t.Context(), `UPDATE watch_write_lock SET version = ? WHERE id = 1`, int64(1<<61)); err != nil {
		t.Fatal(err)
	}
	pending := WatchPending{Project: "p", TraceID: "trace", Payload: []byte("current")}
	if err := st.PutWatchPending(t.Context(), []WatchPending{pending}); err != nil {
		t.Fatal(err)
	}
	results := []*evalsiv1alpha1.EvaluationResult{{Evaluator: "quality"}}
	if err := st.PutWatchResults(t.Context(), PolicyLease{}, pending, "quality", results); err == nil {
		t.Fatal("fixture did not fail the first write")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if err := st.PutWatchResults(t.Context(), PolicyLease{}, pending, "quality", results); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(versions) != 2 || versions[0] <= 1<<61 || versions[1] <= versions[0] {
		t.Fatalf("version ordering lost across failed commit: %v", versions)
	}
}

type blockedWatchBackend struct {
	TraceStore
	entered chan struct{}
	release chan struct{}
}

func (b *blockedWatchBackend) Close() error { return nil }

func (b *blockedWatchBackend) PutTraces(context.Context, []TraceWrite) error {
	close(b.entered)
	<-b.release
	return nil
}

func TestLeaseChangesWaitForInFlightExternalTraceCommit(t *testing.T) {
	st := open(t)
	backend := &blockedWatchBackend{entered: make(chan struct{}), release: make(chan struct{})}
	st.UseTraces(backend)
	var once sync.Once
	unblock := func() { once.Do(func() { close(backend.release) }) }
	defer unblock()
	ctx := t.Context()
	a := testPolicyLease(t, st, "a")
	sum, record := trace("p", "trace", "studio", time.Now())
	p := WatchPending{Project: "p", TraceID: "trace", Payload: []byte("current")}
	if err := st.PutWatchPending(ctx, []WatchPending{p}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.PutWatchTraces(ctx, a, []WatchTraceWrite{{Pending: p, Trace: TraceWrite{Summary: sum, Record: record}}})
		done <- err
	}()
	select {
	case <-backend.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("external write did not start")
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := st.ReleaseLease(short, PolicyEngineLease, "a"); err == nil {
		t.Fatal("lease changed during external commit")
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("external write did not commit")
	}
	if err := st.ReleaseLease(ctx, PolicyEngineLease, "a"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.AcquireLease(ctx, PolicyEngineLease, "b", time.Minute); err != nil || !ok {
		t.Fatalf("takeover after commit: %v %v", ok, err)
	}
}

func TestClickHouseFencedVersionsRejectLateOldDelivery(t *testing.T) {
	traces, ok := traceBackends(t)["clickhouse"]
	if !ok {
		t.Skip("set EVALSI_TEST_CLICKHOUSE_URL for real replacement-order validation")
	}
	st := open(t)
	st.UseTraces(traces)
	ctx := t.Context()
	oldCtx, err := st.watchVersionContext(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	newCtx, err := st.watchVersionContext(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	sum, record := trace("p", "late-insert", "studio", time.Now())
	record.Output = &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: "new"}}
	if err := traces.PutTrace(newCtx, sum, record); err != nil {
		t.Fatal(err)
	}
	record.Output = &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: "old"}}
	if err := traces.PutTrace(oldCtx, sum, record); err != nil {
		t.Fatal(err)
	}
	result := func(v float64) []*evalsiv1alpha1.EvaluationResult {
		return []*evalsiv1alpha1.EvaluationResult{{Evaluator: "quality", Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED, Scores: []*evalsiv1alpha1.Score{{Value: &evalsiv1alpha1.Score_Number{Number: v}}}}}
	}
	if err := traces.PutTraceResults(newCtx, "p", "late-insert", "policy", result(1)); err != nil {
		t.Fatal(err)
	}
	if err := traces.PutTraceResults(oldCtx, "p", "late-insert", "policy", result(0)); err != nil {
		t.Fatal(err)
	}
	_, got, err := st.GetTrace(ctx, "p", "late-insert")
	if err != nil || got.GetOutput().GetText() != "new" {
		t.Fatalf("late old INSERT replaced trace: %v %v", got, err)
	}
	scores, err := st.TraceResults(ctx, "p", "late-insert")
	if err != nil || len(scores) != 1 || scores[0].GetResults()[0].GetScores()[0].GetNumber() != 1 {
		t.Fatalf("late old INSERT replaced scores: %v %v", scores, err)
	}
}

func testPolicyLease(t *testing.T, st *Store, owner string) PolicyLease {
	t.Helper()
	lease, ok, err := st.AcquirePolicyLease(t.Context(), owner, time.Minute)
	if err != nil || !ok {
		t.Fatalf("policy lease: %v %v", ok, err)
	}
	return lease
}
