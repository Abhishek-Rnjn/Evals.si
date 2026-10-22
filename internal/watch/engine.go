package watch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
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
	"github.com/abhishek-rnjn/evals.si/internal/objstore"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Options configures the engine.
type Options struct {
	// Root for promoted datasets (<root>/promoted/<project>/<name>.jsonl):
	// a directory or s3://bucket/prefix. Empty disables promotion.
	DatasetsDir string
	// The object-storage client, when DatasetsDir is s3://.
	Objects *objstore.Client
	// In a cluster: called after this replica changes policies, so the
	// others reload; and the elected policy engine's statistics, which a
	// replica that is not the leader returns.
	Changed     func()
	RemoteStats func(name string) (*evalsiv1alpha1.PolicyStats, bool, error)
	// Traces evaluated together per worker call.
	BatchSize int
	// How long the dispatcher waits to fill a batch.
	FlushInterval time.Duration
	// In-memory wakeups waiting for evaluation; overflow is recovered from SQL.
	QueueSize int
	Logger    *slog.Logger
	Redactor  *ingest.Redactor
	// Client for alert webhooks.
	HTTPClient *http.Client
	// Called for a short result handoff; respect ctx and do not wait for remote delivery.
	OnResults func(ctx context.Context, policy string, record *evalsiv1alpha1.Record, info ingest.TraceInfo, results []*evalsiv1alpha1.EvaluationResult) error
	// Defers replay until a pulled trace can hand results to its source runner.
	ResultsReady func(ctx context.Context, info ingest.TraceInfo) bool
	// Nonempty in a cluster: every durable write must hold the policy-engine lease.
	Lease store.PolicyLease
	// Called when a policy's alert starts or stops firing, with the body its
	// own alert webhook receives (for the WebhookService's alert events).
	OnAlert func(project, policy string, firing bool, body []byte)
}

type item struct {
	record  *evalsiv1alpha1.Record
	summary *evalsiv1alpha1.TraceSummary
	info    ingest.TraceInfo
	// When set, only these policies score the trace (a trace source's list).
	only    []string
	stream  string
	payload []byte
	pend    store.WatchPending
}

// pendingKey identifies one accepted trace revision awaiting acknowledgement.
type pendingKey struct {
	project, trace, stream, recordHash, snapshotHash string
	revision, scoreRevision                          int64
}

func keyOf(it item) pendingKey {
	p := it.pending()
	return pendingKey{p.Project, p.TraceID, p.Stream, p.RecordHash, p.SnapshotHash, p.Revision, p.ScoreRevision}
}

// handoff is one policy's finished evaluation of one trace, and how far
// delivering it has got.
type handoff struct {
	c                        *compiled // a changed policy evaluates afresh
	scores                   map[string]float64
	results                  []*evalsiv1alpha1.EvaluationResult
	stored, handed, promoted bool
	failures                 int
	retryAt                  time.Time
}

const (
	maxHandoffBackoff = time.Minute
	// Entries normally go with their acknowledgement; this bounds the rest.
	maxHandoffs = 10000
)

func (h *handoff) failed(now time.Time) {
	h.failures++
	h.retryAt = now.Add(min(time.Second<<min(h.failures, 6), maxHandoffBackoff))
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
	// Traces waiting to be stored (Enqueue), and whether the writer is gone.
	writes     chan ingest.Trace
	writing    sync.RWMutex
	writerGone bool
	now        func() time.Time

	mu          sync.Mutex
	policies    map[string]*policyState
	unavailable map[string]string
	processing  sync.Mutex
	leaderMu    sync.Mutex
	leaderCtx   context.Context
	stopLeader  context.CancelFunc
	leaderLease store.PolicyLease
	// Set on a replica that is not the elected policy engine.
	follower atomic.Bool
	// Used only by Run's recovery loop; failed work must not starve later rows.
	recoveryCursor store.WatchPending
	// Evaluated traces whose results are not yet stored and handed off, per
	// policy: a retry must not re-run paid evaluators or re-count statistics.
	// Guarded by processing; an entry goes when its trace is acknowledged.
	handoffs map[pendingKey]map[string]*handoff

	TracesIngested atomic.Int64
	TracesDropped  atomic.Int64
	StoreErrors    atomic.Int64
}

// New loads stored policies. Policies that no longer compile are logged and
// retain accepted work until restored, disabled or intentionally deleted.
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
	if opts.Redactor == nil {
		var err error
		opts.Redactor, err = ingest.NewRedactor(ingest.RedactionConfig{})
		if err != nil {
			return nil, err
		}
	}
	e := &Engine{
		store: st, eval: eval, opts: opts, log: opts.Logger,
		queue: make(chan item, opts.QueueSize), writes: make(chan ingest.Trace, opts.QueueSize), now: time.Now,
		policies: map[string]*policyState{}, unavailable: map[string]string{},
		handoffs: map[pendingKey]map[string]*handoff{},
	}
	e.leaderCtx, e.stopLeader = context.WithCancel(context.Background())
	e.leaderLease = opts.Lease
	stored, err := st.Policies(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range stored {
		if p.GetProject() == "" {
			p.Project = DefaultProject // stored before projects were enforced
		}
		c, err := compile(ctx, p, eval)
		if err != nil {
			e.log.Error("skipping stored policy", "policy", p.GetName(), "err", err)
			if !p.GetDisabled() {
				e.unavailable[p.GetName()] = p.GetProject()
			}
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

// SetLeader marks this replica as the elected policy engine (in a cluster;
// a single replica always is). Only the leader receives traces, so only its
// statistics count.
func (e *Engine) SetLeader(leader bool) {
	e.leaderMu.Lock()
	defer e.leaderMu.Unlock()
	if !leader {
		e.follower.Store(true)
		e.stopLeader()
	} else if e.follower.Load() {
		e.leaderCtx, e.stopLeader = context.WithCancel(context.Background())
		e.follower.Store(false)
	}
}

// SetPolicyLease starts a new processing term even when the process owner
// stayed the same across a missed A -> B -> A lease cycle.
func (e *Engine) SetPolicyLease(lease store.PolicyLease) {
	e.leaderMu.Lock()
	defer e.leaderMu.Unlock()
	if e.follower.Load() || e.leaderLease != lease {
		e.stopLeader()
		e.leaderCtx, e.stopLeader = context.WithCancel(context.Background())
		e.leaderLease = lease
		e.follower.Store(false)
	}
}

func (e *Engine) processingContext(parent context.Context) (context.Context, store.PolicyLease, context.CancelFunc) {
	e.leaderMu.Lock()
	defer e.leaderMu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(e.leaderCtx, cancel)
	if e.follower.Load() || e.leaderCtx.Err() != nil {
		cancel()
	}
	return ctx, e.leaderLease, func() { stop(); cancel() }
}

// Reload re-reads policies from the store (another replica changed them).
// Unchanged policies keep their state; changed ones keep their counters.
func (e *Engine) Reload(ctx context.Context) error { return e.reload(ctx, false) }

// Recheck reloads every policy and compiles it again, unchanged ones too,
// so changed credential grants or judge scopes apply: a policy that now
// names what its project may not use is dropped (and logged) until the
// grant returns or the policy changes.
func (e *Engine) Recheck(ctx context.Context) error { return e.reload(ctx, true) }

func (e *Engine) reload(ctx context.Context, force bool) error {
	stored, err := e.store.Policies(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	next := map[string]*policyState{}
	unavailable := map[string]string{}
	for _, p := range stored {
		if p.GetProject() == "" {
			p.Project = DefaultProject
		}
		old := e.policies[p.GetName()]
		if !force && old != nil && proto.Equal(old.c.policy, p) {
			next[p.GetName()] = old
			continue
		}
		c, err := compile(ctx, p, e.eval)
		if err != nil {
			e.log.Error("skipping stored policy", "policy", p.GetName(), "err", err)
			if !p.GetDisabled() {
				unavailable[p.GetName()] = p.GetProject()
			}
			continue
		}
		st := newState(c)
		if old != nil {
			st.seen, st.matched, st.sampled, st.evaluated, st.promoted, st.errored = old.seen, old.matched, old.sampled, old.evaluated, old.promoted, old.errored
		}
		next[p.GetName()] = st
	}
	e.policies = next
	e.unavailable = unavailable
	return nil
}

// Validate checks a policy as Apply would, without storing it.
func (e *Engine) Validate(ctx context.Context, p *evalsiv1alpha1.OnlineEvalPolicy) error {
	_, err := e.validate(ctx, p)
	return err
}

func (e *Engine) validate(ctx context.Context, p *evalsiv1alpha1.OnlineEvalPolicy) (*compiled, error) {
	if p.GetProject() == "" {
		p.Project = DefaultProject
	}
	c, err := compile(ctx, p, e.eval)
	if connect.CodeOf(err) == connect.CodePermissionDenied {
		return nil, err
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if p.GetPromote().GetDataset() != "" && e.opts.DatasetsDir == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("promotion needs datasets_dir in the server config"))
	}
	return c, nil
}

// Apply validates, stores and activates a policy, replacing one with the same name.
func (e *Engine) Apply(ctx context.Context, p *evalsiv1alpha1.OnlineEvalPolicy) error {
	c, err := e.validate(ctx, p)
	if err != nil {
		return err
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
	delete(e.unavailable, p.GetName())
	if e.opts.Changed != nil {
		e.opts.Changed()
	}
	return nil
}

// Ingest durably queues an assembled trace for storage and policy evaluation.
func (e *Engine) Ingest(t ingest.Trace) { e.IngestBatch([]ingest.Trace{t}) }

// IngestBatch persists pending work, then wakes the dispatcher, which stores
// each trace before its scores. A full in-memory queue defers work to recovery.
func (e *Engine) IngestBatch(traces []ingest.Trace) {
	_ = e.ingest(context.Background(), traces, nil, false)
}

// IngestBatchContext is IngestBatch for callers that must know the traces are
// safe: it returns the store error (queuing nothing) and, instead of dropping
// when the queue is full, waits for room until ctx is done. A trace source
// advances its watermark only after this returns nil. Only the named policies
// score the traces; none named means every policy that selects them.
func (e *Engine) IngestBatchContext(ctx context.Context, traces []ingest.Trace, policies []string) error {
	return e.ingest(ctx, traces, policies, true)
}

func (e *Engine) ingest(ctx context.Context, traces []ingest.Trace, only []string, durable bool) error {
	items := make([]item, 0, len(traces))
	for _, t := range traces {
		t = e.opts.Redactor.Trace(t)
		record, info := ingest.ToRecord(t)
		if info.Project == "" {
			info.Project = DefaultProject
		}
		items = append(items, item{record: record, info: info, only: slices.Clone(only), stream: workStream(info, only)})
	}
	pending := make([]store.WatchPending, 0, len(items))
	for i := range items {
		record, err := (proto.MarshalOptions{Deterministic: true}).Marshal(items[i].record)
		if err != nil {
			return err
		}
		var acceptance [16]byte
		if _, err := rand.Read(acceptance[:]); err != nil {
			return err
		}
		payload, err := encodePending(pendingPayload{Acceptance: acceptance, Record: record, Info: items[i].info, Only: items[i].only, Stream: items[i].stream})
		if err != nil {
			return err
		}
		items[i].payload = payload
		pending = append(pending, (&items[i]).initPending())
	}
	if err := e.store.PutWatchPending(ctx, pending); err != nil {
		e.StoreErrors.Add(int64(len(items)))
		e.log.Error("persisting online evaluation queue", "err", err)
		return err
	}
	e.TracesIngested.Add(int64(len(traces)))
	for _, it := range items {
		if !durable {
			select {
			case e.queue <- it:
			default:
				e.TracesDropped.Add(1)
			}
			continue
		}
		select {
		case e.queue <- it:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Enqueue hands a trace to the writer, which stores traces in batches; it
// is the assembler's emit callback, so it does not block. When the writer
// is behind by a full buffer, the trace is stored right away instead.
func (e *Engine) Enqueue(t ingest.Trace) {
	e.writing.RLock()
	if !e.writerGone {
		select {
		case e.writes <- t:
			e.writing.RUnlock()
			return
		default:
		}
	}
	e.writing.RUnlock()
	e.Ingest(t)
}

func (e *Engine) hasNoPolicies() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.policies) == 0 && len(e.unavailable) == 0
}

func (e *Engine) ingestDirect(ctx context.Context, traces []ingest.Trace) {
	pctx, lease, cancel := e.processingContext(ctx)
	defer cancel()
	if pctx.Err() != nil || !e.hasNoPolicies() {
		e.IngestBatch(traces)
		return
	}
	writes := make([]store.TraceWrite, len(traces))
	workers := min(len(traces), 8)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(traces); i += workers {
				t := e.opts.Redactor.Trace(traces[i])
				record, info := ingest.ToRecord(t)
				if info.Project == "" {
					info.Project = DefaultProject
				}
				writes[i] = store.TraceWrite{Summary: Summary(record, info), Record: record}
			}
		}(w)
	}
	wg.Wait()
	if err := e.store.PutUnwatchedTraces(pctx, lease, writes); err != nil {
		if !errors.Is(err, store.ErrWatchStale) {
			e.StoreErrors.Add(int64(len(writes)))
			e.log.Error("storing traces", "err", err)
		}
		return
	}
	// A policy applied while these were stored would never see them: queue
	// them as well. Storing an unchanged trace again keeps its head.
	if !e.hasNoPolicies() {
		e.IngestBatch(traces)
		return
	}
	e.TracesIngested.Add(int64(len(writes)))
}

// write stores traces handed to Enqueue, a batch per transaction, until ctx
// is done, then drains what is left.
func (e *Engine) write(ctx context.Context) {
	const maxBatch = 2000
	batch := make([]ingest.Trace, 0, maxBatch)
	storeBatch := func(b []ingest.Trace) {
		if !e.follower.Load() && e.hasNoPolicies() {
			e.ingestDirect(ctx, b)
			return
		}
		e.IngestBatch(b)
	}
	for {
		select {
		case t := <-e.writes:
			batch = append(batch[:0], t)
		drain:
			for len(batch) < maxBatch {
				select {
				case t := <-e.writes:
					batch = append(batch, t)
				default:
					break drain
				}
			}
			storeBatch(batch)
		case <-ctx.Done():
			// From here on Enqueue stores traces itself (the assembler's
			// last flush comes after this).
			e.writing.Lock()
			e.writerGone = true
			e.writing.Unlock()
			for {
				select {
				case t := <-e.writes:
					e.Ingest(t)
				default:
					return
				}
			}
		}
	}
}

// Run evaluates queued traces in micro-batches until ctx is done.
func (e *Engine) Run(ctx context.Context) {
	writer := make(chan struct{})
	go func() { defer close(writer); e.write(ctx) }()
	defer func() { <-writer }()
	var batch []item
	timer := time.NewTimer(e.opts.FlushInterval)
	defer timer.Stop()
	flush := func() {
		if len(batch) > 0 {
			e.process(ctx, batch)
			batch = nil
		}
	}
	e.recoverPending(ctx)
	recovery := time.NewTicker(time.Second)
	defer recovery.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-recovery.C:
			flush()
			e.recoverPending(ctx)
		case it := <-e.queue:
			batch = append(batch, it)
			// Take what is already queued, up to one worker call.
		drainQueue:
			for len(batch) < e.opts.BatchSize {
				select {
				case next := <-e.queue:
					batch = append(batch, next)
				default:
					break drainQueue
				}
			}
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
	if len(batch) == 0 {
		return
	}
	e.processing.Lock()
	defer e.processing.Unlock()
	ctx, lease, cancel := e.processingContext(ctx)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	writes := make([]store.WatchTraceWrite, len(batch))
	for i, it := range batch {
		sum := it.summary
		if sum == nil {
			sum = Summary(it.record, it.info)
		}
		writes[i] = store.WatchTraceWrite{Pending: it.pending(), Trace: store.TraceWrite{Summary: sum, Record: it.record}}
	}
	// Visibility does not depend on policy compilation or source readiness.
	// The store checks the lease and exact pending revision under its write lock.
	stored, err := e.store.PutWatchTraces(ctx, lease, writes)
	if err != nil {
		if !errors.Is(err, store.ErrWatchStale) && ctx.Err() == nil {
			e.StoreErrors.Add(int64(len(writes)))
			e.log.Error("storing accepted online traces", "err", err)
		}
		return
	}
	var current []item
	for i, it := range batch {
		if stored[i] && e.policiesReady(it) && (e.opts.ResultsReady == nil || e.opts.ResultsReady(ctx, it.info)) {
			current = append(current, it)
		}
	}
	if len(current) == 0 || ctx.Err() != nil {
		return
	}
	if len(e.handoffs) > maxHandoffs {
		clear(e.handoffs)
	}
	complete := true
	for _, st := range e.snapshot() {
		if !e.processPolicy(ctx, lease, st, current) {
			complete = false
		}
	}
	if complete && ctx.Err() == nil {
		acks := make([]store.WatchPending, len(current))
		for i, it := range current {
			acks[i] = it.pending()
		}
		if err := e.store.AckWatchPendingBatch(ctx, lease, acks); err != nil && ctx.Err() == nil {
			e.log.Error("acknowledging online trace", "err", err)
		} else if err == nil {
			for _, it := range current {
				delete(e.handoffs, keyOf(it))
			}
		}
	}
}

func (e *Engine) processPolicy(ctx context.Context, lease store.PolicyLease, st *policyState, batch []item) bool {
	complete := true
	c := st.c
	type candidate struct {
		item
		scores  map[string]float64
		results []*evalsiv1alpha1.EvaluationResult
		// Set for a retry: evaluated and counted already.
		h     *handoff
		fresh bool
	}
	var sampled []*candidate
	var seen, matched int64
	now := e.now()
	for _, it := range batch {
		if it.info.Project != c.policy.GetProject() {
			continue // policies only see their own project's traces
		}
		if len(it.only) > 0 && !slices.Contains(it.only, c.policy.GetName()) {
			continue
		}
		if h := e.handoffs[keyOf(it)][c.policy.GetName()]; h != nil && h.c == c {
			if h.stored && h.handed && h.promoted {
				continue // delivered; the batch waits on another policy
			}
			if now.Before(h.retryAt) {
				complete = false
				continue
			}
			sampled = append(sampled, &candidate{item: it, scores: h.scores, results: h.results, h: h})
			continue
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
	stagesFailed := false
	for _, stg := range c.stages {
		var todo []*candidate
		for _, cand := range sampled {
			if cand.h == nil && eval(stg.when, activation(cand.info, cand.scores)) {
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
			complete = false
			stagesFailed = true
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
	if ctx.Err() != nil {
		return false
	}
	var evaluated, promoted, fresh int64
	for _, cand := range sampled {
		h := cand.h
		if h == nil {
			fresh++
			if len(cand.results) == 0 {
				continue
			}
			evaluated++
			h = &handoff{c: c, scores: cand.scores, results: cand.results, promoted: c.promote == nil}
			cand.h = h
			cand.fresh = true
			if !stagesFailed {
				k := keyOf(cand.item)
				if e.handoffs[k] == nil {
					e.handoffs[k] = map[string]*handoff{}
				}
				e.handoffs[k][c.policy.GetName()] = h
			}
		}
		if !h.stored {
			if err := e.store.PutWatchResults(ctx, lease, cand.pending(), c.policy.GetName(), cand.results); err != nil {
				if errors.Is(err, store.ErrWatchStale) || ctx.Err() != nil {
					return false
				}
				e.StoreErrors.Add(1)
				e.log.Error("storing trace results", "trace", cand.record.GetId(), "err", err)
				h.failed(now)
				complete = false
				continue
			}
			h.stored = true
		}
		if !h.handed {
			h.handed = true
			if e.opts.OnResults != nil {
				if err := e.opts.OnResults(ctx, c.policy.GetName(), cand.record, cand.info, cand.results); err != nil {
					h.handed = false
					h.failed(now)
					complete = false
					e.log.Warn("online result handoff failed", "policy", c.policy.GetName(), "err", err)
				}
			}
		}
		if !h.promoted {
			h.promoted = true
			if eval(c.promote, activation(cand.info, cand.scores)) {
				if err := e.promoteRecord(c.policy.GetProject(), c.policy.GetPromote().GetDataset(), cand.record, cand.scores); err != nil {
					h.promoted = false
					e.log.Error("promoting trace", "trace", cand.record.GetId(), "err", err)
				} else {
					promoted++
				}
			}
		}
	}
	if ctx.Err() != nil {
		return false
	}
	var fired []alertEvent
	e.mu.Lock()
	st.seen += seen
	st.matched += matched
	st.sampled += fresh
	st.evaluated += evaluated
	st.promoted += promoted
	st.errored += errored
	for _, cand := range sampled {
		if !cand.fresh {
			continue
		}
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
	return complete && ctx.Err() == nil
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
	project string
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
			Below: a.Below, Above: a.Above, At: now, webhook: a.GetWebhook(), project: st.c.policy.GetProject(),
		})
	}
	return out
}

func (e *Engine) notify(ev alertEvent) {
	e.log.Warn("alert", "policy", ev.Policy, "metric", ev.Metric, "firing", ev.Firing, "mean", ev.Mean, "n", ev.N)
	body, _ := json.Marshal(ev)
	if e.opts.OnAlert != nil {
		e.opts.OnAlert(ev.project, ev.Policy, ev.Firing, body)
	}
	if ev.webhook == "" {
		return
	}
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
	_, _, err = datasets.Append(context.Background(), e.opts.DatasetsDir, e.opts.Objects, project, dataset, [][]byte{row})
	return err
}

// Stats reports a policy's counters, windows and alerts.
func (e *Engine) Stats(name string) (*evalsiv1alpha1.PolicyStats, bool) {
	if e.follower.Load() && e.opts.RemoteStats != nil {
		st, ok, err := e.opts.RemoteStats(name)
		if err == nil {
			return st, ok
		}
		e.log.Warn("asking the policy engine for statistics", "policy", name, "err", err)
	}
	return e.LocalStats(name)
}

// LocalStats are this replica's own statistics for a policy.
func (e *Engine) LocalStats(name string) (*evalsiv1alpha1.PolicyStats, bool) {
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
	delete(e.unavailable, name)
	e.mu.Unlock()
	if e.opts.Changed != nil {
		e.opts.Changed()
	}
	return nil
}

// Summary is the stored summary of a trace: what ListTraces returns, and what
// a trace.scored webhook carries.
func Summary(record *evalsiv1alpha1.Record, info ingest.TraceInfo) *evalsiv1alpha1.TraceSummary {
	summary := &evalsiv1alpha1.TraceSummary{
		Project: info.Project, Labels: info.Labels,
		TraceId: record.GetId(), Service: info.Service, Name: info.Name,
		Duration: durationpb.New(time.Duration(info.DurationMS * float64(time.Millisecond))),
		Error:    info.Error, Steps: int32(info.Steps),
	}
	if steps := record.GetTrajectory().GetSteps(); len(steps) > 0 {
		summary.StartTime = steps[0].GetStartTime()
	}
	return summary
}

// An unavailable evaluator/grant is retryable; an intentionally deleted or
// disabled policy is absent from this map and no longer blocks accepted work.
func (e *Engine) policiesReady(it item) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for name, project := range e.unavailable {
		if project == it.info.Project && (len(it.only) == 0 || slices.Contains(it.only, name)) {
			return false
		}
	}
	return true
}
