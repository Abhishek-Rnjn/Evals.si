package store

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestRevisionMigrationPreservesLegacyScoresAndIsIdempotent(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	w := revisionTrace("legacy", "same", time.Now())
	if err := st.PutTrace(ctx, w.Trace.Summary, w.Trace.Record); err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`DROP INDEX watch_score_dirty`,
		`ALTER TABLE watch_score_references DROP COLUMN dirty`,
		`DROP TABLE trace_results`,
		`CREATE TABLE trace_results(project TEXT NOT NULL,trace_id TEXT NOT NULL,policy TEXT NOT NULL,evaluator TEXT NOT NULL,result BLOB NOT NULL,tenant_id TEXT NOT NULL DEFAULT '',PRIMARY KEY(project,trace_id,policy,evaluator))`,
		`UPDATE traces SET record_hash=''`,
		`ALTER TABLE leases DROP COLUMN epoch`,
		`ALTER TABLE watch_pending DROP COLUMN revision`,
		`ALTER TABLE watch_pending DROP COLUMN record_hash`,
	} {
		if _, err := tx.ExecContext(ctx, st.db.d.ddl(statement)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO trace_results(project,trace_id,policy,evaluator,result) VALUES(?,?,?,?,?)`, "p", "revision", "quality", "quality", marshal(revisionScores(1)[0])); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateWatchState(ctx, tx, st.db.d); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertCurrentRevisionScore(t, st, 1)
	acceptRevision(t, st, w)
	publishRevision(t, st, w)
	assertCurrentRevisionScore(t, st, 1) // Identical first publication preserves old bound scores.
	changed := revisionTrace("legacy", "changed", w.Trace.Summary.GetStartTime().AsTime())
	acceptRevision(t, st, changed)
	publishRevision(t, st, changed)
	if rows, err := st.TraceResults(ctx, "p", "revision"); err != nil || len(rows) != 0 {
		t.Fatalf("legacy score exposed on changed output: %v %v", rows, err)
	}
}

func TestClickHouseRevisionMigrationPreservesLegacyScores(t *testing.T) {
	traces, ok := traceBackends(t)["clickhouse"]
	if !ok {
		t.Skip("set EVALSI_TEST_CLICKHOUSE_URL")
	}
	c := traces.(*clickhouse)
	ctx := t.Context()
	// Recreate the pre-revision table shape, including an old populated row.
	for _, statement := range []string{
		`DROP VIEW trace_results_current`,
		`DROP VIEW trace_current`,
		`ALTER TABLE trace_results MODIFY ORDER BY(project,trace_id,policy,evaluator)`,
		`ALTER TABLE trace_results DROP COLUMN record_hash`,
		`ALTER TABLE traces DROP COLUMN record_hash, DROP COLUMN deleted`,
	} {
		if err := c.exec(ctx, statement, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	w := revisionTrace("legacy", "same", time.Now())
	params := chParams{"summary": b64(w.Trace.Summary), "record": b64(w.Trace.Record), "score": b64(revisionScores(1)[0])}
	if err := c.exec(ctx, `INSERT INTO traces(project,trace_id,service,start_ns,summary,record,version) SELECT 'p','revision','studio',1,base64Decode({summary:String}),base64Decode({record:String}),1`, params, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.exec(ctx, `INSERT INTO trace_results(project,trace_id,policy,evaluator,result,version) SELECT 'p','revision','quality','quality',base64Decode({score:String}),1`, params, nil); err != nil {
		t.Fatal(err)
	}
	var migrated TraceStore
	for i := 0; i < 2; i++ {
		var err error
		migrated, err = OpenClickHouse(ctx, ClickHouseConfig{URL: os.Getenv("EVALSI_TEST_CLICKHOUSE_URL"), Database: c.db}, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	st := open(t)
	st.UseTraces(migrated)
	assertCurrentRevisionScore(t, st, 1)
	acceptRevision(t, st, w)
	publishRevision(t, st, w)
	assertCurrentRevisionScore(t, st, 1)
	changed := revisionTrace("legacy", "changed", time.Now())
	acceptRevision(t, st, changed)
	publishRevision(t, st, changed)
	if rows, err := st.TraceResults(ctx, "p", "revision"); err != nil || len(rows) != 0 {
		t.Fatalf("legacy score exposed on changed ClickHouse output: %v %v", rows, err)
	}
}

func TestLegacyScoreExpirySurvivesProjectionReplacement(t *testing.T) {
	for _, backend := range []string{"sql", "clickhouse"} {
		for _, same := range []bool{false, true} {
			t.Run(backend+map[bool]string{false: "/changed input", true: "/same input"}[same], func(t *testing.T) {
				traces, ok := traceBackends(t)[backend]
				if !ok {
					t.Skip("set EVALSI_TEST_CLICKHOUSE_URL")
				}
				st := open(t)
				if backend == "clickhouse" {
					st.UseTraces(traces)
				}
				ctx := t.Context()
				cutoff := time.Now()
				old := revisionTrace("legacy", "old", cutoff.Add(-time.Hour))
				if backend == "clickhouse" {
					c := traces.(*clickhouse)
					params := chParams{"summary": b64(old.Trace.Summary), "record": b64(old.Trace.Record), "score": b64(revisionScores(1)[0]), "start": fmt.Sprint(old.Pending.StartNS)}
					if err := c.exec(ctx, `INSERT INTO traces(project,trace_id,service,start_ns,summary,record,version) SELECT 'p','revision','studio',{start:Int64},base64Decode({summary:String}),base64Decode({record:String}),1`, params, nil); err != nil {
						t.Fatal(err)
					}
					if err := c.exec(ctx, `INSERT INTO trace_results(project,trace_id,policy,evaluator,result,version) SELECT 'p','revision','quality','quality',base64Decode({score:String}),1`, params, nil); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := st.PutTrace(ctx, old.Trace.Summary, old.Trace.Record); err != nil {
						t.Fatal(err)
					}
					if _, err := st.db.ExecContext(ctx, `UPDATE traces SET record_hash=''`); err != nil {
						t.Fatal(err)
					}
					if _, err := st.db.ExecContext(ctx, `DELETE FROM watch_score_references`); err != nil {
						t.Fatal(err)
					}
					if _, err := st.db.ExecContext(ctx, `INSERT INTO trace_results(project,trace_id,policy,evaluator,record_hash,result) VALUES(?,?,?,?,?,?)`, "p", "revision", "quality", "quality", "", marshal(revisionScores(1)[0])); err != nil {
						t.Fatal(err)
					}
				}
				input := "new"
				if same {
					input = "old"
				}
				newer := revisionTrace("new", input, cutoff.Add(time.Hour))
				acceptRevision(t, st, newer)
				publishRevision(t, st, newer)
				if same {
					assertCurrentRevisionScore(t, st, 1)
				}
				for pass := 0; pass < 2; pass++ {
					if _, err := st.DeleteTracesBefore(ctx, cutoff); err != nil {
						t.Fatal(err)
					}
				}
				want := 0
				if same {
					want = 1
					assertCurrentRevisionScore(t, st, 1)
				}
				if backend == "clickhouse" {
					assertClickHousePayloadCount(t, traces.(*clickhouse), "trace_results FINAL", want)
				} else {
					var n int
					if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM trace_results`).Scan(&n); err != nil || n != want {
						t.Fatalf("legacy expiry lost after replacement: %d want %d: %v", n, want, err)
					}
				}
			})
		}
	}
}
