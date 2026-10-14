// Package langsmith reads traces from LangSmith and writes scores back as
// feedback. Decision 0016 names LangSmith as a later store (milestone M5).
//
// Built from the pinned langsmith Python client 0.4.37: runs are read with
// POST /runs/query (root runs list the traces, a trace's runs become its
// spans), projects are found with GET /sessions?name=, and feedback is
// created with POST /feedback and updated with PATCH /feedback/{id}. Not yet
// run against a live LangSmith.
package langsmith

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// maxPage is the most runs one query returns.
const maxPage = 100

// The first window List reads; an empty window doubles the next, up to maxWindow.
const (
	minWindow = time.Hour
	maxWindow = 7 * 24 * time.Hour
)

const labelPrefix = "evalsi.label."

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// The run fields the connector reads.
var (
	listFields  = []string{"id", "trace_id", "name", "start_time", "end_time", "status", "total_tokens"}
	fetchFields = []string{
		"id", "trace_id", "parent_run_id", "name", "run_type", "start_time", "end_time", "status", "error",
		"inputs", "outputs", "extra", "tags", "prompt_tokens", "completion_tokens", "total_tokens", "session_id",
	}
)

type connector struct {
	base      string
	apiKey    string
	workspace string
	client    *http.Client
	projects  []string
	// Project IDs by location, resolved on first use.
	sessions []string
	now      func() time.Time
}

// Factory builds the connector for a source with connector "langsmith".
// endpoint is the API URL the langsmith client takes as LANGSMITH_ENDPOINT
// (https://api.smith.langchain.com, https://eu.api.smith.langchain.com, or a
// self-hosted https://<host>/api/v1). locations names one or more tracing
// projects, by name or ID. The credential is an API key, or
// "<workspace ID>:<API key>" for a key that spans workspaces.
func Factory(src *evalsiv1alpha1.TraceSource, token string, client *http.Client) (source.Connector, error) {
	if v := src.GetVariant(); v != "" {
		return nil, fmt.Errorf("langsmith has no variants, got %q", v)
	}
	if len(src.GetLocations()) == 0 {
		return nil, fmt.Errorf("langsmith: locations must name at least one tracing project")
	}
	for _, l := range src.GetLocations() {
		if strings.TrimSpace(l) == "" {
			return nil, fmt.Errorf("langsmith: a location is empty")
		}
	}
	u, err := url.Parse(src.GetEndpoint())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("langsmith: endpoint must be an http(s) URL, got %q", src.GetEndpoint())
	}
	c := &connector{base: strings.TrimRight(src.GetEndpoint(), "/"), client: client, projects: src.GetLocations(), now: time.Now}
	if token != "" && token != source.ValidationToken {
		c.apiKey = token
		if ws, key, ok := strings.Cut(token, ":"); ok {
			if !uuidRE.MatchString(ws) || key == "" {
				return nil, fmt.Errorf("langsmith: the credential must be an API key or <workspace ID>:<API key>")
			}
			c.workspace, c.apiKey = ws, key
		}
	}
	return c, nil
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("langsmith: HTTP %d: %s", e.Status, e.Body) }

func (c *connector) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	if c.workspace != "" {
		req.Header.Set("X-Tenant-Id", c.workspace)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		e := &apiError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw[:min(len(raw), 300)]))}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			after := 30 * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
				after = time.Duration(s) * time.Second
			}
			return &source.RetryAfterError{After: after, Err: e}
		}
		return e
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// resolve finds the IDs of the source's projects. A location that is a UUID
// is taken as an ID; a name is looked up.
func (c *connector) resolve(ctx context.Context) ([]string, error) {
	if c.sessions != nil {
		return c.sessions, nil
	}
	var ids []string
	for _, p := range c.projects {
		if uuidRE.MatchString(p) {
			ids = append(ids, strings.ToLower(p))
			continue
		}
		var found []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := c.call(ctx, http.MethodGet, "/sessions", url.Values{"name": {p}, "limit": {"1"}}, nil, &found); err != nil {
			return nil, err
		}
		if len(found) == 0 || found[0].ID == "" {
			return nil, fmt.Errorf("langsmith: tracing project %q not found", p)
		}
		ids = append(ids, found[0].ID)
	}
	c.sessions = ids
	return ids, nil
}

// run is the part of a LangSmith run the connector reads.
type run struct {
	ID               string         `json:"id"`
	TraceID          string         `json:"trace_id"`
	ParentRunID      *string        `json:"parent_run_id"`
	Name             string         `json:"name"`
	RunType          string         `json:"run_type"`
	StartTime        string         `json:"start_time"`
	EndTime          *string        `json:"end_time"`
	Status           string         `json:"status"`
	Error            *string        `json:"error"`
	Inputs           any            `json:"inputs"`
	Outputs          any            `json:"outputs"`
	Extra            map[string]any `json:"extra"`
	Tags             []string       `json:"tags"`
	PromptTokens     *float64       `json:"prompt_tokens"`
	CompletionTokens *float64       `json:"completion_tokens"`
	TotalTokens      *float64       `json:"total_tokens"`
	SessionID        string         `json:"session_id"`
}

// query reads every page of a runs query.
func (c *connector) query(ctx context.Context, body map[string]any) ([]run, error) {
	var all []run
	for {
		var resp struct {
			Runs    []run `json:"runs"`
			Cursors struct {
				Next *string `json:"next"`
			} `json:"cursors"`
		}
		if err := c.call(ctx, http.MethodPost, "/runs/query", nil, body, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Runs...)
		if len(resp.Runs) == 0 || resp.Cursors.Next == nil || *resp.Cursors.Next == "" {
			return all, nil
		}
		body["cursor"] = *resp.Cursors.Next
	}
}

// List reads the root runs (one per trace) that started in a window from
// req.Since or the cursor, oldest first. The runs query's order is not part
// of what the client relies on, so a page is one whole window, sorted here,
// and the cursor is where the next window starts; that keeps "oldest first"
// across pages, which the manager's per-page progress relies on.
func (c *connector) List(ctx context.Context, req source.ListRequest) (source.ListResult, error) {
	from := req.Since
	window := minWindow
	if req.Cursor != "" {
		start, w, ok := strings.Cut(req.Cursor, "|")
		t, err := time.Parse(time.RFC3339Nano, start)
		d, derr := time.ParseDuration(w)
		if !ok || err != nil || derr != nil {
			return source.ListResult{}, fmt.Errorf("langsmith: bad cursor %q", req.Cursor)
		}
		from, window = t, d
	}
	sessions, err := c.resolve(ctx)
	if err != nil {
		return source.ListResult{}, err
	}
	now := c.now()
	for {
		to := from.Add(window)
		roots, err := c.query(ctx, map[string]any{
			"session": sessions, "is_root": true, "start_time": from.UTC().Format(time.RFC3339Nano),
			"filter": fmt.Sprintf(`and(gte(start_time, %q), lt(start_time, %q))`, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano)),
			"select": listFields, "limit": maxPage,
		})
		if err != nil {
			return source.ListResult{}, err
		}
		next := ""
		if to.Before(now) {
			next = to.UTC().Format(time.RFC3339Nano) + "|" + window.String()
		}
		if len(roots) == 0 && next != "" {
			// Nothing in this window: try a wider one, so a long quiet
			// stretch costs a few requests, not one per hour.
			from, window = to, min(window*2, maxWindow)
			continue
		}
		out := source.ListResult{Next: next}
		seen := map[string]bool{}
		for _, r := range roots {
			id := r.TraceID
			if id == "" {
				id = r.ID // a root run's ID is its trace's
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			started, err := parseTime(r.StartTime)
			if err != nil {
				return source.ListResult{}, fmt.Errorf("langsmith: run %s has start_time %q: %w", r.ID, r.StartTime, err)
			}
			if started.Before(from) || !started.Before(to) {
				continue // outside the window: the filter was not applied as asked
			}
			end, tokens := "", ""
			if r.EndTime != nil {
				end = *r.EndTime
			}
			if r.TotalTokens != nil {
				tokens = strconv.FormatFloat(*r.TotalTokens, 'f', -1, 64)
			}
			h := sha256.Sum256([]byte(end + "|" + r.Status + "|" + tokens))
			out.Infos = append(out.Infos, source.Info{
				ID: id, Started: started, InProgress: r.EndTime == nil || r.Status == "pending", Digest: hex.EncodeToString(h[:8]),
			})
		}
		sort.SliceStable(out.Infos, func(i, j int) bool { return out.Infos[i].Started.Before(out.Infos[j].Started) })
		return out, nil
	}
}

// Fetch reads each trace's runs and makes them spans.
func (c *connector) Fetch(ctx context.Context, infos []source.Info) ([]ingest.Trace, error) {
	sessions, err := c.resolve(ctx)
	if err != nil {
		return nil, err
	}
	var out []ingest.Trace
	for _, in := range infos {
		runs, err := c.query(ctx, map[string]any{"session": sessions, "trace": in.ID, "select": fetchFields, "limit": maxPage})
		if err != nil {
			return nil, err
		}
		if len(runs) == 0 {
			continue
		}
		out = append(out, toTrace(in.ID, runs))
	}
	return out, nil
}

// parseTime reads a run's timestamp. LangSmith writes them in UTC, without a
// zone ("2026-10-09T12:00:00.123456").
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	return time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.UTC)
}

func nanos(s string) uint64 {
	t, err := parseTime(s)
	if err != nil || t.UnixNano() < 0 {
		return 0
	}
	return uint64(t.UnixNano())
}

// kinds maps LangSmith run types to the OpenInference span kinds the ingest
// conventions read.
var kinds = map[string]string{
	"llm": "LLM", "chain": "CHAIN", "tool": "TOOL", "retriever": "RETRIEVER", "embedding": "EMBEDDING",
	"prompt": "CHAIN", "parser": "CHAIN",
}

// sessionKeys are the metadata keys LangSmith groups a trace into a thread by.
var sessionKeys = []string{"session_id", "thread_id", "conversation_id"}

// toTrace builds OTLP spans from runs: OpenInference attributes for kind,
// input and output, model and token counts, so a LangSmith trace reads like
// a pushed one.
func toTrace(traceID string, runs []run) ingest.Trace {
	tid := idBytes(traceID, 16)
	res := ingest.Attrs{source.AttrSourceTraceID: traceID, "service.name": "langsmith"}
	labels := map[string]string{}
	t := ingest.Trace{TraceID: hex.EncodeToString(tid)}
	for _, r := range runs {
		root := r.ParentRunID == nil || *r.ParentRunID == ""
		sp := &tracepb.Span{TraceId: tid, SpanId: idBytes(r.ID, 8), Name: r.Name}
		if !root {
			sp.ParentSpanId = idBytes(*r.ParentRunID, 8)
		} else if r.Name != "" {
			res["service.name"] = r.Name
		}
		if r.SessionID != "" {
			res["langsmith.project_id"] = r.SessionID
		}
		sp.StartTimeUnixNano = nanos(r.StartTime)
		if r.EndTime != nil {
			sp.EndTimeUnixNano = nanos(*r.EndTime)
		}
		if r.Status == "error" || (r.Error != nil && *r.Error != "") {
			msg := ""
			if r.Error != nil {
				msg = *r.Error
			}
			sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: msg}
		}
		attrs := []*commonpb.KeyValue{str("langsmith.run_type", r.RunType)}
		if k := kinds[r.RunType]; k != "" {
			attrs = append(attrs, str("openinference.span.kind", k))
		}
		if s := text(r.Inputs); s != "" {
			attrs = append(attrs, str("input.value", s))
		}
		if s := text(r.Outputs); s != "" {
			attrs = append(attrs, str("output.value", s))
		}
		meta, _ := r.Extra["metadata"].(map[string]any)
		if r.RunType == "llm" {
			if m := model(r.Extra, meta); m != "" {
				attrs = append(attrs, str("llm.model_name", m))
			}
			// Chains carry the sum of their children's tokens, so only model
			// calls count, as the ingest conventions expect.
			for attr, n := range map[string]*float64{
				"llm.token_count.prompt": r.PromptTokens, "llm.token_count.completion": r.CompletionTokens, "llm.token_count.total": r.TotalTokens,
			} {
				if n != nil {
					attrs = append(attrs, &commonpb.KeyValue{Key: attr, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(*n)}}})
				}
			}
		}
		for k, v := range meta {
			s := text(v)
			attrs = append(attrs, str("langsmith.metadata."+k, s))
			if l, ok := strings.CutPrefix(k, labelPrefix); ok && l != "" {
				labels[l] = s
			}
		}
		for _, k := range sessionKeys {
			if s, ok := meta[k].(string); ok && s != "" && root {
				attrs = append(attrs, str("session.id", s))
				break
			}
		}
		if len(r.Tags) > 0 {
			attrs = append(attrs, str("langsmith.tags", strings.Join(r.Tags, ",")))
		}
		sp.Attributes = attrs
		t.Spans = append(t.Spans, ingest.Span{Span: sp, Resource: res, Labels: labels})
	}
	return t
}

// model is the model a run called: the name LangChain records in metadata,
// else the invocation's own.
func model(extra, meta map[string]any) string {
	if s, ok := meta["ls_model_name"].(string); ok && s != "" {
		return s
	}
	params, _ := extra["invocation_params"].(map[string]any)
	for _, k := range []string{"model", "model_name"} {
		if s, ok := params[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// text is a run's inputs or outputs as the string the ingest conventions
// read. LangChain wraps a chain's text in a one-key object ({"input": "..."},
// {"output": "..."}): that is unwrapped to the text; anything else is JSON,
// which the conventions read messages from.
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case map[string]any:
		if len(x) == 0 {
			return ""
		}
		if len(x) == 1 {
			for _, inner := range x {
				if s, ok := inner.(string); ok {
					return s
				}
			}
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

// idBytes is a LangSmith ID as an OpenTelemetry ID of n bytes: its hex when it
// is a UUID and n is 16, else a hash of it.
func idBytes(id string, n int) []byte {
	if b, err := hex.DecodeString(strings.ReplaceAll(id, "-", "")); err == nil && len(b) == n {
		return b
	}
	h := sha256.Sum256([]byte(id))
	return h[:n]
}

// feedbackID is the feedback's ID for a trace, policy and metric: a version-4
// shaped UUID from their hash, so a score sent again names the same feedback.
func feedbackID(s source.Score) string {
	h := sha256.Sum256([]byte(s.TraceID + "|" + s.Policy + "|" + s.Metric))
	b := h[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	x := hex.EncodeToString(b)
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

// WriteBack records each score as feedback on the trace's root run. A score
// written before is updated in place (PATCH /feedback/{id}); a new one is
// created under an ID derived from the trace, policy and metric, and updated
// instead if LangSmith already has that ID (an earlier write the manager did
// not record).
func (c *connector) WriteBack(ctx context.Context, scores []source.Score, prior map[store.WriteKey]store.SourceWrite) ([]store.SourceWrite, error) {
	var done []store.SourceWrite
	for _, s := range scores {
		update := map[string]any{}
		switch v := s.Value.(type) {
		case float64:
			update["score"] = v
		case bool:
			update["score"] = map[bool]float64{true: 1, false: 0}[v]
		case string:
			update["value"] = v
		default:
			continue
		}
		// Always sent, so an update with no rationale clears the old comment.
		update["comment"] = s.Rationale
		id, written := feedbackID(s), false
		if p, ok := prior[s.Key()]; ok && p.RemoteID != "" {
			id, written = p.RemoteID, true
		}
		var err error
		var ae *apiError
		if written {
			err = c.call(ctx, http.MethodPatch, "/feedback/"+id, nil, update, nil)
			if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
				// Deleted in LangSmith: create it again. Only a changed
				// score reaches here; the manager does not resend one whose
				// value it already wrote.
				written = false
			}
		}
		if !written {
			err = c.call(ctx, http.MethodPost, "/feedback", nil, createBody(id, s, update), nil)
			if errors.As(err, &ae) && ae.Status == http.StatusConflict {
				err = c.call(ctx, http.MethodPatch, "/feedback/"+id, nil, update, nil)
			}
		}
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			continue // the trace is gone
		}
		if err != nil {
			return done, err
		}
		done = append(done, store.SourceWrite{TraceID: s.TraceID, Policy: s.Policy, Metric: s.Metric, RemoteID: id, Digest: s.Digest(), Written: time.Now().UTC()})
	}
	return done, nil
}

// createBody is a new feedback: on the trace's root run, whose ID is the
// trace's, from the API or, for a judge's score, a model.
func createBody(id string, s source.Score, update map[string]any) map[string]any {
	kind := "api"
	if s.Judged {
		kind = "model"
	}
	body := map[string]any{
		"id": id, "run_id": s.TraceID, "trace_id": s.TraceID, "key": s.Metric,
		"feedback_source": map[string]any{"type": kind, "metadata": map[string]string{
			"evalsi.policy": s.Policy, "evalsi.evaluator": s.Evaluator, "evalsi.judged": strconv.FormatBool(s.Judged),
		}},
	}
	for k, v := range update {
		body[k] = v
	}
	return body
}
