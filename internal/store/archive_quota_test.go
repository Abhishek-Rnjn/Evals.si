package store

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

func TestArchivedRunsReleaseQuotaAndMigrate(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	for _, id := range []string{"whole", "archived", "other"} {
		project := "p"
		if id == "other" {
			project = "q"
		}
		run := &evalsiv1alpha1.Run{Id: id, Project: project, CreatedAt: timestamppb.Now(), Status: evalsiv1alpha1.RunStatus_RUN_STATUS_SUCCEEDED}
		if err := st.CreateRun(ctx, run, nil); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.CountRuns(ctx, "p"); err != nil || n != 2 {
		t.Fatalf("before archive: %d, %v", n, err)
	}
	for range 2 {
		if _, err := st.ArchiveRun(ctx, "archived", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		for _, p := range []string{"p", "q"} {
			if n, err := st.CountRuns(ctx, p); err != nil || n != 1 {
				t.Fatalf("quota for %s: %d, %v", p, n, err)
			}
		}
		if run, err := st.GetRun(ctx, "archived"); err != nil || run.GetArchivedAt() == nil {
			t.Fatalf("archived summary lost: %v, %v", run, err)
		}
	}
	check()
	// Emulate an older database: the protobuf has archived_at but SQL has no marker.
	if _, err := st.db.ExecContext(ctx, `DROP INDEX runs_quota`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `ALTER TABLE runs DROP COLUMN archived`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		tx, err := st.db.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrateArchivedRuns(ctx, tx, st.db.d); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		check()
	}
}
