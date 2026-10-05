package sinks

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/version"
)

// OTelConfig exports scores as OpenTelemetry events over OTLP/HTTP. Each
// online score becomes a gen_ai.evaluation.result event carrying the
// evaluated trace's IDs, so a tracing backend shows it next to the trace.
type OTelConfig struct {
	// OTLP/HTTP base URL, for example http://collector:4318; events go to /v1/logs.
	Endpoint string `json:"endpoint"`
	// Header name to environment variable holding its value (for API keys).
	HeadersEnv map[string]string `json:"headers_env,omitempty"`
	// service.name of the exported events; default "evalsi".
	ServiceName string `json:"service_name,omitempty"`
}

func (c *OTelConfig) validate() error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("otel.endpoint must be an http(s) URL, got %q", c.Endpoint)
	}
	return nil
}

type otelSink struct {
	cfg    OTelConfig
	url    string
	client *http.Client
}

func newOTel(cfg OTelConfig, client *http.Client) *otelSink {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "evalsi"
	}
	return &otelSink{cfg: cfg, url: strings.TrimRight(cfg.Endpoint, "/") + "/v1/logs", client: client}
}

func (o *otelSink) Name() string { return "otel" }

const (
	eventResult = "gen_ai.evaluation.result"
	eventRun    = "evalsi.run.metric"
)

func str(k, v string) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_StringValue{StringValue: v}}}
}

func num(k string, v float64) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_DoubleValue{DoubleValue: v}}}
}

func integer(k string, v int64) *common.KeyValue {
	return &common.KeyValue{Key: k, Value: &common.AnyValue{Value: &common.AnyValue_IntValue{IntValue: v}}}
}

// ExportTrace sends one event per score, linked to the evaluated trace.
func (o *otelSink) ExportTrace(ctx context.Context, t *Trace) error {
	traceID, _ := hex.DecodeString(t.TraceID)
	spanID, _ := hex.DecodeString(t.RootSpanID)
	if len(traceID) != 16 {
		traceID = nil
	}
	if len(spanID) != 8 {
		spanID = nil
	}
	ts := uint64(t.Time.UnixNano())
	var records []*logs.LogRecord
	for _, r := range t.Results {
		if r.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
			continue
		}
		for _, s := range r.GetScores() {
			attrs := []*common.KeyValue{
				str("gen_ai.evaluation.name", metricName(r, s)),
				str("evalsi.evaluator.ref", r.GetEvaluatorRef()),
				str("evalsi.policy", t.Policy),
			}
			if v, ok := scoreValue(s); ok {
				attrs = append(attrs, num("gen_ai.evaluation.score.value", v))
			}
			switch v := s.GetValue().(type) {
			case *evalsiv1alpha1.Score_Passed:
				label := "fail"
				if v.Passed {
					label = "pass"
				}
				attrs = append(attrs, str("gen_ai.evaluation.score.label", label))
			case *evalsiv1alpha1.Score_Label:
				attrs = append(attrs, str("gen_ai.evaluation.score.label", v.Label))
			}
			if s.GetExplanation() != "" {
				attrs = append(attrs, str("gen_ai.evaluation.explanation", s.GetExplanation()))
			}
			if t.Service != "" {
				attrs = append(attrs, str("evalsi.trace.service", t.Service))
			}
			records = append(records, &logs.LogRecord{
				TimeUnixNano: ts, ObservedTimeUnixNano: ts, EventName: eventResult,
				SeverityNumber: logs.SeverityNumber_SEVERITY_NUMBER_INFO,
				TraceId:        traceID, SpanId: spanID, Attributes: attrs,
			})
		}
	}
	return o.send(ctx, records)
}

// ExportRun sends one event per metric summary of a finished run.
func (o *otelSink) ExportRun(ctx context.Context, run *evalsiv1alpha1.Run) error {
	if !terminal(run.GetStatus()) {
		return nil
	}
	ts := uint64(run.GetFinishedAt().AsTime().UnixNano())
	status := strings.TrimPrefix(run.GetStatus().String(), "RUN_STATUS_")
	var records []*logs.LogRecord
	for _, s := range run.GetSummaries() {
		attrs := []*common.KeyValue{
			str("evalsi.run.id", run.GetId()),
			str("evalsi.run.name", run.GetName()),
			str("evalsi.run.status", status),
			str("gen_ai.evaluation.name", s.GetMetric()),
			integer("evalsi.metric.n", s.GetN()),
			integer("evalsi.metric.skipped", s.GetSkipped()),
			integer("evalsi.metric.errors", s.GetErrors()),
		}
		if s.Mean != nil {
			attrs = append(attrs, num("gen_ai.evaluation.score.value", s.GetMean()))
		}
		if ci := s.GetCi(); ci != nil {
			attrs = append(attrs, num("evalsi.metric.ci_low", ci.GetLow()), num("evalsi.metric.ci_high", ci.GetHigh()))
		}
		if run.GetProject() != "" {
			attrs = append(attrs, str("evalsi.project", run.GetProject()))
		}
		records = append(records, &logs.LogRecord{
			TimeUnixNano: ts, ObservedTimeUnixNano: ts, EventName: eventRun,
			SeverityNumber: logs.SeverityNumber_SEVERITY_NUMBER_INFO, Attributes: attrs,
		})
	}
	return o.send(ctx, records)
}

func (o *otelSink) send(ctx context.Context, records []*logs.LogRecord) error {
	if len(records) == 0 {
		return nil
	}
	body, err := proto.Marshal(&collogs.ExportLogsServiceRequest{ResourceLogs: []*logs.ResourceLogs{{
		Resource: &resource.Resource{Attributes: []*common.KeyValue{str("service.name", o.cfg.ServiceName)}},
		ScopeLogs: []*logs.ScopeLogs{{
			Scope:      &common.InstrumentationScope{Name: "evals.si", Version: version.Version},
			LogRecords: records,
		}},
	}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	for h, env := range o.cfg.HeadersEnv {
		req.Header.Set(h, os.Getenv(env))
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		err := fmt.Errorf("otlp: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		// OTLP/HTTP: 429, 502, 503 and 504 are retryable; other errors are not.
		switch resp.StatusCode {
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return err
		}
		return permanent{err}
	}
	return nil
}
