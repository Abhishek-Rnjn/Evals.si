package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/abhishek-rnjn/evals.si/internal/authz"
)

func versions(t *testing.T, s *Store) []AppliedMigration {
	t.Helper()
	got, err := s.Migrations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func reopen(t *testing.T, path string) (*Store, error) {
	t.Helper()
	s, err := Open(path)
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

// withSteps runs f with the store's migrations replaced, as a different
// release of evalsid would have them.
func withSteps(t *testing.T, steps []migration, f func()) {
	t.Helper()
	saved := migrations
	migrations = steps
	defer func() { migrations = saved }()
	f()
}

func TestMigrationsAreRecordedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evalsi.db")
	s, err := reopen(t, path)
	if err != nil {
		t.Fatal(err)
	}
	first := versions(t, s)
	if len(first) != len(migrations) || first[len(first)-1].Version != SchemaVersion() {
		t.Fatalf("applied %+v, want %d steps", first, len(migrations))
	}
	for _, m := range first {
		if m.MinReader != 0 {
			t.Errorf("step %d raised the minimum reader to %d; every released step is an expand step", m.Version, m.MinReader)
		}
	}
	_ = s.Close()
	s, err = reopen(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if again := versions(t, s); len(again) != len(first) || !again[0].AppliedAt.Equal(first[0].AppliedAt) {
		t.Fatalf("reopening re-recorded steps: %+v", again)
	}
}

// A database from before versioning has its tables but no schema_migrations:
// the steps are idempotent, so they are recorded without losing data.
func TestAnUnversionedDatabaseIsAdopted(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "evalsi.db")
	s, err := reopen(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, authz.Project{Name: "legacy"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err = reopen(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := versions(t, s); len(got) != len(migrations) {
		t.Fatalf("adopted %+v", got)
	}
	if projects, err := s.AuthProjects(ctx); err != nil || len(projects) != 1 || projects[0].Name != "legacy" {
		t.Fatalf("data lost while adopting: %v %v", projects, err)
	}
}

func nextStep(contract bool) migration {
	return migration{version: SchemaVersion() + 1, name: "next", contract: contract,
		apply: func(ctx context.Context, q querier, d dialect) error {
			_, err := q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS next_release (id INTEGER PRIMARY KEY)`)
			return err
		}}
}

// N-1 compatibility: after release N applies an expand step, release N-1
// still opens the database (a rolling upgrade, or a rollback without a
// restore). After a contract step it refuses, with a clear error.
func TestThePreviousReleaseReadsAnExpandedSchemaButNotAContractedOne(t *testing.T) {
	current := append([]migration(nil), migrations...)
	for _, tc := range []struct {
		name     string
		contract bool
	}{{"expand", false}, {"contract", true}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "evalsi.db")
			withSteps(t, append(append([]migration(nil), current...), nextStep(tc.contract)), func() {
				s, err := reopen(t, path)
				if err != nil {
					t.Fatal(err)
				}
				_ = s.Close()
			})
			withSteps(t, current, func() {
				_, err := reopen(t, path)
				switch {
				case tc.contract && !errors.Is(err, ErrSchemaTooNew):
					t.Fatalf("N-1 opened a contracted schema: %v", err)
				case !tc.contract && err != nil:
					t.Fatalf("N-1 refused an expanded schema: %v", err)
				}
			})
		})
	}
}

// Both backends record the same steps (CI runs this with PostgreSQL too).
func TestMigrationsAreRecordedOnEitherBackend(t *testing.T) {
	if got := versions(t, open(t)); len(got) != len(migrations) || got[len(got)-1].Version != SchemaVersion() {
		t.Fatalf("applied %+v", got)
	}
}
