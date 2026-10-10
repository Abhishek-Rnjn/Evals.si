// Package webhooks delivers events to HTTP endpoints, signed with HMAC.
//
// An event (a finished run, a scored trace, an alert) writes one delivery row
// per matching webhook (an outbox in the store), and a dispatcher, run by
// whichever replica holds a lease, sends them and retries failures with
// growing delays. A restart or a crashed replica loses nothing.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/store"
	"github.com/abhishek-rnjn/evals.si/internal/version"
)

// Event types, as they appear in the Evalsi-Event header and payloads.
const (
	EventRunFinished   = "run.finished"
	EventRunGateFailed = "run.gate_failed"
	EventTraceScored   = "trace.scored"
	EventAlertFired    = "alert.fired"
	EventAlertResolved = "alert.resolved"
	EventPing          = "ping"
)

// Header names of a delivery.
const (
	HeaderEvent     = "Evalsi-Event"
	HeaderDelivery  = "Evalsi-Delivery"
	HeaderSignature = "Evalsi-Signature"
)

const (
	leaseName     = "webhooks-dispatcher"
	leaseTTL      = 60 * time.Second
	batchSize     = 16
	pollInterval  = time.Second
	pruneInterval = time.Hour
	// How long a project's "does any webhook want trace.scored" answer is
	// reused, so live traffic does not read the webhooks table per trace.
	traceHooksTTL = 5 * time.Second
)

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)

// Options tune the service; zero values take the defaults.
type Options struct {
	Logger *slog.Logger
	// Client sends deliveries; its redirects are never followed.
	Client      *http.Client
	MaxAttempts int
	Timeout     time.Duration
	Retention   time.Duration
	// Backoff is the wait after the nth failed attempt.
	Backoff func(attempt int) time.Duration
	// Owner names this replica for the dispatcher lease.
	Owner string
	Now   func() time.Time
}

// Service implements WebhookService and runs the dispatcher.
type Service struct {
	store *store.Store
	opts  Options
	log   *slog.Logger
	wake  chan struct{}

	mu       sync.Mutex
	lastPrun time.Time
	counts   map[string]int64 // by outcome, for /metrics
	// Per project: whether a webhook lists trace.scored, and when that was read.
	traceHooks map[string]cachedBool
}

type cachedBool struct {
	value bool
	at    time.Time
}

// New makes the service.
func New(st *store.Store, opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 6
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.Retention <= 0 {
		opts.Retention = 7 * 24 * time.Hour
	}
	if opts.Backoff == nil {
		opts.Backoff = defaultBackoff
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Owner == "" {
		opts.Owner = "webhooks-" + randomHex(6)
	}
	if opts.Client == nil {
		opts.Client = &http.Client{}
	}
	client := *opts.Client
	client.Timeout = opts.Timeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	opts.Client = &client
	return &Service{store: st, opts: opts, log: opts.Logger, wake: make(chan struct{}, 1), counts: map[string]int64{}, traceHooks: map[string]cachedBool{}}
}

// defaultBackoff: 5s, 30s, 3m, 15m, then an hour.
func defaultBackoff(attempt int) time.Duration {
	steps := []time.Duration{5 * time.Second, 30 * time.Second, 3 * time.Minute, 15 * time.Minute, time.Hour}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(steps) {
		return steps[len(steps)-1]
	}
	return steps[attempt-1]
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Sign returns the Evalsi-Signature header value for a body sent at t:
// "t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>".
func Sign(secret string, t time.Time, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", t.Unix())
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", t.Unix(), hex.EncodeToString(mac.Sum(nil)))
}

// Verify checks a signature header against a body, for receivers written in
// Go and for tests. A timestamp further than tolerance from now is refused
// (zero tolerance skips that check).
func Verify(secret, header string, body []byte, tolerance time.Duration, now time.Time) error {
	var ts int64
	var sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sig = v
		}
	}
	if ts == 0 || sig == "" {
		return errors.New("malformed signature header")
	}
	if tolerance > 0 {
		if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
			return errors.New("timestamp outside the tolerance")
		}
	}
	want := Sign(secret, time.Unix(ts, 0), body)
	_, wantSig, _ := strings.Cut(want, ",v1=")
	if !hmac.Equal([]byte(wantSig), []byte(sig)) {
		return errors.New("signature mismatch")
	}
	return nil
}

type payload struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Project   string          `json:"project"`
	Run       json.RawMessage `json:"run,omitempty"`
	Trace     json.RawMessage `json:"trace,omitempty"`
	Policy    string          `json:"policy,omitempty"`
	Results   json.RawMessage `json:"results,omitempty"`
	Alert     json.RawMessage `json:"alert,omitempty"`
}

// event is what an event adds to its payload and delivery rows.
type event struct {
	project, runID, traceID, policy string
	run, trace, results, alert      json.RawMessage
}

// runJSON is a run as a receiver sees it: without its spec, which holds the
// target's and the dataset's settings and is not the receiver's business.
func runJSON(run *evalsiv1alpha1.Run) (json.RawMessage, error) {
	r := proto.Clone(run).(*evalsiv1alpha1.Run)
	r.Spec = nil
	return protojson.MarshalOptions{}.Marshal(r)
}

// RunFinished queues the events of a run that reached a final status:
// run.finished always, and run.gate_failed when a gate failed. It is the
// runs manager's OnFinished hook and never fails a run.
func (s *Service) RunFinished(run *evalsiv1alpha1.Run) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := []string{EventRunFinished}
	if run.GetStatus() == evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED {
		events = append(events, EventRunGateFailed)
	}
	rj, err := runJSON(run)
	if err == nil {
		err = s.enqueue(ctx, event{project: run.GetProject(), runID: run.GetId(), run: rj}, events)
	}
	if err != nil {
		s.log.Error("queueing webhook deliveries", "run", run.GetId(), "err", err)
	}
}

// TraceScored queues trace.scored for the webhooks of the trace's project
// that list it: the trace's summary and one policy's results. It is called
// for every trace a policy scores, so it reads the project's webhooks at
// most every few seconds and writes nothing when none listens.
func (s *Service) TraceScored(trace *evalsiv1alpha1.TraceSummary, policy string, results []*evalsiv1alpha1.EvaluationResult) {
	project := trace.GetProject()
	if project == "" {
		project = "default"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !s.wantsTraces(ctx, project) {
		return
	}
	tj, err := protojson.Marshal(trace)
	if err != nil {
		s.log.Error("queueing webhook deliveries", "trace", trace.GetTraceId(), "err", err)
		return
	}
	rs := make([]json.RawMessage, len(results))
	for i, r := range results {
		if rs[i], err = protojson.Marshal(r); err != nil {
			s.log.Error("queueing webhook deliveries", "trace", trace.GetTraceId(), "err", err)
			return
		}
	}
	rj, _ := json.Marshal(rs)
	ev := event{project: project, traceID: trace.GetTraceId(), policy: policy, trace: tj, results: rj}
	if err := s.enqueue(ctx, ev, []string{EventTraceScored}); err != nil {
		s.log.Error("queueing webhook deliveries", "trace", trace.GetTraceId(), "err", err)
	}
}

// AlertChanged queues alert.fired or alert.resolved for a policy's alert.
// alert is the body the policy's own alert webhook receives.
func (s *Service) AlertChanged(project, policy string, firing bool, alert []byte) {
	if project == "" {
		project = "default"
	}
	name := EventAlertResolved
	if firing {
		name = EventAlertFired
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.enqueue(ctx, event{project: project, policy: policy, alert: alert}, []string{name}); err != nil {
		s.log.Error("queueing webhook deliveries", "policy", policy, "err", err)
	}
}

// wantsTraces reports whether a webhook of the project lists trace.scored,
// from a cache refreshed every traceHooksTTL.
func (s *Service) wantsTraces(ctx context.Context, project string) bool {
	now := s.opts.Now()
	s.mu.Lock()
	c, ok := s.traceHooks[project]
	s.mu.Unlock()
	if ok && now.Sub(c.at) < traceHooksTTL {
		return c.value
	}
	hooks, err := s.store.ListWebhooks(ctx, project)
	if err != nil {
		s.log.Warn("reading webhooks", "project", project, "err", err)
		return true // enqueue reads them again and reports its own error
	}
	want := false
	for _, w := range hooks {
		if !w.GetDisabled() && slices.Contains(w.GetEvents(), evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_TRACE_SCORED) {
			want = true
		}
	}
	s.mu.Lock()
	s.traceHooks[project] = cachedBool{value: want, at: now}
	s.mu.Unlock()
	return want
}

// forgetTraceHooks drops a project's cached answer, after its webhooks change
// on this replica.
func (s *Service) forgetTraceHooks(project string) {
	s.mu.Lock()
	delete(s.traceHooks, project)
	s.mu.Unlock()
}

func eventName(e evalsiv1alpha1.WebhookEvent) string {
	switch e {
	case evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_RUN_FINISHED:
		return EventRunFinished
	case evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_RUN_GATE_FAILED:
		return EventRunGateFailed
	case evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_TRACE_SCORED:
		return EventTraceScored
	case evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_ALERT_FIRED:
		return EventAlertFired
	case evalsiv1alpha1.WebhookEvent_WEBHOOK_EVENT_ALERT_RESOLVED:
		return EventAlertResolved
	}
	return ""
}

// subscribed reports whether a webhook receives an event. No events means
// every event but trace.scored, which follows live traffic and must be asked
// for by name.
func subscribed(w *evalsiv1alpha1.Webhook, event string) bool {
	if len(w.GetEvents()) == 0 {
		return event != EventTraceScored
	}
	for _, e := range w.GetEvents() {
		if eventName(e) == event {
			return true
		}
	}
	return false
}

func (s *Service) enqueue(ctx context.Context, ev event, events []string) error {
	project := ev.project
	if project == "" {
		project = "default"
	}
	hooks, err := s.store.ListWebhooks(ctx, project)
	if err != nil {
		return err
	}
	now := s.opts.Now()
	queued := false
	for _, w := range hooks {
		if w.GetDisabled() {
			continue
		}
		for _, name := range events {
			if !subscribed(w, name) {
				continue
			}
			id := "whd_" + randomHex(12)
			body, err := json.Marshal(payload{ID: id, Type: name, CreatedAt: now.UTC(), Project: project,
				Run: ev.run, Trace: ev.trace, Policy: ev.policy, Results: ev.results, Alert: ev.alert})
			if err != nil {
				return err
			}
			d := &store.Delivery{ID: id, Project: project, Webhook: w.GetName(), Event: name,
				RunID: ev.runID, TraceID: ev.traceID, Policy: ev.policy, Payload: body, Created: now, Next: now}
			if err := s.store.AddDelivery(ctx, d); err != nil {
				return err
			}
			queued = true
		}
	}
	if queued {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// Run is the dispatcher: it sends due deliveries until ctx ends. Only the
// replica holding the lease sends, so a delivery is not sent twice at once.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	defer func() {
		_ = s.store.ReleaseLease(context.Background(), leaseName, s.opts.Owner)
	}()
	for {
		s.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.wake:
		}
	}
}

func (s *Service) dispatch(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	ok, err := s.store.AcquireLease(ctx, leaseName, s.opts.Owner, leaseTTL)
	if err != nil {
		s.log.Warn("webhook dispatcher lease", "err", err)
		return
	}
	if !ok {
		return
	}
	due, err := s.store.DueDeliveries(ctx, s.opts.Now(), batchSize)
	if err != nil {
		s.log.Warn("reading due webhook deliveries", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, d := range due {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.attempt(ctx, d)
		}()
	}
	wg.Wait()
	s.mu.Lock()
	prune := s.opts.Now().Sub(s.lastPrun) > pruneInterval
	if prune {
		s.lastPrun = s.opts.Now()
	}
	s.mu.Unlock()
	if prune {
		_ = s.store.PruneDeliveries(ctx, s.opts.Now().Add(-s.opts.Retention))
	}
}

// send posts one signed delivery and reports the status (0: no answer).
func (s *Service) send(ctx context.Context, w *evalsiv1alpha1.Webhook, id, event string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.GetUrl(), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "evalsi-webhooks/"+version.Version)
	req.Header.Set(HeaderEvent, event)
	req.Header.Set(HeaderDelivery, id)
	req.Header.Set(HeaderSignature, Sign(w.GetSecret(), s.opts.Now(), body))
	resp, err := s.opts.Client.Do(req)
	if err != nil {
		// A url.Error repeats the URL, which may carry a token in its query.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func (s *Service) attempt(ctx context.Context, d *store.Delivery) {
	now := s.opts.Now()
	w, err := s.store.GetWebhook(ctx, d.Project, d.Webhook)
	switch {
	case errors.Is(err, store.ErrNotFound):
		d.State, d.Error = store.DeliveryFailed, "the webhook was deleted"
	case err != nil:
		s.log.Warn("reading webhook", "webhook", d.Webhook, "err", err)
		return
	case w.GetDisabled():
		d.State, d.Error = store.DeliveryFailed, "the webhook is disabled"
	default:
		code, serr := s.send(ctx, w, d.ID, d.Event, d.Payload)
		if ctx.Err() != nil {
			return // shutting down: leave it pending
		}
		d.Attempts++
		d.StatusCode = code
		switch {
		case serr == nil:
			d.State, d.Error, d.Delivered = store.DeliveryDelivered, "", s.opts.Now()
		case d.Attempts >= s.opts.MaxAttempts:
			d.State, d.Error = store.DeliveryFailed, serr.Error()
		default:
			d.Error, d.Next = serr.Error(), now.Add(s.opts.Backoff(d.Attempts))
		}
	}
	s.count(d)
	if err := s.store.RecordAttempt(ctx, d); err != nil {
		s.log.Warn("recording webhook attempt", "delivery", d.ID, "err", err)
		return
	}
	switch d.State {
	case store.DeliveryDelivered:
		s.log.Debug("webhook delivered", "webhook", d.Webhook, "event", d.Event, "attempts", d.Attempts)
	case store.DeliveryFailed:
		s.log.Warn("webhook delivery failed", "project", d.Project, "webhook", d.Webhook, "event", d.Event, "attempts", d.Attempts, "err", d.Error)
	}
}

func (s *Service) count(d *store.Delivery) {
	outcome := "retry"
	switch d.State {
	case store.DeliveryDelivered:
		outcome = "delivered"
	case store.DeliveryFailed:
		outcome = "failed"
	}
	s.mu.Lock()
	s.counts[outcome]++
	s.mu.Unlock()
}

// WriteMetrics adds the delivery counters to /metrics.
func (s *Service) WriteMetrics(b io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(b, "# HELP evalsi_webhook_attempts_total Webhook delivery attempts by outcome (delivered, retry or failed).\n# TYPE evalsi_webhook_attempts_total counter\n")
	for _, outcome := range []string{"delivered", "retry", "failed"} {
		fmt.Fprintf(b, "evalsi_webhook_attempts_total{outcome=%q} %d\n", outcome, s.counts[outcome])
	}
}

func newSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b)
}
