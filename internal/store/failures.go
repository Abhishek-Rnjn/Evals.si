package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

const failureSchema = `
CREATE TABLE IF NOT EXISTS failure_cases (
  project TEXT NOT NULL, id TEXT NOT NULL, trace_id TEXT NOT NULL,
  source_sha256 TEXT NOT NULL, revision INTEGER NOT NULL,
  status INTEGER NOT NULL, assignee TEXT NOT NULL, body BLOB NOT NULL,
  PRIMARY KEY (project, id), UNIQUE (project, trace_id, source_sha256)
);
CREATE INDEX IF NOT EXISTS failures_by_status ON failure_cases (project, status, id);
CREATE INDEX IF NOT EXISTS failures_by_assignee ON failure_cases (project, assignee, id);
CREATE TABLE IF NOT EXISTS failure_events (
  project TEXT NOT NULL, id TEXT NOT NULL, revision INTEGER NOT NULL, body BLOB NOT NULL,
  PRIMARY KEY (project, id, revision)
);
`

var ErrFailureChanged = errors.New("failure revision changed; reload before retrying")

// CaptureFailure atomically deduplicates and records the initial event. A retry
// never overwrites human edits or reopens an approved/dismissed case.
func (s *Store) CaptureFailure(ctx context.Context, f *pb.FailureCase, event *pb.FailureEvent) (*pb.FailureCase, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	f.Id = newItemID()
	result, err := tx.ExecContext(ctx, `INSERT INTO failure_cases (project,id,trace_id,source_sha256,revision,status,assignee,body) VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (project,trace_id,source_sha256) DO NOTHING`, f.GetProject(), f.GetId(), f.GetSource().GetTraceId(), f.GetSourceSha256(), f.GetRevision(), int32(f.GetStatus()), f.GetAssignee(), marshal(f))
	if err != nil {
		return nil, false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	if n == 0 {
		var body []byte
		if err := tx.QueryRowContext(ctx, `SELECT body FROM failure_cases WHERE project=? AND trace_id=? AND source_sha256=?`, f.GetProject(), f.GetSource().GetTraceId(), f.GetSourceSha256()).Scan(&body); err != nil {
			return nil, false, err
		}
		f = &pb.FailureCase{}
		if err := proto.Unmarshal(body, f); err != nil {
			return nil, false, err
		}
	} else if err := appendFailureEvent(ctx, tx, f, event); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return f, n == 0, nil
}

func appendFailureEvent(ctx context.Context, q querier, f *pb.FailureCase, event *pb.FailureEvent) error {
	_, err := q.ExecContext(ctx, `INSERT INTO failure_events (project,id,revision,body) VALUES (?,?,?,?)`, f.GetProject(), f.GetId(), f.GetRevision(), marshal(event))
	return err
}

func (s *Store) GetFailure(ctx context.Context, project, id string) (*pb.FailureCase, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM failure_cases WHERE project=? AND id=?`, project, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f := &pb.FailureCase{}
	return f, proto.Unmarshal(body, f)
}

// SaveFailure makes a revision and its payload-free event one transaction.
func (s *Store) SaveFailure(ctx context.Context, f *pb.FailureCase, expected int64, event *pb.FailureEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE failure_cases SET revision=?,status=?,assignee=?,body=? WHERE project=? AND id=? AND revision=?`, f.GetRevision(), int32(f.GetStatus()), f.GetAssignee(), marshal(f), f.GetProject(), f.GetId(), expected)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrFailureChanged
	}
	if err := appendFailureEvent(ctx, tx, f, event); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteFailure(ctx context.Context, project, id string, expected int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `DELETE FROM failure_cases WHERE project=? AND id=? AND revision=?`, project, id, expected)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrFailureChanged
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM failure_events WHERE project=? AND id=?`, project, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListFailures uses a stable ID cursor. Filtering and authorization may make a
// returned page short; the caller must follow next even for an empty page.
func (s *Store) ListFailures(ctx context.Context, project string, status pb.FailureStatus, assignee string, size int, cursor string) ([]*pb.FailureCase, string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM failure_cases WHERE project=? AND (?=0 OR status=?) AND (?='' OR assignee=?) AND (?='' OR id<?) ORDER BY id DESC LIMIT ?`, project, int32(status), int32(status), assignee, assignee, cursor, cursor, size+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*pb.FailureCase
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, "", err
		}
		f := &pb.FailureCase{}
		if err := proto.Unmarshal(body, f); err != nil {
			return nil, "", err
		}
		f.Record = nil
		out = append(out, f)
	}
	next := ""
	if len(out) > size {
		out, next = out[:size], out[size-1].GetId()
	}
	return out, next, rows.Err()
}

func (s *Store) ListFailureEvents(ctx context.Context, project, id string, size int, before int64) ([]*pb.FailureEvent, string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM failure_events WHERE project=? AND id=? AND (?=0 OR revision<?) ORDER BY revision DESC LIMIT ?`, project, id, before, before, size+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*pb.FailureEvent
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, "", err
		}
		event := &pb.FailureEvent{}
		if err := proto.Unmarshal(body, event); err != nil {
			return nil, "", err
		}
		out = append(out, event)
	}
	next := ""
	if len(out) > size {
		out, next = out[:size], strconv.FormatInt(out[size-1].GetRevision(), 10)
	}
	return out, next, rows.Err()
}

// SelectedFailures reads the whole explicit selection in one SQL statement,
// so export never combines approval decisions from different database snapshots.
func (s *Store) SelectedFailures(ctx context.Context, project string, ids []string) ([]*pb.FailureCase, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := []any{project}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM failure_cases WHERE project=? AND id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.FailureCase
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		f := &pb.FailureCase{}
		if err := proto.Unmarshal(body, f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
