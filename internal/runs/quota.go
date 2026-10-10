package runs

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

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
			// There is no API to delete runs yet (docs/LEFTOVERS.md), so the
			// way out is a larger quota.
			return exhausted("project %s has %d stored runs and its quota (max_stored_runs) is %d; this needs %d more: ask an administrator to raise quotas.max_stored_runs", project, n, lim.MaxStoredRuns, runs)
		}
	}
	if lim.JudgeTokensPerDay == 0 && lim.TargetTokensPerDay == 0 {
		return nil
	}
	used, err := m.store.UsageOn(ctx, project, m.today())
	if err != nil {
		return err
	}
	if err := overDaily(project, lim.JudgeTokensPerDay, lim.TargetTokensPerDay, used, true); err != nil {
		return connect.NewError(connect.CodeResourceExhausted, err)
	}
	return nil
}

func overDaily(project string, judge, target int64, used store.Usage, atLimit bool) error {
	over := func(u, l int64) bool { return l > 0 && (u > l || (atLimit && u >= l)) }
	if over(used.JudgeTokens, judge) {
		return fmt.Errorf("project %s used %d judge tokens today, its quota (judge_tokens_per_day %d)", project, used.JudgeTokens, judge)
	}
	if over(used.TargetTokens, target) {
		return fmt.Errorf("project %s used %d target tokens today, its quota (target_tokens_per_day %d)", project, used.TargetTokens, target)
	}
	return nil
}

// checkQuota counts the run's new token use toward its project's day and
// stops the run once the project is over a daily quota.
func (ex *execution) checkQuota(ctx context.Context) error {
	project := ex.run.GetProject()
	lim := ex.m.opts.Quotas.For(project)
	if lim.JudgeTokensPerDay == 0 && lim.TargetTokensPerDay == 0 {
		return nil
	}
	judge, target := tokens(ex.run.GetJudgeUsage()), tokens(ex.run.GetTargetUsage())
	add := store.Usage{JudgeTokens: judge - ex.countedJudge, TargetTokens: target - ex.countedTarget}
	if add.JudgeTokens == 0 && add.TargetTokens == 0 {
		return nil
	}
	used, err := ex.m.store.AddUsage(ctx, project, ex.m.today(), add)
	if err != nil {
		return err
	}
	ex.countedJudge, ex.countedTarget = judge, target
	if err := overDaily(project, lim.JudgeTokensPerDay, lim.TargetTokensPerDay, used, false); err != nil {
		return fmt.Errorf("quota exceeded: %w", err)
	}
	return nil
}
