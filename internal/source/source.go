// Package source pulls traces from the stores that already hold them (MLflow,
// later Langfuse and Phoenix), hands them to the online policy engine, and
// writes the scores back. Decision 0016 is the design.
//
// A Connector speaks one store's API. The Manager owns everything the stores
// share: the watermark and its lookback, deduplication, deferral of traces
// still in progress, the rate cap, retries, status, and write-back
// bookkeeping.
package source

import (
	"context"
	"fmt"
	"net/http"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/ingest"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

// Info is one trace a store lists.
type Info struct {
	// The store's own ID for the trace (MLflow: "tr-<hex>").
	ID string
	// When the trace started. Stores filter and order by this, but log a
	// trace when it ends, which is why the manager re-reads a window.
	Started time.Time
	// The store still reports the trace as running.
	InProgress bool
	// Changes when the trace's content does (more spans, a new state), so
	// that an unchanged trace read again is not scored again.
	Digest string
	// Set by stores that list spans rather than traces (Phoenix, which the
	// connector assembles); otherwise the manager asks Fetch for the spans.
	Trace *ingest.Trace
}

// ListRequest asks for traces that started at or after Since, oldest first.
type ListRequest struct {
	Since  time.Time
	Cursor string
	Limit  int
}

// ListResult is one page.
type ListResult struct {
	Infos []Info
	// Opaque; empty at the end.
	Next string
}

// Score is one metric of one evaluated trace, to write back.
type Score struct {
	// The store's ID for the trace (Info.ID).
	TraceID string
	Policy  string
	// The evaluator's ref, shown as where the score came from.
	Evaluator string
	Metric    string
	// A float64, bool or string.
	Value     any
	Rationale string
	// The score came from a judge model rather than code.
	Judged bool
}

// Digest identifies a score's value, so one already written is not sent again.
func (s Score) Digest() string { return fmt.Sprintf("%v|%s", s.Value, s.Rationale) }

// Connector is one store.
type Connector interface {
	// List returns a page of traces that started at or after req.Since.
	List(ctx context.Context, req ListRequest) (ListResult, error)
	// Fetch returns the spans of the listed traces whose Trace is nil. A trace
	// the store no longer has is left out.
	Fetch(ctx context.Context, infos []Info) ([]ingest.Trace, error)
	// WriteBack records scores on the store's traces. prior holds what an
	// earlier call wrote, keyed by (trace ID, metric), so a changed score
	// updates it in place. It returns what it wrote, for the manager to keep.
	WriteBack(ctx context.Context, scores []Score, prior map[[2]string]store.SourceWrite) ([]store.SourceWrite, error)
}

// Factory builds a connector for a source. token is the resolved credential
// ("" when the source names none).
type Factory func(src *evalsiv1alpha1.TraceSource, token string, client *http.Client) (Connector, error)

// RetryAfterError is a store asking to be left alone for a while (HTTP 429 or
// 503 with Retry-After).
type RetryAfterError struct {
	After time.Duration
	Err   error
}

func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("the store asked for %s: %v", e.After, e.Err)
}

func (e *RetryAfterError) Unwrap() error { return e.Err }

// Ingester is the policy engine's entry for pulled traces
// (watch.Engine.IngestBatchContext).
type Ingester interface {
	IngestBatchContext(ctx context.Context, traces []ingest.Trace, policies []string) error
}

// Resolver turns a source's credentials into the token, after checking the
// grants (decision 0015). An empty token means the source needs none.
type Resolver func(ctx context.Context, src *evalsiv1alpha1.TraceSource) (string, error)

// Label and attribute names the manager puts on pulled traces.
const (
	// LabelSource is the trace label naming the source, for policy selectors.
	LabelSource = "source"
	// AttrSourceTraceID is the resource attribute holding the store's own trace
	// ID, which write-back needs.
	AttrSourceTraceID = "evalsi.source.trace_id"
)
