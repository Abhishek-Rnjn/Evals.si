package server

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"github.com/abhishek-rnjn/evals.si/internal/authz"
	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/credentials"
	"github.com/abhishek-rnjn/evals.si/internal/watch"
)

// auditDenial writes a refused credential or judge reference to the audit
// log, as the caller whose request named it (or "system" for a stored
// policy or guardrail checked again when loaded).
func auditDenial(ctx context.Context, auditor *authz.Auditor, d credentials.Denial) {
	ev := &evalsiv1alpha1.AuditEvent{
		Principal: "system", Action: "credentials.use", Project: d.Project,
		Resource: d.Subject, Allowed: false, Reason: d.Reason,
	}
	if chk := authz.CheckerFrom(ctx); chk != nil {
		ev.Principal = chk.Principal.ID()
		ev.Procedure, ev.Source = chk.Meta.Procedure, chk.Meta.Source
		ev.RequestId = chk.Meta.Headers.Get("X-Request-Id")
	}
	auditor.Record(ctx, ev)
}

// reloadInterval is how often the config file is read again for changed
// credential grants and judge scopes; SIGHUP reads it at once.
var reloadInterval = 15 * time.Second

// reloadCredentials applies changed credential grants and judge projects
// from the config file without a restart: requests are checked against them
// at once, stored online policies are checked again (and dropped while they
// name what is no longer granted), and guardrails recompile on their next
// use. Other settings need a restart.
func reloadCredentials(ctx context.Context, cfg config.Config, creds *credentials.Policy, watcher *watch.Engine, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	tick := time.NewTicker(reloadInterval)
	defer tick.Stop()
	current := cfg.CredentialSettings()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		case <-tick.C:
		}
		next, err := cfg.Reload()
		if err != nil {
			log.Error("reloading the config: keeping the current credential grants", "err", err)
			continue
		}
		settings := next.CredentialSettings()
		if reflect.DeepEqual(settings, current) {
			continue
		}
		current = settings
		creds.Update(settings)
		log.Info("credential grants and judge projects reloaded", "grants", len(settings.Credentials.Grants), "enforced", creds.Enforced())
		if err := watcher.Recheck(ctx); err != nil {
			log.Error("re-checking online policies against the new grants", "err", err)
		}
	}
}
