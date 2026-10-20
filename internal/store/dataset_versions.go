package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"

	"google.golang.org/protobuf/proto"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

const datasetCatalogSchema = `
CREATE TABLE IF NOT EXISTS dataset_versions (
  project TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL,
  state INTEGER NOT NULL, owner TEXT NOT NULL, revision INTEGER NOT NULL,
  body BLOB NOT NULL,
  PRIMARY KEY (project, name, version)
);
CREATE INDEX IF NOT EXISTS dataset_versions_by_state ON dataset_versions (project, state, name, version);
CREATE TABLE IF NOT EXISTS dataset_version_events (
  project TEXT NOT NULL, name TEXT NOT NULL, version TEXT NOT NULL,
  revision INTEGER NOT NULL, body BLOB NOT NULL,
  PRIMARY KEY (project, name, version, revision)
);
`

var ErrDatasetVersionChanged = errors.New("dataset version review changed; reload before retrying")
var ErrDatasetVersionIntegrity = errors.New("dataset version content does not match its stored digest")

// DatasetContentHash v1 covers every immutable field and ordered record. Review
// state and publication times are separate from the independently approved pin.
func DatasetContentHash(content *pb.DatasetVersionContent) (string, error) {
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("evalsi.dataset-catalog/v1\x00"), encoded...))
	return hex.EncodeToString(sum[:]), nil
}

func verifyDatasetVersion(dataset *pb.DatasetVersion) error {
	hash, err := DatasetContentHash(dataset.GetContent())
	if err != nil {
		return err
	}
	if hash != dataset.GetSha256() || int64(len(dataset.GetContent().GetRecords())) != dataset.GetRecords() {
		return ErrDatasetVersionIntegrity
	}
	return nil
}

func appendDatasetVersionEvent(ctx context.Context, q querier, d *pb.DatasetVersion, event *pb.DatasetVersionEvent) error {
	c := d.GetContent()
	_, err := q.ExecContext(ctx, `INSERT INTO dataset_version_events (project,name,version,revision,body) VALUES (?,?,?,?,?)`, c.GetProject(), c.GetName(), c.GetVersion(), d.GetRevision(), marshal(event))
	return err
}

// PublishDatasetVersion never replaces even an identical name/version. Its
// immutable content and first event become visible in one transaction.
func (s *Store) PublishDatasetVersion(ctx context.Context, d *pb.DatasetVersion, event *pb.DatasetVersionEvent) error {
	if err := verifyDatasetVersion(d); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	c := d.GetContent()
	result, err := tx.ExecContext(ctx, `INSERT INTO dataset_versions (project,name,version,state,owner,revision,body) VALUES (?,?,?,?,?,?,?) ON CONFLICT(project,name,version) DO NOTHING`, c.GetProject(), c.GetName(), c.GetVersion(), int32(d.GetState()), c.GetOwner(), d.GetRevision(), marshal(d))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrExists
	}
	if err := appendDatasetVersionEvent(ctx, tx, d, event); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetDatasetVersion(ctx context.Context, project, name, version string) (*pb.DatasetVersion, error) {
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM dataset_versions WHERE project=? AND name=? AND version=?`, project, name, version).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d := &pb.DatasetVersion{}
	if err := proto.Unmarshal(body, d); err != nil {
		return nil, err
	}
	if d.GetContent().GetProject() != project || d.GetContent().GetName() != name || d.GetContent().GetVersion() != version {
		return nil, ErrDatasetVersionIntegrity
	}
	return d, verifyDatasetVersion(d)
}

// ReviewDatasetVersion changes only decision fields, with a revision guard.
// Content is copied from the current stored version, never from the caller.
func (s *Store) ReviewDatasetVersion(ctx context.Context, project, name, version string, expected int64, decision pb.DatasetVersionState, actor, note string, event *pb.DatasetVersionEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var body []byte
	if err := tx.QueryRowContext(ctx, `SELECT body FROM dataset_versions WHERE project=? AND name=? AND version=?`, project, name, version).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	d := &pb.DatasetVersion{}
	if err := proto.Unmarshal(body, d); err != nil {
		return err
	}
	if err := verifyDatasetVersion(d); err != nil {
		return err
	}
	if d.GetRevision() != expected {
		return ErrDatasetVersionChanged
	}
	d.State, d.Revision, d.ReviewedBy, d.ReviewNote, d.ReviewedAt = decision, expected+1, actor, note, event.GetTime()
	result, err := tx.ExecContext(ctx, `UPDATE dataset_versions SET state=?,revision=?,body=? WHERE project=? AND name=? AND version=? AND revision=?`, int32(decision), d.GetRevision(), marshal(d), project, name, version, expected)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrDatasetVersionChanged
	}
	if err := appendDatasetVersionEvent(ctx, tx, d, event); err != nil {
		return err
	}
	return tx.Commit()
}

// ListDatasetVersions orders by name/version; the caller supplies a validated
// two-part cursor. Records are verified and removed before returning a summary.
func (s *Store) ListDatasetVersions(ctx context.Context, project, name, owner string, state pb.DatasetVersionState, size int, afterName, afterVersion string) ([]*pb.DatasetVersion, string, string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM dataset_versions WHERE project=? AND (?='' OR name=?) AND (?='' OR owner=?) AND (?=0 OR state=?) AND (name>? OR (name=? AND version>?)) ORDER BY name,version LIMIT ?`, project, name, name, owner, owner, int32(state), int32(state), afterName, afterName, afterVersion, size+1)
	if err != nil {
		return nil, "", "", err
	}
	defer rows.Close()
	var out []*pb.DatasetVersion
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, "", "", err
		}
		d := &pb.DatasetVersion{}
		if err := proto.Unmarshal(body, d); err != nil {
			return nil, "", "", err
		}
		if err := verifyDatasetVersion(d); err != nil {
			return nil, "", "", err
		}
		d.Content.Records = nil
		out = append(out, d)
	}
	nextName, nextVersion := "", ""
	if len(out) > size {
		out = out[:size]
		nextName, nextVersion = out[size-1].GetContent().GetName(), out[size-1].GetContent().GetVersion()
	}
	return out, nextName, nextVersion, rows.Err()
}

func (s *Store) DatasetVersionEvents(ctx context.Context, project, name, version string, size int, before int64) ([]*pb.DatasetVersionEvent, string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body FROM dataset_version_events WHERE project=? AND name=? AND version=? AND (?=0 OR revision<?) ORDER BY revision DESC LIMIT ?`, project, name, version, before, before, size+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*pb.DatasetVersionEvent
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, "", err
		}
		event := &pb.DatasetVersionEvent{}
		if err := proto.Unmarshal(body, event); err != nil {
			return nil, "", err
		}
		out = append(out, event)
	}
	next := ""
	if len(out) > size {
		out = out[:size]
		next = strconv.FormatInt(out[size-1].GetRevision(), 10)
	}
	return out, next, rows.Err()
}
