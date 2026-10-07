package sinks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// postJSON POSTs body as JSON. 4xx answers (except 429) are permanent.
func postJSON(ctx context.Context, client *http.Client, target string, header http.Header, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		e := fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return permanent{e}
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func httpURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an http(s) URL, got %q", field, raw)
	}
	return nil
}

func newEventID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LangfuseConfig exports runs as Langfuse traces with a score per metric,
// and online scores as scores on the traces they evaluated.
type LangfuseConfig struct {
	// For example https://cloud.langfuse.com or http://langfuse:3000.
	Host string `json:"host"`
	// Environment variables holding the project's public and secret keys.
	PublicKeyEnv string `json:"public_key_env"`
	SecretKeyEnv string `json:"secret_key_env"`
	// Write online scores to Langfuse traces (they must share trace IDs,
	// for example traces sent to Langfuse over OTLP).
	TraceFeedback bool `json:"trace_feedback,omitempty"`
}

func (c *LangfuseConfig) validate() error {
	if err := httpURL("langfuse.host", c.Host); err != nil {
		return err
	}
	if c.PublicKeyEnv == "" || c.SecretKeyEnv == "" {
		return fmt.Errorf("langfuse needs public_key_env and secret_key_env")
	}
	return nil
}

type langfuseSink struct {
	cfg    LangfuseConfig
	client *http.Client
}

func newLangfuse(cfg LangfuseConfig, client *http.Client) *langfuseSink {
	cfg.Host = strings.TrimRight(cfg.Host, "/")
	return &langfuseSink{cfg: cfg, client: client}
}

func (l *langfuseSink) Name() string { return "langfuse" }

type langfuseEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	Timestamp string         `json:"timestamp"`
	Body      map[string]any `json:"body"`
}

func (l *langfuseSink) ingest(ctx context.Context, events []langfuseEvent) error {
	if len(events) == 0 {
		return nil
	}
	h := http.Header{}
	creds := os.Getenv(l.cfg.PublicKeyEnv) + ":" + os.Getenv(l.cfg.SecretKeyEnv)
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	var out struct {
		Errors []struct {
			ID      string `json:"id"`
			Status  int    `json:"status"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := postJSON(ctx, l.client, l.cfg.Host+"/api/public/ingestion", h, map[string]any{"batch": events}, &out); err != nil {
		return fmt.Errorf("langfuse: %w", err)
	}
	if len(out.Errors) > 0 {
		e := out.Errors[0]
		return permanent{fmt.Errorf("langfuse rejected %d of %d events, first %s: %d %s", len(out.Errors), len(events), e.ID, e.Status, e.Message)}
	}
	return nil
}

func langfuseScore(traceID, name string, s *evalsiv1alpha1.Score, comment string, meta map[string]any) map[string]any {
	body := map[string]any{"id": newEventID(), "traceId": traceID, "name": name, "metadata": meta}
	switch v := s.GetValue().(type) {
	case *evalsiv1alpha1.Score_Number:
		body["value"], body["dataType"] = v.Number, "NUMERIC"
	case *evalsiv1alpha1.Score_Passed:
		value := 0.0
		if v.Passed {
			value = 1
		}
		body["value"], body["dataType"] = value, "BOOLEAN"
	case *evalsiv1alpha1.Score_Label:
		body["value"], body["dataType"] = v.Label, "CATEGORICAL"
	default:
		return nil
	}
	if comment != "" {
		body["comment"] = comment
	}
	return body
}

// ExportRun writes the run as a trace named after it, with one score per
// metric mean (and its interval in the comment).
func (l *langfuseSink) ExportRun(ctx context.Context, run *evalsiv1alpha1.Run) error {
	if !terminal(run.GetStatus()) {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	traceID := "evalsi-run-" + run.GetId()
	name := run.GetName()
	if name == "" {
		name = run.GetId()
	}
	events := []langfuseEvent{{ID: newEventID(), Type: "trace-create", Timestamp: now, Body: map[string]any{
		"id": traceID, "name": "evalsi run " + name,
		"tags":     []string{"evalsi", "project:" + run.GetProject()},
		"metadata": map[string]any{"run_id": run.GetId(), "project": run.GetProject(), "status": run.GetStatus().String(), "records": run.GetRecords()},
	}}}
	for _, s := range run.GetSummaries() {
		if s.Mean == nil {
			continue
		}
		comment := fmt.Sprintf("n=%d", s.GetN())
		if ci := s.GetCi(); ci != nil {
			comment += fmt.Sprintf(", %.0f%% CI [%.4g, %.4g]", ci.GetLevel()*100, ci.GetLow(), ci.GetHigh())
		}
		body := langfuseScore(traceID, s.GetMetric(), &evalsiv1alpha1.Score{Value: &evalsiv1alpha1.Score_Number{Number: s.GetMean()}}, comment,
			map[string]any{"evaluator": s.GetEvaluator(), "skipped": s.GetSkipped(), "errors": s.GetErrors()})
		events = append(events, langfuseEvent{ID: newEventID(), Type: "score-create", Timestamp: now, Body: body})
	}
	return l.ingest(ctx, events)
}

// ExportTrace writes online scores onto the Langfuse trace with the same ID.
func (l *langfuseSink) ExportTrace(ctx context.Context, t *Trace) error {
	if !l.cfg.TraceFeedback || t.TraceID == "" {
		return nil
	}
	now := t.Time.UTC().Format(time.RFC3339Nano)
	var events []langfuseEvent
	for _, r := range t.Results {
		if r.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
			continue
		}
		for _, s := range r.GetScores() {
			body := langfuseScore(t.TraceID, metricName(r, s), s, s.GetExplanation(),
				map[string]any{"policy": t.Policy, "evaluator": r.GetEvaluatorRef()})
			if body != nil {
				events = append(events, langfuseEvent{ID: newEventID(), Type: "score-create", Timestamp: now, Body: body})
			}
		}
	}
	return l.ingest(ctx, events)
}
