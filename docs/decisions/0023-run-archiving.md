# 0023. Archiving keeps a run's summary and drops its data

- **Status:** Accepted, 2026-10-12

## Context

Retention ([decision 0020](0020-pipeline-retries-and-scheduled-runs.md)) deletes runs. Deleting loses the one thing worth keeping for a long time: what the run measured. A run's metric summaries, gates, usage and spec are small and live in the run record. Its records, outputs and results are large, and are what a run costs to keep.

## Decision

1. **`ArchiveRun` drops a finished run's `run_records`, `run_outputs`, `run_results` and webhook deliveries, in one transaction, and sets `Run.archived_at`.** The run, its labels and project stay. It has `DeleteRun`'s conditions (finished, not leased) and idempotency (archiving twice returns the run unchanged).
2. **It takes the `runs.delete` permission.** It removes what deleting removes, so no new permission or role change is needed. Archiving is audited as `runs.delete`, like a refused or allowed `DeleteRun`.
3. **Every reader says why it cannot proceed.** `ListRunResults`, `ResumeRun`, `CompareRuns`, `PromoteResults` and a dataset taken from a run answer `FailedPrecondition` (or `InvalidArgument` for a dataset) naming the archive. `SliceRuns` skips archived runs. `GetRun`, `ListRuns` and `DeleteRun` work as before.
4. **Automatic archiving is `runs.archive_after` and `runs.project_archive_after`** ("0" never), run by the retention janitor each hour, before deletion is considered. It must be shorter than the retention it is combined with, checked at startup. `evalsi.keep: "true"` exempts a run from both.
5. **Automatic actions are audited.** Each automatic delete or archive writes an event with principal `system:retention`, action `runs.delete` or `runs.archive`, `procedure: runs.Retain` and the cutoff in `detail`.

## Consequences

- An archived run does not count toward `max_stored_runs`. Archiving releases its quota while preserving the summary. An indexed SQL marker is updated in the archive transaction; startup backfills existing archives from their protobuf summaries.
- The janitor reads every run older than `archive_after` that has not been deleted, each hour, to find the ones not yet archived. With no retention the set only grows; the read is of run summaries, not results.
- Anything that reads results or compares an archived run fails with a clear message: `evalsi analyze` skips archived runs when listing and refuses one named with `--run`, `evalsi report --server` shows the summary only, and `evalsi compare`, the flywheel commands and a checkpoint regression gate against an archived base-model run answer `FailedPrecondition`. Label runs you compare against `evalsi.keep: "true"`, or set `archive_after` longer than they are needed.
- Not built: per-label retention rules (only the `evalsi.keep` exemption), and an "unarchive". Data already delivered to sinks and promoted datasets are untouched.
