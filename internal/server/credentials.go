package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// reloadConfig applies changed credential grants, judge projects and rate
// limits from the config file without a restart. For grants and judges:
// requests are checked against them at once, stored online policies are
// checked again (and dropped while they name what is no longer granted),
// and guardrails recompile on their next use. Rate limits: existing buckets
// take the new rates (see limiter.update). Other settings need a restart.
func reloadConfig(ctx context.Context, cfg config.Config, creds *credentials.Policy, watcher *watch.Engine, limits *limiter, auditor *authz.Auditor, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	tick := time.NewTicker(reloadInterval)
	defer tick.Stop()
	current := cfg.CredentialSettings()
	currentLimits := cfg.RateLimits
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		case <-tick.C:
		}
		next, err := cfg.Reload()
		if err != nil {
			log.Error("reloading the config: keeping the current credential grants and rate limits", "err", err)
			continue
		}
		if !reflect.DeepEqual(next.RateLimits, currentLimits) {
			reloadRateLimits(ctx, limits, currentLimits, next.RateLimits, auditor, log)
			currentLimits = next.RateLimits
		}
		settings := next.CredentialSettings()
		if reflect.DeepEqual(settings, current) {
			continue
		}
		auditConfig(ctx, auditor, "config.reload", map[string]any{
			"changed": changedSections(current, settings),
			"before":  current, "after": settings,
			"hash": configDigest(settings),
		})
		current = settings
		creds.Update(settings)
		log.Info("credential grants and judge projects reloaded", "grants", len(settings.Credentials.Grants), "enforced", creds.Enforced())
		if err := watcher.Recheck(ctx); err != nil {
			log.Error("re-checking online policies against the new grants", "err", err)
		}
	}
}

// reloadRateLimits applies changed rate limits and audits the change. What a
// reload cannot change is logged and kept: limits off at start (no limiter
// to update) and the shared settings.
func reloadRateLimits(ctx context.Context, limits *limiter, before, after config.RateLimits, auditor *authz.Auditor, log *slog.Logger) {
	if limits == nil {
		if !after.Disabled {
			log.Warn("rate_limits changed, but limits were off at start (or authentication is): restart to turn them on")
		}
		return
	}
	if before.Shared != after.Shared || before.SharedWindow != after.SharedWindow || before.SharedSync != after.SharedSync {
		log.Warn("rate_limits.shared settings changed: they apply after a restart; the rates apply now")
		after.Shared, after.SharedWindow, after.SharedSync = before.Shared, before.SharedWindow, before.SharedSync
	}
	auditConfig(ctx, auditor, "config.reload", map[string]any{
		"changed": []string{"rate_limits"}, "before": before, "after": after, "hash": configDigest(after),
	})
	limits.update(after)
	log.Info("rate limits reloaded", "disabled", after.Disabled, "principals", len(after.Principals))
}

// auditConfig records the configuration in effect (decision 0026): at
// startup a hash per security-relevant section, and on a reload what
// changed. Credential grants name variables and hosts, never values.
func auditConfig(ctx context.Context, auditor *authz.Auditor, action string, detail map[string]any) {
	raw, _ := json.Marshal(detail)
	auditor.Record(ctx, &evalsiv1alpha1.AuditEvent{
		Principal: "system:config", Action: action, Resource: "config", Allowed: true,
		Reason: "configuration in effect", Detail: string(raw),
	})
}

// configSections are the sections whose changes are audited, as hashes.
func configSections(cfg config.Config) map[string]string {
	return map[string]string{
		"credentials":   configDigest(cfg.Credentials),
		"judges":        configDigest(cfg.CredentialSettings().Judges),
		"rbac":          configDigest(cfg.RBAC),
		"authorization": configDigest(cfg.Authorization),
		"audit":         configDigest(cfg.Audit),
		"rate_limits":   configDigest(cfg.RateLimits),
	}
}

func configDigest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func changedSections(before, after credentials.Settings) []string {
	var out []string
	if !reflect.DeepEqual(before.Credentials, after.Credentials) {
		out = append(out, "credentials")
	}
	if !reflect.DeepEqual(before.Judges, after.Judges) {
		out = append(out, "judges")
	}
	return out
}
