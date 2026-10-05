// Package store persists runs in SQLite (pure Go, no cgo).
//
// Runs, their dataset snapshot, target outputs and evaluation results are
// stored as protobuf blobs keyed by (run, record index, trial, evaluator
// index). Writes are idempotent upserts, which is what makes runs resumable:
// a resumed run asks which keys exist and skips them.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// ErrNotFound is returned for unknown run ids.
var ErrNotFound = errors.New("store: not found")

// DatasetIndex is the record index used for dataset-scope results.
const DatasetIndex = -1

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  project TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  run BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS runs_by_project ON runs (project, created_at DESC);
CREATE TABLE IF NOT EXISTS run_records (
  run_id TEXT NOT NULL, idx INTEGER NOT NULL, record BLOB NOT NULL,
  PRIMARY KEY (run_id, idx)
);
CREATE TABLE IF NOT EXISTS run_outputs (
  run_id TEXT NOT NULL, record_idx INTEGER NOT NULL, trial INTEGER NOT NULL,
  record BLOB, error TEXT NOT NULL,
  PRIMARY KEY (run_id, record_idx, trial)
);
CREATE TABLE IF NOT EXISTS run_results (
  run_id TEXT NOT NULL, record_idx INTEGER NOT NULL, trial INTEGER NOT NULL, eval_idx INTEGER NOT NULL,
  evaluator TEXT NOT NULL, result BLOB NOT NULL,
  PRIMARY KEY (run_id, record_idx, trial, eval_idx)
);
`

// Store is safe for concurrent use.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection keeps writes serialized and simple.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: creating schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func marshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("store: marshal: %v", err)) // only on programmer error
	}
	return b
}

// CreateRun stores a new run with its dataset snapshot.
func (s *Store) CreateRun(ctx context.Context, run *evalsiv1alpha1.Run, records []*evalsiv1alpha1.Record) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs (id, project, created_at, run) VALUES (?, ?, ?, ?)`,
		run.GetId(), run.GetProject(), run.GetCreatedAt().AsTime().UnixNano(), marshal(run)); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO run_records (run_id, idx, record) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, r := range records {
		if _, err := stmt.ExecContext(ctx, run.GetId(), i, marshal(r)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateRun replaces a run's state.
func (s *Store) UpdateRun(ctx context.Context, run *evalsiv1alpha1.Run) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET run = ? WHERE id = ?`, marshal(run), run.GetId())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func unmarshalRun(blob []byte) (*evalsiv1alpha1.Run, error) {
	run := &evalsiv1alpha1.Run{}
	return run, proto.Unmarshal(blob, run)
}

// GetRun returns a run by id.
func (s *Store) GetRun(ctx context.Context, id string) (*evalsiv1alpha1.Run, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT run FROM runs WHERE id = ?`, id).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return unmarshalRun(blob)
}

// ListRuns returns runs newest first, all projects when project is empty.
// pageToken is an opaque offset from a previous call.
func (s *Store) ListRuns(ctx context.Context, project string, pageSize int, pageToken string) ([]*evalsiv1alpha1.Run, string, error) {
	offset := 0
	if pageToken != "" {
		var err error
		if offset, err = strconv.Atoi(pageToken); err != nil || offset < 0 {
			return nil, "", fmt.Errorf("store: bad page token")
		}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run FROM runs WHERE (? = '' OR project = ?) ORDER BY created_at DESC, id LIMIT ? OFFSET ?`,
		project, project, pageSize+1, offset)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var runs []*evalsiv1alpha1.Run
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, "", err
		}
		run, err := unmarshalRun(blob)
		if err != nil {
			return nil, "", err
		}
		runs = append(runs, run)
	}
	next := ""
	if len(runs) > pageSize {
		runs = runs[:pageSize]
		next = strconv.Itoa(offset + pageSize)
	}
	return runs, next, rows.Err()
}

// RunsWithStatus lists every run in one of the given states (used at startup).
func (s *Store) RunsWithStatus(ctx context.Context, statuses ...evalsiv1alpha1.RunStatus) ([]*evalsiv1alpha1.Run, error) {
	want := map[evalsiv1alpha1.RunStatus]bool{}
	for _, st := range statuses {
		want[st] = true
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run FROM runs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.Run
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		run, err := unmarshalRun(blob)
		if err != nil {
			return nil, err
		}
		if want[run.GetStatus()] {
			out = append(out, run)
		}
	}
	return out, rows.Err()
}

// Records returns a run's dataset snapshot in order.
func (s *Store) Records(ctx context.Context, runID string) ([]*evalsiv1alpha1.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM run_records WHERE run_id = ? ORDER BY idx`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*evalsiv1alpha1.Record
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		r := &evalsiv1alpha1.Record{}
		if err := proto.Unmarshal(blob, r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Output is what the target produced for one record in one trial.
type Output struct {
	RecordIdx int
	Trial     int
	// Record with the target's output and usage; nil when generation failed.
	Record *evalsiv1alpha1.Record
	Error  string
}

// PutOutputs upserts target outputs.
func (s *Store) PutOutputs(ctx context.Context, runID string, outputs []Output) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, o := range outputs {
		var blob []byte
		if o.Record != nil {
			blob = marshal(o.Record)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO run_outputs (run_id, record_idx, trial, record, error) VALUES (?, ?, ?, ?, ?)`,
			runID, o.RecordIdx, o.Trial, blob, o.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Outputs returns every stored output of a run, keyed by [record index, trial].
func (s *Store) Outputs(ctx context.Context, runID string) (map[[2]int]Output, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record_idx, trial, record, error FROM run_outputs WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[[2]int]Output{}
	for rows.Next() {
		var o Output
		var blob []byte
		if err := rows.Scan(&o.RecordIdx, &o.Trial, &blob, &o.Error); err != nil {
			return nil, err
		}
		if blob != nil {
			o.Record = &evalsiv1alpha1.Record{}
			if err := proto.Unmarshal(blob, o.Record); err != nil {
				return nil, err
			}
		}
		out[[2]int{o.RecordIdx, o.Trial}] = o
	}
	return out, rows.Err()
}

// Result is one stored evaluation result with its key.
type Result struct {
	RecordIdx int
	Trial     int
	EvalIdx   int
	Result    *evalsiv1alpha1.EvaluationResult
}

// Key identifies a result.
type Key struct{ RecordIdx, Trial, EvalIdx int }

// PutResults upserts results.
func (s *Store) PutResults(ctx context.Context, runID string, results []Result) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range results {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO run_results (run_id, record_idx, trial, eval_idx, evaluator, result) VALUES (?, ?, ?, ?, ?, ?)`,
			runID, r.RecordIdx, r.Trial, r.EvalIdx, r.Result.GetEvaluator(), marshal(r.Result)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ResultKeys returns the keys of every stored result of a run.
func (s *Store) ResultKeys(ctx context.Context, runID string) (map[Key]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record_idx, trial, eval_idx FROM run_results WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Key]bool{}
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.RecordIdx, &k.Trial, &k.EvalIdx); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// Results returns a run's results ordered by trial, record, evaluator, with
// dataset-scope results last in each trial. evaluator filters when non-empty.
func (s *Store) Results(ctx context.Context, runID, evaluator string, limit, offset int) ([]Result, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT record_idx, trial, eval_idx, result FROM run_results
		WHERE run_id = ? AND (? = '' OR evaluator = ?)
		ORDER BY trial, CASE WHEN record_idx < 0 THEN 1 ELSE 0 END, record_idx, eval_idx
		LIMIT ? OFFSET ?`, runID, evaluator, evaluator, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Result
	for rows.Next() {
		var r Result
		var blob []byte
		if err := rows.Scan(&r.RecordIdx, &r.Trial, &r.EvalIdx, &blob); err != nil {
			return nil, err
		}
		r.Result = &evalsiv1alpha1.EvaluationResult{}
		if err := proto.Unmarshal(blob, r.Result); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
