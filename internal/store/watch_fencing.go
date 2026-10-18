package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"google.golang.org/protobuf/proto"
)

var ErrWatchStale = errors.New("online work is no longer current")

// Serializes persistence with ingestion, lease changes and retention. SQL
// projections share this transaction; worker calls never hold this lock.
func (s *Store) beginWatchWrite(ctx context.Context) (*txn, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE watch_write_lock SET version=version WHERE id=1`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func checkWatchLease(ctx context.Context, q querier, lease PolicyLease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease.Owner == "" {
		if lease.Epoch != 0 {
			return ErrWatchStale
		}
		return nil
	}
	var owner string
	var expires, epoch int64
	err := q.QueryRowContext(ctx, `SELECT owner,expires_ns,epoch FROM leases WHERE name=?`, PolicyEngineLease).Scan(&owner, &expires, &epoch)
	if isNoRows(err) {
		return ErrWatchStale
	}
	if err != nil {
		return err
	}
	if owner != lease.Owner || epoch != lease.Epoch || epoch == 0 || expires < time.Now().UnixNano() {
		return ErrWatchStale
	}
	return nil
}

type watchVersionKey struct{}
type watchReservation struct{ Start, End int64 }

// Reservations commit before a remote write so a failed request cannot reuse
// its version. The write lock later verifies reservation order, including a
// competing reservation in the gap between these two transactions.
func (s *Store) watchVersionContext(ctx context.Context, count int) (context.Context, error) {
	if _, ok := s.traces.(*clickhouse); !ok || count == 0 {
		return ctx, nil
	}
	end, err := nextWatchClock(ctx, s.db, count)
	return context.WithValue(ctx, watchVersionKey{}, watchReservation{Start: end - int64(count) + 1, End: end}), err
}
func traceWriteVersion(ctx context.Context) int64 {
	if r, ok := ctx.Value(watchVersionKey{}).(watchReservation); ok {
		return r.Start
	}
	return time.Now().UnixNano()
}

var errWatchReservationStale = errors.New("watch write reservation was superseded")

func (s *Store) beginReservedWatchWrite(ctx context.Context) (*txn, error) {
	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return nil, err
	}
	reservation, ok := ctx.Value(watchVersionKey{}).(watchReservation)
	if !ok {
		return tx, nil
	}
	var current int64
	err = tx.QueryRowContext(ctx, `SELECT version FROM watch_write_lock WHERE id=1`).Scan(&current)
	if err != nil || current != reservation.End {
		_ = tx.Rollback()
		if err != nil {
			return nil, err
		}
		return nil, errWatchReservationStale
	}
	return tx, nil
}

func (s *Store) beginVersionedWatchWrite(ctx context.Context, count int) (context.Context, *txn, error) {
	for {
		reserved, err := s.watchVersionContext(ctx, count)
		if err != nil {
			return ctx, nil, err
		}
		tx, err := s.beginReservedWatchWrite(reserved)
		if errors.Is(err, errWatchReservationStale) {
			continue
		}
		return reserved, tx, err
	}
}

type WatchTraceWrite struct {
	Pending WatchPending
	Trace   TraceWrite
}

// Read-only replays of published or historical work do not contend on the
// global write lock. Every later result/ack still takes the lease/revision fence.
func (s *Store) publishedWatchTraces(ctx context.Context, items []WatchTraceWrite) ([]bool, bool, error) {
	eligible := make([]bool, len(items))
	for i, item := range items {
		var revision, head int64
		var hash string
		var published, deleted bool
		err := s.db.QueryRowContext(ctx, `SELECT p.revision,p.record_hash,h.revision,h.published,h.deleted FROM watch_pending p JOIN watch_trace_heads h ON p.project=h.project AND p.trace_id=h.trace_id WHERE p.project=? AND p.trace_id=? AND p.stream=? AND p.payload=?`, item.Pending.Project, item.Pending.TraceID, item.Pending.Stream, item.Pending.Payload).Scan(&revision, &hash, &head, &published, &deleted)
		if isNoRows(err) {
			// A legacy row can lack a head; it needs the fenced adoption path.
			exists, lookupErr := watchPendingMatches(ctx, s.db, item.Pending)
			if lookupErr != nil {
				return nil, false, lookupErr
			}
			if exists {
				return nil, false, nil
			}
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if hash == "" {
			return nil, false, nil
		}
		if hash != RecordHash(item.Trace.Record) {
			return nil, false, ErrWatchStale
		}
		if revision == head && !published && !deleted {
			return nil, false, nil
		}
		eligible[i] = !deleted
	}
	return eligible, true, nil
}

// Eligible slots retain independent evaluation work. Only the newest accepted
// record across streams is published, once per revision, even during outages.
func (s *Store) prepareWatchTraces(ctx context.Context, tx *txn, items []WatchTraceWrite) ([]bool, []TraceWrite, []watchHead, error) {
	eligible := make([]bool, len(items))
	var writes []TraceWrite
	var heads []watchHead
	seen := map[[2]string]bool{}
	for i, item := range items {
		revision, scoreRevision, hash, err := loadWatchPending(ctx, tx, item.Pending)
		if isNoRows(err) {
			continue
		}
		if err != nil {
			return nil, nil, nil, err
		}
		revision, scoreRevision, hash, err = s.bindLegacyWatch(ctx, tx, item, revision, scoreRevision, hash)
		if err != nil {
			return nil, nil, nil, err
		}
		if hash != RecordHash(item.Trace.Record) {
			return nil, nil, nil, ErrWatchStale
		}
		h, err := getWatchHead(ctx, tx, item.Pending.Project, item.Pending.TraceID)
		if err != nil {
			return nil, nil, nil, err
		}
		eligible[i] = true
		key := [2]string{h.Project, h.TraceID}
		if h.Deleted || h.Revision != revision || h.Published || seen[key] {
			continue
		}
		seen[key] = true
		writes = append(writes, item.Trace)
		heads = append(heads, h)
	}
	return eligible, writes, heads, nil
}

func (s *Store) PutWatchTraces(ctx context.Context, lease PolicyLease, items []WatchTraceWrite) ([]bool, error) {
	if err := checkWatchLease(ctx, s.db, lease); err != nil {
		return nil, err
	}
	if eligible, done, err := s.publishedWatchTraces(ctx, items); err != nil || done {
		return eligible, err
	}

	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if err := checkWatchLease(ctx, tx, lease); err != nil {
		return nil, err
	}
	eligible, writes, heads, err := s.prepareWatchTraces(ctx, tx, items)
	if err != nil {
		return nil, err
	}
	if _, external := s.traces.(*clickhouse); external && len(writes) > 0 {
		_ = tx.Rollback()
		ctx, tx, err = s.beginVersionedWatchWrite(ctx, len(items))
		if err != nil {
			return nil, err
		}
		if err := checkWatchLease(ctx, tx, lease); err != nil {
			return nil, err
		}
		eligible, writes, heads, err = s.prepareWatchTraces(ctx, tx, items)
		if err != nil {
			return nil, err
		}
	}
	if len(writes) > 0 {
		projections := map[[2]string]string{}
		for _, h := range heads {
			projections[[2]string{h.Project, h.TraceID}] = scoreBinding(h.Hash, h.ScoreRevision)
		}
		ctx = context.WithValue(ctx, watchProjectionKey{}, projections)
		if _, ok := s.traces.(*sqlTraces); ok {
			err = putSQLTraces(ctx, tx, writes)
		} else {
			err = s.traces.PutTraces(ctx, writes)
		}
		if err != nil {
			return nil, err
		}
		for _, h := range heads {
			if _, err := tx.ExecContext(ctx, `UPDATE watch_trace_heads SET published=1,projection_hash=? WHERE project=? AND trace_id=? AND revision=? AND deleted=0`, scoreBinding(h.Hash, h.ScoreRevision), h.Project, h.TraceID, h.Revision); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return eligible, nil
}

func (s *Store) PutWatchResults(ctx context.Context, lease PolicyLease, item WatchPending, policy string, results []*evalsiv1alpha1.EvaluationResult) error {
	ctx, tx, err := s.beginVersionedWatchWrite(ctx, 1)
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if err := checkWatchLease(ctx, tx, lease); err != nil {
		return err
	}
	_, scoreRevision, hash, err := loadWatchPending(ctx, tx, item)
	if isNoRows(err) {
		return ErrWatchStale
	}
	if err != nil {
		return err
	}
	h, headErr := getWatchHead(ctx, tx, item.Project, item.TraceID)
	binding := scoreBinding(hash, scoreRevision)
	if headErr == nil && h.ScoreRevision == scoreRevision && h.Hash == hash {
		binding = h.ProjectionHash
	}
	if headErr != nil && !isNoRows(headErr) {
		return headErr
	}
	ctx = context.WithValue(ctx, watchScoreBindingKey{}, binding)
	if _, ok := s.traces.(*sqlTraces); ok {
		err = putSQLTraceResults(ctx, tx, item.Project, item.TraceID, policy, results)
	} else {
		err = s.traces.PutTraceResults(ctx, item.Project, item.TraceID, policy, results)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

type watchScoreBindingKey struct{}

// RecordHash binds scores to evaluator input independently of protobuf map order.
func RecordHash(record *evalsiv1alpha1.Record) string {
	blob, err := (proto.MarshalOptions{Deterministic: true}).Marshal(record)
	if err != nil {
		panic(err)
	}
	hash := sha256.Sum256(blob)
	return hex.EncodeToString(hash[:])
}

type watchProjectionKey struct{}

func scoreBinding(hash string, revision int64) string {
	sum := sha256.Sum256([]byte(hash + ":" + strconv.FormatInt(revision, 10)))
	return hex.EncodeToString(sum[:])
}
func traceProjection(ctx context.Context, summary *evalsiv1alpha1.TraceSummary, record *evalsiv1alpha1.Record) string {
	if bindings, ok := ctx.Value(watchProjectionKey{}).(map[[2]string]string); ok {
		if hash, ok := bindings[[2]string{summary.GetProject(), summary.GetTraceId()}]; ok {
			return hash
		}
	}
	return RecordHash(record)
}

// SnapshotHash tracks query-visible metadata independently of evaluator input.
func SnapshotHash(summary *evalsiv1alpha1.TraceSummary, record *evalsiv1alpha1.Record) string {
	blob, err := (proto.MarshalOptions{Deterministic: true}).Marshal(summary)
	if err != nil {
		panic(err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(RecordHash(record)))
	_, _ = hash.Write(blob)
	return hex.EncodeToString(hash.Sum(nil))
}
