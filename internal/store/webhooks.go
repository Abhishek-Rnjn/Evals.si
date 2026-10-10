package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// webhookSchema holds webhooks and their deliveries (trace_id and policy are
// added by addTenantColumns). A delivery row is the outbox: an event writes
// one per matching webhook, and the dispatcher (whichever replica holds the
// lease) sends it, retrying until it is delivered or out of attempts, so a
// restart loses nothing.
const webhookSchema = `
CREATE TABLE IF NOT EXISTS webhooks (
  project TEXT NOT NULL, name TEXT NOT NULL, body BLOB NOT NULL, updated_ns INTEGER NOT NULL,
  PRIMARY KEY (project, name)
);
CREATE TABLE IF NOT EXISTS webhook_deliveries (
  id TEXT PRIMARY KEY, project TEXT NOT NULL, webhook TEXT NOT NULL, event TEXT NOT NULL, run_id TEXT NOT NULL,
  payload BLOB NOT NULL, state INTEGER NOT NULL, attempts INTEGER NOT NULL,
  status_code INTEGER NOT NULL, error TEXT NOT NULL,
  created_ns INTEGER NOT NULL, next_ns INTEGER NOT NULL, delivered_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS webhook_deliveries_due ON webhook_deliveries (state, next_ns);
CREATE INDEX IF NOT EXISTS webhook_deliveries_by_webhook ON webhook_deliveries (project, webhook, created_ns);
`

// PutWebhook creates or replaces a webhook.
func (s *Store) PutWebhook(ctx context.Context, w *evalsiv1alpha1.Webhook) error {
	body, err := proto.Marshal(w)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO webhooks (project, name, body, updated_ns) VALUES (?, ?, ?, ?)
		 ON CONFLICT (project, name) DO UPDATE SET body = excluded.body, updated_ns = excluded.updated_ns`,
		w.GetProject(), w.GetName(), body, w.GetUpdatedAt().AsTime().UnixNano())
	return err
}

// GetWebhook returns a webhook, secret included.
func (s *Store) GetWebhook(ctx context.Context, project, name string) (*evalsiv1alpha1.Webhook, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM webhooks WHERE project = ? AND name = ?`, project, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w := &evalsiv1alpha1.Webhook{}
	return w, proto.Unmarshal(body, w)
}

// ListWebhooks returns a project's webhooks by name ("" lists every project's),
// secrets included.
func (s *Store) ListWebhooks(ctx context.Context, project string) ([]*evalsiv1alpha1.Webhook, error) {
	q, args := `SELECT body FROM webhooks ORDER BY project, name`, []any{}
	if project != "" {
		q, args = `SELECT body FROM webhooks WHERE project = ? ORDER BY name`, []any{project}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.Webhook
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		w := &evalsiv1alpha1.Webhook{}
		if err := proto.Unmarshal(body, w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteWebhook removes a webhook and its deliveries.
func (s *Store) DeleteWebhook(ctx context.Context, project, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE project = ? AND name = ?`, project, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE project = ? AND webhook = ?`, project, name)
	return err
}

// Delivery states, as stored.
const (
	DeliveryPending   = 1
	DeliveryDelivered = 2
	DeliveryFailed    = 3
)

// Delivery is one event for one webhook.
type Delivery struct {
	ID, Project, Webhook, Event string
	// The run of a run event; the trace and policy of a trace or alert event.
	RunID, TraceID, Policy   string
	Payload                  []byte
	State                    int
	Attempts                 int
	StatusCode               int
	Error                    string
	Created, Next, Delivered time.Time
}

func ns(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNS(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// AddDelivery queues a delivery.
func (s *Store) AddDelivery(ctx context.Context, d *Delivery) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webhook_deliveries (id, project, webhook, event, run_id, trace_id, policy, payload, state, attempts, status_code, error, created_ns, next_ns, delivered_ns)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, '', ?, ?, 0)`,
		d.ID, d.Project, d.Webhook, d.Event, d.RunID, d.TraceID, d.Policy, d.Payload, DeliveryPending, ns(d.Created), ns(d.Next))
	return err
}

const deliveryColumns = `id, project, webhook, event, run_id, trace_id, policy, payload, state, attempts, status_code, error, created_ns, next_ns, delivered_ns`

func scanDeliveries(rows *sql.Rows) ([]*Delivery, error) {
	defer rows.Close()
	var out []*Delivery
	for rows.Next() {
		var d Delivery
		var created, next, delivered int64
		if err := rows.Scan(&d.ID, &d.Project, &d.Webhook, &d.Event, &d.RunID, &d.TraceID, &d.Policy, &d.Payload, &d.State, &d.Attempts,
			&d.StatusCode, &d.Error, &created, &next, &delivered); err != nil {
			return nil, err
		}
		d.Created, d.Next, d.Delivered = fromNS(created), fromNS(next), fromNS(delivered)
		out = append(out, &d)
	}
	return out, rows.Err()
}

// DueDeliveries returns pending deliveries whose next attempt is due, oldest first.
func (s *Store) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]*Delivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deliveryColumns+` FROM webhook_deliveries WHERE state = ? AND next_ns <= ? ORDER BY next_ns LIMIT ?`,
		DeliveryPending, ns(now), limit)
	if err != nil {
		return nil, err
	}
	return scanDeliveries(rows)
}

// RecentDeliveries returns a webhook's latest deliveries, newest first.
func (s *Store) RecentDeliveries(ctx context.Context, project, webhook string, limit int) ([]*Delivery, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deliveryColumns+` FROM webhook_deliveries WHERE project = ? AND webhook = ? ORDER BY created_ns DESC, id LIMIT ?`,
		project, webhook, limit)
	if err != nil {
		return nil, err
	}
	return scanDeliveries(rows)
}

// RecordAttempt saves the outcome of one attempt: the new state, and when to
// try again for a pending one.
func (s *Store) RecordAttempt(ctx context.Context, d *Delivery) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET state = ?, attempts = ?, status_code = ?, error = ?, next_ns = ?, delivered_ns = ? WHERE id = ?`,
		d.State, d.Attempts, d.StatusCode, d.Error, ns(d.Next), ns(d.Delivered), d.ID)
	return err
}

// PruneDeliveries deletes finished deliveries created before cutoff.
func (s *Store) PruneDeliveries(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE state <> ? AND created_ns < ?`, DeliveryPending, ns(cutoff))
	return err
}
