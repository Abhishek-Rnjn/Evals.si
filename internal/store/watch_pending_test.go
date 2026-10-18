package store

import (
	"testing"
	"time"
)

func TestPendingAcknowledgmentCannotDeleteNewerTrace(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	old := WatchPending{Project: "p", TraceID: "same", Payload: []byte("old")}
	newer := WatchPending{Project: "p", TraceID: "same", Payload: []byte("new")}
	other := WatchPending{Project: "q", TraceID: "same", Payload: []byte("other project")}
	if err := st.PutWatchPending(ctx, []WatchPending{old, other}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutWatchPending(ctx, []WatchPending{newer}); err != nil {
		t.Fatal(err)
	}
	if err := st.AckWatchPending(ctx, "", old); err != nil {
		t.Fatal(err)
	}
	for _, want := range []WatchPending{newer, other} {
		if ok, err := st.WatchPendingMatches(ctx, want); err != nil || !ok {
			t.Fatalf("lost pending work: %v, %v", ok, err)
		}
	}
}

func TestTraceRetentionRemovesPendingCopies(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	now := time.Now()
	old := WatchPending{Project: "p", TraceID: "old", Payload: []byte("old"), StartNS: now.Add(-time.Hour).UnixNano()}
	newer := WatchPending{Project: "p", TraceID: "new", Payload: []byte("new"), StartNS: now.UnixNano()}
	if err := st.PutWatchPending(ctx, []WatchPending{old, newer}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteTracesBefore(ctx, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if exists, err := st.WatchPendingMatches(ctx, old); err != nil || exists {
		t.Fatalf("old copy retained: %v, %v", exists, err)
	}
	if exists, err := st.WatchPendingMatches(ctx, newer); err != nil || !exists {
		t.Fatalf("new copy removed: %v, %v", exists, err)
	}
}

func TestPendingStreamsPaginateAndAcknowledgeIndependently(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	for _, stream := range []string{"a", "b", "c"} {
		if err := st.PutWatchPending(ctx, []WatchPending{{Project: "p", TraceID: "same", Stream: stream, Payload: []byte(stream)}}); err != nil {
			t.Fatal(err)
		}
	}
	var cursor WatchPending
	for _, stream := range []string{"a", "b", "c"} {
		rows, err := st.ListWatchPending(ctx, 1, cursor)
		if err != nil || len(rows) != 1 || rows[0].Stream != stream {
			t.Fatalf("bad stream page: %v %v", rows, err)
		}
		cursor = rows[0]
		if err := st.AckWatchPending(ctx, "", cursor); err != nil {
			t.Fatal(err)
		}
	}
	if rows, err := st.ListWatchPending(ctx, 10); err != nil || len(rows) != 0 {
		t.Fatalf("pending work remains: %v %v", rows, err)
	}
}

func TestPendingMigrationPreservesLegacyWork(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`DROP TABLE watch_pending`,
		`CREATE TABLE watch_pending (project TEXT NOT NULL, trace_id TEXT NOT NULL, payload BLOB NOT NULL, start_ns INTEGER NOT NULL, PRIMARY KEY (project, trace_id))`,
		`INSERT INTO watch_pending VALUES ('p','legacy','old',123)`,
	} {
		if _, err := tx.ExecContext(ctx, st.db.d.ddl(statement)); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateWatchPending(ctx, tx, st.db.d); err != nil {
		t.Fatal(err)
	}
	if err := migrateWatchPending(ctx, tx, st.db.d); err != nil {
		t.Fatal(err)
	}
	var stream string
	var start int64
	if err := tx.QueryRowContext(ctx, `SELECT stream,start_ns FROM watch_pending WHERE trace_id='legacy'`).Scan(&stream, &start); err != nil || stream != "" || start != 123 {
		t.Fatalf("legacy work lost: %q %d %v", stream, start, err)
	}
}
