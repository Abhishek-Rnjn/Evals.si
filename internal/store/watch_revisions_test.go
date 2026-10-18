package store

import (
	"context"
	"errors"
	"testing"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func revisionTrace(stream, text string, start time.Time) WatchTraceWrite {
	summary, record := trace("p", "revision", "studio", start)
	record.Output = &evalsiv1alpha1.Content{Kind: &evalsiv1alpha1.Content_Text{Text: text}}
	return WatchTraceWrite{Pending: WatchPending{Project: "p", TraceID: "revision", Stream: stream, Payload: []byte(stream + text), RecordHash: RecordHash(record), SnapshotHash: SnapshotHash(summary, record), StartNS: start.UnixNano()}, Trace: TraceWrite{Summary: summary, Record: record}}
}
func revisionScores(value float64) []*evalsiv1alpha1.EvaluationResult {
	return []*evalsiv1alpha1.EvaluationResult{{Evaluator: "quality", Outcome: evalsiv1alpha1.Outcome_OUTCOME_SCORED, Scores: []*evalsiv1alpha1.Score{{Value: &evalsiv1alpha1.Score_Number{Number: value}}}}}
}
func acceptRevision(t *testing.T, st *Store, w WatchTraceWrite) {
	t.Helper()
	if err := st.PutWatchPending(t.Context(), []WatchPending{w.Pending}); err != nil {
		t.Fatal(err)
	}
}
func publishRevision(t *testing.T, st *Store, w WatchTraceWrite) {
	t.Helper()
	ok, err := st.PutWatchTraces(t.Context(), PolicyLease{}, []WatchTraceWrite{w})
	if err != nil || len(ok) != 1 || !ok[0] {
		t.Fatalf("publication: %v %v", ok, err)
	}
}

func TestIndependentStreamsNeverRepublishOlderContentOrScores(t *testing.T) {
	for backend, traces := range traceBackends(t) {
		t.Run(backend, func(t *testing.T) {
			st := open(t)
			if backend == "clickhouse" {
				st.UseTraces(traces)
			}
			ctx := t.Context()
			start := time.Now()
			old := revisionTrace("blocked", "old", start)
			newer := revisionTrace("ready", "new", start)
			acceptRevision(t, st, old)
			publishRevision(t, st, old)
			if err := st.PutWatchResults(ctx, PolicyLease{}, old.Pending, "quality", revisionScores(0)); err != nil {
				t.Fatal(err)
			}
			acceptRevision(t, st, newer)
			publishRevision(t, st, newer)
			// The changed record is visible before its evaluator is ready, with no scores
			// borrowed from the previous output on any query path.
			if got, err := st.TraceResults(ctx, "p", "revision"); err != nil || len(got) != 0 {
				t.Fatalf("old score exposed beside new output: %v %v", got, err)
			}
			if got, _, err := st.ListTraces(ctx, TraceFilter{}, 10, ""); err != nil || len(got) != 1 || got[0].GetResults() != 0 {
				t.Fatalf("obsolete score count: %v %v", got, err)
			}
			if got, err := st.QueryTraces(ctx, TraceQuery{Project: "p", Policy: "quality"}); err != nil || len(got) != 0 {
				t.Fatalf("obsolete query score: %v %v", got, err)
			}
			if got, err := st.ScoredTraces(ctx, ScoreQuery{Projects: []string{"p"}}); err != nil || len(got) != 0 {
				t.Fatalf("obsolete scored trace: %v %v", got, err)
			}
			if err := st.PutWatchResults(ctx, PolicyLease{}, newer.Pending, "quality", revisionScores(1)); err != nil {
				t.Fatal(err)
			}
			if err := st.AckWatchPending(ctx, PolicyLease{}, newer.Pending); err != nil {
				t.Fatal(err)
			}
			// The blocked stream is still accepted work, but cannot publish its old record.
			publishRevision(t, st, old)
			if err := st.PutWatchResults(ctx, PolicyLease{}, old.Pending, "quality", revisionScores(0)); err != nil {
				t.Fatal(err)
			}
			_, record, err := st.GetTrace(ctx, "p", "revision")
			if err != nil || record.GetOutput().GetText() != "new" {
				t.Fatalf("older stream restored stale content: %v %v", record, err)
			}
			assertCurrentRevisionScore(t, st, 1)
			// A new delivery of identical content preserves its valid scores.
			same := newer
			same.Pending.Payload = []byte("new acceptance of identical content")
			acceptRevision(t, st, same)
			publishRevision(t, st, same)
			assertCurrentRevisionScore(t, st, 1)
		})
	}
}

func assertCurrentRevisionScore(t *testing.T, st *Store, want float64) {
	t.Helper()
	got, err := st.TraceResults(t.Context(), "p", "revision")
	if err != nil || len(got) != 1 || len(got[0].GetResults()) != 1 || got[0].GetResults()[0].GetScores()[0].GetNumber() != want {
		t.Fatalf("current score: %v %v", got, err)
	}
}

type countingWatchBackend struct {
	TraceStore
	writes int
}

func (c *countingWatchBackend) PutTraces(context.Context, []TraceWrite) error { c.writes++; return nil }
func (c *countingWatchBackend) Close() error                                  { return nil }

func TestPublicationStatusSurvivesDispatcherRestart(t *testing.T) {
	st := open(t)
	backend := &countingWatchBackend{}
	st.UseTraces(backend)
	w := revisionTrace("blocked", "same", time.Now())
	acceptRevision(t, st, w)
	publishRevision(t, st, w)
	// A prolonged readiness outage must require no metadata writes either.
	readonly, readwrite := `PRAGMA query_only=ON`, `PRAGMA query_only=OFF`
	if st.db.d == postgresDialect {
		st.db.db.SetMaxOpenConns(1)
		readonly = `SET default_transaction_read_only=on`
		readwrite = `SET default_transaction_read_only=off`
	}
	if _, err := st.db.ExecContext(t.Context(), readonly); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = st.db.ExecContext(context.Background(), readwrite) }()
	// A new Store wrapper has no in-memory publication history.
	restarted := newStore(st.db)
	restarted.UseTraces(backend)
	for i := 0; i < 10; i++ {
		publishRevision(t, restarted, w)
	}
	if backend.writes != 1 {
		t.Fatalf("readiness outage rewrote published content %d times", backend.writes)
	}
	if pending, err := st.ListWatchPending(t.Context(), 10); err != nil || len(pending) != 1 {
		t.Fatalf("publication acknowledged unfinished evaluation: %v %v", pending, err)
	}
}

func TestPolicyLeaseEpochFencesMissedOwnerRoundTrip(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	a1 := testPolicyLease(t, st, "a")
	renewed := testPolicyLease(t, st, "a")
	if renewed != a1 {
		t.Fatal("live renewal changed leadership term")
	}
	w := revisionTrace("ready", "same", time.Now())
	acceptRevision(t, st, w)
	if _, err := st.PutWatchTraces(ctx, a1, []WatchTraceWrite{w}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseLease(ctx, PolicyEngineLease, "a"); err != nil {
		t.Fatal(err)
	}
	b := testPolicyLease(t, st, "b")
	// B's results commit, but its handoff has not succeeded, so work stays pending.
	if err := st.PutWatchResults(ctx, b, w.Pending, "quality", revisionScores(1)); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseLease(ctx, PolicyEngineLease, "b"); err != nil {
		t.Fatal(err)
	}
	a2 := testPolicyLease(t, st, "a")
	if a2.Owner != a1.Owner || a2.Epoch <= b.Epoch || b.Epoch <= a1.Epoch {
		t.Fatalf("nonmonotonic terms: %v %v %v", a1, b, a2)
	}
	if err := st.PutWatchResults(ctx, a1, w.Pending, "quality", revisionScores(0)); !errors.Is(err, ErrWatchStale) {
		t.Fatalf("missed A-B-A term committed: %v", err)
	}
	if _, err := st.PutWatchTraces(ctx, a1, []WatchTraceWrite{w}); !errors.Is(err, ErrWatchStale) {
		t.Fatalf("old term published: %v", err)
	}
	if err := st.AckWatchPending(ctx, a1, w.Pending); !errors.Is(err, ErrWatchStale) {
		t.Fatalf("old term acknowledged: %v", err)
	}
	assertCurrentRevisionScore(t, st, 1)
	if err := st.AckWatchPending(ctx, a2, w.Pending); err != nil {
		t.Fatal(err)
	}
}

func TestClickHouseLateInsertCannotUndoCompletedRetention(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(map[bool]string{false: "never published", true: "previously published"}[published], func(t *testing.T) {
			traces, ok := traceBackends(t)["clickhouse"]
			if !ok {
				t.Skip("set EVALSI_TEST_CLICKHOUSE_URL")
			}
			st := open(t)
			st.UseTraces(traces)
			ctx := t.Context()
			w := revisionTrace("ready", "expired", time.Now().Add(-time.Hour))
			acceptRevision(t, st, w)
			if published {
				publishRevision(t, st, w)
			}
			lateCtx, err := st.watchVersionContext(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			h, err := getWatchHead(ctx, st.db, "p", "revision")
			if err != nil {
				t.Fatal(err)
			}
			binding := scoreBinding(h.Hash, h.ScoreRevision)
			lateCtx = context.WithValue(lateCtx, watchScoreBindingKey{}, binding)
			lateCtx = context.WithValue(lateCtx, watchProjectionKey{}, map[[2]string]string{{"p", "revision"}: binding})
			if _, err := st.DeleteTracesBefore(ctx, time.Now()); err != nil {
				t.Fatal(err)
			}
			// Simulate the server completing requests after their client timed out and
			// after retention committed both pending removal and its tombstone.
			if err := traces.PutTrace(lateCtx, w.Trace.Summary, w.Trace.Record); err != nil {
				t.Fatal(err)
			}
			if err := traces.PutTraceResults(lateCtx, "p", "revision", "quality", revisionScores(0)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.GetTrace(ctx, "p", "revision"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("late trace resurrected: %v", err)
			}
			if rows, _, err := st.ListTraces(ctx, TraceFilter{}, 10, ""); err != nil || len(rows) != 0 {
				t.Fatalf("late trace listed: %v %v", rows, err)
			}
			if rows, err := st.TraceProjects(ctx, "revision"); err != nil || len(rows) != 0 {
				t.Fatalf("late trace project exposed: %v %v", rows, err)
			}
			if rows, err := st.TraceResults(ctx, "p", "revision"); err != nil || len(rows) != 0 {
				t.Fatalf("late scores exposed: %v %v", rows, err)
			}
			if rows, err := st.QueryTraces(ctx, TraceQuery{Project: "p"}); err != nil || len(rows) != 0 {
				t.Fatalf("late trace queried: %v %v", rows, err)
			}
			if rows, err := st.ScoredTraces(ctx, ScoreQuery{}); err != nil || len(rows) != 0 {
				t.Fatalf("late score queried: %v %v", rows, err)
			}
			// A deliberate new acceptance is distinct from the timed-out old request.
			w.Pending.Payload = []byte("intentional backfill")
			acceptRevision(t, st, w)
			publishRevision(t, st, w)
			if _, _, err := st.GetTrace(ctx, "p", "revision"); err != nil {
				t.Fatalf("new acceptance rejected after retention: %v", err)
			}
			if rows, err := st.TraceResults(ctx, "p", "revision"); err != nil || len(rows) != 0 {
				t.Fatalf("new acceptance revived an expired score: %v %v", rows, err)
			}
		})
	}
}

func TestSupersededReservationCannotStartExternalCommit(t *testing.T) {
	st := open(t)
	st.UseTraces(&clickhouse{})
	old, err := st.watchVersionContext(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	current, err := st.watchVersionContext(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if tx, err := st.beginReservedWatchWrite(old); !errors.Is(err, errWatchReservationStale) {
		if tx != nil {
			_ = tx.Rollback()
		}
		t.Fatalf("old reservation entered commit: %v", err)
	}
	tx, err := st.beginReservedWatchWrite(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type racingRevisionReader struct {
	TraceStore
	revisionTraceReader
	afterRead func()
}

func (r *racingRevisionReader) getTraceRevision(ctx context.Context, project, id string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, string, error) {
	summary, record, binding, err := r.revisionTraceReader.getTraceRevision(ctx, project, id)
	if err == nil && r.afterRead != nil {
		r.afterRead()
	}
	return summary, record, binding, err
}

func TestTraceResponsePinsScoresWhenPublicationChangesBetweenReads(t *testing.T) {
	for backend, traces := range traceBackends(t) {
		t.Run(backend, func(t *testing.T) {
			writer := open(t)
			if backend == "clickhouse" {
				writer.UseTraces(traces)
			}
			old := revisionTrace("old", "old", time.Now())
			newer := revisionTrace("new", "new", old.Trace.Summary.GetStartTime().AsTime())
			acceptRevision(t, writer, old)
			publishRevision(t, writer, old)
			if err := writer.PutWatchResults(t.Context(), PolicyLease{}, old.Pending, "quality", revisionScores(0)); err != nil {
				t.Fatal(err)
			}
			reader := newStore(writer.db)
			reader.UseTraces(&racingRevisionReader{TraceStore: writer.traces, revisionTraceReader: writer.traces.(revisionTraceReader), afterRead: func() {
				acceptRevision(t, writer, newer)
				publishRevision(t, writer, newer)
				if err := writer.PutWatchResults(t.Context(), PolicyLease{}, newer.Pending, "quality", revisionScores(1)); err != nil {
					t.Fatal(err)
				}
			}})
			_, record, scores, err := reader.GetTraceWithResults(t.Context(), "p", "revision")
			if err != nil || record.GetOutput().GetText() != "old" || len(scores) != 1 || scores[0].GetResults()[0].GetScores()[0].GetNumber() != 0 {
				t.Fatalf("response mixed record and score revisions: %v %v %v", record, scores, err)
			}
			assertCurrentRevisionScore(t, writer, 1)
		})
	}
}

func TestSummaryOnlyRevisionPublishesMetadataAndPreservesScores(t *testing.T) {
	for backend, traces := range traceBackends(t) {
		t.Run(backend, func(t *testing.T) {
			st := open(t)
			if backend == "clickhouse" {
				st.UseTraces(traces)
			}
			old := revisionTrace("old", "same input", time.Now())
			acceptRevision(t, st, old)
			publishRevision(t, st, old)
			if err := st.PutWatchResults(t.Context(), PolicyLease{}, old.Pending, "quality", revisionScores(1)); err != nil {
				t.Fatal(err)
			}
			newer := revisionTrace("new", "same input", old.Trace.Summary.GetStartTime().AsTime())
			newer.Trace.Summary.Service = "new service"
			newer.Pending.SnapshotHash = SnapshotHash(newer.Trace.Summary, newer.Trace.Record)
			acceptRevision(t, st, newer)
			publishRevision(t, st, newer)
			publishRevision(t, st, old)
			summary, _, err := st.GetTrace(t.Context(), "p", "revision")
			if err != nil || summary.GetService() != "new service" {
				t.Fatalf("metadata-only update was lost: %v %v", summary, err)
			}
			assertCurrentRevisionScore(t, st, 1)
		})
	}
}

func TestLegacyRecoveryCannotPublishOverNewerSummary(t *testing.T) {
	for backend, traces := range traceBackends(t) {
		t.Run(backend, func(t *testing.T) {
			st := open(t)
			if backend == "clickhouse" {
				st.UseTraces(traces)
			}
			ctx := t.Context()
			old := revisionTrace("legacy", "same input", time.Now())
			// Simulate a pending row created before revision/snapshot columns existed.
			if _, err := st.db.ExecContext(ctx, `INSERT INTO watch_pending(project,trace_id,stream,payload,start_ns) VALUES(?,?,?,?,?)`, old.Pending.Project, old.Pending.TraceID, old.Pending.Stream, old.Pending.Payload, old.Pending.StartNS); err != nil {
				t.Fatal(err)
			}
			newer := revisionTrace("new", "same input", old.Trace.Summary.GetStartTime().AsTime())
			newer.Trace.Summary.Service = "new service"
			newer.Pending.SnapshotHash = SnapshotHash(newer.Trace.Summary, newer.Trace.Record)
			acceptRevision(t, st, newer)
			publishRevision(t, st, old)
			if err := st.PutWatchResults(ctx, PolicyLease{}, old.Pending, "quality", revisionScores(1)); err != nil {
				t.Fatal(err)
			}
			publishRevision(t, st, newer)
			summary, _, err := st.GetTrace(ctx, "p", "revision")
			if err != nil || summary.GetService() != "new service" {
				t.Fatalf("legacy recovery displaced newer summary: %v %v", summary, err)
			}
			assertCurrentRevisionScore(t, st, 1)
		})
	}
}

func TestSQLRetentionRemovesUnpublishedHistoricalScores(t *testing.T) {
	for _, visible := range []bool{false, true} {
		t.Run(map[bool]string{false: "no projection", true: "unexpired projection"}[visible], func(t *testing.T) {
			st := open(t)
			ctx := t.Context()
			cutoff := time.Now()
			old := revisionTrace("old", "old input", cutoff.Add(-time.Hour))
			newer := revisionTrace("new", "new input", old.Trace.Summary.GetStartTime().AsTime())
			acceptRevision(t, st, old)
			acceptRevision(t, st, newer)
			publishRevision(t, st, old) // Historical evaluation creates no trace row.
			if err := st.PutWatchResults(ctx, PolicyLease{}, old.Pending, "quality", revisionScores(0)); err != nil {
				t.Fatal(err)
			}
			if visible {
				live := revisionTrace("live", "live input", cutoff.Add(time.Hour))
				if err := st.PutTrace(ctx, live.Trace.Summary, live.Trace.Record); err != nil {
					t.Fatal(err)
				}
				if err := st.PutTraceResults(ctx, "p", "revision", "quality", revisionScores(1)); err != nil {
					t.Fatal(err)
				}
			}
			for pass := 0; pass < 2; pass++ {
				if _, err := st.DeleteTracesBefore(ctx, cutoff); err != nil {
					t.Fatal(err)
				}
				if visible {
					assertCurrentRevisionScore(t, st, 1)
				} else {
					var count int
					if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trace_results`).Scan(&count); err != nil || count != 0 {
						t.Fatalf("retention left historical score payloads: %d %v", count, err)
					}
				}
				if pass == 0 && st.db.d == postgresDialect {
					var before, after string
					if err := st.db.QueryRowContext(ctx, `SELECT xmin::text FROM watch_trace_heads WHERE project='p' AND trace_id='revision'`).Scan(&before); err != nil {
						t.Fatal(err)
					}
					if _, err := st.DeleteTracesBefore(ctx, cutoff); err != nil {
						t.Fatal(err)
					}
					if err := st.db.QueryRowContext(ctx, `SELECT xmin::text FROM watch_trace_heads WHERE project='p' AND trace_id='revision'`).Scan(&after); err != nil || before != after {
						t.Fatalf("repeat retention rewrote a tombstone: %s -> %s, %v", before, after, err)
					}
				}
				if pass == 0 && !visible {
					// Also collect payloads stranded by an earlier retention pass
					// that already tombstoned the head but omitted orphan scores.
					if _, err := st.db.ExecContext(ctx, `INSERT INTO trace_results(project,trace_id,policy,evaluator,record_hash,result) VALUES(?,?,?,?,?,?)`, "p", "revision", "quality", "quality", "historical", marshal(revisionScores(0)[0])); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
