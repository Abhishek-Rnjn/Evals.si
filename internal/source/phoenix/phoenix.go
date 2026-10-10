// Package phoenix reads traces from Arize Phoenix and writes scores back as
// trace annotations. Decision 0016, item 14.
package phoenix

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

// maxPage is the most traces or spans Phoenix returns for one request
// (the OpenAPI spec's limit maximum).
const maxPage = 1000

// labelPrefix marks span attributes that become labels on the pulled trace.
const labelPrefix = "evalsi.label."

type connector struct {
	base    string
	token   string
	client  *http.Client
	project string
}

// Factory builds the connector for a source with connector "phoenix". Its one
// location is the Phoenix project (name or ID).
func Factory(src *evalsiv1alpha1.TraceSource, token string, client *http.Client) (source.Connector, error) {
	if v := src.GetVariant(); v != "" {
		return nil, fmt.Errorf("phoenix has no variants, got %q", v)
	}
	if len(src.GetLocations()) != 1 {
		return nil, fmt.Errorf("phoenix: locations must name exactly one Phoenix project")
	}
	p := src.GetLocations()[0]
	if p == "" || strings.ContainsAny(p, "/?#") {
		return nil, fmt.Errorf("phoenix: project %q must not contain '/', '?' or '#'", p)
	}
	u, err := url.Parse(src.GetEndpoint())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("phoenix: endpoint must be an http(s) URL, got %q", src.GetEndpoint())
	}
	return &connector{base: strings.TrimRight(src.GetEndpoint(), "/"), token: token, client: client, project: p}, nil
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("phoenix: HTTP %d: %s", e.Status, e.Body) }

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
	if c.token != "" {
		// Phoenix's API keys are bearer tokens when authentication is on.
		req.Header.Set("Authorization", "Bearer "+c.token)
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

func (c *connector) projectPath(rest string) string {
	return "/v1/projects/" + url.PathEscape(c.project) + rest
}

// List reads a page of traces that started at or after req.Since, oldest
// first, with their span summaries: a trace whose root span has not arrived
// is still running.
func (c *connector) List(ctx context.Context, req source.ListRequest) (source.ListResult, error) {
	q := url.Values{
		"start_time": {req.Since.UTC().Format(time.RFC3339Nano)}, "sort": {"start_time"}, "order": {"asc"},
		"limit": {strconv.Itoa(max(1, min(req.Limit, maxPage)))}, "include_spans": {"true"},
	}
	if req.Cursor != "" {
		q.Set("cursor", req.Cursor)
	}
	var resp struct {
		Data []struct {
			TraceID   string `json:"trace_id"`
			StartTime string `json:"start_time"`
			EndTime   string `json:"end_time"`
			Spans     []struct {
				ParentID *string `json:"parent_id"`
			} `json:"spans"`
		} `json:"data"`
		Next *string `json:"next_cursor"`
	}
	if err := c.call(ctx, http.MethodGet, c.projectPath("/traces"), q, nil, &resp); err != nil {
		return source.ListResult{}, err
	}
	out := source.ListResult{}
	if resp.Next != nil {
		out.Next = *resp.Next
	}
	for _, t := range resp.Data {
		started, err := time.Parse(time.RFC3339Nano, t.StartTime)
		if err != nil {
			return source.ListResult{}, fmt.Errorf("phoenix: trace %s has start_time %q: %w", t.TraceID, t.StartTime, err)
		}
		root := false
		for _, s := range t.Spans {
			root = root || s.ParentID == nil
		}
		h := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%t", len(t.Spans), t.EndTime, root)))
		out.Infos = append(out.Infos, source.Info{
			ID: t.TraceID, Started: started.UTC(), InProgress: !root, Digest: hex.EncodeToString(h[:8]),
		})
	}
	return out, nil
}

// span is a span as GET /v1/projects/{project}/spans returns it.
type span struct {
	Name    string `json:"name"`
	Context struct {
		TraceID string `json:"trace_id"`
		SpanID  string `json:"span_id"`
	} `json:"context"`
	SpanKind      string         `json:"span_kind"`
	ParentID      *string        `json:"parent_id"`
	StartTime     string         `json:"start_time"`
	EndTime       string         `json:"end_time"`
	StatusCode    string         `json:"status_code"`
	StatusMessage string         `json:"status_message"`
	Attributes    map[string]any `json:"attributes"`
	Events        []struct {
		Name       string         `json:"name"`
		Timestamp  string         `json:"timestamp"`
		Attributes map[string]any `json:"attributes"`
	} `json:"events"`
}

// Fetch reads the full spans of the listed traces.
func (c *connector) Fetch(ctx context.Context, infos []source.Info) ([]ingest.Trace, error) {
	byTrace := map[string][]span{}
	const batch = 50
	for start := 0; start < len(infos); start += batch {
		q := url.Values{"limit": {strconv.Itoa(maxPage)}}
		for _, in := range infos[start:min(start+batch, len(infos))] {
			q.Add("trace_id", in.ID)
		}
		for {
			var resp struct {
				Data []span  `json:"data"`
				Next *string `json:"next_cursor"`
			}
			if err := c.call(ctx, http.MethodGet, c.projectPath("/spans"), q, nil, &resp); err != nil {
				return nil, err
			}
			for _, s := range resp.Data {
				byTrace[s.Context.TraceID] = append(byTrace[s.Context.TraceID], s)
			}
			if resp.Next == nil || *resp.Next == "" {
				break
			}
			q.Set("cursor", *resp.Next)
		}
	}
	var out []ingest.Trace
	for _, in := range infos {
		spans := byTrace[in.ID]
		if len(spans) == 0 {
			continue
		}
		t, err := c.toTrace(in.ID, spans)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// toTrace turns Phoenix's spans into OTLP spans, putting back what Phoenix
// moved out of the attributes (the OpenInference span kind) so the ingest
// conventions read them as for a pushed trace.
func (c *connector) toTrace(traceID string, spans []span) (ingest.Trace, error) {
	res := ingest.Attrs{source.AttrSourceTraceID: traceID, "service.name": c.project, "phoenix.project": c.project}
	labels := map[string]string{}
	t := ingest.Trace{TraceID: traceID}
	for _, s := range spans {
		sp := &tracepb.Span{Name: s.Name}
		var err error
		if sp.TraceId, err = hex.DecodeString(s.Context.TraceID); err != nil {
			return ingest.Trace{}, fmt.Errorf("phoenix: trace id %q: %w", s.Context.TraceID, err)
		}
		if sp.SpanId, err = hex.DecodeString(s.Context.SpanID); err != nil {
			return ingest.Trace{}, fmt.Errorf("phoenix: span id %q: %w", s.Context.SpanID, err)
		}
		if s.ParentID != nil {
			if sp.ParentSpanId, err = hex.DecodeString(*s.ParentID); err != nil {
				return ingest.Trace{}, fmt.Errorf("phoenix: parent id %q: %w", *s.ParentID, err)
			}
		}
		sp.StartTimeUnixNano, sp.EndTimeUnixNano = nanos(s.StartTime), nanos(s.EndTime)
		switch s.StatusCode {
		case "OK":
			sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK}
		case "ERROR":
			sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: s.StatusMessage}
		}
		attrs := s.Attributes
		if _, ok := attrs["openinference.span.kind"]; !ok && s.SpanKind != "" && s.SpanKind != "UNKNOWN" {
			attrs = withKey(attrs, "openinference.span.kind", s.SpanKind)
		}
		sp.Attributes = keyValues(attrs)
		for k, v := range attrs {
			if l, ok := strings.CutPrefix(k, labelPrefix); ok && l != "" {
				labels[l] = fmt.Sprint(v)
			}
		}
		for _, ev := range s.Events {
			sp.Events = append(sp.Events, &tracepb.Span_Event{Name: ev.Name, TimeUnixNano: nanos(ev.Timestamp), Attributes: keyValues(ev.Attributes)})
		}
		t.Spans = append(t.Spans, ingest.Span{Span: sp, Resource: res, Labels: labels})
	}
	return t, nil
}

func withKey(m map[string]any, k string, v any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for a, b := range m {
		out[a] = b
	}
	out[k] = v
	return out
}

func nanos(s string) uint64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.UnixNano() < 0 {
		return 0
	}
	return uint64(t.UnixNano())
}

// keyValues turns Phoenix's JSON attributes into OTLP ones. Nested objects
// (Phoenix stores some attributes unflattened) become key-value lists.
func keyValues(m map[string]any) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, 0, len(m))
	for k, v := range m {
		out = append(out, &commonpb.KeyValue{Key: k, Value: anyValue(v)})
	}
	return out
}

func anyValue(v any) *commonpb.AnyValue {
	switch x := v.(type) {
	case string:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: x}}
	case bool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: x}}
	case float64:
		if x == float64(int64(x)) {
			return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(x)}}
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: x}}
	case []any:
		vals := make([]*commonpb.AnyValue, len(x))
		for i, e := range x {
			vals[i] = anyValue(e)
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: vals}}}
	case map[string]any:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: keyValues(x)}}}
	case nil:
		return &commonpb.AnyValue{}
	}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fmt.Sprint(v)}}
}

// WriteBack records each score as a trace annotation. Phoenix updates an
// annotation in place when one with the same name and identifier exists on
// the trace (verified on 20.20.0), so a changed score replaces the old one
// and a retry adds nothing.
func (c *connector) WriteBack(ctx context.Context, scores []source.Score, _ map[store.WriteKey]store.SourceWrite) ([]store.SourceWrite, error) {
	var done []store.SourceWrite
	for _, s := range scores {
		kind := "CODE"
		if s.Judged {
			kind = "LLM"
		}
		result := map[string]any{}
		switch v := s.Value.(type) {
		case float64:
			result["score"] = v
		case bool:
			result["label"] = map[bool]string{true: "pass", false: "fail"}[v]
			result["score"] = map[bool]float64{true: 1, false: 0}[v]
		case string:
			result["label"] = v
		}
		if s.Rationale != "" {
			result["explanation"] = s.Rationale
		}
		body := map[string]any{"data": []any{map[string]any{
			"name": s.Metric, "annotator_kind": kind, "trace_id": s.TraceID, "identifier": "evalsi/" + s.Policy,
			"result": result, "metadata": map[string]string{"evalsi.policy": s.Policy, "evalsi.evaluator": s.Evaluator},
		}}}
		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		err := c.call(ctx, http.MethodPost, "/v1/trace_annotations", url.Values{"sync": {"true"}}, body, &resp)
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			continue // the trace was deleted
		}
		if err != nil {
			return done, err
		}
		id := ""
		if len(resp.Data) > 0 {
			id = resp.Data[0].ID
		}
		done = append(done, store.SourceWrite{TraceID: s.TraceID, Policy: s.Policy, Metric: s.Metric, RemoteID: id, Digest: s.Digest(), Written: time.Now().UTC()})
	}
	return done, nil
}
