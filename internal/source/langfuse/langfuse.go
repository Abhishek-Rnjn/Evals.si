// Package langfuse reads traces from Langfuse and writes scores back.
// Decision 0016, item 13.
//
// Not verified against a running Langfuse: it is written and tested against
// the pinned OpenAPI spec (Langfuse cannot run in the environment this was
// built in). The spec says the older read endpoints lag by about ten minutes
// and that GET /api/public/v2/observations is the real-time path, so traces
// are read as observations: root observations list the traces, and a trace's
// observations become its spans.
package langfuse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// maxPage is the most observations one request returns (the spec's limit maximum).
const maxPage = 1000

// The first window List reads; an empty window doubles the next, up to maxWindow.
const (
	minWindow = time.Hour
	maxWindow = 7 * 24 * time.Hour
)

const labelPrefix = "evalsi.label."

type connector struct {
	base        string
	auth        string
	client      *http.Client
	environment string
	now         func() time.Time
}

// Factory builds the connector for a source with connector "langfuse". The
// credential is "<public key>:<secret key>" (Langfuse's basic auth); a
// location, when given, is the Langfuse environment to read.
func Factory(src *evalsiv1alpha1.TraceSource, token string, client *http.Client) (source.Connector, error) {
	if v := src.GetVariant(); v != "" {
		return nil, fmt.Errorf("langfuse has no variants, got %q", v)
	}
	if len(src.GetLocations()) > 1 {
		return nil, fmt.Errorf("langfuse: locations names at most one environment")
	}
	u, err := url.Parse(src.GetEndpoint())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("langfuse: endpoint must be an http(s) URL, got %q", src.GetEndpoint())
	}
	c := &connector{base: strings.TrimRight(src.GetEndpoint(), "/"), client: client, now: time.Now}
	if len(src.GetLocations()) == 1 {
		c.environment = src.GetLocations()[0]
	}
	if token != "" {
		if !strings.Contains(token, ":") {
			return nil, fmt.Errorf("langfuse: the credential must be <public key>:<secret key>")
		}
		c.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(token))
	}
	return c, nil
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("langfuse: HTTP %d: %s", e.Status, e.Body) }

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
	if c.auth != "" {
		req.Header.Set("Authorization", c.auth)
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

// observation is the part of ObservationV2 (the spec) the connector reads.
type observation struct {
	ID                string         `json:"id"`
	TraceID           string         `json:"traceId"`
	ParentID          *string        `json:"parentObservationId"`
	Type              string         `json:"type"`
	IsRoot            bool           `json:"isRootObservation"`
	Name              string         `json:"name"`
	StartTime         string         `json:"startTime"`
	EndTime           *string        `json:"endTime"`
	UpdatedAt         string         `json:"updatedAt"`
	Level             string         `json:"level"`
	StatusMessage     string         `json:"statusMessage"`
	Input             any            `json:"input"`
	Output            any            `json:"output"`
	Metadata          map[string]any `json:"metadata"`
	Model             string         `json:"model"`
	UsageDetails      map[string]any `json:"usageDetails"`
	Environment       string         `json:"environment"`
	TraceName         string         `json:"traceName"`
	ProviderTraceTags []string       `json:"tags"`
}

func (c *connector) observations(ctx context.Context, q url.Values) ([]observation, error) {
	var all []observation
	for {
		var resp struct {
			Data []observation `json:"data"`
			Meta struct {
				Cursor *string `json:"cursor"`
			} `json:"meta"`
		}
		if err := c.call(ctx, http.MethodGet, "/api/public/v2/observations", q, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Data...)
		if resp.Meta.Cursor == nil || *resp.Meta.Cursor == "" {
			return all, nil
		}
		q.Set("cursor", *resp.Meta.Cursor)
	}
}

// List reads the root observations (one per trace) that started in a window
// from req.Since or the cursor, oldest first. The spec does not say in which
// order observations come, so a page is one whole window, sorted here, and
// the cursor is where the next window starts; that keeps "oldest first"
// across pages, which the manager's per-page progress relies on.
func (c *connector) List(ctx context.Context, req source.ListRequest) (source.ListResult, error) {
	from := req.Since
	window := minWindow
	if req.Cursor != "" {
		start, w, ok := strings.Cut(req.Cursor, "|")
		t, err := time.Parse(time.RFC3339Nano, start)
		d, derr := time.ParseDuration(w)
		if !ok || err != nil || derr != nil {
			return source.ListResult{}, fmt.Errorf("langfuse: bad cursor %q", req.Cursor)
		}
		from, window = t, d
	}
	now := c.now()
	for {
		to := from.Add(window)
		q := url.Values{
			"fromStartTime": {from.UTC().Format(time.RFC3339Nano)}, "toStartTime": {to.UTC().Format(time.RFC3339Nano)},
			"isRootObservation": {"true"}, "fields": {"core,basic,time"}, "limit": {strconv.Itoa(maxPage)},
		}
		if c.environment != "" {
			q.Set("environment", c.environment)
		}
		roots, err := c.observations(ctx, q)
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
		for _, o := range roots {
			if seen[o.TraceID] {
				continue // two logical roots in one trace: the trace is listed once
			}
			seen[o.TraceID] = true
			started, err := time.Parse(time.RFC3339Nano, o.StartTime)
			if err != nil {
				return source.ListResult{}, fmt.Errorf("langfuse: observation %s has startTime %q: %w", o.ID, o.StartTime, err)
			}
			end := ""
			if o.EndTime != nil {
				end = *o.EndTime
			}
			h := sha256.Sum256([]byte(end + "|" + o.UpdatedAt + "|" + o.Level))
			out.Infos = append(out.Infos, source.Info{
				ID: o.TraceID, Started: started.UTC(), InProgress: o.EndTime == nil, Digest: hex.EncodeToString(h[:8]),
			})
		}
		sort.SliceStable(out.Infos, func(i, j int) bool { return out.Infos[i].Started.Before(out.Infos[j].Started) })
		return out, nil
	}
}

// Fetch reads each trace's observations and makes them spans.
func (c *connector) Fetch(ctx context.Context, infos []source.Info) ([]ingest.Trace, error) {
	var out []ingest.Trace
	for _, in := range infos {
		q := url.Values{
			"traceId": {in.ID}, "fields": {"core,basic,time,io,metadata,model,usage"}, "limit": {strconv.Itoa(maxPage)},
		}
		obs, err := c.observations(ctx, q)
		if err != nil {
			return nil, err
		}
		if len(obs) == 0 {
			continue
		}
		out = append(out, toTrace(in.ID, obs))
	}
	return out, nil
}

// kinds maps Langfuse observation types to the OpenInference span kinds the
// ingest conventions read.
var kinds = map[string]string{
	"GENERATION": "LLM", "AGENT": "AGENT", "TOOL": "TOOL", "RETRIEVER": "RETRIEVER", "EMBEDDING": "EMBEDDING",
	"CHAIN": "CHAIN", "EVALUATOR": "EVALUATOR", "GUARDRAIL": "GUARDRAIL", "SPAN": "CHAIN", "EVENT": "CHAIN",
}

// toTrace builds OTLP spans from observations: OpenInference attributes for
// kind, input and output, model and token counts, so a Langfuse trace reads
// like a pushed one.
func toTrace(traceID string, obs []observation) ingest.Trace {
	tid := idBytes(traceID, 16)
	res := ingest.Attrs{source.AttrSourceTraceID: traceID, "service.name": "langfuse"}
	labels := map[string]string{}
	t := ingest.Trace{TraceID: hex.EncodeToString(tid)}
	for _, o := range obs {
		if o.TraceName != "" {
			res["service.name"] = o.TraceName
		}
		sp := &tracepb.Span{TraceId: tid, SpanId: idBytes(o.ID, 8), Name: o.Name}
		if o.ParentID != nil && *o.ParentID != "" && !o.IsRoot {
			sp.ParentSpanId = idBytes(*o.ParentID, 8)
		}
		sp.StartTimeUnixNano = nanos(o.StartTime)
		if o.EndTime != nil {
			sp.EndTimeUnixNano = nanos(*o.EndTime)
		}
		if o.Level == "ERROR" {
			sp.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: o.StatusMessage}
		}
		attrs := []*commonpb.KeyValue{str("langfuse.observation.type", o.Type)}
		if k := kinds[o.Type]; k != "" {
			attrs = append(attrs, str("openinference.span.kind", k))
		}
		if s := text(o.Input); s != "" {
			attrs = append(attrs, str("input.value", s))
		}
		if s := text(o.Output); s != "" {
			attrs = append(attrs, str("output.value", s))
		}
		if o.Model != "" {
			attrs = append(attrs, str("llm.model_name", o.Model))
		}
		for key, attr := range map[string]string{"input": "llm.token_count.prompt", "output": "llm.token_count.completion", "total": "llm.token_count.total"} {
			if n, ok := o.UsageDetails[key].(float64); ok {
				attrs = append(attrs, &commonpb.KeyValue{Key: attr, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: int64(n)}}})
			}
		}
		for k, v := range o.Metadata {
			s := text(v)
			attrs = append(attrs, str("langfuse.metadata."+k, s))
			if l, ok := strings.CutPrefix(k, labelPrefix); ok && l != "" {
				labels[l] = s
			}
		}
		if o.Environment != "" {
			res["langfuse.environment"] = o.Environment
		}
		sp.Attributes = attrs
		t.Spans = append(t.Spans, ingest.Span{Span: sp, Resource: res, Labels: labels})
	}
	return t
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// text is an observation field as the string the ingest conventions read: the
// spec returns input and output as raw strings, metadata values as JSON.
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

// idBytes is a Langfuse ID as an OpenTelemetry ID of n bytes: its hex when it
// is hex of that length (OTel-ingested traces; UUIDs without dashes), else a
// hash of it.
func idBytes(id string, n int) []byte {
	if b, err := hex.DecodeString(strings.ReplaceAll(id, "-", "")); err == nil && len(b) == n {
		return b
	}
	h := sha256.Sum256([]byte(id))
	return h[:n]
}

func nanos(s string) uint64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.UnixNano() < 0 {
		return 0
	}
	return uint64(t.UnixNano())
}

// WriteBack creates a score per metric on the trace, with an ID derived from
// the trace, policy and metric. Whether Langfuse replaces a score sent again
// with the same ID is not stated in the spec and not verified; the manager
// sends a score again only when its value changed.
func (c *connector) WriteBack(ctx context.Context, scores []source.Score, _ map[[2]string]store.SourceWrite) ([]store.SourceWrite, error) {
	var done []store.SourceWrite
	for _, s := range scores {
		h := sha256.Sum256([]byte(s.TraceID + "|" + s.Policy + "|" + s.Metric))
		id := "evalsi-" + hex.EncodeToString(h[:12])
		body := map[string]any{
			"id": id, "traceId": s.TraceID, "name": s.Metric, "source": "API",
			"metadata": map[string]string{"evalsi.policy": s.Policy, "evalsi.evaluator": s.Evaluator, "evalsi.judged": strconv.FormatBool(s.Judged)},
		}
		switch v := s.Value.(type) {
		case float64:
			body["value"], body["dataType"] = v, "NUMERIC"
		case bool:
			body["value"], body["dataType"] = map[bool]float64{true: 1, false: 0}[v], "BOOLEAN"
		case string:
			body["value"], body["dataType"] = v, "CATEGORICAL"
		default:
			continue
		}
		if s.Rationale != "" {
			body["comment"] = s.Rationale
		}
		var resp struct {
			ID string `json:"id"`
		}
		err := c.call(ctx, http.MethodPost, "/api/public/scores", nil, body, &resp)
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			continue
		}
		if err != nil {
			return done, err
		}
		if resp.ID == "" {
			resp.ID = id
		}
		done = append(done, store.SourceWrite{TraceID: s.TraceID, Metric: s.Metric, RemoteID: resp.ID, Digest: s.Digest(), Written: time.Now().UTC()})
	}
	return done, nil
}
