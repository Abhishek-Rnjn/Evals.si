package authz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/auth"
)

// AuditStore persists audit events.
type AuditStore interface {
	AppendAudit(ctx context.Context, ev *evalsiv1alpha1.AuditEvent) (int64, error)
	DeleteAuditBefore(ctx context.Context, t time.Time) (int64, error)
}

// ErrAuditUnavailable fails a call in strict mode when its audit entry could
// not be written.
var ErrAuditUnavailable = errors.New("the audit log is unavailable, so the change was not made")

// Auditor appends mutating and denied calls to the audit log, and hands
// each event to an exporter (the OTel sink) when one is set.
type Auditor struct {
	store  AuditStore
	export func(*evalsiv1alpha1.AuditEvent)
	log    *slog.Logger
	now    func() time.Time
	// strict: an audit write that fails is returned by Append, so the change
	// it describes is not made (decision 0026).
	strict   bool
	failures atomic.Int64
	// Kinds whose allowed reads are recorded (audit.reads); "all" for every.
	reads map[string]bool
}

// ReadKinds are the resource kinds audit.reads accepts: those of the
// "<kind>.read" permissions, except reading your own identity.
func ReadKinds() []string {
	var out []string
	for _, p := range Permissions {
		if kind, ok := strings.CutSuffix(p.Name, ".read"); ok && kind != "self" {
			out = append(out, kind)
		}
	}
	return out
}

// SetReads chooses which reads are recorded (audit.reads).
func (a *Auditor) SetReads(kinds []string) {
	if a == nil {
		return
	}
	a.reads = map[string]bool{}
	for _, k := range kinds {
		a.reads[k] = true
	}
}

// AuditsRead reports whether an allowed call of this read permission is recorded.
func (a *Auditor) AuditsRead(action string) bool {
	if a == nil || len(a.reads) == 0 {
		return false
	}
	kind, ok := strings.CutSuffix(action, ".read")
	return ok && kind != "self" && (a.reads["all"] || a.reads[kind])
}

// NewAuditor builds an auditor. export may be nil.
func NewAuditor(st AuditStore, export func(*evalsiv1alpha1.AuditEvent), log *slog.Logger) *Auditor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Auditor{store: st, export: export, log: log, now: time.Now}
}

// SetStrict chooses strict mode (audit.mode: strict).
func (a *Auditor) SetStrict(strict bool) {
	if a != nil {
		a.strict = strict
	}
}

// Strict reports whether a failed audit write fails the call.
func (a *Auditor) Strict() bool { return a != nil && a.strict }

// Record stores an event whose outcome is already decided (a denial, or a
// detail entry written after a change). A failure is logged and counted, not
// returned.
func (a *Auditor) Record(ctx context.Context, ev *evalsiv1alpha1.AuditEvent) {
	_ = a.write(ctx, ev)
}

// Append stores an event written before the change it describes. In strict
// mode a failed write returns ErrAuditUnavailable and the caller must not
// make the change; in best-effort mode it is logged and counted like Record.
func (a *Auditor) Append(ctx context.Context, ev *evalsiv1alpha1.AuditEvent) error {
	if err := a.write(ctx, ev); err != nil && a.strict {
		return fmt.Errorf("%w: %v", ErrAuditUnavailable, err)
	}
	return nil
}

// WriteMetrics writes the count of failed audit writes.
func (a *Auditor) WriteMetrics(w io.Writer) {
	if a == nil {
		return
	}
	const name = "evalsi_audit_write_failures_total"
	fmt.Fprintf(w, "# HELP %s Audit entries that could not be written.\n# TYPE %s counter\n%s %d\n", name, name, name, a.failures.Load())
}

func (a *Auditor) write(ctx context.Context, ev *evalsiv1alpha1.AuditEvent) error {
	if a == nil {
		return nil
	}
	if ev.Time == nil {
		ev.Time = timestamppb.New(a.now())
	}
	withAuthContext(ctx, ev)
	// The request may be cancelled right after a denial; the record must still land.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	id, err := a.store.AppendAudit(ctx, ev)
	if err != nil {
		a.failures.Add(1)
		a.log.Error("writing audit event", "action", ev.GetAction(), "principal", ev.GetPrincipal(), "strict", a.strict, "err", err)
		return err
	}
	ev.Id = id
	if !ev.GetAllowed() {
		a.log.Warn("access denied", "principal", ev.GetPrincipal(), "action", ev.GetAction(),
			"project", ev.GetProject(), "resource", ev.GetResource(), "reason", ev.GetReason(), "source", ev.GetSource())
	}
	if a.export != nil {
		a.export(ev)
	}
	return nil
}

// Retain deletes events older than retention, hourly, until ctx is done.
func (a *Auditor) Retain(ctx context.Context, retention time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		cutoff := a.now().Add(-retention)
		if n, err := a.store.DeleteAuditBefore(ctx, cutoff); err != nil {
			a.log.Error("audit retention", "err", err)
		} else if n > 0 {
			a.log.Info("audit retention", "deleted", n)
			// The chain now starts later: say why, in the chain itself.
			a.Record(ctx, &evalsiv1alpha1.AuditEvent{
				Principal: "system:retention", Action: "audit.retention", Resource: "audit", Allowed: true,
				Reason: "audit retention", Detail: fmt.Sprintf(`{"deleted":%d,"before":%q}`, n, cutoff.UTC().Format(time.RFC3339)),
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// withAuthContext records how the event's principal authenticated, from the
// request's principal, when the event is about that principal (a system
// entry, such as retention, has none). Never a token, key or hash.
func withAuthContext(ctx context.Context, ev *evalsiv1alpha1.AuditEvent) {
	p := auth.PrincipalFrom(ctx)
	if p == nil || p.Anonymous() || p.ID() != ev.GetPrincipal() {
		return
	}
	ev.AuthMethod = p.Method
	ev.Groups = append([]string(nil), p.Groups...)
	// Delegation is not built: the subject and actor are the caller.
	ev.Subject, ev.Actor = p.ID(), p.ID()
	switch p.Method {
	case auth.MethodJWT:
		ev.Issuer, _ = p.Claims["iss"].(string)
		ev.TokenId, _ = p.Claims["jti"].(string)
		ev.KeyId = p.TokenKeyID
	case auth.MethodAPIKey:
		ev.ApiKey = p.KeyName
	case auth.MethodMTLS:
		ev.CertFingerprint = p.CertFingerprint
	}
}
