package sinks

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// PhoenixConfig writes online scores to Arize Phoenix as trace annotations
// on the traces they evaluated (which must also be in Phoenix, sharing
// trace IDs). Runs are not exported to Phoenix.
type PhoenixConfig struct {
	// For example http://phoenix:6006.
	Endpoint string `json:"endpoint"`
	// Environment variable holding an API key, if the server needs one.
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

func (c *PhoenixConfig) validate() error { return httpURL("phoenix.endpoint", c.Endpoint) }

type phoenixSink struct {
	cfg    PhoenixConfig
	client *http.Client
}

func newPhoenix(cfg PhoenixConfig, client *http.Client) *phoenixSink {
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	return &phoenixSink{cfg: cfg, client: client}
}

func (p *phoenixSink) Name() string { return "phoenix" }

func (p *phoenixSink) ExportRun(context.Context, *evalsiv1alpha1.Run) error { return nil }

func (p *phoenixSink) ExportTrace(ctx context.Context, t *Trace) error {
	if t.TraceID == "" {
		return nil
	}
	var data []map[string]any
	for _, r := range t.Results {
		if r.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
			continue
		}
		for _, s := range r.GetScores() {
			kind := "CODE"
			if _, judged := s.GetMetadata()["judge_model"]; judged {
				kind = "LLM"
			}
			result := map[string]any{}
			if v, ok := scoreValue(s); ok {
				result["score"] = v
			}
			if l, ok := s.GetValue().(*evalsiv1alpha1.Score_Label); ok {
				result["label"] = l.Label
			}
			if pass, ok := s.GetValue().(*evalsiv1alpha1.Score_Passed); ok {
				result["label"] = map[bool]string{true: "pass", false: "fail"}[pass.Passed]
			}
			if len(result) == 0 {
				continue
			}
			if s.GetExplanation() != "" {
				result["explanation"] = s.GetExplanation()
			}
			data = append(data, map[string]any{
				"trace_id": t.TraceID, "name": metricName(r, s), "annotator_kind": kind, "result": result,
				"metadata": map[string]string{"evalsi.policy": t.Policy, "evalsi.evaluator": r.GetEvaluatorRef()},
			})
		}
	}
	if len(data) == 0 {
		return nil
	}
	h := http.Header{}
	if p.cfg.APIKeyEnv != "" {
		h.Set("Authorization", "Bearer "+os.Getenv(p.cfg.APIKeyEnv))
	}
	if err := postJSON(ctx, p.client, p.cfg.Endpoint+"/v1/trace_annotations?sync=false", h, map[string]any{"data": data}, nil); err != nil {
		return fmt.Errorf("phoenix: %w", err)
	}
	return nil
}
