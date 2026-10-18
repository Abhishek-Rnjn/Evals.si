package clickhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

var epoch = time.Date(2026, 10, 9, 12, 0, 0, 123, time.UTC)

func spec(endpoint string) *evalsiv1alpha1.TraceSource {
	return &evalsiv1alpha1.TraceSource{Connector: "clickhouse", Endpoint: endpoint, Locations: []string{"observability.otel_traces"}, WriteBack: &evalsiv1alpha1.WriteBack{Enabled: true}}
}

func fixtureRows() []spanRow {
	root := spanRow{TraceID: traceID, SpanID: "1111111111111111", StartNS: strconv.FormatInt(epoch.UnixNano(), 10), DurationNS: "3000000000",
		SpanName: "support-agent", SpanKind: "Internal", StatusCode: "Ok", ServiceName: "support",
		ResourceAttributes: map[string]string{"evalsi.label.workflow": "support", "tenant": "acme"},
		SpanAttributes:     map[string]string{"openinference.span.kind": "AGENT", "input.value": "Where is my order?", "output.value": "It ships tomorrow.", "session.id": "thread-1"}}
	llm := spanRow{TraceID: traceID, SpanID: "2222222222222222", ParentID: root.SpanID, StartNS: strconv.FormatInt(epoch.Add(time.Second).UnixNano(), 10), DurationNS: "1000000000",
		SpanName: "model", SpanKind: "Client", StatusCode: "Ok", ScopeName: "test-scope", ScopeVersion: "1.0", TraceState: "test=state",
		SpanAttributes: map[string]string{"openinference.span.kind": "LLM", "llm.model_name": "fixture-model", "llm.token_count.prompt": "21", "llm.token_count.completion": "4"}}
	tool := spanRow{TraceID: traceID, SpanID: "3333333333333333", ParentID: root.SpanID, StartNS: strconv.FormatInt(epoch.Add(2*time.Second).UnixNano(), 10), DurationNS: "500000000",
		SpanName: "lookup-order", SpanKind: "Internal", StatusCode: "Error", StatusMessage: "synthetic timeout",
		SpanAttributes: map[string]string{"openinference.span.kind": "TOOL", "input.value": `{"order":"42"}`},
		EventTimes:     []string{strconv.FormatInt(epoch.Add(2500*time.Millisecond).UnixNano(), 10)}, EventNames: []string{"exception"}, EventAttributes: []map[string]string{{"exception.message": "synthetic timeout"}},
		LinkTraceIDs: []string{traceID}, LinkSpanIDs: []string{llm.SpanID}, LinkStates: []string{"test=link"}, LinkAttributes: []map[string]string{{"link": "value"}}}
	return []spanRow{root, llm, tool}
}

func fakeConnector(t *testing.T, handler http.HandlerFunc) *connector {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := Factory(spec(srv.URL), "reader:pass:with:colon", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return c.(*connector)
}

func TestFactoryRefusesUnsafeConfiguration(t *testing.T) {
	for _, location := range []string{"", "otel_traces", "a.b.c", "db.table;DROP TABLE x", "db.`table`", "db.table-name", "db. table"} {
		s := spec("https://example.com")
		s.Locations = []string{location}
		if _, err := Factory(s, "", http.DefaultClient); err == nil {
			t.Errorf("accepted location %q", location)
		}
	}
	for _, endpoint := range []string{"file:///tmp/x", "https://u:p@example.com", "https://example.com?query=DROP", "https://example.com?", "https://example.com#fragment"} {
		if _, err := Factory(spec(endpoint), "", http.DefaultClient); err == nil {
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
	for _, token := range []string{"reader", ":password"} {
		if _, err := Factory(spec("https://example.com"), token, http.DefaultClient); err == nil {
			t.Error("accepted malformed credential")
		}
	}
	for _, token := range []string{"", "reader:", source.ValidationToken} {
		if _, err := Factory(spec("https://example.com/proxy/"), token, http.DefaultClient); err != nil {
			t.Fatal(err)
		}
	}
	s := spec("https://example.com")
	s.Variant = "json"
	if _, err := Factory(s, "", http.DefaultClient); err == nil {
		t.Fatal("accepted unsupported schema variant")
	}
	s.Variant, s.Locations = "", []string{"db.a", "db.b"}
	if _, err := Factory(s, "", http.DefaultClient); err == nil {
		t.Fatal("accepted multiple trace tables")
	}
}

func TestListingUsesBoundKeysetPages(t *testing.T) {
	secondID := strings.Repeat("f", 32)
	calls := 0
	c := fakeConnector(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		u, p, ok := r.BasicAuth()
		if !ok || u != "reader" || p != "pass:with:colon" || r.Method != http.MethodPost {
			t.Error("missing HTTP basic auth or wrong method")
		}
		sql, _ := io.ReadAll(r.Body)
		q := r.URL.Query()
		if q.Get("param_since") != strconv.FormatInt(epoch.UnixNano(), 10) || q.Get("param_limit") != "1" || !strings.Contains(string(sql), "{since:Int64}") || !strings.Contains(string(sql), "`observability`.`otel_traces`") || q.Get("wait_end_of_query") != "1" {
			t.Error("missing bounded/parameterized query")
		}
		id := traceID
		if calls == 2 {
			id = secondID
			if q.Get("param_id") != traceID || q.Get("param_start") != strconv.FormatInt(epoch.UnixNano(), 10) || !strings.Contains(string(sql), "{id:String}") {
				t.Error("cursor did not bind timestamp and trace ID")
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ID": id, "StartNS": strconv.FormatInt(epoch.UnixNano(), 10), "Digest": "digest"})
	})
	first, err := c.List(t.Context(), source.ListRequest{Since: epoch, Limit: 1})
	if err != nil || len(first.Infos) != 1 || first.Infos[0].ID != traceID || first.Next == "" {
		t.Fatalf("first page: %v, %v", first, err)
	}
	second, err := c.List(t.Context(), source.ListRequest{Since: epoch, Limit: 1, Cursor: first.Next})
	if err != nil || len(second.Infos) != 1 || second.Infos[0].ID != secondID {
		t.Fatalf("second page: %v, %v", second, err)
	}
	for _, req := range []source.ListRequest{{Since: epoch, Cursor: "not-a-cursor"}, {Since: epoch.Add(time.Second), Cursor: first.Next}} {
		if _, err := c.List(t.Context(), req); err == nil {
			t.Error("accepted invalid or mismatched cursor")
		}
	}
	if calls != 2 {
		t.Fatal("invalid cursor made a remote request")
	}
}

func TestFetchedCollectorSpansBecomeARecord(t *testing.T) {
	c := fakeConnector(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("param_trace") != traceID {
			t.Error("trace ID was not bound")
		}
		for _, row := range fixtureRows() {
			_ = json.NewEncoder(w).Encode(row)
		}
	})
	traces, err := c.Fetch(t.Context(), []source.Info{{ID: strings.ToUpper(traceID)}})
	if err != nil || len(traces) != 1 {
		t.Fatalf("fetch: %v, %v", traces, err)
	}
	rec, info := ingest.ToRecord(traces[0])
	if rec.GetInput().GetText() != "Where is my order?" || rec.GetOutput().GetText() != "It ships tomorrow." || rec.GetUsage().GetInputTokens() != 21 || rec.GetUsage().GetOutputTokens() != 4 {
		t.Fatalf("lost record fields: %v", rec)
	}
	if !info.Error || len(info.Tools) != 1 || info.Tools[0] != "lookup-order" || info.Service != "support" || info.Labels["workflow"] != "support" || rec.GetTrajectory().GetSessionId() != "thread-1" {
		t.Fatalf("lost trace facts: %+v", info)
	}
	if got := traces[0].Spans[2].Span; len(got.Events) != 1 || len(got.Links) != 1 || got.Events[0].TimeUnixNano != uint64(epoch.Add(2500*time.Millisecond).UnixNano()) || got.Links[0].TraceState != "test=link" {
		t.Fatal("lost nested event/link columns")
	}
	if traces[0].Spans[1].Span.TraceState != "test=state" || traces[0].Spans[0].Resource[source.AttrSourceTraceID] != traceID {
		t.Fatal("lost trace identity/state")
	}
}

func TestMalformedOrConflictingSpansFailClosed(t *testing.T) {
	for _, mutate := range []func(*spanRow){
		func(r *spanRow) { r.TraceID = strings.Repeat("0", 32) },
		func(r *spanRow) { r.SpanID = "bad" },
		func(r *spanRow) { r.ParentID = "bad" },
		func(r *spanRow) { r.StartNS = "-1" },
		func(r *spanRow) { r.DurationNS = "18446744073709551615" },
		func(r *spanRow) { r.EventNames = []string{"unmatched"} },
		func(r *spanRow) { r.StatusCode = "success-ish" },
		func(r *spanRow) { r.SpanKind = "agent" },
	} {
		r := fixtureRows()[0]
		mutate(&r)
		if _, err := toTrace(traceID, []spanRow{r}); err == nil {
			t.Error("accepted malformed span")
		}
	}
	rows := fixtureRows()
	if _, err := toTrace(traceID, rows[1:]); err == nil {
		t.Fatal("rootless trace was silently scored")
	}
	if trace, err := toTrace(traceID, append(rows, rows[0])); err != nil || len(trace.Spans) != 3 {
		t.Fatal("identical duplicate was not deduplicated", err)
	}
	conflict := rows[0]
	conflict.SpanName = "changed"
	if _, err := toTrace(traceID, append(rows, conflict)); err == nil {
		t.Fatal("conflicting span rows were silently scored")
	}
	c := fakeConnector(t, func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i <= maxSpansPerTrace; i++ {
			_, _ = io.WriteString(w, "{}\n")
		}
	})
	if _, err := c.Fetch(t.Context(), []source.Info{{ID: traceID}}); err == nil {
		t.Fatal("oversized trace was truncated")
	}
}

func TestHTTPFailuresAndTruncatedResults(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		c := fakeConnector(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "17")
			w.WriteHeader(status)
		})
		_, err := c.List(t.Context(), source.ListRequest{Since: epoch})
		var retry *source.RetryAfterError
		if !errors.As(err, &retry) || retry.After != 17*time.Second {
			t.Fatalf("retry-after: %v", err)
		}
	}
	c := fakeConnector(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"ID":`) })
	if _, err := c.List(t.Context(), source.ListRequest{Since: epoch}); err == nil {
		t.Fatal("truncated query result was accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.List(ctx, source.ListRequest{Since: epoch}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestWriteBackTypedReplacementAndPartialFailure(t *testing.T) {
	var written []map[string]any
	fail := false
	c := fakeConnector(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if q := r.URL.Query().Get("query"); !strings.Contains(q, "`otel_traces_evalsi_scores`") || strings.Contains(q, "quote'policy") {
			t.Error("unsafe insert query")
		}
		var row map[string]any
		_ = json.NewDecoder(r.Body).Decode(&row)
		written = append(written, row)
	})
	base := source.Score{TraceID: traceID, Policy: "quote'policy", Metric: "metric", Evaluator: "fixture", Value: 0.25, Rationale: "old"}
	first, err := c.WriteBack(t.Context(), []source.Score{base}, nil)
	if err != nil || len(first) != 1 {
		t.Fatal(err)
	}
	base.Value, base.Rationale = true, ""
	second, err := c.WriteBack(t.Context(), []source.Score{base}, map[store.WriteKey]store.SourceWrite{base.Key(): first[0]})
	if err != nil || second[0].RemoteID != first[0].RemoteID || written[1]["Passed"] != true || written[1]["Score"] != float64(1) || written[1]["Rationale"] != "" || written[1]["Value"] != nil {
		t.Fatalf("typed replacement: %v", err)
	}
	base.Value = "label"
	if _, err := c.WriteBack(t.Context(), []source.Score{base}, nil); err != nil || written[2]["Value"] != "label" || written[2]["Score"] != nil || written[2]["Passed"] != nil {
		t.Fatal("old typed fields were not cleared", err)
	}
	base.Value = math.NaN()
	if _, err := c.WriteBack(t.Context(), []source.Score{base}, nil); err == nil {
		t.Fatal("accepted NaN")
	}
	base.Value = 1.0
	fail = true
	if done, err := c.WriteBack(t.Context(), []source.Score{base}, nil); err == nil || len(done) != 0 {
		t.Fatal("failed insert was acknowledged")
	}
	fail = false
	bad := base
	bad.TraceID = "invalid"
	if done, err := c.WriteBack(t.Context(), []source.Score{base, bad}, nil); err == nil || len(done) != 1 {
		t.Fatal("partial progress was lost")
	}
}

// This test runs in ordinary Go CI against its existing ClickHouse service.
// An isolated database is created from the pinned upstream schema and dropped.
func TestAgainstLiveClickHouse(t *testing.T) {
	endpoint := os.Getenv("EVALSI_TEST_CLICKHOUSE_URL")
	if endpoint == "" {
		t.Skip("set EVALSI_TEST_CLICKHOUSE_URL for live ClickHouse verification")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	token := ""
	if u.User != nil {
		pass, _ := u.User.Password()
		token = u.User.Username() + ":" + pass
		u.User = nil
	}
	db := "source_test_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	s := spec(u.String())
	s.Locations = []string{db + ".otel_traces"}
	conn, err := Factory(s, token, &http.Client{Timeout: 45 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c := conn.(*connector)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	mustQuery := func(sql string, params url.Values, data []byte) []byte {
		t.Helper()
		raw, err := c.query(ctx, sql, params, data)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	mustQuery("CREATE DATABASE "+db, nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.query(ctx, "DROP DATABASE "+db, nil, nil); err != nil {
			t.Error(err)
		}
	})
	template, err := os.ReadFile("testdata/traces_table.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustQuery(fmt.Sprintf(string(template), db, "otel_traces", "", "MergeTree()", ""), nil, nil)
	scoresDDL, err := os.ReadFile("../../../examples/sources/clickhouse-scores.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustQuery(strings.Replace(string(scoresDDL), "CREATE TABLE IF NOT EXISTS otel_traces_evalsi_scores", "CREATE TABLE IF NOT EXISTS "+c.scores, 1), nil, nil)
	insert := func(rows []spanRow) {
		for _, row := range rows {
			ns, _ := strconv.ParseInt(row.StartNS, 10, 64)
			events := make([]string, len(row.EventTimes))
			for i, ns := range row.EventTimes {
				n, _ := strconv.ParseInt(ns, 10, 64)
				events[i] = time.Unix(0, n).UTC().Format(time.RFC3339Nano)
			}
			data, _ := json.Marshal(map[string]any{"Timestamp": time.Unix(0, ns).UTC().Format(time.RFC3339Nano), "TraceId": row.TraceID, "SpanId": row.SpanID,
				"ParentSpanId": row.ParentID, "SpanName": row.SpanName, "SpanKind": row.SpanKind, "ServiceName": row.ServiceName,
				"SpanAttributes": row.SpanAttributes, "ResourceAttributes": row.ResourceAttributes, "Duration": row.DurationNS,
				"StatusCode": row.StatusCode, "StatusMessage": row.StatusMessage, "TraceState": row.TraceState,
				"ScopeName": row.ScopeName, "ScopeVersion": row.ScopeVersion,
				"Events.Timestamp": events, "Events.Name": row.EventNames, "Events.Attributes": row.EventAttributes,
				"Links.TraceId": row.LinkTraceIDs, "Links.SpanId": row.LinkSpanIDs, "Links.TraceState": row.LinkStates, "Links.Attributes": row.LinkAttributes})
			mustQuery("INSERT INTO "+c.table+" FORMAT JSONEachRow", nil, append(data, '\n'))
		}
	}
	rows := fixtureRows()
	// Start with a child only: no completed root, hence no eligible trace.
	insert(rows[1:])
	listed, err := c.List(ctx, source.ListRequest{Since: epoch, Limit: 1})
	if err != nil || len(listed.Infos) != 0 {
		t.Fatalf("rootless trace listed: %v, %v", listed, err)
	}
	insert(rows[:1])
	// Check the actual fetch query against the pinned Collector schema: wrapping
	// TraceId in lower() disables its bloom-filter index and scans retained data.
	plan := mustQuery("EXPLAIN indexes=1 "+c.fetchQuery(), url.Values{"trace": {traceID}}, nil)
	if !strings.Contains(string(plan), "idx_trace_id") {
		t.Fatalf("fetch does not use the trace-ID skipping index: %s", plan)
	}
	listed, err = c.List(ctx, source.ListRequest{Since: epoch, Limit: 1})
	if err != nil || len(listed.Infos) != 1 || listed.Next == "" {
		t.Fatalf("list: %v, %v", listed, err)
	}
	digest := listed.Infos[0].Digest
	insert(rows[:1]) // exporter retry is identical
	again, err := c.List(ctx, source.ListRequest{Since: epoch, Limit: 1})
	if err != nil || again.Infos[0].Digest != digest {
		t.Fatal("identical insert changed digest", err)
	}
	fetched, err := c.Fetch(ctx, listed.Infos)
	if err != nil || len(fetched) != 1 || len(fetched[0].Spans) != 3 {
		t.Fatalf("fetch: %v, %v", fetched, err)
	}
	if got := fetched[0].Spans[2].Span; len(got.Events) != 1 || len(got.Links) != 1 || got.Events[0].TimeUnixNano != uint64(epoch.Add(2500*time.Millisecond).UnixNano()) {
		t.Fatal("nested fields did not round-trip through ClickHouse")
	}
	tooNew, err := c.List(ctx, source.ListRequest{Since: epoch.Add(time.Second)})
	if err != nil || len(tooNew.Infos) != 0 {
		t.Fatal("children after Since incorrectly included a trace whose root predates it", err)
	}
	last := rows[2]
	last.SpanID = "4444444444444444"
	last.StartNS = strconv.FormatInt(epoch.Add(-time.Second).UnixNano(), 10)
	insert([]spanRow{last})
	again, err = c.List(ctx, source.ListRequest{Since: epoch, Limit: 1})
	if err != nil || again.Infos[0].Digest == digest || !again.Infos[0].Started.Equal(epoch) {
		t.Fatal("late child did not change digest", err)
	}
	root2 := rows[0]
	root2.TraceID = strings.Repeat("f", 32)
	insert([]spanRow{root2})
	next, err := c.List(ctx, source.ListRequest{Since: epoch, Limit: 1, Cursor: listed.Next})
	if err != nil || len(next.Infos) != 1 || next.Infos[0].ID != root2.TraceID {
		t.Fatalf("equal-time keyset page: %v, %v", next, err)
	}
	score := source.Score{TraceID: traceID, Policy: "p", Metric: "m", Evaluator: "fixture", Value: 0.25, Rationale: "old comment"}
	var remote string
	for _, value := range []any{0.25, true, "label"} {
		score.Value = value
		written, err := c.WriteBack(ctx, []source.Score{score}, nil)
		if err != nil || len(written) != 1 || (remote != "" && remote != written[0].RemoteID) {
			t.Fatalf("feedback write: %v, %v", written, err)
		}
		remote, score.Rationale = written[0].RemoteID, ""
	}
	raw := mustQuery("SELECT Score, Value, Passed, Rationale FROM "+c.scores+" FINAL FORMAT JSONEachRow", nil, nil)
	got, err := decodeRows[map[string]any](raw)
	if err != nil || len(got) != 1 || got[0]["Value"] != "label" || got[0]["Score"] != nil || got[0]["Passed"] != nil || got[0]["Rationale"] != "" {
		t.Fatalf("current score: %s, %v", raw, err)
	}
	// The runtime account needs no DDL, no access to unrelated databases, and
	// no SELECT on the score table. Setup remains an operator action.
	reader := "reader_" + db
	mustQuery("CREATE USER "+reader+" IDENTIFIED WITH sha256_password BY 'fixture-password'", nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.query(ctx, "DROP USER "+reader, nil, nil); err != nil {
			t.Error(err)
		}
	})
	mustQuery("GRANT SELECT ON "+db+".otel_traces TO "+reader, nil, nil)
	mustQuery("GRANT INSERT ON "+db+".otel_traces_evalsi_scores TO "+reader, nil, nil)
	limited, err := Factory(s, reader+":fixture-password", c.client)
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := limited.List(ctx, source.ListRequest{Since: epoch, Limit: 1})
	if err != nil || len(eligible.Infos) != 1 {
		t.Fatal("SELECT-only trace access failed", err)
	}
	if _, err := limited.Fetch(ctx, eligible.Infos); err != nil {
		t.Fatal(err)
	}
	if _, err := limited.WriteBack(ctx, []source.Score{score}, nil); err != nil {
		t.Fatal("INSERT-only score access failed", err)
	}
	if _, err := limited.(*connector).query(ctx, "SELECT * FROM "+c.scores, nil, nil); err == nil {
		t.Fatal("limited account could read the score table")
	}
	t.Log("live Collector Map schema, root completion, duplicate/late-span digest, keyset pages and typed score replacement passed")
}
