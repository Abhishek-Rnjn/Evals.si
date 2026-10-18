package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type retentionTestTransport struct {
	base                     http.RoundTripper
	failPrefix               string
	maxURI, maxBatch, staged int
}

func (r *retentionTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if n := len(req.URL.RequestURI()); n > r.maxURI {
		r.maxURI = n
	}
	if r.maxURI > 4096 {
		return nil, fmt.Errorf("retention URI exceeds fixture limit: %d", r.maxURI)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	query := req.URL.Query().Get("query")
	if query == "" {
		query = string(body)
	}
	if strings.HasPrefix(query, "INSERT INTO trace_score_references") {
		n := bytes.Count(body, []byte{'\n'})
		r.staged += n
		r.maxBatch = max(r.maxBatch, n)
	}
	if r.failPrefix != "" && strings.HasPrefix(query, r.failPrefix) {
		r.failPrefix = ""
		return nil, errors.New("fixture: cleanup failed after tombstone")
	}
	return r.base.RoundTrip(req)
}

func TestClickHouseRetentionStagesBoundedCompleteReferences(t *testing.T) {
	st, c := reviewClickHouse(t)
	ctx := t.Context()
	cutoff := time.Now()
	expired := revisionTrace("expired", "old", cutoff.Add(-time.Hour))
	acceptRevision(t, st, expired)
	publishRevision(t, st, expired)
	a := revisionTrace("a", "live", cutoff.Add(time.Hour))
	b := revisionTrace("b", "live", a.Trace.Summary.GetStartTime().AsTime())
	b.Trace.Summary.Service = "new summary"
	b.Pending.SnapshotHash = SnapshotHash(b.Trace.Summary, b.Trace.Record)
	acceptRevision(t, st, a)
	acceptRevision(t, st, b)
	publishRevision(t, st, a)
	if err := st.PutWatchResults(ctx, PolicyLease{}, a.Pending, "independent", revisionScores(1)); err != nil {
		t.Fatal(err)
	}
	if err := st.AckWatchPending(ctx, PolicyLease{}, a.Pending); err != nil {
		t.Fatal(err)
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// These sort ahead of the real protected binding, placing it after several
	// transfer batches. None has a result or an expiring trace.
	for i := 0; i < 1600; i++ {
		if err := retainWatchScore(ctx, tx, "a", fmt.Sprintf("unscored-%04d", i), strings.Repeat("b", 64), cutoff.Add(time.Hour).UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	transport := &retentionTestTransport{base: http.DefaultTransport}
	c.client.Transport = transport
	if _, err := st.DeleteTracesBefore(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	publishRevision(t, st, b)
	assertCurrentRevisionScore(t, st, 1)
	if transport.staged < 1600 || transport.maxBatch > retentionBatchSize {
		t.Fatalf("unbounded or incomplete staging: %+v", transport)
	}
	staged := transport.staged
	if _, err := st.DeleteTracesBefore(ctx, cutoff); err != nil {
		t.Fatal(err)
	}
	if transport.staged != staged {
		t.Fatalf("unchanged reference population restaged: %d -> %d", staged, transport.staged)
	}
}

func TestClickHouseRetentionPartialFailureAllowsIdenticalReacceptance(t *testing.T) {
	for _, phase := range []string{"INSERT INTO trace_retention_requests", "DELETE FROM traces "} {
		t.Run(phase, func(t *testing.T) {
			st, c := reviewClickHouse(t)
			ctx := t.Context()
			cutoff := time.Now()
			w := revisionTrace("ready", "same input", cutoff.Add(-time.Hour))
			acceptRevision(t, st, w)
			publishRevision(t, st, w)
			if err := st.PutWatchResults(ctx, PolicyLease{}, w.Pending, "quality", revisionScores(0)); err != nil {
				t.Fatal(err)
			}
			c.client.Transport = &retentionTestTransport{base: http.DefaultTransport, failPrefix: phase}
			if _, err := st.DeleteTracesBefore(ctx, cutoff); err == nil {
				t.Fatal("expected partial cleanup failure")
			}
			w.Pending.Payload = []byte("identical reacceptance after partial retention")
			acceptRevision(t, st, w)
			publishRevision(t, st, w)
			if _, _, err := st.GetTrace(ctx, "p", "revision"); err != nil {
				t.Fatalf("accepted trace stayed hidden after partial retention: %v", err)
			}
			if results, err := st.TraceResults(ctx, "p", "revision"); err != nil || len(results) != 0 {
				t.Fatalf("reacceptance revived expired scores: %v %v", results, err)
			}
		})
	}
}

type publicationTimeoutTransport struct {
	base  http.RoundTripper
	query string
	body  []byte
}

func (r *publicationTimeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	query := req.URL.Query().Get("query")
	if r.query == "" && strings.HasPrefix(query, "INSERT INTO traces ") {
		r.query = query
		var err error
		r.body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		return nil, context.DeadlineExceeded
	}
	return r.base.RoundTrip(req)
}

func TestRetentionFencesSupersededTimedOutPublication(t *testing.T) {
	for _, sameStream := range []bool{false, true} {
		t.Run(map[bool]string{false: "independent streams", true: "same stream"}[sameStream], func(t *testing.T) {
			st, c := reviewClickHouse(t)
			ctx := t.Context()
			cutoff := time.Now()
			old := revisionTrace("old", "expired", cutoff.Add(-time.Hour))
			acceptRevision(t, st, old)
			transport := &publicationTimeoutTransport{base: http.DefaultTransport}
			c.client.Transport = transport
			if _, err := st.PutWatchTraces(ctx, PolicyLease{}, []WatchTraceWrite{old}); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected uncertain publication: %v", err)
			}
			newer := revisionTrace("new", "unexpired", cutoff.Add(time.Hour))
			if sameStream {
				newer.Pending.Stream = old.Pending.Stream
			}
			acceptRevision(t, st, newer)
			// The metadata wrapper has no in-memory history of the old attempt.
			restarted := newStore(st.db)
			restarted.UseTraces(c)
			if _, err := restarted.DeleteTracesBefore(ctx, cutoff); err != nil {
				t.Fatal(err)
			}
			if err := c.exec(ctx, transport.query, nil, bytes.NewReader(transport.body)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.GetTrace(ctx, "p", "revision"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("superseded late publication escaped retention: %v", err)
			}
			publishRevision(t, restarted, newer)
			_, record, err := st.GetTrace(ctx, "p", "revision")
			if err != nil || record.GetOutput().GetText() != "unexpired" {
				t.Fatalf("fence blocked surviving work: %v %v", record, err)
			}
		})
	}
}

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
