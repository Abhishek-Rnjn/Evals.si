package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// MLflowConfig exports runs as MLflow runs and, optionally, online scores as
// assessments on the MLflow traces they evaluated.
type MLflowConfig struct {
	// For example http://mlflow:5000.
	TrackingURI string `json:"tracking_uri"`
	// Experiment for runs; default: the run's project, else "evalsi".
	Experiment string `json:"experiment,omitempty"`
	// Environment variable holding a bearer token, if the server needs one.
	TokenEnv string `json:"token_env,omitempty"`
	// Write online scores back to MLflow traces as feedback assessments (MLflow 3).
	// Only useful when the traces also go to MLflow: trace IDs must match.
	TraceFeedback bool `json:"trace_feedback,omitempty"`
}

func (c *MLflowConfig) validate() error {
	u, err := url.Parse(c.TrackingURI)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("mlflow.tracking_uri must be an http(s) URL, got %q", c.TrackingURI)
	}
	return nil
}

type mlflowSink struct {
	cfg    MLflowConfig
	client *http.Client

	mu          sync.Mutex
	experiments map[string]string
}

func newMLflow(cfg MLflowConfig, client *http.Client) *mlflowSink {
	cfg.TrackingURI = strings.TrimRight(cfg.TrackingURI, "/")
	return &mlflowSink{cfg: cfg, client: client, experiments: map[string]string{}}
}

func (m *mlflowSink) Name() string { return "mlflow" }

type mlflowError struct {
	Status int
	Code   string `json:"error_code"`
	Msg    string `json:"message"`
}

func (e *mlflowError) Error() string {
	return fmt.Sprintf("mlflow: HTTP %d %s: %s", e.Status, e.Code, e.Msg)
}

// call POSTs (or GETs, with body nil) an MLflow REST endpoint.
func (m *mlflowSink) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.cfg.TrackingURI+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.cfg.TokenEnv != "" {
		req.Header.Set("Authorization", "Bearer "+os.Getenv(m.cfg.TokenEnv))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		e := &mlflowError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, e)
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return permanent{e}
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (m *mlflowSink) experimentID(ctx context.Context, name string) (string, error) {
	m.mu.Lock()
	id, ok := m.experiments[name]
	m.mu.Unlock()
	if ok {
		return id, nil
	}
	var got struct {
		Experiment struct {
			ID string `json:"experiment_id"`
		} `json:"experiment"`
	}
	err := m.call(ctx, http.MethodGet, "/api/2.0/mlflow/experiments/get-by-name?experiment_name="+url.QueryEscape(name), nil, &got)
	var me *mlflowError
	if errors.As(err, &me) && me.Code == "RESOURCE_DOES_NOT_EXIST" {
		var created struct {
			ID string `json:"experiment_id"`
		}
		err = m.call(ctx, http.MethodPost, "/api/2.0/mlflow/experiments/create", map[string]any{"name": name}, &created)
		if errors.As(err, &me) && me.Code == "RESOURCE_ALREADY_EXISTS" {
			return m.experimentID(ctx, name) // another writer won the race
		}
		got.Experiment.ID = created.ID
	}
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.experiments[name] = got.Experiment.ID
	m.mu.Unlock()
	return got.Experiment.ID, nil
}

type kv struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type metric struct {
	Key       string  `json:"key"`
	Value     float64 `json:"value"`
	Timestamp int64   `json:"timestamp"`
	Step      int64   `json:"step"`
}

var badKey = regexp.MustCompile(`[^A-Za-z0-9_\-. /]`)

// ExportRun creates (or, for a resumed run, finds) the MLflow run tagged with
// our run ID, logs its metrics, params and gates, and closes it.
func (m *mlflowSink) ExportRun(ctx context.Context, run *evalsiv1alpha1.Run) error {
	if !terminal(run.GetStatus()) {
		return nil
	}
	name := m.cfg.Experiment
	if name == "" {
		name = run.GetProject()
	}
	if name == "" {
		name = "evalsi"
	}
	expID, err := m.experimentID(ctx, name)
	if err != nil {
		return err
	}
	runID, err := m.findRun(ctx, expID, run.GetId())
	if err != nil {
		return err
	}
	start := run.GetStartedAt().AsTime().UnixMilli()
	if runID == "" {
		var created struct {
			Run struct {
				Info struct {
					ID string `json:"run_id"`
				} `json:"info"`
			} `json:"run"`
		}
		runName := run.GetName()
		if runName == "" {
			runName = run.GetId()
		}
		body := map[string]any{
			"experiment_id": expID, "run_name": runName, "start_time": start,
			"tags": []kv{{"evalsi.run_id", run.GetId()}, {"mlflow.source.name", "evalsi"}},
		}
		if err := m.call(ctx, http.MethodPost, "/api/2.0/mlflow/runs/create", body, &created); err != nil {
			return err
		}
		runID = created.Run.Info.ID
	}

	end := run.GetFinishedAt().AsTime().UnixMilli()
	var metrics []metric
	for _, s := range run.GetSummaries() {
		key := badKey.ReplaceAllString(s.GetMetric(), "_")
		metrics = append(metrics, metric{key + ".n", float64(s.GetN()), end, 0})
		if s.Mean == nil {
			continue
		}
		metrics = append(metrics, metric{key, s.GetMean(), end, 0})
		if ci := s.GetCi(); ci != nil {
			metrics = append(metrics, metric{key + ".ci_low", ci.GetLow(), end, 0}, metric{key + ".ci_high", ci.GetHigh(), end, 0})
		}
	}
	spec := run.GetSpec()
	refs := make([]string, 0, len(spec.GetEvaluators()))
	for _, e := range spec.GetEvaluators() {
		refs = append(refs, e.GetRef())
	}
	params := []kv{
		{"evaluators", truncate(strings.Join(refs, ","), 6000)},
		{"dataset_sha256", run.GetDatasetSha256()},
		{"records", strconv.FormatInt(run.GetRecords(), 10)},
		{"trials", strconv.Itoa(int(max(spec.GetTrials(), 1)))},
	}
	if t := spec.GetTarget(); t != nil {
		params = append(params, kv{"target.connector", t.GetConnector()}, kv{"target.model", t.GetModel()})
	}
	tags := []kv{{"evalsi.status", strings.TrimPrefix(run.GetStatus().String(), "RUN_STATUS_")}}
	if run.GetProject() != "" {
		tags = append(tags, kv{"evalsi.project", run.GetProject()})
	}
	if run.GetError() != "" {
		tags = append(tags, kv{"evalsi.error", truncate(run.GetError(), 5000)})
	}
	for _, g := range run.GetGates() {
		tags = append(tags, kv{"evalsi.gate." + badKey.ReplaceAllString(g.GetGate().GetMetric(), "_"), strconv.FormatBool(g.GetPassed())})
	}
	// Params are immutable in MLflow; a resumed run logs the same values again, which is allowed.
	for i := 0; i < len(metrics) || i == 0; i += 1000 {
		batch := map[string]any{"run_id": runID, "metrics": metrics[i:min(i+1000, len(metrics))]}
		if i == 0 {
			batch["params"], batch["tags"] = params, tags
		}
		if err := m.call(ctx, http.MethodPost, "/api/2.0/mlflow/runs/log-batch", batch, nil); err != nil {
			return err
		}
	}
	status := "FINISHED"
	switch run.GetStatus() {
	case evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED, evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR:
		status = "FAILED"
	case evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED:
		status = "KILLED"
	}
	return m.call(ctx, http.MethodPost, "/api/2.0/mlflow/runs/update",
		map[string]any{"run_id": runID, "status": status, "end_time": end}, nil)
}

func (m *mlflowSink) findRun(ctx context.Context, expID, evalsiID string) (string, error) {
	var found struct {
		Runs []struct {
			Info struct {
				ID string `json:"run_id"`
			} `json:"info"`
		} `json:"runs"`
	}
	body := map[string]any{
		"experiment_ids": []string{expID},
		"filter":         fmt.Sprintf("tags.`evalsi.run_id` = '%s'", strings.ReplaceAll(evalsiID, "'", "")),
		"max_results":    1,
	}
	if err := m.call(ctx, http.MethodPost, "/api/2.0/mlflow/runs/search", body, &found); err != nil {
		return "", err
	}
	if len(found.Runs) == 0 {
		return "", nil
	}
	return found.Runs[0].Info.ID, nil
}

// ExportTrace logs each score as a feedback assessment on the MLflow trace
// with the same ID ("tr-" + the OpenTelemetry trace ID).
func (m *mlflowSink) ExportTrace(ctx context.Context, t *Trace) error {
	if !m.cfg.TraceFeedback || t.TraceID == "" {
		return nil
	}
	traceID := "tr-" + t.TraceID
	for _, r := range t.Results {
		if r.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
			continue
		}
		for _, s := range r.GetScores() {
			source := "CODE"
			if _, judged := s.GetMetadata()["judge_model"]; judged {
				source = "LLM_JUDGE"
			}
			assessment := map[string]any{
				"assessment_name": metricName(r, s),
				"trace_id":        traceID,
				"source":          map[string]any{"source_type": source, "source_id": r.GetEvaluatorRef()},
				"feedback":        map[string]any{"value": feedbackValue(s)},
				"metadata":        map[string]string{"evalsi.policy": t.Policy},
			}
			if s.GetExplanation() != "" {
				assessment["rationale"] = s.GetExplanation()
			}
			err := m.call(ctx, http.MethodPost, "/api/3.0/mlflow/traces/"+url.PathEscape(traceID)+"/assessments",
				map[string]any{"assessment": assessment}, nil)
			var me *mlflowError
			if errors.As(err, &me) && me.Status == http.StatusNotFound {
				return permanent{fmt.Errorf("trace %s is not in MLflow: %w", traceID, err)}
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func feedbackValue(s *evalsiv1alpha1.Score) any {
	switch v := s.GetValue().(type) {
	case *evalsiv1alpha1.Score_Number:
		return v.Number
	case *evalsiv1alpha1.Score_Passed:
		return v.Passed
	case *evalsiv1alpha1.Score_Label:
		return v.Label
	case *evalsiv1alpha1.Score_Structured:
		return v.Structured.AsInterface()
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
