// Package sinks exports evaluation results to the systems teams already use.
// Finished runs and online trace scores fan out to every configured sink in
// the background. A slow or failing sink is retried, then logged and
// counted. It never blocks or fails a run or a trace.
package sinks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// Config is one entry of the sinks list in evalsi.yaml; exactly one field is set.
type Config struct {
	MLflow *MLflowConfig `json:"mlflow,omitempty"`
	OTel   *OTelConfig   `json:"otel,omitempty"`
}

// Validate checks that exactly one sink is described and that it is usable.
func (c Config) Validate() error {
	switch {
	case c.MLflow != nil && c.OTel != nil:
		return errors.New("each sinks entry configures one sink")
	case c.MLflow != nil:
		return c.MLflow.validate()
	case c.OTel != nil:
		return c.OTel.validate()
	}
	return errors.New("empty sinks entry; use mlflow or otel")
}

// Trace is one trace's results from an online policy.
type Trace struct {
	// Hex OpenTelemetry IDs of the evaluated trace and its root span.
	TraceID    string
	RootSpanID string
	Service    string
	Policy     string
	Results    []*evalsiv1alpha1.EvaluationResult
	Time       time.Time
}

// Sink is one destination.
type Sink interface {
	Name() string
	ExportRun(ctx context.Context, run *evalsiv1alpha1.Run) error
	ExportTrace(ctx context.Context, t *Trace) error
}

// permanent marks errors that retrying cannot fix (bad config, unknown trace).
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// Dispatcher queues exports and runs them on background workers.
type Dispatcher struct {
	sinks []Sink
	queue chan job
	log   *slog.Logger
	wg    sync.WaitGroup
	once  sync.Once
	// Retry backoff; shortened in tests.
	backoff time.Duration

	Exported atomic.Int64
	Failed   atomic.Int64
	Dropped  atomic.Int64
}

// AuditExporter is a sink that also exports audit events.
type AuditExporter interface {
	ExportAudit(ctx context.Context, ev *evalsiv1alpha1.AuditEvent) error
	exportsAudit() bool
}

type job struct {
	sink  Sink
	run   *evalsiv1alpha1.Run
	trace *Trace
	audit *evalsiv1alpha1.AuditEvent
}

// New builds the configured sinks. With none configured the dispatcher is a no-op.
func New(cfgs []Config, client *http.Client, log *slog.Logger) (*Dispatcher, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	d := &Dispatcher{queue: make(chan job, 1024), log: log, backoff: time.Second}
	for i, c := range cfgs {
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("sinks[%d]: %w", i, err)
		}
		switch {
		case c.MLflow != nil:
			d.sinks = append(d.sinks, newMLflow(*c.MLflow, client))
		case c.OTel != nil:
			d.sinks = append(d.sinks, newOTel(*c.OTel, client))
		}
	}
	return d, nil
}

// Start runs the export workers until Close.
func (d *Dispatcher) Start(workers int) {
	for range max(workers, 1) {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for j := range d.queue {
				d.export(j)
			}
		}()
	}
}

// Close stops accepting work and waits for queued exports, up to ctx.
func (d *Dispatcher) Close(ctx context.Context) {
	d.once.Do(func() { close(d.queue) })
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		d.log.Warn("sink exports still pending at shutdown")
	}
}

// Run queues a finished run for every sink.
func (d *Dispatcher) Run(run *evalsiv1alpha1.Run) {
	if d == nil {
		return
	}
	run = proto.Clone(run).(*evalsiv1alpha1.Run)
	for _, s := range d.sinks {
		d.enqueue(job{sink: s, run: run})
	}
}

// Trace queues one trace's results for every sink.
func (d *Dispatcher) Trace(t *Trace) {
	if d == nil {
		return
	}
	for _, s := range d.sinks {
		d.enqueue(job{sink: s, trace: t})
	}
}

// Audit queues an audit event for every sink that exports them.
func (d *Dispatcher) Audit(ev *evalsiv1alpha1.AuditEvent) {
	if d == nil {
		return
	}
	for _, s := range d.sinks {
		if a, ok := s.(AuditExporter); ok && a.exportsAudit() {
			d.enqueue(job{sink: s, audit: proto.Clone(ev).(*evalsiv1alpha1.AuditEvent)})
		}
	}
}

func (d *Dispatcher) enqueue(j job) {
	defer func() {
		// Close already happened: the export is dropped like a full queue.
		if recover() != nil {
			d.Dropped.Add(1)
		}
	}()
	select {
	case d.queue <- j:
	default:
		d.Dropped.Add(1)
		d.log.Warn("sink queue full; export dropped", "sink", j.sink.Name())
	}
}

func (d *Dispatcher) export(j job) {
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(d.backoff << (attempt - 1))
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		switch {
		case j.audit != nil:
			err = j.sink.(AuditExporter).ExportAudit(ctx, j.audit)
		case j.run != nil:
			err = j.sink.ExportRun(ctx, j.run)
		default:
			err = j.sink.ExportTrace(ctx, j.trace)
		}
		cancel()
		var p permanent
		if err == nil || errors.As(err, &p) {
			break
		}
	}
	if err != nil {
		d.Failed.Add(1)
		d.log.Warn("sink export failed", "sink", j.sink.Name(), "err", err)
		return
	}
	d.Exported.Add(1)
}

// scoreValue is a score as a number when it has one (passed counts as 1 or 0).
func scoreValue(s *evalsiv1alpha1.Score) (float64, bool) {
	switch v := s.GetValue().(type) {
	case *evalsiv1alpha1.Score_Number:
		return v.Number, true
	case *evalsiv1alpha1.Score_Passed:
		if v.Passed {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// metricName mirrors the summary naming: the evaluator's alias, plus the
// score name for multi-metric evaluators.
func metricName(r *evalsiv1alpha1.EvaluationResult, s *evalsiv1alpha1.Score) string {
	short, _, _ := strings.Cut(r.GetEvaluatorRef(), "@")
	short = short[strings.LastIndex(short, "/")+1:]
	if s.GetName() == "" || s.GetName() == short || s.GetName() == r.GetEvaluator() {
		return r.GetEvaluator()
	}
	return r.GetEvaluator() + "." + s.GetName()
}

func terminal(st evalsiv1alpha1.RunStatus) bool {
	switch st {
	case evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED,
		evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED:
		return true
	}
	return false
}
