package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Defaults for a source that does not set them.
const (
	DefaultPollInterval     = 30 * time.Second
	DefaultMaxTraceDuration = 30 * time.Minute
	DefaultMaxInProgressAge = time.Hour
)

// Options configures the manager.
type Options struct {
	Factories map[string]Factory
	Resolve   Resolver
	HTTP      *http.Client
	Logger    *slog.Logger
	// How often stored sources are re-read to start, stop and restart runners.
	SyncEvery time.Duration
	// Traces per List call.
	PageSize int
	// The cap on pulled traces per second for a source that sets none.
	MaxRate int
	Now     func() time.Time
}

// Manager runs one runner per source. It runs only on the replica that holds
// the policy-engine lease, so a source is never pulled twice at once; a
// failover resumes from the stored watermark.
type Manager struct {
	st   *store.Store
	ing  Ingester
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	runners map[key]*runner

	ScoresDropped atomic.Int64
}

type key struct{ project, name string }

// New returns a manager; Run starts it.
func New(st *store.Store, ing Ingester, opts Options) *Manager {
	if opts.SyncEvery <= 0 {
		opts.SyncEvery = 5 * time.Second
	}
	if opts.PageSize <= 0 {
		opts.PageSize = 100
	}
	if opts.MaxRate <= 0 {
		opts.MaxRate = 200
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	return &Manager{st: st, ing: ing, opts: opts, log: opts.Logger, runners: map[key]*runner{}}
}

// Run keeps a runner for every stored source that is not paused, until ctx is
// done; it then stops them all and waits.
func (m *Manager) Run(ctx context.Context) {
	defer m.stopAll()
	t := time.NewTicker(m.opts.SyncEvery)
	defer t.Stop()
	for {
		m.sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *Manager) sync(ctx context.Context) {
	sources, err := m.st.ListSources(ctx, "")
	if err != nil {
		if ctx.Err() == nil {
			m.log.Error("listing trace sources", "err", err)
		}
		return
	}
	want := map[key]*evalsiv1alpha1.TraceSource{}
	for _, s := range sources {
		if !s.GetPaused() {
			want[key{s.GetProject(), s.GetName()}] = s
		}
	}
	m.mu.Lock()
	var stopped []*runner
	for k, r := range m.runners {
		next, ok := want[k]
		if ok && proto.Equal(next, r.src) {
			continue
		}
		stopped = append(stopped, r)
		delete(m.runners, k)
	}
	m.mu.Unlock()
	for _, r := range stopped {
		r.stop()
	}
	m.mu.Lock()
	for k, s := range want {
		if _, ok := m.runners[k]; ok {
			continue
		}
		r := m.newRunner(s)
		m.runners[k] = r
		r.start(ctx)
	}
	m.mu.Unlock()
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	rs := make([]*runner, 0, len(m.runners))
	for _, r := range m.runners {
		rs = append(rs, r)
	}
	m.runners = map[key]*runner{}
	m.mu.Unlock()
	for _, r := range rs {
		r.stop()
	}
}

// Status returns a source with its status filled in from the stored state and
// the running runner.
func (m *Manager) Status(ctx context.Context, src *evalsiv1alpha1.TraceSource) (*evalsiv1alpha1.TraceSource, error) {
	out := proto.Clone(src).(*evalsiv1alpha1.TraceSource)
	st, err := m.st.SourceState(ctx, src.GetProject(), src.GetName())
	if err != nil {
		return nil, err
	}
	status := &evalsiv1alpha1.SourceStatus{
		Pulled: st.Pulled, Scored: st.Scored, Deferred: int32(st.Deferred), LastError: st.LastError,
	}
	if !st.Watermark.IsZero() {
		status.Watermark = timestamppb.New(st.Watermark)
	}
	if !st.LastErrorAt.IsZero() {
		status.LastErrorAt = timestamppb.New(st.LastErrorAt)
	}
	if !st.LastWriteAt.IsZero() {
		status.LastWriteBackAt = timestamppb.New(st.LastWriteAt)
	}
	if !st.LastPullAt.IsZero() {
		status.LastPullAt = timestamppb.New(st.LastPullAt)
		status.LagSeconds = max(0, m.opts.Now().Sub(st.LastPullAt).Seconds())
	}
	m.mu.Lock()
	r := m.runners[key{src.GetProject(), src.GetName()}]
	m.mu.Unlock()
	switch {
	case src.GetPaused():
		status.Phase = evalsiv1alpha1.SourcePhase_SOURCE_PHASE_PAUSED
	case r == nil:
		// Not running here (a follower, or not started yet): what was stored.
		status.Phase = phaseOf(st)
	default:
		status.Phase = evalsiv1alpha1.SourcePhase(r.phase.Load())
		status.RecordsPerSecond = r.rate()
	}
	out.Status = status
	return out, nil
}

func phaseOf(st store.SourceState) evalsiv1alpha1.SourcePhase {
	switch {
	case st.LastError != "" && st.LastErrorAt.After(st.LastPullAt):
		return evalsiv1alpha1.SourcePhase_SOURCE_PHASE_ERROR
	case !st.BackfillFrom.IsZero():
		return evalsiv1alpha1.SourcePhase_SOURCE_PHASE_BACKFILLING
	case !st.Watermark.IsZero():
		return evalsiv1alpha1.SourcePhase_SOURCE_PHASE_TAILING
	}
	return evalsiv1alpha1.SourcePhase_SOURCE_PHASE_UNSPECIFIED
}

// OnResults receives each evaluated trace's results (the engine's OnResults
// hook) and queues the scores of a pulled trace for write-back. It never
// blocks.
func (m *Manager) OnResults(policy string, _ *evalsiv1alpha1.Record, info ingest.TraceInfo, results []*evalsiv1alpha1.EvaluationResult) {
	name := info.Labels[LabelSource]
	if name == "" {
		return
	}
	m.mu.Lock()
	r := m.runners[key{info.Project, name}]
	m.mu.Unlock()
	if r == nil || !r.src.GetWriteBack().GetEnabled() {
		return
	}
	traceID, _ := info.Resource[AttrSourceTraceID].(string)
	if traceID == "" {
		return
	}
	for _, res := range results {
		if res.GetOutcome() != evalsiv1alpha1.Outcome_OUTCOME_SCORED {
			continue
		}
		for _, sc := range res.GetScores() {
			v, ok := scoreValue(sc)
			if !ok {
				continue
			}
			_, judged := sc.GetMetadata()["judge_model"]
			s := Score{
				TraceID: traceID, Policy: policy, Evaluator: res.GetEvaluatorRef(), Metric: metricName(res, sc),
				Value: v, Rationale: sc.GetExplanation(), Judged: judged,
			}
			select {
			case r.scores <- s:
			default:
				m.ScoresDropped.Add(1)
			}
		}
	}
}

// metricName is the name the sinks and the policy statistics give a score.
func metricName(r *evalsiv1alpha1.EvaluationResult, s *evalsiv1alpha1.Score) string {
	short := r.GetEvaluatorRef()
	for i := 0; i < len(short); i++ {
		if short[i] == '@' {
			short = short[:i]
			break
		}
	}
	for i := len(short) - 1; i >= 0; i-- {
		if short[i] == '/' {
			short = short[i+1:]
			break
		}
	}
	if s.GetName() == "" || s.GetName() == short || s.GetName() == r.GetEvaluator() {
		return r.GetEvaluator()
	}
	return r.GetEvaluator() + "." + s.GetName()
}

func scoreValue(s *evalsiv1alpha1.Score) (any, bool) {
	switch v := s.GetValue().(type) {
	case *evalsiv1alpha1.Score_Number:
		return v.Number, true
	case *evalsiv1alpha1.Score_Passed:
		return v.Passed, true
	case *evalsiv1alpha1.Score_Label:
		return v.Label, true
	}
	return nil, false
}

// --- runners ---

type runner struct {
	m    *Manager
	src  *evalsiv1alpha1.TraceSource
	log  *slog.Logger
	done chan struct{}

	cancel context.CancelFunc
	phase  atomic.Int32
	scores chan Score

	rateMu    sync.Mutex
	rateCount int64
	rateSince time.Time
	rateValue float64
}

func (m *Manager) newRunner(src *evalsiv1alpha1.TraceSource) *runner {
	return &runner{
		m: m, src: proto.Clone(src).(*evalsiv1alpha1.TraceSource), done: make(chan struct{}),
		log:    m.log.With("source", src.GetProject()+"/"+src.GetName(), "connector", src.GetConnector()),
		scores: make(chan Score, 4096),
	}
}

func (r *runner) start(ctx context.Context) {
	ctx, r.cancel = context.WithCancel(ctx)
	go func() { defer close(r.done); r.run(ctx) }()
}

func (r *runner) stop() {
	r.cancel()
	<-r.done
}

func dur(d *durationpb.Duration, def time.Duration) time.Duration {
	if d.IsValid() && d.AsDuration() > 0 {
		return d.AsDuration()
	}
	return def
}

func (r *runner) interval() time.Duration {
	return dur(r.src.GetPoll().GetInterval(), DefaultPollInterval)
}

func (r *runner) record(n int) {
	r.rateMu.Lock()
	defer r.rateMu.Unlock()
	now := r.m.opts.Now()
	if r.rateSince.IsZero() {
		r.rateSince = now
	}
	r.rateCount += int64(n)
	if el := now.Sub(r.rateSince); el >= 10*time.Second {
		r.rateValue = float64(r.rateCount) / el.Seconds()
		r.rateCount, r.rateSince = 0, now
	}
}

func (r *runner) rate() float64 {
	r.rateMu.Lock()
	defer r.rateMu.Unlock()
	return r.rateValue
}

func (r *runner) run(ctx context.Context) {
	var conn Connector
	failures := 0
	wait := time.Duration(0) // first cycle at once
	var pending []Score
	var flush <-chan time.Time
	for {
		poll := time.NewTimer(wait)
	idle:
		for {
			select {
			case <-ctx.Done():
				poll.Stop()
				return
			case s := <-r.scores:
				pending = append(pending, s)
				if flush == nil {
					flush = time.After(500 * time.Millisecond)
				}
			case <-flush:
				flush = nil
				if conn != nil {
					pending = r.writeBack(ctx, conn, pending)
				}
			case <-poll.C:
				break idle
			}
		}
		var err error
		if conn == nil {
			conn, err = r.connect(ctx)
		}
		if err == nil {
			err = r.cycle(ctx, conn)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			failures++
			wait = backoff(r.interval(), failures, err)
			r.fail(ctx, err)
			r.log.Warn("pulling traces failed", "err", err, "retry_in", wait)
			continue
		}
		failures = 0
		wait = r.interval()
	}
}

func backoff(interval time.Duration, failures int, err error) time.Duration {
	var ra *RetryAfterError
	if errors.As(err, &ra) && ra.After > 0 {
		return ra.After
	}
	d := interval
	for i := 1; i < failures && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

func (r *runner) connect(ctx context.Context) (Connector, error) {
	f := r.m.opts.Factories[r.src.GetConnector()]
	if f == nil {
		return nil, fmt.Errorf("no %q connector is built", r.src.GetConnector())
	}
	token := ""
	if r.m.opts.Resolve != nil {
		var err error
		if token, err = r.m.opts.Resolve(ctx, r.src); err != nil {
			return nil, err
		}
	}
	return f(r.src, token, r.m.opts.HTTP)
}

func (r *runner) fail(ctx context.Context, cause error) {
	r.phase.Store(int32(evalsiv1alpha1.SourcePhase_SOURCE_PHASE_ERROR))
	st, err := r.m.st.SourceState(ctx, r.src.GetProject(), r.src.GetName())
	if err != nil {
		return
	}
	st.LastError, st.LastErrorAt = cause.Error(), r.m.opts.Now().UTC()
	_ = r.m.st.PutSourceState(ctx, r.src.GetProject(), r.src.GetName(), st)
}

// cycle reads every trace that started at or after the watermark, hands the
// new ones to the policy engine, and moves the watermark.
//
// The watermark is never past cycleStart - maxTraceDuration. Stores log a trace
// when it ends but index it by when it started, so a trace that is still
// running when this cycle looks is only found by a later cycle that reaches
// back far enough; a trace longer than maxTraceDuration is missed. It is also
// never past the start of a deferred (in-progress) trace.
func (r *runner) cycle(ctx context.Context, conn Connector) error {
	src, st := r.src, store.SourceState{}
	project, name := src.GetProject(), src.GetName()
	var err error
	if st, err = r.m.st.SourceState(ctx, project, name); err != nil {
		return err
	}
	cycleStart := r.m.opts.Now().UTC()
	maxDur := dur(src.GetMaxTraceDuration(), DefaultMaxTraceDuration)
	maxProgress := dur(src.GetMaxInProgressAge(), DefaultMaxInProgressAge)
	horizon := cycleStart.Add(-maxDur)
	if st.Watermark.IsZero() {
		// First cycle: read history when asked to, otherwise only what comes.
		if since := src.GetBackfill().GetSince().AsDuration(); since > 0 {
			st.Watermark = cycleStart.Add(-since)
			st.BackfillFrom = st.Watermark
		} else {
			st.Watermark = cycleStart
		}
	}
	if st.BackfillFrom.IsZero() {
		r.phase.Store(int32(evalsiv1alpha1.SourcePhase_SOURCE_PHASE_TAILING))
	} else {
		r.phase.Store(int32(evalsiv1alpha1.SourcePhase_SOURCE_PHASE_BACKFILLING))
	}
	rate := src.GetPoll().GetMaxRecordsPerSecond()
	if rate <= 0 {
		rate = int32(r.m.opts.MaxRate)
	}

	var earliestDeferred time.Time
	deferred := 0
	cursor := ""
	// The query stays the same for the whole cycle: a page cursor is only
	// valid for the query that produced it, and the watermark moves per page.
	since := st.Watermark
	for {
		res, err := conn.List(ctx, ListRequest{Since: since, Cursor: cursor, Limit: r.m.opts.PageSize})
		if err != nil {
			return err
		}
		ids := make([]string, len(res.Infos))
		for i, in := range res.Infos {
			ids[i] = in.ID
		}
		seen, err := r.m.st.SeenTraces(ctx, project, name, ids)
		if err != nil {
			return err
		}
		var fresh []Info
		var lastStart time.Time
		for _, in := range res.Infos {
			if in.InProgress && cycleStart.Sub(in.Started) < maxProgress {
				deferred++
				if earliestDeferred.IsZero() || in.Started.Before(earliestDeferred) {
					earliestDeferred = in.Started
				}
				continue
			}
			if in.Started.After(lastStart) && (earliestDeferred.IsZero() || in.Started.Before(earliestDeferred)) {
				lastStart = in.Started
			}
			if s, ok := seen[in.ID]; ok && s.Digest == in.Digest {
				continue
			}
			fresh = append(fresh, in)
		}
		traces, err := r.collect(ctx, conn, fresh)
		if err != nil {
			return err
		}
		if len(traces) > 0 {
			if err := sleep(ctx, time.Duration(float64(len(traces))/float64(rate)*float64(time.Second))); err != nil {
				return err
			}
			if err := r.m.ing.IngestBatchContext(ctx, traces, src.GetPolicies()); err != nil {
				return err
			}
			marks := make([]store.SeenTrace, 0, len(traces))
			byID := map[string]Info{}
			for _, in := range fresh {
				byID[in.ID] = in
			}
			for _, t := range traces {
				id := sourceID(t)
				in := byID[id]
				marks = append(marks, store.SeenTrace{TraceID: id, Digest: in.Digest, Started: in.Started})
			}
			if err := r.m.st.MarkSeen(ctx, project, name, marks); err != nil {
				return err
			}
			st.Pulled += int64(len(traces))
			st.LastPullAt = cycleStart
			r.record(len(traces))
		}
		// Progress, committed per page so a long backfill resumes where it was.
		if next := minTime(lastStart, horizon, earliestDeferred); next.After(st.Watermark) {
			st.Watermark = next
		}
		st.Deferred = deferred
		if err := r.m.st.PutSourceState(ctx, project, name, st); err != nil {
			return err
		}
		if res.Next == "" || len(res.Infos) == 0 {
			break
		}
		cursor = res.Next
	}
	// The cycle covered everything the store has: the watermark moves to the horizon.
	if next := minTime(horizon, earliestDeferred); next.After(st.Watermark) {
		st.Watermark = next
	}
	if !st.BackfillFrom.IsZero() && !st.Watermark.Before(horizon.Add(-r.interval())) {
		st.BackfillFrom = time.Time{}
		r.phase.Store(int32(evalsiv1alpha1.SourcePhase_SOURCE_PHASE_TAILING))
	}
	st.Deferred, st.LastPullAt, st.LastError, st.LastErrorAt = deferred, cycleStart, "", time.Time{}
	if err := r.m.st.PutSourceState(ctx, project, name, st); err != nil {
		return err
	}
	// Traces that started before the watermark cannot be listed again.
	if _, err := r.m.st.PruneSeen(ctx, project, name, st.Watermark.Add(-time.Minute)); err != nil {
		return err
	}
	return nil
}

// collect gets the spans of the fresh traces and labels them as the source's.
func (r *runner) collect(ctx context.Context, conn Connector, fresh []Info) ([]ingest.Trace, error) {
	var out []ingest.Trace
	var need []Info
	for _, in := range fresh {
		if in.Trace != nil {
			out = append(out, r.label(*in.Trace, in.ID))
		} else {
			need = append(need, in)
		}
	}
	if len(need) > 0 {
		got, err := conn.Fetch(ctx, need)
		if err != nil {
			return nil, err
		}
		for _, t := range got {
			out = append(out, r.label(t, sourceID(t)))
		}
	}
	return out, nil
}

// sourceID is the store's own ID that label stored on the trace.
func sourceID(t ingest.Trace) string {
	for _, sp := range t.Spans {
		if id, _ := sp.Resource[AttrSourceTraceID].(string); id != "" {
			return id
		}
	}
	return t.TraceID
}

// label puts the source's project and labels on a pulled trace, and the
// store's ID in a resource attribute for write-back.
func (r *runner) label(t ingest.Trace, id string) ingest.Trace {
	t.Project = r.src.GetProject()
	spans := make([]ingest.Span, len(t.Spans))
	for i, sp := range t.Spans {
		sp.Project = r.src.GetProject()
		labels := maps.Clone(sp.Labels)
		if labels == nil {
			labels = map[string]string{}
		}
		maps.Copy(labels, r.src.GetTraceLabels())
		labels[LabelSource] = r.src.GetName()
		sp.Labels = labels
		res := maps.Clone(sp.Resource)
		if res == nil {
			res = ingest.Attrs{}
		}
		res[AttrSourceTraceID] = id
		sp.Resource = res
		spans[i] = sp
	}
	t.Spans = spans
	return t
}

// writeBack sends the scores, remembers what the store returned, and returns
// what could not be sent (kept for the next flush).
func (r *runner) writeBack(ctx context.Context, conn Connector, scores []Score) []Score {
	if len(scores) == 0 || !r.src.GetWriteBack().GetEnabled() {
		return nil
	}
	project, name := r.src.GetProject(), r.src.GetName()
	ids := make([]string, 0, len(scores))
	for _, s := range scores {
		ids = append(ids, s.TraceID)
	}
	prior, err := r.m.st.SourceWrites(ctx, project, name, ids)
	if err != nil {
		r.log.Warn("reading write-back records", "err", err)
		return scores
	}
	var todo []Score
	for _, s := range scores {
		if w, ok := prior[[2]string{s.TraceID, s.Metric}]; ok && w.Digest == s.Digest() {
			continue // already written, unchanged
		}
		todo = append(todo, s)
	}
	if len(todo) == 0 {
		return nil
	}
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].TraceID < todo[j].TraceID })
	written, err := conn.WriteBack(ctx, todo, prior)
	for _, w := range written {
		if perr := r.m.st.PutSourceWrite(ctx, project, name, w); perr != nil {
			r.log.Warn("recording a written score", "err", perr)
		}
	}
	if len(written) > 0 {
		if st, serr := r.m.st.SourceState(ctx, project, name); serr == nil {
			st.LastWriteAt = r.m.opts.Now().UTC()
			st.Scored += int64(len(written))
			_ = r.m.st.PutSourceState(ctx, project, name, st)
		}
	}
	if err != nil {
		r.log.Warn("writing scores back failed", "err", err)
		// Keep only what was not written; the next flush retries it.
		done := map[[2]string]bool{}
		for _, w := range written {
			done[[2]string{w.TraceID, w.Metric}] = true
		}
		var rest []Score
		for _, s := range todo {
			if !done[[2]string{s.TraceID, s.Metric}] {
				rest = append(rest, s)
			}
		}
		return rest
	}
	return nil
}

func minTime(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.IsZero() {
			continue
		}
		if out.IsZero() || t.Before(out) {
			out = t
		}
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
