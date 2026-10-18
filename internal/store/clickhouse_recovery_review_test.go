package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func reviewClickHouse(t *testing.T) (*Store, *clickhouse) {
	t.Helper()
	backend, ok := traceBackends(t)["clickhouse"]
	if !ok {
		t.Skip("set EVALSI_TEST_CLICKHOUSE_URL")
	}
	st := open(t)
	st.UseTraces(backend)
	return st, backend.(*clickhouse)
}

func TestClickHouseRetentionPreservesLiveProjectionAgainstExpiredHead(t *testing.T) {
	st, _ := reviewClickHouse(t)
	ctx := t.Context()
	cutoff := time.Now()
	live := revisionTrace("live", "live", cutoff.Add(time.Hour))
	acceptRevision(t, st, live)
	publishRevision(t, st, live)
	if err := st.PutWatchResults(ctx, PolicyLease{}, live.Pending, "quality", revisionScores(1)); err != nil {
		t.Fatal(err)
	}
	expired := revisionTrace("expired", "expired", cutoff.Add(-time.Hour))
	acceptRevision(t, st, expired)
	late, err := st.watchVersionContext(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTracesBefore(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		_, record, err := st.GetTrace(ctx, "p", "revision")
		if err != nil || record.GetOutput().GetText() != "live" {
			t.Fatalf("expired head hid live projection: %v %v", record, err)
		}
		assertCurrentRevisionScore(t, st, 1)
		publishRevision(t, st, live) // Its unexpired pending evaluation stays eligible.
		if pass == 0 {
			if err := st.traces.PutTrace(late, expired.Trace.Summary, expired.Trace.Record); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DeleteTracesBefore(ctx, cutoff); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestClickHouseLegacyScoreCopyCannotReplaceFreshEvaluation(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(map[bool]string{false: "already stored", true: "delayed completion"}[delayed], func(t *testing.T) {
			st, c := reviewClickHouse(t)
			ctx := t.Context()
			old := revisionTrace("legacy", "same", time.Now())
			params := chParams{"summary": b64(old.Trace.Summary), "record": b64(old.Trace.Record), "score": b64(revisionScores(0)[0])}
			missing := revisionScores(0)[0]
			missing.Evaluator = "missing"
			params["missing"] = b64(missing)
			if err := c.exec(ctx, `INSERT INTO traces(project,trace_id,service,start_ns,summary,record,version) SELECT 'p','revision','studio',1,base64Decode({summary:String}),base64Decode({record:String}),1`, params, nil); err != nil {
				t.Fatal(err)
			}
			if err := c.exec(ctx, `INSERT INTO trace_results(project,trace_id,policy,evaluator,result,version) SELECT 'p','revision','quality',evaluator,if(evaluator='missing',base64Decode({missing:String}),base64Decode({score:String})),1 FROM (SELECT arrayJoin(['quality','missing']) AS evaluator)`, params, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `INSERT INTO watch_pending(project,trace_id,stream,payload,start_ns) VALUES(?,?,?,?,?)`, old.Pending.Project, old.Pending.TraceID, old.Pending.Stream, old.Pending.Payload, old.Pending.StartNS); err != nil {
				t.Fatal(err)
			}
			newer := revisionTrace("new", "same", old.Trace.Summary.GetStartTime().AsTime())
			newer.Trace.Summary.Service = "new service"
			newer.Pending.SnapshotHash = SnapshotHash(newer.Trace.Summary, newer.Trace.Record)
			acceptRevision(t, st, newer)
			publishRevision(t, st, old)
			deliver := func() {
				if err := st.PutWatchResults(ctx, PolicyLease{}, old.Pending, "quality", revisionScores(1)); err != nil {
					t.Fatal(err)
				}
			}
			if delayed {
				late, err := st.watchVersionContext(ctx, 1)
				if err != nil {
					t.Fatal(err)
				}
				h, err := getWatchHead(ctx, st.db, "p", "revision")
				if err != nil {
					t.Fatal(err)
				}
				late = context.WithValue(late, watchScoreBindingKey{}, scoreBinding(h.Hash, h.ScoreRevision))
				deliver = func() {
					if err := c.PutTraceResults(late, "p", "revision", "quality", revisionScores(1)); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				deliver()
			}
			publishRevision(t, st, newer)
			if delayed {
				deliver()
			}
			results, err := st.TraceResults(ctx, "p", "revision")
			if err != nil || len(results) != 1 || len(results[0].GetResults()) != 2 {
				t.Fatalf("missing migrated or fresh result: %v %v", results, err)
			}
			for _, r := range results[0].GetResults() {
				want := float64(0)
				if r.GetEvaluator() == "quality" {
					want = 1
				}
				if r.GetScores()[0].GetNumber() != want {
					t.Fatalf("migration replaced fresh evaluation: %v", r)
				}
			}
		})
	}
}

func TestClickHouseRetentionCleansCrossMonthAndLatePayloads(t *testing.T) {
	st, c := reviewClickHouse(t)
	ctx := t.Context()
	october := revisionTrace("october", "later span", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	september := revisionTrace("september", "earlier span", time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC))
	for _, w := range []WatchTraceWrite{october, september} {
		acceptRevision(t, st, w)
		publishRevision(t, st, w)
	}
	late, err := st.watchVersionContext(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.DeleteTracesBefore(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	assertClickHousePayloadCount(t, c, "traces WHERE deleted=0", 0)
	// Requests that outlive their clients may finish after the first cleanup.
	if err := c.PutTrace(late, october.Trace.Summary, october.Trace.Record); err != nil {
		t.Fatal(err)
	}
	late = context.WithValue(late, watchScoreBindingKey{}, "expired")
	if err := c.PutTraceResults(late, "p", "revision", "quality", revisionScores(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTracesBefore(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	assertClickHousePayloadCount(t, c, "traces WHERE deleted=0", 0)
	assertClickHousePayloadCount(t, c, "trace_results", 0)
	assertClickHousePayloadCount(t, c, "traces FINAL WHERE deleted=1", 1)
}

func assertClickHousePayloadCount(t *testing.T, c *clickhouse, table string, want int) {
	t.Helper()
	got := -1
	err := c.query(t.Context(), `SELECT count() AS n FROM `+table, nil, func(line []byte) error {
		var row struct {
			N int `json:"n"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		got = row.N
		return nil
	})
	if err != nil || got != want {
		t.Fatalf("payload count %s: got %d want %d: %v", table, got, want, err)
	}
}
