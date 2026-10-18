package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1/evalsiv1alpha1connect"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
)

// Runs on the e2e job's existing ClickHouse service, through source admission,
// the real policy engine and Python worker, and reads the feedback table back.
func TestTraceSourceClickHouse(t *testing.T) {
	endpoint := os.Getenv("EVALSI_E2E_CLICKHOUSE_URL")
	if endpoint == "" || os.Getenv("EVALSI_E2E_WORKER") == "" {
		t.Skip("set EVALSI_E2E_CLICKHOUSE_URL and EVALSI_E2E_WORKER")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	user, password := "", ""
	if u.User != nil {
		user = u.User.Username()
		password, _ = u.User.Password()
		u.User = nil
	}
	requestURL := *u
	q := requestURL.Query()
	q.Set("date_time_input_format", "best_effort")
	q.Set("wait_end_of_query", "1")
	requestURL.RawQuery = q.Encode()
	token := ""
	if user != "" {
		token = user + ":" + password
	}
	t.Setenv("EVALSI_CLICKHOUSE_SOURCE_CREDENTIAL", token)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}
	query := func(ctx context.Context, sql string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), strings.NewReader(sql))
		if err != nil {
			return nil, err
		}
		if user != "" {
			req.SetBasicAuth(user, password)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("ClickHouse HTTP %d: %.300s (%v)", resp.StatusCode, raw, err)
		}
		return raw, nil
	}
	mustQuery := func(sql string) []byte {
		t.Helper()
		raw, err := query(ctx, sql)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	db := "source_e2e_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	mustQuery("CREATE DATABASE " + db)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := query(ctx, "DROP DATABASE "+db); err != nil {
			t.Error(err)
		}
	})
	template, err := os.ReadFile("../../internal/source/clickhouse/testdata/traces_table.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustQuery(fmt.Sprintf(string(template), db, "otel_traces", "", "MergeTree()", ""))
	scoresDDL, err := os.ReadFile("../../examples/sources/clickhouse-scores.sql")
	if err != nil {
		t.Fatal(err)
	}
	mustQuery(strings.Replace(string(scoresDDL), "CREATE TABLE IF NOT EXISTS otel_traces_evalsi_scores", "CREATE TABLE IF NOT EXISTS "+db+".otel_traces_evalsi_scores", 1))
	seed := func(traceID string, started time.Time, root bool) {
		spanID, parentID, kind, status, name := "2222222222222222", "1111111111111111", "TOOL", "Error", "lookup-order"
		if root {
			spanID, parentID, kind, status, name = parentID, "", "AGENT", "Ok", "support-agent"
		}
		row := map[string]any{"TraceId": traceID, "SpanId": spanID, "ParentSpanId": parentID,
			"Timestamp": started.Format(time.RFC3339Nano), "Duration": 1000000000, "SpanName": name,
			"SpanKind": "Internal", "StatusCode": status, "ServiceName": "support",
			"ResourceAttributes": map[string]string{"evalsi.label.workflow": "support"},
			"SpanAttributes":     map[string]string{"openinference.span.kind": kind, "input.value": "question", "output.value": "answer"}}
		raw, _ := json.Marshal(row)
		mustQuery("INSERT INTO " + db + ".otel_traces FORMAT JSONEachRow\n" + string(raw))
	}
	const history = "4bf92f3577b34da6a3ce929d0e0e4736"
	const pending = "4bf92f3577b34da6a3ce929d0e0e4737"
	old, recent := time.Now().UTC().Add(-90*time.Minute), time.Now().UTC().Add(-time.Minute)
	seed(history, old, true)
	seed(history, old.Add(time.Second), false)
	seed(pending, recent.Add(time.Second), false) // root has not ended/exported
	e := start(t, func(c *config.Config) {
		c.Sources.AllowHosts = []string{u.Hostname()}
		if token != "" {
			c.Credentials.Grants = []credentials.Grant{{Env: "EVALSI_CLICKHOUSE_SOURCE_CREDENTIAL", Projects: []string{"default"}, Hosts: []string{u.Hostname()}, AllowHTTP: u.Scheme == "http"}}
		}
	})
	monitor := evalsiv1alpha1connect.NewMonitorServiceClient(h2cClient(), e.base, connect.WithGRPC())
	if _, err := monitor.ApplyPolicy(ctx, connect.NewRequest(&evalsiv1alpha1.ApplyPolicyRequest{Policy: &evalsiv1alpha1.OnlineEvalPolicy{
		Name: "clickhouse-check", Stages: []*evalsiv1alpha1.CascadeStage{{Evaluators: []*evalsiv1alpha1.EvaluatorRef{{Ref: "tool-errors"}}}},
	}})); err != nil {
		t.Fatal(err)
	}
	src := &evalsiv1alpha1.TraceSource{
		Name: "clickhouse-check", Connector: "clickhouse", Endpoint: u.String(), Locations: []string{db + ".otel_traces"},
		Policies: []string{"clickhouse-check"}, Backfill: &evalsiv1alpha1.Backfill{Since: durationpb.New(2 * time.Hour)},
		Poll: &evalsiv1alpha1.Poll{Interval: durationpb.New(time.Second)}, WriteBack: &evalsiv1alpha1.WriteBack{Enabled: true},
	}
	if token != "" {
		src.Credentials = &evalsiv1alpha1.SourceCredentials{Env: "EVALSI_CLICKHOUSE_SOURCE_CREDENTIAL"}
	}
	sources := evalsiv1alpha1connect.NewSourceServiceClient(h2cClient(), e.base, connect.WithGRPC())
	if _, err := sources.ApplySource(ctx, connect.NewRequest(&evalsiv1alpha1.ApplySourceRequest{Source: src})); err != nil {
		t.Fatal(err)
	}
	status := func() *evalsiv1alpha1.SourceStatus {
		got, err := sources.GetSource(ctx, connect.NewRequest(&evalsiv1alpha1.GetSourceRequest{Name: src.Name}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.GetSource().GetStatus()
	}
	waitFor := func(n int64) {
		t.Helper()
		var st *evalsiv1alpha1.SourceStatus
		for {
			st = status()
			if st.GetPulled() == n && st.GetScored() == n && st.GetPhase() == evalsiv1alpha1.SourcePhase_SOURCE_PHASE_TAILING {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("waiting for %d traces: %v (%v)", n, st, ctx.Err())
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	waitFor(1)
	if raw := mustQuery("SELECT count() FROM " + db + ".otel_traces_evalsi_scores FINAL WHERE TraceId='" + pending + "'"); strings.TrimSpace(string(raw)) != "0" {
		t.Fatal("rootless trace was scored")
	}
	seed(pending, recent, true)
	waitFor(2)
	feedback := mustQuery("SELECT TraceId, Metric, Score, Policy FROM " + db + ".otel_traces_evalsi_scores FINAL ORDER BY TraceId FORMAT JSONEachRow")
	decoder := json.NewDecoder(strings.NewReader(string(feedback)))
	for _, traceID := range []string{history, pending} {
		var row struct {
			TraceId, Metric, Policy string
			Score                   float64
		}
		if err := decoder.Decode(&row); err != nil || row.TraceId != traceID || row.Metric != "tool-errors" || row.Score != 1 || row.Policy != src.Name {
			t.Fatalf("feedback: %s (%v)", feedback, err)
		}
		got, err := evalsiv1alpha1connect.NewTraceServiceClient(h2cClient(), e.base, connect.WithGRPC()).GetTrace(ctx, connect.NewRequest(&evalsiv1alpha1.GetTraceRequest{TraceId: traceID}))
		if err != nil || got.Msg.GetRecord().GetOutput().GetText() != "answer" || len(got.Msg.GetRecord().GetTrajectory().GetSteps()) != 2 {
			t.Fatalf("pulled record: %v (%v)", got, err)
		}
	}
	time.Sleep(3 * time.Second) // repeated polls must not create more results
	if st := status(); st.GetPulled() != 2 || st.GetScored() != 2 || st.GetLastError() != "" {
		t.Fatalf("duplicate or failed polling: %v", st)
	}
}
