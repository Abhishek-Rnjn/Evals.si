// Package mlflow reads traces from an MLflow tracking server (open-source
// 3.x) and writes scores back as assessments. Decision 0016, items 7 to 12.
package mlflow

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
	"google.golang.org/protobuf/encoding/protojson"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/source"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// maxSearch is the most traces MLflow returns for one search (service.proto,
// SearchTracesV3.max_results).
const maxSearch = 500

// labelPrefix marks trace tags that become labels on the pulled trace, so a
// studio can tag its traces evalsi.label.workflow and select on it.
const labelPrefix = "evalsi.label."

type connector struct {
	base   string
	token  string
	client *http.Client
	src    *evalsiv1alpha1.TraceSource
}

// Factory builds the connector for a source with connector "mlflow".
func Factory(src *evalsiv1alpha1.TraceSource, token string, client *http.Client) (source.Connector, error) {
	switch v := src.GetVariant(); v {
	case "", "oss":
	default:
		return nil, fmt.Errorf("mlflow variant %q is not built (decision 0016): only \"oss\" is", v)
	}
	if len(src.GetLocations()) == 0 {
		return nil, fmt.Errorf("mlflow: locations must list at least one experiment ID or name")
	}
	u, err := url.Parse(src.GetEndpoint())
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("mlflow: endpoint must be an http(s) URL, got %q", src.GetEndpoint())
	}
	return &connector{base: strings.TrimRight(src.GetEndpoint(), "/"), token: token, client: client, src: src}, nil
}

type apiError struct {
	Status int
	Code   string `json:"error_code"`
	Msg    string `json:"message"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("mlflow: HTTP %d %s: %s", e.Status, e.Code, e.Msg)
}

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
		e := &apiError{Status: resp.StatusCode}
		if json.Unmarshal(raw, e) != nil || e.Msg == "" {
			e.Msg = strings.TrimSpace(string(raw[:min(len(raw), 300)]))
		}
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

// traceInfo is the part of TraceInfoV3 (service.proto) the connector reads.
type traceInfo struct {
	TraceID           string            `json:"trace_id"`
	RequestTime       string            `json:"request_time"`
	ExecutionDuration string            `json:"execution_duration"`
	State             string            `json:"state"`
	Metadata          map[string]string `json:"trace_metadata"`
	Tags              map[string]string `json:"tags"`
	Location          struct {
		Experiment struct {
			ID string `json:"experiment_id"`
		} `json:"mlflow_experiment"`
	} `json:"trace_location"`
	Assessments []assessment `json:"assessments"`
}

type assessment struct {
	ID     string `json:"assessment_id"`
	Name   string `json:"assessment_name"`
	Source struct {
		ID string `json:"source_id"`
	} `json:"source"`
}

// List searches the experiments for traces that started at or after req.Since,
// oldest first. MLflow indexes a trace by its start time but only knows of it
// once it ends (decision 0016, item 15), which is why the manager reads back.
func (c *connector) List(ctx context.Context, req source.ListRequest) (source.ListResult, error) {
	ids, err := c.experimentIDs(ctx)
	if err != nil {
		return source.ListResult{}, err
	}
	locations := make([]map[string]any, len(ids))
	for i, id := range ids {
		locations[i] = map[string]any{"type": "MLFLOW_EXPERIMENT", "mlflow_experiment": map[string]any{"experiment_id": id}}
	}
	body := map[string]any{
		"locations":   locations,
		"filter":      fmt.Sprintf("trace.timestamp_ms >= %d", req.Since.UnixMilli()),
		"order_by":    []string{"timestamp_ms ASC"},
		"max_results": max(1, min(req.Limit, maxSearch)),
	}
	if req.Cursor != "" {
		body["page_token"] = req.Cursor
	}
	var resp struct {
		Traces []traceInfo `json:"traces"`
		Next   string      `json:"next_page_token"`
	}
	if err := c.call(ctx, http.MethodPost, "/api/3.0/mlflow/traces/search", nil, body, &resp); err != nil {
		return source.ListResult{}, err
	}
	out := source.ListResult{Next: resp.Next}
	for _, t := range resp.Traces {
		started, err := time.Parse(time.RFC3339Nano, t.RequestTime)
		if err != nil {
			return source.ListResult{}, fmt.Errorf("mlflow: trace %s has request_time %q: %w", t.TraceID, t.RequestTime, err)
		}
		out.Infos = append(out.Infos, source.Info{
			ID: t.TraceID, Started: started.UTC(), InProgress: t.State == "IN_PROGRESS", Digest: digest(t),
		})
	}
	return out, nil
}

// experimentIDs resolves the locations: an experiment ID as is, anything else
// as an experiment name (MLflow assigns IDs, so a spec written before the
// experiment exists can only know its name).
func (c *connector) experimentIDs(ctx context.Context) ([]string, error) {
	out := make([]string, 0, len(c.src.GetLocations()))
	for _, loc := range c.src.GetLocations() {
		if _, err := strconv.ParseUint(loc, 10, 64); err == nil {
			out = append(out, loc)
			continue
		}
		var resp struct {
			Experiment struct {
				ID string `json:"experiment_id"`
			} `json:"experiment"`
		}
		err := c.call(ctx, http.MethodGet, "/api/2.0/mlflow/experiments/get-by-name", url.Values{"experiment_name": {loc}}, nil, &resp)
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil, fmt.Errorf("mlflow: no experiment named %q (yet)", loc)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Experiment.ID)
	}
	return out, nil
}

// digest changes when a trace's state, duration or size does. A trace that
// gains spans after it was first seen gets a new digest and is scored again.
func digest(t traceInfo) string {
	h := sha256.Sum256([]byte(t.State + "|" + t.ExecutionDuration + "|" + t.Metadata["mlflow.trace.sizeBytes"] + "|" + t.Metadata["mlflow.trace.sizeStats"]))
	return hex.EncodeToString(h[:8])
}

// Fetch gets the spans of the listed traces.
func (c *connector) Fetch(ctx context.Context, infos []source.Info) ([]ingest.Trace, error) {
	var out []ingest.Trace
	const batch = 50
	for start := 0; start < len(infos); start += batch {
		q := url.Values{}
		for _, in := range infos[start:min(start+batch, len(infos))] {
			q.Add("trace_ids", in.ID)
		}
		var resp struct {
			Traces []struct {
				Info  traceInfo         `json:"trace_info"`
				Spans []json.RawMessage `json:"spans"`
			} `json:"traces"`
		}
		if err := c.call(ctx, http.MethodGet, "/api/3.0/mlflow/traces/batchGet", q, nil, &resp); err != nil {
			return nil, err
		}
		for _, t := range resp.Traces {
			tr, err := toTrace(t.Info, t.Spans)
			if err != nil {
				return nil, err
			}
			if len(tr.Spans) > 0 {
				out = append(out, tr)
			}
		}
	}
	return out, nil
}

// toTrace turns MLflow's spans (OTLP spans in protobuf JSON: base64 IDs) into
// an ingest trace, so the conventions ToRecord already reads apply.
func toTrace(info traceInfo, raws []json.RawMessage) (ingest.Trace, error) {
	res := ingest.Attrs{
		source.AttrSourceTraceID: info.TraceID,
		"service.name":           firstNonEmpty(info.Tags["mlflow.traceName"], "mlflow"),
		"mlflow.experiment_id":   info.Location.Experiment.ID,
	}
	labels := map[string]string{}
	for k, v := range info.Tags {
		res["mlflow.tag."+k] = v
		if l, ok := strings.CutPrefix(k, labelPrefix); ok && l != "" {
			labels[l] = v
		}
	}
	for k, v := range info.Metadata {
		res["mlflow.metadata."+k] = v
	}
	t := ingest.Trace{}
	for _, raw := range raws {
		sp := &tracepb.Span{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, sp); err != nil {
			return ingest.Trace{}, fmt.Errorf("mlflow: trace %s has a span that is not OTLP JSON: %w", info.TraceID, err)
		}
		for _, kv := range sp.Attributes {
			if strings.HasPrefix(kv.Key, "mlflow.span") {
				kv.Value = asJSONString(kv.Value)
			}
		}
		if t.TraceID == "" {
			t.TraceID = hex.EncodeToString(sp.TraceId)
		}
		t.Spans = append(t.Spans, ingest.Span{Span: sp, Resource: res, Labels: labels})
	}
	return t, nil
}

// asJSONString re-encodes a structured attribute as the JSON string MLflow's
// OTLP exporter sends, which is what the ingest conventions read. The REST API
// returns the decoded value.
func asJSONString(v *commonpb.AnyValue) *commonpb.AnyValue {
	switch v.GetValue().(type) {
	case *commonpb.AnyValue_KvlistValue, *commonpb.AnyValue_ArrayValue:
		raw, err := json.Marshal(ingest.AnyValue(v))
		if err != nil {
			return v
		}
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: string(raw)}}
	}
	return v
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// WriteBack records each score as a feedback assessment on its trace. A score
// already written updates its assessment in place; with no record of one (the
// bookkeeping was lost) the trace's own assessments are searched for the same
// name and source before another is created.
func (c *connector) WriteBack(ctx context.Context, scores []source.Score, prior map[store.WriteKey]store.SourceWrite) ([]store.SourceWrite, error) {
	var done []store.SourceWrite
	existing := map[string][]assessment{}
	for _, s := range scores {
		sourceID := "evalsi/" + s.Policy
		remote := prior[s.Key()].RemoteID
		if remote == "" {
			if _, ok := existing[s.TraceID]; !ok {
				found, err := c.assessments(ctx, s.TraceID)
				if err != nil {
					return done, err
				}
				existing[s.TraceID] = found
			}
			for _, a := range existing[s.TraceID] {
				if a.Name == s.Metric && a.Source.ID == sourceID {
					remote = a.ID
				}
			}
		}
		feedback := map[string]any{"value": s.Value}
		var err error
		if remote != "" {
			a := map[string]any{"assessment_id": remote, "trace_id": s.TraceID, "assessment_name": s.Metric, "feedback": feedback, "rationale": s.Rationale}
			err = c.call(ctx, http.MethodPatch, "/api/3.0/mlflow/traces/"+url.PathEscape(s.TraceID)+"/assessments/"+url.PathEscape(remote), nil,
				map[string]any{"assessment": a, "update_mask": "feedback,rationale"}, nil)
		} else {
			kind := "CODE"
			if s.Judged {
				kind = "LLM_JUDGE"
			}
			a := map[string]any{
				"assessment_name": s.Metric, "trace_id": s.TraceID, "feedback": feedback,
				"source":   map[string]any{"source_type": kind, "source_id": sourceID},
				"metadata": map[string]string{"evalsi.policy": s.Policy, "evalsi.evaluator": s.Evaluator},
			}
			if s.Rationale != "" {
				a["rationale"] = s.Rationale
			}
			var created struct {
				Assessment assessment `json:"assessment"`
			}
			err = c.call(ctx, http.MethodPost, "/api/3.0/mlflow/traces/"+url.PathEscape(s.TraceID)+"/assessments", nil, map[string]any{"assessment": a}, &created)
			remote = created.Assessment.ID
		}
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			continue // the trace was deleted; there is nothing to write on
		}
		if err != nil {
			return done, err
		}
		done = append(done, store.SourceWrite{TraceID: s.TraceID, Policy: s.Policy, Metric: s.Metric, RemoteID: remote, Digest: s.Digest(), Written: time.Now().UTC()})
	}
	return done, nil
}

// assessments reads the assessments already on a trace.
func (c *connector) assessments(ctx context.Context, traceID string) ([]assessment, error) {
	var resp struct {
		Trace struct {
			Info traceInfo `json:"trace_info"`
		} `json:"trace"`
	}
	err := c.call(ctx, http.MethodGet, "/api/3.0/mlflow/traces/"+url.PathEscape(traceID), nil, nil, &resp)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, nil
	}
	return resp.Trace.Info.Assessments, err
}
