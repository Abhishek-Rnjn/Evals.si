package runs

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	"github.com/abhishek-rnjn/evals.si/internal/config"
	"github.com/abhishek-rnjn/evals.si/internal/store"
)

func (m *Manager) today() string { return m.opts.Now().UTC().Format("2006-01-02") }

// projectSlot is the project's concurrency limiter, or nil when unlimited.
func (m *Manager) projectSlot(project string) chan struct{} {
	limit := m.opts.Quotas.For(project).MaxConcurrentRuns
	if limit <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	slot, ok := m.projectSlots[project]
	if !ok || cap(slot) != limit {
		slot = make(chan struct{}, limit)
		m.projectSlots[project] = slot
	}
	return slot
}

func exhausted(format string, args ...any) error {
	return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(format, args...))
}

// checkQuota refuses runs new runs when the project would pass its
// stored-run limit or has used up a daily token quota. Every way of creating
// runs calls it, under m.admit with the inserts, so a replica cannot admit
// past the limit; replicas admitting at the same moment can still each take
// the last slot.
func (m *Manager) checkQuota(ctx context.Context, project string, runs int) error {
	lim := m.opts.Quotas.For(project)
	if lim.MaxStoredRuns > 0 {
		n, err := m.store.CountRuns(ctx, project)
		if err != nil {
			return err
		}
		if n+runs > lim.MaxStoredRuns {
			return exhausted("project %s has %d stored runs and its quota (max_stored_runs) is %d; this needs %d more: archive or delete finished runs, or ask an administrator to raise quotas.max_stored_runs", project, n, lim.MaxStoredRuns, runs)
		}
	}
	if !lim.DailyLimited() {
		return nil
	}
	used, err := m.store.UsageOn(ctx, project, m.today())
	if err != nil {
		return err
	}
	if err := overDaily(project, lim, used, true); err != nil {
		return connect.NewError(connect.CodeResourceExhausted, err)
	}
	return nil
}

func overDaily(project string, lim config.QuotaLimits, used store.Usage, atLimit bool) error {
	over := func(u, l int64) bool { return l > 0 && (u > l || (atLimit && u >= l)) }
	if over(used.JudgeTokens, lim.JudgeTokensPerDay) {
		return fmt.Errorf("project %s used %d judge tokens today, its quota (judge_tokens_per_day %d)", project, used.JudgeTokens, lim.JudgeTokensPerDay)
	}
	if over(used.TargetTokens, lim.TargetTokensPerDay) {
		return fmt.Errorf("project %s used %d target tokens today, its quota (target_tokens_per_day %d)", project, used.TargetTokens, lim.TargetTokensPerDay)
	}
	if over(used.SandboxMillis, lim.SandboxSecondsPerDay*1000) {
		return fmt.Errorf("project %s used %d seconds of sandboxed work today, its quota (sandbox_seconds_per_day %d)", project, used.SandboxMillis/1000, lim.SandboxSecondsPerDay)
	}
	return nil
}

// addSandbox counts sandboxed work done for this run (an agent task, a code
// evaluator), to be added to the project's day at the next checkQuota.
func (ex *execution) addSandbox(d time.Duration) {
	if d > 0 {
		ex.sandboxMillis.Add(d.Milliseconds())
	}
}

// checkQuota counts the run's new token use toward its project's day and
// stops the run once the project is over a daily quota.
func (ex *execution) checkQuota(ctx context.Context) error {
	project := ex.run.GetProject()
	lim := ex.m.opts.Quotas.For(project)
	if !lim.DailyLimited() {
		return nil
	}
	judge, target, sandbox := tokens(ex.run.GetJudgeUsage()), tokens(ex.run.GetTargetUsage()), ex.sandboxMillis.Load()
	add := store.Usage{JudgeTokens: judge - ex.countedJudge, TargetTokens: target - ex.countedTarget, SandboxMillis: sandbox - ex.countedSandbox}
	if add.JudgeTokens == 0 && add.TargetTokens == 0 && add.SandboxMillis == 0 {
		return nil
	}
	used, err := ex.m.store.AddUsage(ctx, project, ex.m.today(), add)
	if err != nil {
		return err
	}
	ex.countedJudge, ex.countedTarget, ex.countedSandbox = judge, target, sandbox
	if err := overDaily(project, lim, used, false); err != nil {
		return fmt.Errorf("quota exceeded: %w", err)
	}
	return nil
}
