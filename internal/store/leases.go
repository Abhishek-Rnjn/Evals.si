package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PolicyEngineLease identifies the cluster's elected online policy engine.
const PolicyEngineLease = "policy-engine"

// leaseSchema holds run and policy-engine leases shared by replicas.
const leaseSchema = `
CREATE TABLE IF NOT EXISTS leases (
  name TEXT PRIMARY KEY, owner TEXT NOT NULL, expires_ns INTEGER NOT NULL, epoch INTEGER NOT NULL DEFAULT 0
);
`

// AcquireLease takes or renews a lease for ttl. It succeeds when the lease is
// free, expired, or already held by owner; a live lease of another owner is
// left alone. One conditional upsert decides, so concurrent replicas cannot
// both win.
func (s *Store) AcquireLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	if name == PolicyEngineLease {
		_, ok, err := s.AcquirePolicyLease(ctx, owner, ttl)
		return ok, err
	}
	res, err := s.writeLease(ctx, name, func(q querier) (sql.Result, error) {
		now := time.Now()
		return q.ExecContext(ctx, `INSERT INTO leases (name, owner, expires_ns) VALUES (?, ?, ?)
            ON CONFLICT (name) DO UPDATE SET owner = excluded.owner, expires_ns = excluded.expires_ns
            WHERE leases.owner = excluded.owner OR leases.expires_ns < ?`, name, owner, now.Add(ttl).UnixNano(), now.UnixNano())
	})
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	result, err := s.writeLease(ctx, name, func(q querier) (sql.Result, error) {
		now := time.Now()
		return q.ExecContext(ctx, `UPDATE leases SET expires_ns = ? WHERE name = ? AND owner = ? AND expires_ns >= ?`, now.Add(ttl).UnixNano(), name, owner, now.UnixNano())
	})
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

// ReleaseLease gives up a lease, if owner holds it.
func (s *Store) ReleaseLease(ctx context.Context, name, owner string) error {
	_, err := s.writeLease(ctx, name, func(q querier) (sql.Result, error) {
		return q.ExecContext(ctx, `DELETE FROM leases WHERE name = ? AND owner = ?`, name, owner)
	})
	return err
}

// LeaseHolder returns who holds a live lease ("" when none does).
func (s *Store) LeaseHolder(ctx context.Context, name string) (string, error) {
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT owner FROM leases WHERE name = ? AND expires_ns >= ?`, name, time.Now().UnixNano()).Scan(&owner)
	if err != nil && isNoRows(err) {
		return "", nil
	}
	return owner, err
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// Policy-engine lease changes share the persistence fence, so a takeover
// cannot pass an in-flight trace/result commit after its lease check.
func (s *Store) writeLease(ctx context.Context, name string, write func(querier) (sql.Result, error)) (sql.Result, error) {
	if name != PolicyEngineLease {
		return write(s.db)
	}
	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := write(tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

// PolicyLease identifies one acquisition, including reacquisition by the same process.
type PolicyLease struct {
	Owner string
	Epoch int64
}

func (s *Store) AcquirePolicyLease(ctx context.Context, owner string, ttl time.Duration) (PolicyLease, bool, error) {
	if owner == "" {
		return PolicyLease{}, false, errors.New("policy lease owner must not be empty")
	}
	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return PolicyLease{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var held string
	var expires, epoch int64
	err = tx.QueryRowContext(ctx, `SELECT owner,expires_ns,epoch FROM leases WHERE name=?`, PolicyEngineLease).Scan(&held, &expires, &epoch)
	if err != nil && !isNoRows(err) {
		return PolicyLease{}, false, err
	}
	now := time.Now()
	if err == nil && held != owner && expires >= now.UnixNano() {
		return PolicyLease{}, false, nil
	}
	if isNoRows(err) || expires < now.UnixNano() || epoch == 0 {
		epoch, err = nextWatchClock(ctx, tx, 1)
		if err != nil {
			return PolicyLease{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO leases(name,owner,expires_ns,epoch) VALUES(?,?,?,?) ON CONFLICT(name) DO UPDATE SET owner=excluded.owner,expires_ns=excluded.expires_ns,epoch=excluded.epoch`, PolicyEngineLease, owner, now.Add(ttl).UnixNano(), epoch); err != nil {
		return PolicyLease{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return PolicyLease{}, false, err
	}
	return PolicyLease{Owner: owner, Epoch: epoch}, true, nil
}
