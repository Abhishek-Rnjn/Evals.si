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
		`CREATE TABLE IF NOT EXISTS ` + db + `.trace_score_references (
 project String,trace_id String,record_hash String,start_ns Int64
 ) ENGINE = ReplacingMergeTree(start_ns) ORDER BY(project,trace_id,record_hash)`,
		`CREATE TABLE IF NOT EXISTS ` + db + `.trace_retention_requests (
 project String,trace_id String,start_ns Int64,version Int64
 ) ENGINE = ReplacingMergeTree(version) ORDER BY(project,trace_id)`,
		// Reserved for a tenant boundary above projects (decision 0004).
		`ALTER TABLE ` + db + `.traces ADD COLUMN IF NOT EXISTS tenant_id String DEFAULT ''`,
		`ALTER TABLE ` + db + `.trace_results ADD COLUMN IF NOT EXISTS tenant_id String DEFAULT ''`,
		`ALTER TABLE ` + db + `.traces ADD COLUMN IF NOT EXISTS record_hash String, ADD COLUMN IF NOT EXISTS deleted UInt8`,
		`ALTER TABLE ` + db + `.trace_results ADD COLUMN IF NOT EXISTS record_hash String, MODIFY ORDER BY (project,trace_id,policy,evaluator,record_hash)`,
		`CREATE OR REPLACE VIEW ` + db + `.trace_current AS SELECT * FROM ` + db + `.traces FINAL WHERE deleted=0`,
		`CREATE OR REPLACE VIEW ` + db + `.trace_results_current AS SELECT r.* FROM ` + db + `.trace_results AS r FINAL INNER JOIN ` + db + `.trace_current t ON r.project=t.project AND r.trace_id=t.trace_id AND r.record_hash=t.record_hash`,
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
	q.Set("do_not_merge_across_partitions_select_final", "0")
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
	return c.PutTraces(ctx, []TraceWrite{{Summary: summary, Record: record}})
}

// PutTraces inserts every trace in one INSERT (one part, as ClickHouse prefers).
func (c *clickhouse) PutTraces(ctx context.Context, traces []TraceWrite) error {
	if len(traces) == 0 {
		return nil
	}
	if skip, _ := ctx.Value(watchSkipLegacyCopyKey{}).(bool); !skip {
		if err := c.copyLegacyTraceScores(ctx, traces); err != nil {
			return err
		}
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	version := traceWriteVersion(ctx)
	for i, t := range traces {
		_ = enc.Encode(map[string]any{
			"project": t.Summary.GetProject(), "trace_id": t.Summary.GetTraceId(), "service": t.Summary.GetService(),
			"start_ns": t.Summary.GetStartTime().AsTime().UnixNano(), "summary": b64(t.Summary), "record": b64(t.Record),
			// Later traces in a batch win over earlier ones with the same id.
			"version": version + int64(i), "record_hash": traceProjection(ctx, t.Summary, t.Record), "deleted": 0,
		})
	}
	return c.exec(ctx, `INSERT INTO traces (project, trace_id, service, start_ns, summary, record, version, record_hash, deleted) SELECT project, trace_id, service, start_ns,
		base64Decode(summary), base64Decode(record), version, record_hash, deleted
		FROM input('project String, trace_id String, service String, start_ns Int64, summary String, record String, version Int64, record_hash String, deleted UInt8')
		FORMAT JSONEachRow`, nil, &body)
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
	bindings := map[[2]string]string{}
	err := c.query(ctx, `SELECT project, trace_id, base64Encode(summary) AS summary,record_hash FROM trace_current
		WHERE `+where+` AND ({service:String} = '' OR service = {service:String})
		ORDER BY start_ns DESC, trace_id LIMIT {limit:UInt64} OFFSET {offset:UInt64}`, params, func(line []byte) error {
		var row struct {
			Project string `json:"project"`
			TraceID string `json:"trace_id"`
			Summary string `json:"summary"`
			Hash    string `json:"record_hash"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		sum := &evalsiv1alpha1.TraceSummary{}
		if err := unb64(row.Summary, sum); err != nil {
			return err
		}
		sum.Project = row.Project
		sum.Results = 0
		out = append(out, sum)
		ids[[2]string{row.Project, row.TraceID}] = sum
		bindings[[2]string{row.Project, row.TraceID}] = row.Hash
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
	err = c.query(ctx, `SELECT project, trace_id, record_hash,count() AS n FROM trace_results FINAL
		WHERE has({projects:Array(String)}, project) AND has({ids:Array(String)}, trace_id)
		GROUP BY project, trace_id,record_hash`, chParams{"projects": chArray(projects), "ids": chArray(traceIDs)}, func(line []byte) error {
		var row struct {
			Project string `json:"project"`
			TraceID string `json:"trace_id"`
			N       int32  `json:"n"`
			Hash    string `json:"record_hash"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		if s := ids[[2]string{row.Project, row.TraceID}]; s != nil && bindings[[2]string{row.Project, row.TraceID}] == row.Hash {
			s.Results = row.N
		}
		return nil
	})
	return out, next, err
}

func (c *clickhouse) TraceProjects(ctx context.Context, traceID string) ([]string, error) {
	var out []string
	err := c.query(ctx, `SELECT DISTINCT project FROM trace_current WHERE trace_id = {id:String} ORDER BY project`,
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

func (c *clickhouse) GetTrace(ctx context.Context, project, id string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, error) {
	sum, record, _, err := c.getTraceRevision(ctx, project, id)
	return sum, record, err
}

func (c *clickhouse) getTraceRevision(ctx context.Context, project, traceID string) (*evalsiv1alpha1.TraceSummary, *evalsiv1alpha1.Record, string, error) {
	var sum *evalsiv1alpha1.TraceSummary
	var binding string
	var rec *evalsiv1alpha1.Record
	err := c.query(ctx, `SELECT base64Encode(summary) AS summary, base64Encode(record) AS record,record_hash FROM trace_current
		WHERE project = {project:String} AND trace_id = {id:String} LIMIT 1`,
		chParams{"project": project, "id": traceID}, func(line []byte) error {
			var row struct {
				Summary string `json:"summary"`
				Record  string `json:"record"`
				Hash    string `json:"record_hash"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return err
			}
			binding = row.Hash
			sum, rec = &evalsiv1alpha1.TraceSummary{}, &evalsiv1alpha1.Record{}
			if err := unb64(row.Summary, sum); err != nil {
				return err
			}
			return unb64(row.Record, rec)
		})
	if err != nil {
		return nil, nil, "", err
	}
	if sum == nil {
		return nil, nil, "", ErrNotFound
	}
	sum.Project = project
	return sum, rec, binding, nil
}

func (c *clickhouse) PutTraceResults(ctx context.Context, project, traceID, policy string, results []*evalsiv1alpha1.EvaluationResult) error {
	if len(results) == 0 {
		return nil
	}
	hash, ok := ctx.Value(watchScoreBindingKey{}).(string)
	if !ok {
		var err error
		hash, err = c.currentRecordHash(ctx, project, traceID)
		if err != nil {
			return err
		}
	}
	var body bytes.Buffer
	version := traceWriteVersion(ctx)
	for _, r := range results {
		row, _ := json.Marshal(map[string]any{
			"project": project, "trace_id": traceID, "policy": policy, "evaluator": r.GetEvaluator(),
			"result": b64(r), "version": version, "record_hash": hash,
		})
		body.Write(row)
		body.WriteByte('\n')
	}
	return c.exec(ctx, `INSERT INTO trace_results (project, trace_id, policy, evaluator, result, version, record_hash) SELECT project, trace_id, policy, evaluator, base64Decode(result), version, record_hash
		FROM input('project String, trace_id String, policy String, evaluator String, result String, version Int64, record_hash String')
		FORMAT JSONEachRow`, nil, &body)
}

func (c *clickhouse) TraceResults(ctx context.Context, project, id string) ([]*evalsiv1alpha1.PolicyResults, error) {
	return c.traceResultsQuery(ctx, project, id, nil)
}
func (c *clickhouse) traceResultsAt(ctx context.Context, project, id, binding string) ([]*evalsiv1alpha1.PolicyResults, error) {
	return c.traceResultsQuery(ctx, project, id, &binding)
}

func (c *clickhouse) traceResultsQuery(ctx context.Context, project, traceID string, binding *string) ([]*evalsiv1alpha1.PolicyResults, error) {
	var out []*evalsiv1alpha1.PolicyResults
	query := `SELECT policy,base64Encode(result) AS result FROM trace_results_current WHERE project={project:String} AND trace_id={id:String} ORDER BY policy,evaluator`
	params := chParams{"project": project, "id": traceID}
	if binding != nil {
		query = `SELECT policy,base64Encode(result) AS result FROM trace_results FINAL WHERE project={project:String} AND trace_id={id:String} AND record_hash={binding:String} ORDER BY policy,evaluator`
		params["binding"] = *binding
	}
	err := c.query(ctx, query, params, func(line []byte) error {
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
	return c.retainTraces(ctx, t, nil)
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
	var bindings []string
	err := c.query(ctx, `SELECT base64Encode(summary) AS summary, base64Encode(record) AS record,record_hash FROM trace_current
		WHERE project = {project:String} AND ({service:String} = '' OR service = {service:String})
		  AND start_ns >= {since:Int64}
		  AND ({policy:String} = '' OR trace_id IN (SELECT trace_id FROM trace_results_current
		       WHERE project = {project:String} AND policy = {policy:String}))
		ORDER BY start_ns DESC, trace_id LIMIT {limit:UInt64}`, params, func(line []byte) error {
		var row struct {
			Summary string `json:"summary"`
			Record  string `json:"record"`
			Hash    string `json:"record_hash"`
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
		bindings = append(bindings, row.Hash)
		return nil
	})
	if err != nil || q.Policy == "" {
		return out, err
	}
	for i := range out {
		groups, err := c.traceResultsAt(ctx, q.Project, out[i].Summary.GetTraceId(), bindings[i])
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

func (c *clickhouse) ScoredTraces(ctx context.Context, q ScoreQuery) ([]ScoredTrace, error) {
	since, after, cur, limit := scoreBounds(q)
	params := chParams{
		"id": q.TraceID, "service": q.Service, "policy": q.Policy, "evaluator": q.Evaluator,
		"since": strconv.FormatInt(since, 10), "after": strconv.Itoa(after),
		"cs": strconv.FormatInt(cur.StartNS, 10), "ct": cur.TraceID, "cp": cur.Project,
		"limit": strconv.Itoa(limit),
	}
	where := projectsWhere("project", q.Projects, params)
	var out []ScoredTrace
	bindings := map[[2]string]string{}
	err := c.query(ctx, `SELECT project, base64Encode(summary) AS summary,record_hash FROM trace_current
		WHERE `+where+` AND ({id:String} = '' OR trace_id = {id:String})
		  AND ({service:String} = '' OR service = {service:String}) AND start_ns >= {since:Int64}
		  AND ({after:UInt8} = 0 OR start_ns < {cs:Int64} OR (start_ns = {cs:Int64}
		       AND (trace_id > {ct:String} OR (trace_id = {ct:String} AND project > {cp:String}))))
		  AND (project, trace_id) IN (SELECT project, trace_id FROM trace_results_current
		       WHERE `+where+` AND ({policy:String} = '' OR policy = {policy:String})
		         AND ({evaluator:String} = '' OR evaluator = {evaluator:String}))
		ORDER BY start_ns DESC, trace_id, project LIMIT {limit:UInt64}`, params, func(line []byte) error {
		var row struct {
			Project string `json:"project"`
			Summary string `json:"summary"`
			Hash    string `json:"record_hash"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		sum := &evalsiv1alpha1.TraceSummary{}
		if err := unb64(row.Summary, sum); err != nil {
			return err
		}
		sum.Project = row.Project
		out = append(out, ScoredTrace{Summary: sum})
		bindings[[2]string{row.Project, sum.GetTraceId()}] = row.Hash
		return nil
	})
	if err != nil || len(out) == 0 {
		return out, err
	}
	// Every result of the page's traces in one query, grouped per trace.
	var projects, traceIDs []string
	groups := map[[2]string][]*evalsiv1alpha1.PolicyResults{}
	for _, st := range out {
		projects = append(projects, st.Summary.GetProject())
		traceIDs = append(traceIDs, st.Summary.GetTraceId())
		groups[[2]string{st.Summary.GetProject(), st.Summary.GetTraceId()}] = nil
	}
	err = c.query(ctx, `SELECT project, trace_id, policy, base64Encode(result) AS result,record_hash FROM trace_results FINAL
		WHERE has({projects:Array(String)}, project) AND has({ids:Array(String)}, trace_id)
		ORDER BY project, trace_id, policy, evaluator`, chParams{"projects": chArray(projects), "ids": chArray(traceIDs)}, func(line []byte) error {
		var row struct {
			Project string `json:"project"`
			TraceID string `json:"trace_id"`
			Policy  string `json:"policy"`
			Result  string `json:"result"`
			Hash    string `json:"record_hash"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		key := [2]string{row.Project, row.TraceID}
		gs, ok := groups[key]
		if !ok || bindings[key] != row.Hash {
			return nil // another project's trace with the same id
		}
		r := &evalsiv1alpha1.EvaluationResult{}
		if err := unb64(row.Result, r); err != nil {
			return err
		}
		if len(gs) == 0 || gs[len(gs)-1].GetPolicy() != row.Policy {
			gs = append(gs, &evalsiv1alpha1.PolicyResults{Policy: row.Policy})
		}
		gs[len(gs)-1].Results = append(gs[len(gs)-1].Results, r)
		groups[key] = gs
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		key := [2]string{out[i].Summary.GetProject(), out[i].Summary.GetTraceId()}
		out[i].Policies, out[i].Summary.Results = selectResults(groups[key], q.Policy, q.Evaluator)
	}
	return out, nil
}

func (c *clickhouse) currentRecordHash(ctx context.Context, project, id string) (string, error) {
	var hash string
	found := false
	err := c.query(ctx, `SELECT record_hash FROM trace_current WHERE project={project:String} AND trace_id={id:String} LIMIT 1`, chParams{"project": project, "id": id}, func(line []byte) error {
		var row struct {
			Hash string `json:"record_hash"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		hash = row.Hash
		found = true
		return nil
	})
	if err != nil {
		return "", err
	}
	if !found {
		return "", ErrNotFound
	}
	return hash, nil
}

// Preserve scores on an unchanged legacy record when assigning its first hash.
func (c *clickhouse) copyLegacyTraceScores(ctx context.Context, traces []TraceWrite) error {
	byKey := map[[2]string]TraceWrite{}
	for _, t := range traces {
		byKey[[2]string{t.Summary.GetProject(), t.Summary.GetTraceId()}] = t
	}
	type legacyCopy struct {
		Trace   TraceWrite
		OldHash string
	}
	var unchanged []legacyCopy
	var references []map[string]any
	for _, t := range traces {
		references = append(references, map[string]any{"project": t.Summary.GetProject(), "trace_id": t.Summary.GetTraceId(), "record_hash": traceProjection(ctx, t.Summary, t.Record), "start_ns": t.Summary.GetStartTime().AsTime().UnixNano()})
	}
	const chunkSize = 80
	for lo := 0; lo < len(traces); lo += chunkSize {
		chunk := traces[lo:min(lo+chunkSize, len(traces))]
		projectSet := map[string]bool{}
		var projects, ids []string
		for _, t := range chunk {
			p := t.Summary.GetProject()
			if !projectSet[p] {
				projectSet[p] = true
				projects = append(projects, p)
			}
			ids = append(ids, t.Summary.GetTraceId())
		}
		err := c.query(ctx, `SELECT project,trace_id,record_hash,start_ns,base64Encode(record) AS record FROM trace_current WHERE project IN {projects:Array(String)} AND trace_id IN {ids:Array(String)}`, chParams{"projects": chArray(projects), "ids": chArray(ids)}, func(line []byte) error {
			var row struct {
				Project string `json:"project"`
				TraceID string `json:"trace_id"`
				Record  string `json:"record"`
				Hash    string `json:"record_hash"`
				Start   int64  `json:"start_ns"`
			}
			if err := json.Unmarshal(line, &row); err != nil {
				return err
			}
			t, ok := byKey[[2]string{row.Project, row.TraceID}]
			if !ok {
				return nil
			}
			// Preserve the outgoing binding's expiry before overwriting its
			// projection, even when different input means no scores are copied.
			references = append(references, map[string]any{"project": row.Project, "trace_id": row.TraceID, "record_hash": row.Hash, "start_ns": row.Start})
			record := &evalsiv1alpha1.Record{}
			if err := unb64(row.Record, record); err != nil {
				return err
			}
			target, input := traceProjection(ctx, t.Summary, t.Record), RecordHash(record)
			// Copy only when entering/leaving a legacy unversioned binding.
			// A new watch generation after retention must never inherit scores
			// from the expired generation if its old projection is still visible.
			if legacyCopyAllowed(ctx, row.Project, row.TraceID, row.Hash, row.Start) && row.Hash != target && (row.Hash == "" || row.Hash == input || target == input) && proto.Equal(record, t.Record) {
				unchanged = append(unchanged, legacyCopy{Trace: t, OldHash: row.Hash})
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for offset := 0; offset < len(references); offset += retentionBatchSize {
		var body bytes.Buffer
		enc := json.NewEncoder(&body)
		for _, ref := range references[offset:min(offset+retentionBatchSize, len(references))] {
			_ = enc.Encode(ref)
		}
		if err := c.exec(ctx, `INSERT INTO trace_score_references FORMAT JSONEachRow`, nil, &body); err != nil {
			return err
		}
	}
	for _, copy := range unchanged {
		t := copy.Trace
		params := chParams{"project": t.Summary.GetProject(), "id": t.Summary.GetTraceId(), "hash": traceProjection(ctx, t.Summary, t.Record), "old_hash": copy.OldHash}
		// Migration fills only missing keys, like SQL's ON CONFLICT DO NOTHING.
		// Version zero also keeps copies below fresh reserved evaluations whose
		// INSERT may still be completing after its client timed out.
		if err := c.exec(ctx, `INSERT INTO trace_results(project,trace_id,policy,evaluator,record_hash,result,version) SELECT project,trace_id,policy,evaluator,{hash:String},result,0 FROM trace_results_current
			WHERE project={project:String} AND trace_id={id:String} AND record_hash={old_hash:String}
			AND (policy,evaluator) NOT IN (SELECT policy,evaluator FROM trace_results FINAL WHERE project={project:String} AND trace_id={id:String} AND record_hash={hash:String})`, params, nil); err != nil {
			return err
		}
	}
	return nil
}
