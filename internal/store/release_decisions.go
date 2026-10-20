package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	pb "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"google.golang.org/protobuf/proto"
)

const releaseDecisionSchema = `
CREATE TABLE IF NOT EXISTS release_decisions (
 project TEXT NOT NULL, id TEXT NOT NULL, body BLOB NOT NULL, sha256 TEXT NOT NULL,
 PRIMARY KEY (project,id)
);
`

var ErrReleaseIntegrity = errors.New("release decision content failed integrity verification")

func ReleaseDecisionHash(d *pb.ReleaseDecision) string {
	clone := proto.Clone(d).(*pb.ReleaseDecision)
	clone.Sha256 = ""
	raw, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(clone)
	sum := sha256.Sum256(append([]byte("evalsi.release-decision/v1\x00"), raw...))
	return hex.EncodeToString(sum[:])
}
func releaseEnvelopeHash(d *pb.StoredReleaseDecision) string {
	raw, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(d)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (s *Store) CreateReleaseDecision(ctx context.Context, d *pb.StoredReleaseDecision) error {
	decision := d.GetDecision()
	if decision.GetSha256() != ReleaseDecisionHash(decision) {
		return ErrReleaseIntegrity
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO release_decisions(project,id,body,sha256) VALUES(?,?,?,?) ON CONFLICT(project,id) DO NOTHING`, decision.GetProject(), decision.GetId(), marshal(d), releaseEnvelopeHash(d))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrExists
	}
	return nil
}
func decodeRelease(body []byte, expected string) (*pb.StoredReleaseDecision, error) {
	d := &pb.StoredReleaseDecision{}
	if err := proto.Unmarshal(body, d); err != nil {
		return nil, ErrReleaseIntegrity
	}
	if releaseEnvelopeHash(d) != expected || d.GetDecision().GetSha256() != ReleaseDecisionHash(d.GetDecision()) {
		return nil, ErrReleaseIntegrity
	}
	return d, nil
}
func (s *Store) GetReleaseDecision(ctx context.Context, project, id string) (*pb.StoredReleaseDecision, error) {
	var body []byte
	var digest string
	err := s.db.QueryRowContext(ctx, `SELECT body,sha256 FROM release_decisions WHERE project=? AND id=?`, project, id).Scan(&body, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d, err := decodeRelease(body, digest)
	if err != nil {
		return nil, err
	}
	if d.GetDecision().GetProject() != project || d.GetDecision().GetId() != id {
		return nil, ErrReleaseIntegrity
	}
	return d, nil
}

// ListReleaseDecisions returns bounded candidates; the service filters access
// before constructing a cursor from the last visible decision.
func (s *Store) ListReleaseDecisions(ctx context.Context, project, after string, limit int) ([]*pb.StoredReleaseDecision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body,sha256 FROM release_decisions WHERE project=? AND id>? ORDER BY id LIMIT ?`, project, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.StoredReleaseDecision
	for rows.Next() {
		var body []byte
		var digest string
		if err := rows.Scan(&body, &digest); err != nil {
			return nil, err
		}
		d, err := decodeRelease(body, digest)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
