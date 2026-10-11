package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// open returns an empty store: SQLite, or PostgreSQL in a schema of its own
// when EVALSI_TEST_POSTGRES_DSN is set (CI runs the package both ways).
func open(t *testing.T) *Store {
	t.Helper()
	if dsn := os.Getenv("EVALSI_TEST_POSTGRES_DSN"); dsn != "" {
		return openPostgres(t, dsn)
	}
	s, err := Open(filepath.Join(t.TempDir(), "sub", "evalsi.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openPostgres(t *testing.T, dsn string) *Store {
	t.Helper()
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := OpenPostgres(ctx, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Dialect() != "postgres" {
		t.Fatal(s.Dialect())
	}
	return s
}

func run(id, project string, nanos int64) *evalsiv1alpha1.Run {
	return &evalsiv1alpha1.Run{Id: id, Project: project, CreatedAt: timestamppb.New(time.Unix(0, nanos)), Status: evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING}
}

func TestRunsLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	records := []*evalsiv1alpha1.Record{{Id: "a"}, {Id: "b"}}
	for i, id := range []string{"r1", "r2", "r3"} {
		r := run(id, map[bool]string{true: "p", false: "q"}[i < 2], int64(i))
		if err := s.CreateRun(ctx, r, records); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetRun(ctx, "r1")
	if err != nil || got.GetProject() != "p" {
		t.Fatalf("GetRun = %v, %v", got, err)
	}
	if _, err := s.GetRun(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	got.Status = evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING
	if err := s.UpdateRun(ctx, got); err != nil {
		t.Fatal(err)
	}
	running, _ := s.RunsWithStatus(ctx, evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING)
	if len(running) != 1 || running[0].GetId() != "r1" {
		t.Fatalf("RunsWithStatus = %v", running)
	}
	page, next, err := s.ListRuns(ctx, []string{"p"}, 1, "")
	if err != nil || len(page) != 1 || next == "" {
		t.Fatalf("ListRuns page 1 = %v %q %v", page, next, err)
	}
	page2, next2, _ := s.ListRuns(ctx, []string{"p"}, 1, next)
	if len(page2) != 1 || next2 != "" || page2[0].GetId() == page[0].GetId() {
		t.Fatalf("ListRuns page 2 = %v %q", page2, next2)
	}
	all, _, _ := s.ListRuns(ctx, nil, 10, "")
	if len(all) != 3 {
		t.Fatalf("ListRuns all = %d", len(all))
	}
	snap, _ := s.Records(ctx, "r1")
	if len(snap) != 2 || snap[1].GetId() != "b" {
		t.Fatalf("Records = %v", snap)
	}
}

func TestDeleteRunTerminalStateAndLease(t *testing.T) {
	for _, phase := range []evalsiv1alpha1.RunStatus{evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING, evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING, evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED, evalsiv1alpha1.RunStatus_RUN_STATUS_FAILED, evalsiv1alpha1.RunStatus_RUN_STATUS_ERROR, evalsiv1alpha1.RunStatus_RUN_STATUS_CANCELLED} {
		t.Run(phase.String(), func(t *testing.T) {
			st := open(t)
			run := run("delete", "project", 0)
			run.Status = phase
			if err := st.CreateRun(t.Context(), run, []*evalsiv1alpha1.Record{{Id: "record"}}); err != nil {
				t.Fatal(err)
			}
			if phase == evalsiv1alpha1.RunStatus_RUN_STATUS_PENDING || phase == evalsiv1alpha1.RunStatus_RUN_STATUS_RUNNING {
				if !errors.Is(st.DeleteRun(t.Context(), run.Id), ErrRunActive) {
					t.Fatal("active run was deletable")
				}
				return
			}
			if acquired, err := st.AcquireLease(t.Context(), "run:"+run.Id, "worker", time.Minute); err != nil || !acquired {
				t.Fatalf("lease: %v", err)
			}
			if !errors.Is(st.DeleteRun(t.Context(), run.Id), ErrRunActive) {
				t.Fatal("leased run was deletable")
			}
			if err := st.ReleaseLease(t.Context(), "run:"+run.Id, "worker"); err != nil {
				t.Fatal(err)
			}
			if err := st.AddDelivery(t.Context(), &Delivery{ID: "pending", Project: "project", Webhook: "hook", RunID: run.Id, Payload: []byte("owned payload"), Created: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteRun(t.Context(), run.Id); err != nil {
				t.Fatal(err)
			}
			if _, err := st.GetRun(t.Context(), run.Id); !errors.Is(err, ErrNotFound) {
				t.Fatal("deleted run still exists")
			}
			if records, err := st.Records(t.Context(), run.Id); err != nil || len(records) != 0 {
				t.Fatal("owned records survived deletion")
			}
			if count, err := st.CountRuns(t.Context(), "project"); err != nil || count != 0 {
				t.Fatal("deletion did not recover stored-run quota")
			}
			if deliveries, err := st.RecentDeliveries(t.Context(), "project", "hook", 10); err != nil || len(deliveries) != 0 {
				t.Fatal("owned webhook payload survived deletion")
			}
		})
	}
}

func TestIdempotencyClaimsConflictExpiryAndUncertainOutcome(t *testing.T) {
	st := open(t)
	now := time.Now()
	first, err := st.BeginIdempotency(t.Context(), "scope", "key", "fingerprint", now)
	if err != nil || !first.Claimed {
		t.Fatalf("claim: %+v, %v", first, err)
	}
	retry, err := st.BeginIdempotency(t.Context(), "scope", "key", "fingerprint", now.Add(48*time.Hour))
	if err != nil || retry.Claimed || retry.Completed {
		t.Fatal("unfinished claim was re-executed after expiry")
	}
	if _, err := st.BeginIdempotency(t.Context(), "scope", "key", "different", now); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatal("different request reused a key")
	}
	if err := st.CompleteIdempotency(t.Context(), "scope", "key", "fingerprint", "response", []byte("cached"), 0, "", now); err != nil {
		t.Fatal(err)
	}
	retry, err = st.BeginIdempotency(t.Context(), "scope", "key", "fingerprint", now.Add(time.Hour))
	if err != nil || retry.Claimed || !retry.Completed || string(retry.Response) != "cached" {
		t.Fatal("completed response was not replayable")
	}
	expired, err := st.BeginIdempotency(t.Context(), "scope", "key", "different", now.Add(24*time.Hour))
	if err != nil || !expired.Claimed {
		t.Fatal("completed key did not expire at its TTL")
	}
}

func TestIdempotencyReleaseAndUnresolvedClaimReaper(t *testing.T) {
	st := open(t)
	now := time.Now()
	if first, err := st.BeginIdempotency(t.Context(), "scope", "failed", "fingerprint", now); err != nil || !first.Claimed {
		t.Fatalf("claim: %+v, %v", first, err)
	}
	if err := st.ReleaseIdempotency(t.Context(), "scope", "failed", "fingerprint"); err != nil {
		t.Fatal(err)
	}
	if again, err := st.BeginIdempotency(t.Context(), "scope", "failed", "fingerprint", now); err != nil || !again.Claimed {
		t.Fatalf("a released key was not claimable: %+v, %v", again, err)
	}
	if err := st.CompleteIdempotency(t.Context(), "scope", "failed", "fingerprint", "response", []byte("done"), 0, "", now); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseIdempotency(t.Context(), "scope", "failed", "fingerprint"); err != nil {
		t.Fatal(err)
	}
	if replay, err := st.BeginIdempotency(t.Context(), "scope", "failed", "fingerprint", now.Add(time.Hour)); err != nil || !replay.Completed {
		t.Fatalf("release removed a completed outcome: %+v, %v", replay, err)
	}

	if first, err := st.BeginIdempotency(t.Context(), "scope", "crashed", "fingerprint", now); err != nil || !first.Claimed {
		t.Fatalf("claim: %+v, %v", first, err)
	}
	day := 24 * time.Hour
	if held, err := st.BeginIdempotency(t.Context(), "scope", "crashed", "fingerprint", now.Add(2*day)); err != nil || held.Claimed {
		t.Fatalf("an unresolved claim was re-executed too early: %+v, %v", held, err)
	}
	if reaped, err := st.BeginIdempotency(t.Context(), "scope", "crashed", "fingerprint", now.Add(day+UnresolvedClaimRetention+time.Minute)); err != nil || !reaped.Claimed {
		t.Fatalf("an unresolved claim was never reaped: %+v, %v", reaped, err)
	}
}

func TestIdempotencyCapacityPreservesExistingClaims(t *testing.T) {
	st := open(t)
	now := time.Now()
	for index := range 1024 {
		if _, err := st.db.ExecContext(t.Context(), `INSERT INTO idempotency (scope, key, fingerprint, response, expires_ns) VALUES (?, ?, ?, ?, ?)`, "scope", fmt.Sprint(index), "fingerprint", []byte{}, now.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.BeginIdempotency(t.Context(), "scope", "new", "fingerprint", now); !errors.Is(err, ErrIdempotencyCapacity) {
		t.Fatalf("new claim at capacity: %v", err)
	}
	if retry, err := st.BeginIdempotency(t.Context(), "scope", "0", "fingerprint", now); err != nil || retry.Claimed {
		t.Fatalf("existing claim at capacity: %+v, %v", retry, err)
	}
	if _, err := st.db.ExecContext(t.Context(), `UPDATE idempotency SET completed = 1, response = ? WHERE scope = ? AND key = ?`, make([]byte, 64<<20), "scope", "0"); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteIdempotency(t.Context(), "scope", "1", "fingerprint", "response", []byte("overflow"), 0, "", now); !errors.Is(err, ErrIdempotencyCapacity) {
		t.Fatalf("response at capacity: %v", err)
	}
	if retry, err := st.BeginIdempotency(t.Context(), "scope", "1", "fingerprint", now); err != nil || retry.Completed {
		t.Fatalf("failed completion became replayable: %+v, %v", retry, err)
	}
}

func TestIdempotencyConcurrentCapacity(t *testing.T) {
	for _, limit := range []string{"entries", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			st := open(t)
			now := time.Now()
			if limit == "entries" {
				for index := range 1023 {
					if _, err := st.db.ExecContext(t.Context(), `INSERT INTO idempotency (scope, key, fingerprint, response, expires_ns) VALUES (?, ?, ?, ?, ?)`, "scope", fmt.Sprint(index), "fingerprint", []byte{}, now.Add(24*time.Hour).UnixNano()); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				for _, key := range []string{"first", "second"} {
					if _, err := st.BeginIdempotency(t.Context(), "scope", key, "fingerprint", now); err != nil {
						t.Fatal(err)
					}
				}
			}
			start := make(chan struct{})
			results := make(chan error, 2)
			for _, key := range []string{"first", "second"} {
				go func() {
					<-start
					if limit == "entries" {
						_, err := st.BeginIdempotency(t.Context(), "scope", key, "fingerprint", now)
						results <- err
					} else {
						results <- st.CompleteIdempotency(t.Context(), "scope", key, "fingerprint", "response", make([]byte, (32<<20)+1), 0, "", now)
					}
				}()
			}
			close(start)
			accepted, rejected := 0, 0
			for range 2 {
				err := <-results
				switch {
				case err == nil:
					accepted++
				case errors.Is(err, ErrIdempotencyCapacity):
					rejected++
				default:
					t.Fatalf("concurrent capacity: %v", err)
				}
			}
			if accepted != 1 || rejected != 1 {
				t.Fatalf("accepted %d, rejected %d", accepted, rejected)
			}
		})
	}
}

func TestIdempotencyScopeIsolation(t *testing.T) {
	st := open(t)
	now := time.Now()
	if _, err := st.BeginIdempotency(t.Context(), "unrelated", "expired", "fingerprint", now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginIdempotency(t.Context(), "active", "key", "fingerprint", now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM idempotency WHERE scope = ?`, "unrelated").Scan(&count); err != nil || count != 1 {
		t.Fatalf("another scope was reaped without its lock: count=%d err=%v", count, err)
	}
	if st.db.d == postgresDialect {
		tx, err := st.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended(?, 7266657473690002))`, "blocked"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := st.BeginIdempotency(ctx, "independent", "key", "fingerprint", now); err != nil {
			t.Fatalf("unrelated scope blocked: %v", err)
		}
		blocked, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer stop()
		if _, err := st.BeginIdempotency(blocked, "blocked", "key", "fingerprint", now); err == nil {
			t.Fatal("same scope bypassed its advisory lock")
		}
	}
}

func TestOutputsAndResultsAreIdempotent(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.CreateRun(ctx, run("r", "", 0), nil); err != nil {
		t.Fatal(err)
	}
	out := []Output{{RecordIdx: 0, Trial: 1, Record: &evalsiv1alpha1.Record{Id: "a"}}, {RecordIdx: 1, Trial: 1, Error: "boom"}}
	if err := s.PutOutputs(ctx, "r", out); err != nil {
		t.Fatal(err)
	}
	if err := s.PutOutputs(ctx, "r", out[:1]); err != nil {
		t.Fatal(err)
	}
	outputs, _ := s.Outputs(ctx, "r")
	if len(outputs) != 2 || outputs[[2]int{1, 1}].Error != "boom" || outputs[[2]int{0, 1}].Record.GetId() != "a" {
		t.Fatalf("Outputs = %v", outputs)
	}
	res := func(rec, trial, ev int, name string) Result {
		return Result{RecordIdx: rec, Trial: trial, EvalIdx: ev, Result: &evalsiv1alpha1.EvaluationResult{Evaluator: name}}
	}
	batch := []Result{res(1, 0, 0, "em"), res(0, 0, 1, "j"), res(0, 0, 0, "em"), res(DatasetIndex, 0, 2, "count"), res(0, 1, 0, "em")}
	if err := s.PutResults(ctx, "r", batch); err != nil {
		t.Fatal(err)
	}
	if err := s.PutResults(ctx, "r", batch[:2]); err != nil {
		t.Fatal(err)
	}
	keys, _ := s.ResultKeys(ctx, "r")
	if len(keys) != 5 || !keys[Key{DatasetIndex, 0, 2}] {
		t.Fatalf("ResultKeys = %v", keys)
	}
	ordered, _ := s.Results(ctx, "r", "", 0, 0)
	var order []string
	for _, r := range ordered {
		order = append(order, r.Result.GetEvaluator())
	}
	if want := "em j em count em"; joined(order) != want {
		t.Fatalf("order = %q, want %q", joined(order), want)
	}
	page, _ := s.Results(ctx, "r", "em", 2, 1)
	if len(page) != 2 || page[0].RecordIdx != 1 || page[1].Trial != 1 {
		t.Fatalf("filtered page = %+v", page)
	}
}

func joined(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}

// TestTenantColumns: every project-scoped table carries the reserved
// tenant_id, in new databases and in ones written before it existed.
func TestTenantColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schema + authSchema + leaseSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO policies (name, project, policy) VALUES ('p', 'x', X'')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	for _, open := range []string{path, filepath.Join(t.TempDir(), "new.db")} {
		s, err := Open(open)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range tenantTables {
			var n int
			if err := s.db.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'tenant_id'`, table).Scan(&n); err != nil || n != 1 {
				t.Errorf("%s: %s has no tenant_id (%v)", open, table, err)
			}
		}
		s.Close()
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var tenant string
	if err := s.db.db.QueryRow(`SELECT tenant_id FROM policies WHERE name = 'p'`).Scan(&tenant); err != nil || tenant != "" {
		t.Errorf("existing rows: tenant %q, %v", tenant, err)
	}
}

// Deliveries queued before trace_id and policy existed still list and send.
func TestDeliveryColumnsAdded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schema + authSchema + leaseSchema + webhookSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO webhook_deliveries (id, project, webhook, event, run_id, payload, state, attempts, status_code, error, created_ns, next_ns, delivered_ns)
		VALUES ('whd_1', 'default', 'ci', 'run.finished', 'run-1', X'7B7D', 1, 0, 0, '', 1, 1, 0)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	due, err := s.DueDeliveries(context.Background(), time.Unix(0, 2), 10)
	if err != nil || len(due) != 1 || due[0].RunID != "run-1" || due[0].TraceID != "" || due[0].Policy != "" {
		t.Fatalf("old delivery = %+v, %v", due, err)
	}
	d := &Delivery{ID: "whd_2", Project: "default", Webhook: "studio", Event: "trace.scored", TraceID: "0af7", Policy: "prod", Payload: []byte(`{}`), Created: time.Unix(0, 3), Next: time.Unix(0, 3)}
	if err := s.AddDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecentDeliveries(context.Background(), "default", "studio", 10)
	if err != nil || len(got) != 1 || got[0].TraceID != "0af7" || got[0].Policy != "prod" {
		t.Errorf("new delivery = %+v, %v", got, err)
	}
}

func TestPruneIdempotencySweepsScopesThatWentIdle(t *testing.T) {
	st := open(t)
	now := time.Now()
	count := func() int {
		var n int
		if err := st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM idempotency`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Two scopes that are never used again: one finished request, one claim
	// that never completed. A third finished later and is still valid.
	for _, scope := range []string{"idle-done", "idle-crashed"} {
		if first, err := st.BeginIdempotency(t.Context(), scope, "key", "fingerprint", now); err != nil || !first.Claimed {
			t.Fatalf("claim: %+v, %v", first, err)
		}
	}
	if err := st.CompleteIdempotency(t.Context(), "idle-done", "key", "fingerprint", "response", []byte("cached"), 0, "", now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(23 * time.Hour)
	if _, err := st.BeginIdempotency(t.Context(), "active", "key", "fingerprint", later); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteIdempotency(t.Context(), "active", "key", "fingerprint", "response", []byte("cached"), 0, "", later); err != nil {
		t.Fatal(err)
	}

	day := 24 * time.Hour
	if n, err := st.PruneIdempotency(t.Context(), now.Add(day-time.Minute)); err != nil || n != 0 || count() != 3 {
		t.Fatalf("pruned too early: n=%d rows=%d err=%v", n, count(), err)
	}
	// The finished request expires at 24h; the others are kept.
	if n, err := st.PruneIdempotency(t.Context(), now.Add(day+time.Minute)); err != nil || n != 1 || count() != 2 {
		t.Fatalf("an expired outcome in an idle scope was not swept: n=%d rows=%d err=%v", n, count(), err)
	}
	// The active scope's outcome expired by then too. The crashed claim waited
	// UnresolvedClaimRetention past its expiry, and goes now.
	if n, err := st.PruneIdempotency(t.Context(), now.Add(day+UnresolvedClaimRetention+time.Minute)); err != nil || n != 2 || count() != 0 {
		t.Fatalf("an unresolved claim in an idle scope was not swept: n=%d rows=%d err=%v", n, count(), err)
	}
}
