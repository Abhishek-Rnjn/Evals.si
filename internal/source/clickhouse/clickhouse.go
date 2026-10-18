// Package clickhouse pulls OpenTelemetry Collector clickhouseexporter trace
// tables over the HTTP interface. Decision 0028 defines the supported schema
// and companion score table; this is separate from Evals.si's own trace store.
package clickhouse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

const (
	maxPage          = 1000
	maxSpansPerTrace = 10000
	maxResponse      = 64 << 20
)

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

type connector struct {
	base           *url.URL
	client         *http.Client
	table, scores  string
	user, password string
	writeBack      bool
}

// Factory accepts exactly one location, database.table, using the Collector's
// default Map(String, String) trace schema. Credentials are username:password
// (split at the first colon); empty credentials use the server's default user.
// Operators create table_evalsi_scores before enabling write-back. No DDL is
// executed by the connector, and no SQL or credentials may appear in endpoint.
func Factory(src *evalsiv1alpha1.TraceSource, token string, client *http.Client) (source.Connector, error) {
	if src.GetVariant() != "" {
		return nil, fmt.Errorf("clickhouse: variants are not supported; use the Collector Map trace schema")
	}
	if len(src.GetLocations()) != 1 {
		return nil, fmt.Errorf("clickhouse: locations must name exactly one database.table")
	}
	db, table, ok := strings.Cut(src.GetLocations()[0], ".")
	if !ok || !identifier.MatchString(db) || !identifier.MatchString(table) {
		return nil, fmt.Errorf("clickhouse: location must be database.table with plain SQL identifiers (letters, digits, underscores)")
	}
	u, err := url.Parse(src.GetEndpoint())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("clickhouse: endpoint must be an http(s) URL without credentials, query parameters or fragment")
	}
	if client == nil {
		return nil, fmt.Errorf("clickhouse: HTTP client is required")
	}
	c := &connector{base: u, client: client, table: "`" + db + "`.`" + table + "`", scores: "`" + db + "`.`" + table + "_evalsi_scores`", writeBack: src.GetWriteBack().GetEnabled()}
	if token != "" && token != source.ValidationToken {
		c.user, c.password, ok = strings.Cut(token, ":")
		if !ok || c.user == "" {
			return nil, fmt.Errorf("clickhouse: credential must be username:password")
		}
	}
	return c, nil
}

// query binds predicate values; inserts carry JSONEachRow data. Only validated,
// quoted identifiers are interpolated. wait_end_of_query prevents an HTTP 200
// before a query fails; a malformed/truncated JSON response still fails closed.
func (c *connector) query(ctx context.Context, sql string, params url.Values, data []byte) ([]byte, error) {
	u := *c.base
	q := url.Values{
		"wait_end_of_query": {"1"}, "max_execution_time": {"30"},
		"max_memory_usage": {"536870912"}, "max_result_bytes": {strconv.Itoa(maxResponse)},
		"result_overflow_mode": {"throw"}, "date_time_input_format": {"best_effort"},
	}
	for k, values := range params {
		q["param_"+k] = values
	}
	var body io.Reader = strings.NewReader(sql)
	if data != nil {
		q.Set("query", sql)
		body = bytes.NewReader(data)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), body)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clickhouse source: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cause := fmt.Errorf("clickhouse source: HTTP %d: %.300s", resp.StatusCode, raw)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			after := 30 * time.Second
			if n, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); err == nil && n > 0 {
				after = time.Duration(n) * time.Second
			} else if t, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil && t.After(time.Now()) {
				after = time.Until(t)
			}
			return nil, &source.RetryAfterError{After: after, Err: cause}
		}
		return nil, cause
	}
	if len(raw) > maxResponse {
		return nil, fmt.Errorf("clickhouse source: response exceeds %d bytes", maxResponse)
	}
	return raw, nil
}

func decodeRows[T any](raw []byte) ([]T, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	var rows []T
	for {
		var row T
		if err := d.Decode(&row); err == io.EOF {
			return rows, nil
		} else if err != nil {
			return nil, fmt.Errorf("clickhouse source: invalid JSONEachRow response: %w", err)
		}
		rows = append(rows, row)
	}
}

// Hash all mapped fields, with canonical map order. Identical exporter retries
// do not change a trace's digest, whereas late children or changed data do.
const rowHash = `cityHash64(toJSONString(tuple(Timestamp, lower(TraceId), lower(SpanId), lower(ParentSpanId),
	TraceState, SpanName, SpanKind, ServiceName, mapSort(ResourceAttributes), ScopeName, ScopeVersion,
	mapSort(SpanAttributes), Duration, StatusCode, StatusMessage,
	Events.Timestamp, Events.Name, arrayMap(m -> mapSort(m), Events.Attributes),
	Links.TraceId, Links.SpanId, Links.TraceState, arrayMap(m -> mapSort(m), Links.Attributes))))`

type cursor struct {
	Since int64  `json:"since"`
	Start int64  `json:"start"`
	ID    string `json:"id"`
}

// List selects traces with a completed root. The Collector only writes ended
// spans, so a rootless trace is not yet eligible. The root time filter finds
// a bounded page of candidates, then aggregation reads their complete span
// sets, including children outside the time window. The root start is the
// pagination key, so late children cannot move a trace between pages.
func (c *connector) List(ctx context.Context, req source.ListRequest) (source.ListResult, error) {
	from := req.Since
	if from.IsZero() {
		from = time.Unix(0, 0)
	}
	since := from.UnixNano()
	if since < 0 || !time.Unix(0, since).Equal(from) {
		return source.ListResult{}, fmt.Errorf("clickhouse: since must be within the nonnegative Unix nanosecond range")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, maxPage)
	params := url.Values{"since": {strconv.FormatInt(since, 10)}, "limit": {strconv.Itoa(limit)}}
	page := ""
	if req.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		var cur cursor
		if err != nil || json.Unmarshal(raw, &cur) != nil || cur.Since != since || cur.Start < since || !validID(cur.ID, 16) {
			return source.ListResult{}, fmt.Errorf("clickhouse: invalid cursor for this listing")
		}
		params.Set("start", strconv.FormatInt(cur.Start, 10))
		params.Set("id", cur.ID)
		page = " HAVING (min(Timestamp), lower(TraceId)) > (fromUnixTimestamp64Nano({start:Int64}), {id:String})"
	}
	rootStart := `minIf(Timestamp, ParentSpanId = '' OR ParentSpanId = '0000000000000000')`
	sql := `SELECT lower(TraceId) AS ID, toString(toUnixTimestamp64Nano(` + rootStart + `)) AS StartNS,
		hex(SHA256(toJSONString(arraySort(groupUniqArray(` + rowHash + `))))) AS Digest
		FROM ` + c.table + `
		WHERE TraceId IN (SELECT TraceId FROM ` + c.table + ` WHERE Timestamp >= fromUnixTimestamp64Nano({since:Int64})
		AND (ParentSpanId = '' OR ParentSpanId = '0000000000000000')
		GROUP BY TraceId` + page + ` ORDER BY min(Timestamp), lower(TraceId) LIMIT {limit:UInt32})
		GROUP BY TraceId ORDER BY ` + rootStart + `, lower(TraceId) FORMAT JSONEachRow`
	raw, err := c.query(ctx, sql, params, nil)
	if err != nil {
		return source.ListResult{}, err
	}
	rows, err := decodeRows[struct{ ID, StartNS, Digest string }](raw)
	if err != nil {
		return source.ListResult{}, err
	}
	if len(rows) > limit {
		return source.ListResult{}, fmt.Errorf("clickhouse: listing exceeded its page limit")
	}
	out := source.ListResult{}
	for _, row := range rows {
		ns, err := strconv.ParseInt(row.StartNS, 10, 64)
		if err != nil || ns < since || !validID(row.ID, 16) || row.Digest == "" {
			return source.ListResult{}, fmt.Errorf("clickhouse: invalid trace summary")
		}
		out.Infos = append(out.Infos, source.Info{ID: row.ID, Started: time.Unix(0, ns).UTC(), Digest: row.Digest})
	}
	if len(rows) == limit {
		last := out.Infos[len(out.Infos)-1]
		b, _ := json.Marshal(cursor{Since: since, Start: last.Started.UnixNano(), ID: last.ID})
		out.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}

const spanColumns = `lower(TraceId) AS TraceID, lower(SpanId) AS SpanID, lower(ParentSpanId) AS ParentID,
	toString(toUnixTimestamp64Nano(Timestamp)) AS StartNS, toString(Duration) AS DurationNS,
	TraceState, SpanName, SpanKind, ServiceName, ResourceAttributes, ScopeName, ScopeVersion, SpanAttributes,
	StatusCode, StatusMessage,
	arrayMap(t -> toString(toUnixTimestamp64Nano(t)), Events.Timestamp) AS EventTimes,
	Events.Name AS EventNames, Events.Attributes AS EventAttributes,
	Links.TraceId AS LinkTraceIDs, Links.SpanId AS LinkSpanIDs,
	Links.TraceState AS LinkStates, Links.Attributes AS LinkAttributes`

func (c *connector) fetchQuery() string {
	// Collector IDs are lowercase. Keep the indexed column bare so its bloom
	// filter can skip unrelated data instead of scanning every retained trace.
	return `SELECT ` + spanColumns + ` FROM ` + c.table + `
		WHERE TraceId = {trace:String} ORDER BY Timestamp, SpanId LIMIT ` + strconv.Itoa(maxSpansPerTrace+1) + ` FORMAT JSONEachRow`
}

// Fetch bounds each full trace, refusing oversized or conflicting spans rather
// than scoring a truncated trajectory. Equal duplicate rows are harmless.
func (c *connector) Fetch(ctx context.Context, infos []source.Info) ([]ingest.Trace, error) {
	var out []ingest.Trace
	for _, info := range infos {
		if !validID(info.ID, 16) {
			return nil, fmt.Errorf("clickhouse: invalid trace ID")
		}
		id := strings.ToLower(info.ID)
		raw, err := c.query(ctx, c.fetchQuery(), url.Values{"trace": {id}}, nil)
		if err != nil {
			return nil, err
		}
		rows, err := decodeRows[spanRow](raw)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			continue // deleted between listing and fetch
		}
		if len(rows) > maxSpansPerTrace {
			return nil, fmt.Errorf("clickhouse: trace exceeds %d span rows", maxSpansPerTrace)
		}
		trace, err := toTrace(id, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, trace)
	}
	return out, nil
}

func validID(id string, bytes int) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == bytes && strings.Trim(id, "0") != ""
}

// WriteBack appends a complete replacement row per metric. ReplacingMergeTree
// with Version and FINAL reads gives one current score per trace/policy/metric,
// including replay after the remote insert committed but its receipt was lost.
func (c *connector) WriteBack(ctx context.Context, scores []source.Score, _ map[store.WriteKey]store.SourceWrite) ([]store.SourceWrite, error) {
	if !c.writeBack {
		return nil, fmt.Errorf("clickhouse: write-back is disabled")
	}
	var done []store.SourceWrite
	for _, s := range scores {
		if !validID(s.TraceID, 16) {
			return done, fmt.Errorf("clickhouse: invalid score trace ID")
		}
		row := map[string]any{"TraceId": strings.ToLower(s.TraceID), "Policy": s.Policy, "Metric": s.Metric,
			"Score": nil, "Value": nil, "Passed": nil, "Rationale": s.Rationale, "Evaluator": s.Evaluator,
			"Judged": s.Judged, "Version": uint64(time.Now().UnixNano())}
		switch v := s.Value.(type) {
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return done, fmt.Errorf("clickhouse: non-finite score")
			}
			row["Score"] = v
		case bool:
			row["Score"], row["Passed"] = map[bool]int{false: 0, true: 1}[v], v
		case string:
			row["Value"] = v
		default:
			return done, fmt.Errorf("clickhouse: unsupported score type %T", s.Value)
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return done, err
		}
		response, err := c.query(ctx, `INSERT INTO `+c.scores+` (TraceId, Policy, Metric, Score, Value, Passed, Rationale, Evaluator, Judged, Version) FORMAT JSONEachRow`, nil, append(raw, '\n'))
		if err != nil {
			return done, err
		}
		if len(bytes.TrimSpace(response)) != 0 {
			return done, fmt.Errorf("clickhouse: unexpected insert response")
		}
		h := sha256.Sum256([]byte(c.table + "\x00" + strings.ToLower(s.TraceID) + "\x00" + s.Policy + "\x00" + s.Metric))
		done = append(done, store.SourceWrite{TraceID: s.TraceID, Policy: s.Policy, Metric: s.Metric,
			RemoteID: hex.EncodeToString(h[:]), Digest: s.Digest(), Written: time.Now().UTC()})
	}
	return done, nil
}
