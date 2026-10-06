package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

// ClickHouseConfig points the trace store at ClickHouse.
type ClickHouseConfig struct {
	// The HTTP interface, for example http://clickhouse:8123 (https:// for
	// TLS). It may carry credentials: http://user:password@host:8123.
	URL      string `json:"url"`
	Database string `json:"database,omitempty"`
	User     string `json:"user,omitempty"`
	// Environment variable holding the password.
	PasswordEnv string `json:"password_env,omitempty"`
	// Per-request timeout; default 30s.
	Timeout string `json:"timeout,omitempty"`
	// For https://: a private CA, and a client certificate (mutual TLS).
	TLS *auth.ClientTLSConfig `json:"tls,omitempty"`
}

// clickhouse keeps traces and their online scores in ClickHouse, over its
// HTTP interface. Tables are ReplacingMergeTrees keyed like the SQL tables, so
// re-putting a trace replaces it; reads use FINAL. Traces are partitioned by
// month of their start, results follow their trace. Values travel as query
// parameters ({name:Type}), never in the SQL text, and protobuf blobs travel
// base64-encoded and are stored raw.
type clickhouse struct {
	base     *url.URL
	db       string
	user     string
	password string
	client   *http.Client
	// Set once the database exists; statements before that run in the server's default.
	ready bool
}

var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// OpenClickHouse connects to ClickHouse and creates the database and tables.
func OpenClickHouse(ctx context.Context, cfg ClickHouseConfig, password string) (TraceStore, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("clickhouse: url %q must be http(s)://host:port", cfg.URL)
	}
	db := cfg.Database
	if db == "" {
		db = "evalsi"
	}
	if !identRE.MatchString(db) {
		return nil, fmt.Errorf("clickhouse: database %q is not a plain identifier", db)
	}
	timeout := 30 * time.Second
	if cfg.Timeout != "" {
		if timeout, err = time.ParseDuration(cfg.Timeout); err != nil {
			return nil, fmt.Errorf("clickhouse: timeout: %w", err)
		}
	}
	user := cfg.User
	if u.User != nil {
		// Credentials in the URL (http://user:pass@host:8123).
		user = u.User.Username()
		if p, ok := u.User.Password(); ok && password == "" {
			password = p
		}
		u.User = nil
	}
	hc := &http.Client{Timeout: timeout}
	if cfg.TLS != nil {
		tc, err := auth.ClientTLS(cfg.TLS)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: %w", err)
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = tc
		hc.Transport = tr
	}
	c := &clickhouse{base: u, db: db, user: user, password: password, client: hc}
	if err := c.exec(ctx, `CREATE DATABASE IF NOT EXISTS `+db, nil, nil); err != nil {
		return nil, err
	}
	c.ready = true
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS ` + db + `.traces (
			project String, trace_id String, service LowCardinality(String), start_ns Int64,
			summary String CODEC(ZSTD), record String CODEC(ZSTD), version Int64
		) ENGINE = ReplacingMergeTree(version)
		PARTITION BY toYYYYMM(fromUnixTimestamp64Nano(start_ns))
		ORDER BY (project, trace_id)`,
		`CREATE TABLE IF NOT EXISTS ` + db + `.trace_results (
			project String, trace_id String, policy String, evaluator String,
			result String CODEC(ZSTD), version Int64
		) ENGINE = ReplacingMergeTree(version)
		ORDER BY (project, trace_id, policy, evaluator)`,
	} {
		if err := c.exec(ctx, stmt, nil, nil); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *clickhouse) Close() error { return nil }

type chParams map[string]string

func chArray(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
	}
	return "[" + strings.Join(q, ",") + "]"
}

// do sends one statement. With body, the statement goes in the URL and the
// body carries the data (inserts).
func (c *clickhouse) do(ctx context.Context, query string, params chParams, body io.Reader) (*http.Response, error) {
	u := *c.base
	q := u.Query()
	if c.ready {
		q.Set("database", c.db)
	}
	q.Set("output_format_json_quote_64bit_integers", "0")
	q.Set("date_time_input_format", "best_effort")
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	var req *http.Request
	var err error
	if body != nil {
		q.Set("query", query)
		u.RawQuery = q.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	} else {
		u.RawQuery = q.Encode()
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(query))
	}
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.Header.Set("X-ClickHouse-User", c.user)
	}
	if c.password != "" {
		req.Header.Set("X-ClickHouse-Key", c.password)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		resp.Body.Close()
		return nil, fmt.Errorf("clickhouse: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (c *clickhouse) exec(ctx context.Context, query string, params chParams, body io.Reader) error {
	resp, err := c.do(ctx, query, params, body)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// query runs a SELECT and decodes each JSONEachRow line with fn.
func (c *clickhouse) query(ctx context.Context, query string, params chParams, fn func(line []byte) error) error {
	resp, err := c.do(ctx, query+" FORMAT JSONEachRow", params, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}

func b64(m proto.Message) string { return base64.StdEncoding.EncodeToString(marshal(m)) }

func unb64(s string, m proto.Message) error {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return proto.Unmarshal(raw, m)
}

func (c *clickhouse) PutTrace(ctx context.Context, summary *evalsiv1alpha1.TraceSummary, record *evalsiv1alpha1.Record) error {
	row, _ := json.Marshal(map[string]any{
		"project": summary.GetProject(), "trace_id": summary.GetTraceId(), "service": summary.GetService(),
		"start_ns": summary.GetStartTime().AsTime().UnixNano(), "summary": b64(summary), "record": b64(record),
		"version": time.Now().UnixNano(),
	})
	return c.exec(ctx, `INSERT INTO traces SELECT project, trace_id, service, start_ns,
		base64Decode(summary), base64Decode(record), version
		FROM input('project String, trace_id String, service String, start_ns Int64, summary String, record String, version Int64')
		FORMAT JSONEachRow`, nil, bytes.NewReader(row))
}

func projectsWhere(column string, projects []string, params chParams) string {
	if projects == nil {
		return "1"
	}
	params["projects"] = chArray(projects)
	return "has({projects:Array(String)}, " + column + ")"
}

func (c *clickhouse) ListTraces(ctx context.Context, f TraceFilter, pageSize int, pageToken string) ([]*evalsiv1alpha1.TraceSummary, string, error) {
	offset := 0
	if pageToken != "" {
		var err error
		if offset, err = strconv.Atoi(pageToken); err != nil || offset < 0 {
			return nil, "", errors.New("store: bad page token")
		}
	}
	params := chParams{"service": f.Service, "limit": strconv.Itoa(pageSize + 1), "offset": strconv.Itoa(offset)}
	where := projectsWhere("project", f.Projects, params)
	var out []*evalsiv1alpha1.TraceSummary
	ids := map[[2]string]*evalsiv1alpha1.TraceSummary{}
	err := c.query(ctx, `SELECT project, trace_id, base64Encode(summary) AS summary FROM traces FINAL
		WHERE `+where+` AND ({service:String} = '' OR service = {service:String})
		ORDER BY start_ns DESC, trace_id LIMIT {limit:UInt64} OFFSET {offset:UInt64}`, params, func(line []byte) error {
		var row struct {
			Project string `json:"project"`
			TraceID string `json:"trace_id"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		sum := &evalsiv1alpha1.TraceSummary{}
		if err := unb64(row.Summary, sum); err != nil {
			return err
		}
		sum.Project = row.Project
		out = append(out, sum)
		ids[[2]string{row.Project, row.TraceID}] = sum
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > pageSize {
		out, next = out[:pageSize], strconv.Itoa(offset+pageSize)
	}
	if len(out) == 0 {
		return out, next, nil
	}
	var projects, traceIDs []string
	for _, s := range out {
		projects = append(projects, s.GetProject())
		traceIDs = append(traceIDs, s.GetTraceId())
	}
	err = c.query(ctx, `SELECT project, trace_id, count() AS n FROM trace_results FINAL
		WHERE has({projects:Array(String)}, project) AND has({ids:Array(String)}, trace_id)
		GROUP BY project, trace_id`, chParams{"projects": chArray(projects), "ids": chArray(traceIDs)}, func(line []byte) error {
		var row struct {
			Project string `json:"project"`
			TraceID string `json:"trace_id"`
			N       int32  `json:"n"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		if s := ids[[2]string{row.Project, row.TraceID}]; s != nil {
			s.Results = row.N
		}
		return nil
	})
	return out, next, err
}

func (c *clickhouse) TraceProjects(ctx context.Context, traceID string) ([]string, error) {
	var out []string
	err := c.query(ctx, `SELECT DISTINCT project FROM traces FINAL WHERE trace_id = {id:String} ORDER BY project`,
		chParams{"id": traceID}, func(line []byte) error {
			var row struct {
				Project string `json:"project"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return err
			}
			out = append(out, row.Project)
			return nil
		})
	return out, err
}

func (c *clickhouse) GetTrace(ctx context.Context, project, traceID string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, error) {
	var sum *evalsiv1alpha1.TraceSummary
	var rec *evalsiv1alpha1.Record
	err := c.query(ctx, `SELECT base64Encode(summary) AS summary, base64Encode(record) AS record FROM traces FINAL
		WHERE project = {project:String} AND trace_id = {id:String} LIMIT 1`,
		chParams{"project": project, "id": traceID}, func(line []byte) error {
			var row struct {
				Summary string `json:"summary"`
				Record  string `json:"record"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return err
			}
			sum, rec = &evalsiv1alpha1.TraceSummary{}, &evalsiv1alpha1.Record{}
			if err := unb64(row.Summary, sum); err != nil {
				return err
			}
			return unb64(row.Record, rec)
		})
	if err != nil {
		return nil, nil, err
	}
	if sum == nil {
		return nil, nil, ErrNotFound
	}
	sum.Project = project
	return sum, rec, nil
}

func (c *clickhouse) PutTraceResults(ctx context.Context, project, traceID, policy string, results []*evalsiv1alpha1.EvaluationResult) error {
	if len(results) == 0 {
		return nil
	}
	var body bytes.Buffer
	version := time.Now().UnixNano()
	for _, r := range results {
		row, _ := json.Marshal(map[string]any{
			"project": project, "trace_id": traceID, "policy": policy, "evaluator": r.GetEvaluator(),
			"result": b64(r), "version": version,
		})
		body.Write(row)
		body.WriteByte('\n')
	}
	return c.exec(ctx, `INSERT INTO trace_results SELECT project, trace_id, policy, evaluator, base64Decode(result), version
		FROM input('project String, trace_id String, policy String, evaluator String, result String, version Int64')
		FORMAT JSONEachRow`, nil, &body)
}

func (c *clickhouse) TraceResults(ctx context.Context, project, traceID string) ([]*evalsiv1alpha1.PolicyResults, error) {
	var out []*evalsiv1alpha1.PolicyResults
	err := c.query(ctx, `SELECT policy, base64Encode(result) AS result FROM trace_results FINAL
		WHERE project = {project:String} AND trace_id = {id:String} ORDER BY policy, evaluator`,
		chParams{"project": project, "id": traceID}, func(line []byte) error {
			var row struct {
				Policy string `json:"policy"`
				Result string `json:"result"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return err
			}
			r := &evalsiv1alpha1.EvaluationResult{}
			if err := unb64(row.Result, r); err != nil {
				return err
			}
			if len(out) == 0 || out[len(out)-1].GetPolicy() != row.Policy {
				out = append(out, &evalsiv1alpha1.PolicyResults{Policy: row.Policy})
			}
			last := out[len(out)-1]
			last.Results = append(last.Results, r)
			return nil
		})
	return out, err
}

// DeleteTracesBefore removes traces that started before t, and their
// results, with lightweight deletes. The count is of distinct traces.
func (c *clickhouse) DeleteTracesBefore(ctx context.Context, t time.Time) (int64, error) {
	params := chParams{"before": strconv.FormatInt(t.UnixNano(), 10)}
	var n int64
	err := c.query(ctx, `SELECT count() AS n FROM traces FINAL WHERE start_ns < {before:Int64}`, params, func(line []byte) error {
		var row struct {
			N int64 `json:"n"`
		}
		err := json.Unmarshal(line, &row)
		n = row.N
		return err
	})
	if err != nil || n == 0 {
		return 0, err
	}
	if err := c.exec(ctx, `DELETE FROM trace_results WHERE (project, trace_id) IN
		(SELECT project, trace_id FROM traces WHERE start_ns < {before:Int64})`, params, nil); err != nil {
		return 0, err
	}
	return n, c.exec(ctx, `DELETE FROM traces WHERE start_ns < {before:Int64}`, params, nil)
}

func (c *clickhouse) QueryTraces(ctx context.Context, q TraceQuery) ([]StoredTrace, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 1 << 62
	}
	var since int64
	if !q.Since.IsZero() {
		since = q.Since.UnixNano()
	}
	params := chParams{
		"project": q.Project, "service": q.Service, "policy": q.Policy,
		"since": strconv.FormatInt(since, 10), "limit": strconv.Itoa(limit),
	}
	var out []StoredTrace
	err := c.query(ctx, `SELECT base64Encode(summary) AS summary, base64Encode(record) AS record FROM traces FINAL
		WHERE project = {project:String} AND ({service:String} = '' OR service = {service:String})
		  AND start_ns >= {since:Int64}
		  AND ({policy:String} = '' OR trace_id IN (SELECT trace_id FROM trace_results FINAL
		       WHERE project = {project:String} AND policy = {policy:String}))
		ORDER BY start_ns DESC, trace_id LIMIT {limit:UInt64}`, params, func(line []byte) error {
		var row struct {
			Summary string `json:"summary"`
			Record  string `json:"record"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		st := StoredTrace{Summary: &evalsiv1alpha1.TraceSummary{}, Record: &evalsiv1alpha1.Record{}}
		if err := unb64(row.Summary, st.Summary); err != nil {
			return err
		}
		if err := unb64(row.Record, st.Record); err != nil {
			return err
		}
		st.Summary.Project = q.Project
		out = append(out, st)
		return nil
	})
	if err != nil || q.Policy == "" {
		return out, err
	}
	for i := range out {
		groups, err := c.TraceResults(ctx, q.Project, out[i].Summary.GetTraceId())
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			if g.GetPolicy() == q.Policy {
				out[i].Results = g.GetResults()
			}
		}
	}
	return out, nil
}
