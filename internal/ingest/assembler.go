package ingest

import (
	"encoding/hex"
	"sync"
	"time"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// AssemblerOptions bounds how long and how much the assembler buffers.
type AssemblerOptions struct {
	// Wait after the root span ends for late child spans.
	Grace time.Duration
	// Flush a trace whose root never arrives after this long without new spans.
	Idle time.Duration
	// Flush any trace this long after its first span, root or not.
	MaxAge time.Duration
	// Buffered traces; the oldest is flushed early when exceeded.
	MaxTraces int
	// Spans kept per trace; extra spans are dropped and counted.
	MaxSpans int
}

func (o *AssemblerOptions) defaults() {
	if o.Grace <= 0 {
		o.Grace = 2 * time.Second
	}
	if o.Idle <= 0 {
		o.Idle = 30 * time.Second
	}
	if o.MaxAge <= 0 {
		o.MaxAge = 5 * time.Minute
	}
	if o.MaxTraces <= 0 {
		o.MaxTraces = 10000
	}
	if o.MaxSpans <= 0 {
		o.MaxSpans = 2000
	}
}

type pending struct {
	spans     []Span
	first     time.Time
	last      time.Time
	rootEnded time.Time
}

// Assembler groups spans by trace id and emits each trace once it is complete:
// its root span ended more than Grace ago (the same idea as the Collector's
// tail-sampling decision wait), or it went idle, or it got too old.
type Assembler struct {
	opts AssemblerOptions
	emit func(Trace)
	now  func() time.Time

	mu      sync.Mutex
	traces  map[string]*pending
	dropped int64
}

// NewAssembler calls emit (from Flush's goroutine) for every completed trace.
func NewAssembler(opts AssemblerOptions, emit func(Trace)) *Assembler {
	opts.defaults()
	return &Assembler{opts: opts, emit: emit, now: time.Now, traces: map[string]*pending{}}
}

// Add buffers spans.
func (a *Assembler) Add(spans []Span) {
	now := a.now()
	var evicted []Trace
	a.mu.Lock()
	for _, s := range spans {
		id := hex.EncodeToString(s.Span.GetTraceId())
		p := a.traces[id]
		if p == nil {
			if len(a.traces) >= a.opts.MaxTraces {
				if t, ok := a.evictOldestLocked(); ok {
					evicted = append(evicted, t)
				}
			}
			p = &pending{first: now}
			a.traces[id] = p
		}
		p.last = now
		if len(p.spans) >= a.opts.MaxSpans {
			a.dropped++
			continue
		}
		p.spans = append(p.spans, s)
		if len(s.Span.GetParentSpanId()) == 0 {
			p.rootEnded = now
		}
	}
	a.mu.Unlock()
	for _, t := range evicted {
		a.emit(t)
	}
}

func (a *Assembler) evictOldestLocked() (Trace, bool) {
	var oldestID string
	var oldest time.Time
	for id, p := range a.traces {
		if oldestID == "" || p.first.Before(oldest) {
			oldestID, oldest = id, p.first
		}
	}
	if oldestID == "" {
		return Trace{}, false
	}
	p := a.traces[oldestID]
	delete(a.traces, oldestID)
	return Trace{TraceID: oldestID, Spans: p.spans}, true
}

// Flush emits every complete trace; force emits everything (shutdown).
func (a *Assembler) Flush(force bool) {
	now := a.now()
	var ready []Trace
	a.mu.Lock()
	for id, p := range a.traces {
		done := force ||
			(!p.rootEnded.IsZero() && now.Sub(p.rootEnded) >= a.opts.Grace) ||
			now.Sub(p.last) >= a.opts.Idle ||
			now.Sub(p.first) >= a.opts.MaxAge
		if done {
			ready = append(ready, Trace{TraceID: id, Spans: p.spans})
			delete(a.traces, id)
		}
	}
	a.mu.Unlock()
	for _, t := range ready {
		a.emit(t)
	}
}

// Dropped reports spans dropped because a trace exceeded MaxSpans.
func (a *Assembler) Dropped() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dropped
}

// Pending reports how many traces are buffered.
func (a *Assembler) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.traces)
}

// Run flushes every interval until stop is closed, then flushes everything.
func (a *Assembler) Run(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			a.Flush(true)
			return
		case <-t.C:
			a.Flush(false)
		}
	}
}

// SpansOf flattens an OTLP export request into spans with their resources.
func SpansOf(resourceSpans []*tracepb.ResourceSpans) []Span {
	var out []Span
	for _, rs := range resourceSpans {
		res := ToAttrs(rs.GetResource().GetAttributes())
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				out = append(out, Span{Span: sp, Resource: res})
			}
		}
	}
	return out
}
