package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Run events: the replica executing a run publishes its WatchRun events, so
// a watcher on any replica sees them.

func runEvents(id string) string { return "evalsi.runs." + id + ".events" }

const runCancel = "evalsi.runs.cancel"

// PublishRunEvent sends a run's event to watchers on other replicas.
func (c *Cluster) PublishRunEvent(id string, ev *evalsiv1alpha1.WatchRunResponse) {
	data, err := proto.Marshal(ev)
	if err == nil && len(data) <= maxInline {
		_ = c.nc.Publish(runEvents(id), data)
	}
}

// SubscribeRunEvents delivers a run's events until stop is called.
func (c *Cluster) SubscribeRunEvents(id string) (<-chan *evalsiv1alpha1.WatchRunResponse, func(), error) {
	out := make(chan *evalsiv1alpha1.WatchRunResponse, 1024)
	sub, err := c.nc.Subscribe(runEvents(id), func(m *nats.Msg) {
		ev := &evalsiv1alpha1.WatchRunResponse{}
		if proto.Unmarshal(m.Data, ev) == nil {
			select {
			case out <- ev:
			default: // a slow watcher re-reads the state
			}
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return out, func() { _ = sub.Unsubscribe() }, nil
}

// RequestCancel asks whichever replica executes a run to cancel it.
func (c *Cluster) RequestCancel(id string) error {
	return c.nc.Publish(runCancel, []byte(id))
}

// OnCancel calls fn for every cancellation request.
func (c *Cluster) OnCancel(fn func(id string)) (func(), error) {
	sub, err := c.nc.Subscribe(runCancel, func(m *nats.Msg) { fn(string(m.Data)) })
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

// Policies: replicas share policies through the database; a change is
// broadcast so every replica reloads, and statistics live on the elected
// policy engine.

const (
	policiesChanged = "evalsi.policies.changed"
	policyStats     = "evalsi.policies.stats"
)

// PoliciesChanged tells every replica to reload policies.
func (c *Cluster) PoliciesChanged() { _ = c.nc.Publish(policiesChanged, nil) }

// OnPoliciesChanged calls fn when another replica changed policies.
func (c *Cluster) OnPoliciesChanged(fn func()) (func(), error) {
	sub, err := c.nc.Subscribe(policiesChanged, func(*nats.Msg) { fn() })
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

// Access control: projects, roles and bindings live in the database; a change
// is broadcast so every replica reloads its snapshot at once (each replica
// also reloads periodically, in case a broadcast is missed).

const authzChanged = "evalsi.authz.changed"

// AuthzChanged tells every replica to reload access control.
func (c *Cluster) AuthzChanged() { _ = c.nc.Publish(authzChanged, nil) }

// OnAuthzChanged calls fn when a replica changed access control.
func (c *Cluster) OnAuthzChanged(fn func()) (func(), error) {
	sub, err := c.nc.Subscribe(authzChanged, func(*nats.Msg) { fn() })
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

// ServeStats answers statistics requests (the policy engine leader does).
func (c *Cluster) ServeStats(fn func(name string) (*evalsiv1alpha1.PolicyStats, bool)) (func(), error) {
	sub, err := c.nc.QueueSubscribe(policyStats, "leader", func(m *nats.Msg) {
		st, ok := fn(string(m.Data))
		if !ok {
			_ = m.Respond(nil)
			return
		}
		data, _ := proto.Marshal(st)
		_ = m.Respond(data)
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

// Stats asks the policy engine leader for a policy's statistics.
func (c *Cluster) Stats(name string) (*evalsiv1alpha1.PolicyStats, bool, error) {
	m, err := c.nc.Request(policyStats, []byte(name), 5*time.Second)
	if err != nil {
		return nil, false, err
	}
	if len(m.Data) == 0 {
		return nil, false, nil
	}
	st := &evalsiv1alpha1.PolicyStats{}
	return st, true, proto.Unmarshal(m.Data, st)
}

// Spans: ingest replicas publish what they receive; the policy engine
// leader consumes it.

// PublishSpans stores one OTLP resource's spans, with the project and labels
// the ingest credential assigned, in the span stream.
func (c *Cluster) PublishSpans(ctx context.Context, rs *tracepb.ResourceSpans, project string, labels map[string]string) error {
	data, err := proto.Marshal(rs)
	if err != nil {
		return err
	}
	msg := nats.NewMsg(spanSubject)
	msg.Header.Set(hdrProject, project)
	if len(labels) > 0 {
		raw, _ := json.Marshal(labels)
		msg.Header.Set(hdrLabels, string(raw))
	}
	if err := c.putPayload(ctx, msg, data); err != nil {
		return err
	}
	if _, err := c.js.PublishMsg(ctx, msg); err != nil {
		c.dropPayload(msg.Header)
		return fmt.Errorf("cluster: publishing spans: %w", err)
	}
	return nil
}

// ConsumeSpans feeds the span stream to fn until ctx ends; a message is
// acknowledged once fn returns. Only the policy engine leader calls it.
func (c *Cluster) ConsumeSpans(ctx context.Context, fn func(rs *tracepb.ResourceSpans, project string, labels map[string]string)) error {
	cons, err := c.js.Consumer(ctx, spanStream, "policy-engine")
	if err != nil {
		return err
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		h := nats.Header(m.Headers())
		data, err := c.payload(ctx, h, m.Data())
		if err != nil {
			_ = m.Nak()
			return
		}
		rs := &tracepb.ResourceSpans{}
		if err := proto.Unmarshal(data, rs); err != nil {
			_ = m.Term() // malformed: never deliverable
			return
		}
		var labels map[string]string
		if raw := h.Get(hdrLabels); raw != "" {
			_ = json.Unmarshal([]byte(raw), &labels)
		}
		fn(rs, h.Get(hdrProject), labels)
		c.dropPayload(h)
		_ = m.Ack()
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	cc.Stop()
	return nil
}
