package authz

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// AuditStore persists audit events.
type AuditStore interface {
	AppendAudit(ctx context.Context, ev *evalsiv1alpha1.AuditEvent) (int64, error)
	DeleteAuditBefore(ctx context.Context, t time.Time) (int64, error)
}

// Auditor appends mutating and denied calls to the audit log, and hands
// each event to an exporter (the OTel sink) when one is set.
type Auditor struct {
	store  AuditStore
	export func(*evalsiv1alpha1.AuditEvent)
	log    *slog.Logger
	now    func() time.Time
}

// NewAuditor builds an auditor. export may be nil.
func NewAuditor(st AuditStore, export func(*evalsiv1alpha1.AuditEvent), log *slog.Logger) *Auditor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Auditor{store: st, export: export, log: log, now: time.Now}
}

// Record stores an event. Audit failures are logged, never returned: the
// call it describes has already been decided.
func (a *Auditor) Record(ctx context.Context, ev *evalsiv1alpha1.AuditEvent) {
	if a == nil {
		return
	}
	if ev.Time == nil {
		ev.Time = timestamppb.New(a.now())
	}
	// The request may be cancelled right after a denial; the record must still land.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	id, err := a.store.AppendAudit(ctx, ev)
	if err != nil {
		a.log.Error("writing audit event", "action", ev.GetAction(), "principal", ev.GetPrincipal(), "err", err)
		return
	}
	ev.Id = id
	if !ev.GetAllowed() {
		a.log.Warn("access denied", "principal", ev.GetPrincipal(), "action", ev.GetAction(),
			"project", ev.GetProject(), "resource", ev.GetResource(), "reason", ev.GetReason(), "source", ev.GetSource())
	}
	if a.export != nil {
		a.export(ev)
	}
}

// Retain deletes events older than retention, hourly, until ctx is done.
func (a *Auditor) Retain(ctx context.Context, retention time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := a.store.DeleteAuditBefore(ctx, a.now().Add(-retention)); err != nil {
			a.log.Error("audit retention", "err", err)
		} else if n > 0 {
			a.log.Info("audit retention", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
