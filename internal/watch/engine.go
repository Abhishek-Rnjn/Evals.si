package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/datasets"
	"github.com/abhishek-rnjn/evals.si/internal/evaluation"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Options configures the engine.
type Options struct {
	// Root for promoted datasets (<root>/promoted/<project>/<name>.jsonl). Empty disables promotion.
	DatasetsDir string
	// Traces evaluated together per worker call.
	BatchSize int
	// How long the dispatcher waits to fill a batch.
	FlushInterval time.Duration
	// Traces waiting for evaluation; more are dropped and counted.
	QueueSize int
	Logger    *slog.Logger
	// Client for alert webhooks.
	HTTPClient *http.Client
	// Called with each evaluated trace's results (for sinks); must not block.
	OnResults func(policy string, record *evalsiv1alpha1.Record, info ingest.TraceInfo, results []*evalsiv1alpha1.EvaluationResult)
}

type item struct {
	record *evalsiv1alpha1.Record
	info   ingest.TraceInfo
}

type point struct {
	at    time.Time
	value float64
}

type policyState struct {
	c                                                    *compiled
	seen, matched, sampled, evaluated, promoted, errored int64
	windows                                              map[string][]point
	alerts                                               []*evalsiv1alpha1.AlertState
}

// Engine runs online evaluation policies over assembled traces.
type Engine struct {
	store *store.Store
	eval  *evaluation.Service
	opts  Options
	log   *slog.Logger
	queue chan item
	now   func() time.Time

	mu       sync.Mutex
	policies map[string]*policyState

	TracesIngested atomic.Int64
	TracesDropped  atomic.Int64
	StoreErrors    atomic.Int64
}

// New loads stored policies. Policies that no longer compile (for example an
// evaluator was uninstalled) are logged and skipped.
func New(ctx context.Context, st *store.Store, eval *evaluation.Service, opts Options) (*Engine, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 32
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 500 * time.Millisecond
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 10000
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	e := &Engine{
		store: st, eval: eval, opts: opts, log: opts.Logger,
		queue: make(chan item, opts.QueueSize), now: time.Now,
		policies: map[string]*policyState{},
	}
	stored, err := st.Policies(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range stored {
		if p.GetProject() == "" {
			p.Project = DefaultProject // stored before projects were enforced
		}
		c, err := compile(p, eval)
		if err != nil {
			e.log.Error("skipping stored policy", "policy", p.GetName(), "err", err)
			continue
		}
		e.policies[p.GetName()] = newState(c)
	}
	return e, nil
}

func newState(c *compiled) *policyState {
	st := &policyState{c: c, windows: map[string][]point{}}
	for _, a := range c.policy.GetAlerts() {
		st.alerts = append(st.alerts, &evalsiv1alpha1.AlertState{Alert: a})
	}
	return st
}

// Apply validates, stores and activates a policy, replacing one with the same name.
func (e *Engine) Apply(ctx context.Context, p *evalsiv1alpha1.OnlineEvalPolicy) error {
	if p.GetProject() == "" {
		p.Project = DefaultProject
	}
	c, err := compile(p, e.eval)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if p.GetPromote().GetDataset() != "" && e.opts.DatasetsDir == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("promotion needs datasets_dir in the server config"))
	}
	if err := e.store.PutPolicy(ctx, p); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	st := newState(c)
	if old := e.policies[p.GetName()]; old != nil {
		// Keep counters across edits; windows restart because evaluators may have changed.
		st.seen, st.matched, st.sampled, st.evaluated, st.promoted, st.errored = old.seen, old.matched, old.sampled, old.evaluated, old.promoted, old.errored
	}
	e.policies[p.GetName()] = st
	return nil
}

// Ingest stores an assembled trace and queues it for policy evaluation. It is
// the assembler's emit callback, so it must not block.
func (e *Engine) Ingest(t ingest.Trace) {
	record, info := ingest.ToRecord(t)
	if info.Project == "" {
		info.Project = DefaultProject
	}
	e.TracesIngested.Add(1)
	summary := &evalsiv1alpha1.TraceSummary{
		Project: info.Project, Labels: info.Labels,
		TraceId: record.GetId(), Service: info.Service, Name: info.Name,
		Duration: durationpb.New(time.Duration(info.DurationMS * float64(time.Millisecond))),
		Error:    info.Error, Steps: int32(info.Steps),
	}
	if steps := record.GetTrajectory().GetSteps(); len(steps) > 0 {
		summary.StartTime = steps[0].GetStartTime()
	}
	if err := e.store.PutTrace(context.Background(), summary, record); err != nil {
		e.StoreErrors.Add(1)
		e.log.Error("storing trace", "trace", record.GetId(), "err", err)
	}
	select {
	case e.queue <- item{record: record, info: info}:
	default:
		e.TracesDropped.Add(1)
	}
}

// Run evaluates queued traces in micro-batches until ctx is done.
func (e *Engine) Run(ctx context.Context) {
	var batch []item
	timer := time.NewTimer(e.opts.FlushInterval)
	defer timer.Stop()
	flush := func() {
		if len(batch) > 0 {
			e.process(ctx, batch)
			batch = nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-e.queue:
			batch = append(batch, it)
			if len(batch) >= e.opts.BatchSize {
				flush()
			}
		case <-timer.C:
			flush()
			timer.Reset(e.opts.FlushInterval)
		}
	}
}

func (e *Engine) snapshot() []*policyState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*policyState, 0, len(e.policies))
	for _, st := range e.policies {
		if !st.c.policy.GetDisabled() {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].c.policy.GetName() < out[j].c.policy.GetName() })
	return out
}

func numeric(s *evalsiv1alpha1.Score) (float64, bool) {
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

func (e *Engine) process(ctx context.Context, batch []item) {
	for _, st := range e.snapshot() {
		e.processPolicy(ctx, st, batch)
	}
}

func (e *Engine) processPolicy(ctx context.Context, st *policyState, batch []item) {
	c := st.c
	type candidate struct {
		item
		scores  map[string]float64
		results []*evalsiv1alpha1.EvaluationResult
	}
	var sampled []*candidate
	var seen, matched int64
	for _, it := range batch {
		if it.info.Project != c.policy.GetProject() {
			continue // policies only see their own project's traces
		}
		seen++
		vars := activation(it.info, nil)
		if !eval(c.selector, vars) {
			continue
		}
		matched++
		if c.sampled(it.record.GetId(), vars) {
			sampled = append(sampled, &candidate{item: it, scores: map[string]float64{}})
		}
	}
	var errored int64
	for _, stg := range c.stages {
		var todo []*candidate
		for _, cand := range sampled {
			if eval(stg.when, activation(cand.info, cand.scores)) {
				todo = append(todo, cand)
			}
		}
		if len(todo) == 0 {
			continue
		}
		records := make([]*evalsiv1alpha1.Record, len(todo))
		for i, cand := range todo {
			records[i] = cand.record
		}
		results, err := e.eval.RunRecords(ctx, stg.insts, records)
		if err != nil {
			errored += int64(len(todo))
			e.log.Warn("online evaluation failed", "policy", c.policy.GetName(), "err", err)
			break // later stages depend on this one's scores
		}
		// RunRecords orders results by record, then instance.
		for i, r := range results {
			cand := todo[i/len(stg.insts)]
			cand.results = append(cand.results, r)
			if r.GetOutcome() == evalsiv1alpha1.Outcome_OUTCOME_ERROR {
				errored++
			}
			for _, sc := range r.GetScores() {
				if v, ok := numeric(sc); ok {
					cand.scores[metricName(stg.insts, r.GetEvaluator(), sc.GetName())] = v
				}
			}
		}
	}
	now := e.now()
	var evaluated, promoted int64
	for _, cand := range sampled {
		if len(cand.results) == 0 {
			continue
		}
		evaluated++
		if err := e.store.PutTraceResults(ctx, cand.info.Project, cand.record.GetId(), c.policy.GetName(), cand.results); err != nil {
			e.StoreErrors.Add(1)
			e.log.Error("storing trace results", "trace", cand.record.GetId(), "err", err)
		}
		if e.opts.OnResults != nil {
			e.opts.OnResults(c.policy.GetName(), cand.record, cand.info, cand.results)
		}
		if c.promote != nil && eval(c.promote, activation(cand.info, cand.scores)) {
			if err := e.promoteRecord(c.policy.GetProject(), c.policy.GetPromote().GetDataset(), cand.record, cand.scores); err != nil {
				e.log.Error("promoting trace", "trace", cand.record.GetId(), "err", err)
			} else {
				promoted++
			}
		}
	}
	var fired []alertEvent
	e.mu.Lock()
	st.seen += seen
	st.matched += matched
	st.sampled += int64(len(sampled))
	st.evaluated += evaluated
	st.promoted += promoted
	st.errored += errored
	for _, cand := range sampled {
		for metric, v := range cand.scores {
			st.windows[metric] = append(st.windows[metric], point{at: now, value: v})
		}
	}
	e.pruneLocked(st, now)
	fired = e.checkAlertsLocked(st, now)
	e.mu.Unlock()
	for _, ev := range fired {
		e.notify(ev)
	}
}

// metricName mirrors evaluation.MetricKey for an evaluator instance by name.
func metricName(insts []evaluation.Instance, evaluator, scoreName string) string {
	for _, in := range insts {
		if in.Name == evaluator {
			return evaluation.MetricKey(in, scoreName)
		}
	}
	return evaluator
}

func (e *Engine) pruneLocked(st *policyState, now time.Time) {
	cutoff := now.Add(-st.c.window)
	for metric, pts := range st.windows {
		i := sort.Search(len(pts), func(i int) bool { return !pts[i].at.Before(cutoff) })
		st.windows[metric] = pts[i:]
	}
}

func windowStats(pts []point) (int64, *float64) {
	if len(pts) == 0 {
		return 0, nil
	}
	sum := 0.0
	for _, p := range pts {
		sum += p.value
	}
	m := sum / float64(len(pts))
	return int64(len(pts)), &m
}

type alertEvent struct {
	Policy  string    `json:"policy"`
	Metric  string    `json:"metric"`
	Firing  bool      `json:"firing"`
	Mean    float64   `json:"mean"`
	N       int64     `json:"n"`
	Below   *float64  `json:"below,omitempty"`
	Above   *float64  `json:"above,omitempty"`
	At      time.Time `json:"at"`
	webhook string
}

func (e *Engine) checkAlertsLocked(st *policyState, now time.Time) []alertEvent {
	var out []alertEvent
	for _, as := range st.alerts {
		a := as.GetAlert()
		minN := a.GetMinSamples()
		if minN <= 0 {
			minN = 10
		}
		n, mean := windowStats(st.windows[a.GetMetric()])
		if n < minN || mean == nil {
			continue // not enough evidence either way; keep the current state
		}
		firing := (a.Below != nil && *mean < a.GetBelow()) || (a.Above != nil && *mean > a.GetAbove())
		if firing == as.GetFiring() {
			continue
		}
		as.Firing, as.Since = firing, timestamppb.New(now)
		out = append(out, alertEvent{
			Policy: st.c.policy.GetName(), Metric: a.GetMetric(), Firing: firing, Mean: *mean, N: n,
			Below: a.Below, Above: a.Above, At: now, webhook: a.GetWebhook(),
		})
	}
	return out
}

func (e *Engine) notify(ev alertEvent) {
	e.log.Warn("alert", "policy", ev.Policy, "metric", ev.Metric, "firing", ev.Firing, "mean", ev.Mean, "n", ev.N)
	if ev.webhook == "" {
		return
	}
	body, _ := json.Marshal(ev)
	resp, err := e.opts.HTTPClient.Post(ev.webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		e.log.Error("alert webhook failed", "url", ev.webhook, "err", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		e.log.Error("alert webhook failed", "url", ev.webhook, "status", resp.StatusCode)
	}
}

// promoteRecord appends a trace to the project's promoted dataset
// (promoted/<project>/<dataset>.jsonl), so failing production traces become
// regression cases.
func (e *Engine) promoteRecord(project, dataset string, record *evalsiv1alpha1.Record, scores map[string]float64) error {
	row, err := datasets.Row(record, map[string]any{"online_scores": scores})
	if err != nil {
		return err
	}
	_, err = datasets.Append(e.opts.DatasetsDir, project, dataset, [][]byte{row})
	return err
}

// Stats reports a policy's counters, windows and alerts.
func (e *Engine) Stats(name string) (*evalsiv1alpha1.PolicyStats, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.policies[name]
	if !ok {
		return nil, false
	}
	e.pruneLocked(st, e.now())
	out := &evalsiv1alpha1.PolicyStats{
		Policy: name, TracesSeen: st.seen, TracesMatched: st.matched, TracesSampled: st.sampled,
		TracesEvaluated: st.evaluated, TracesPromoted: st.promoted, EvaluationErrors: st.errored,
	}
	metrics := make([]string, 0, len(st.windows))
	for m := range st.windows {
		metrics = append(metrics, m)
	}
	sort.Strings(metrics)
	for _, m := range metrics {
		n, mean := windowStats(st.windows[m])
		out.Metrics = append(out.Metrics, &evalsiv1alpha1.MetricWindow{Metric: m, N: n, Mean: mean})
	}
	for _, a := range st.alerts {
		out.Alerts = append(out.Alerts, proto.Clone(a).(*evalsiv1alpha1.AlertState))
	}
	return out, true
}

// Policies lists active policies, optionally for one project.
func (e *Engine) Policies(project string) []*evalsiv1alpha1.OnlineEvalPolicy {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []*evalsiv1alpha1.OnlineEvalPolicy
	for _, st := range e.policies {
		if project == "" || st.c.policy.GetProject() == project {
			out = append(out, st.c.policy)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

// Policy returns an active policy by name.
func (e *Engine) Policy(name string) (*evalsiv1alpha1.OnlineEvalPolicy, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.policies[name]
	if st == nil {
		return nil, false
	}
	return st.c.policy, true
}

// DefaultProject holds policies and traces that name no project.
const DefaultProject = "default"

// Delete removes a policy.
func (e *Engine) Delete(ctx context.Context, name string) error {
	if err := e.store.DeletePolicy(ctx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("no policy %q", name))
		}
		return err
	}
	e.mu.Lock()
	delete(e.policies, name)
	e.mu.Unlock()
	return nil
}
