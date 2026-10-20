package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
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
		// Retention removes expired pending rows. Any surviving acceptance
		// still has independent evaluation work, even if a newer, expired
		// stream left the publication head tombstoned.
		eligible[i] = true
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
	if err := checkWatchLease(ctx, tx, lease); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	eligible, writes, heads, err := s.prepareWatchTraces(ctx, tx, items)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	err = s.publishWatchHeads(ctx, tx, lease, len(items), writes, heads, func(ctx context.Context, tx *txn) ([]TraceWrite, []watchHead, error) {
		var err error
		eligible, writes, heads, err = s.prepareWatchTraces(ctx, tx, items)
		return writes, heads, err
	})
	if err != nil {
		return nil, err
	}
	return eligible, nil
}

// publishWatchHeads stores the traces of accepted heads and marks the heads
// published, finishing tx. For ClickHouse it first commits publication
// intents, then reopens the write lock; anything may have changed in between
// (a newer acceptance, retention), so reprepare re-reads what is still current.
func (s *Store) publishWatchHeads(ctx context.Context, tx *txn, lease PolicyLease, count int, writes []TraceWrite, heads []watchHead, reprepare func(context.Context, *txn) ([]TraceWrite, []watchHead, error)) error {
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	var err error
	if _, external := s.traces.(*clickhouse); external && len(writes) > 0 {
		// Commit intents before sending an external request: a timed-out
		// INSERT can outlive both this transaction and a superseding head.
		for _, h := range heads {
			if _, err := tx.ExecContext(ctx, `INSERT INTO watch_publications(project,trace_id,revision,start_ns) VALUES(?,?,?,?) ON CONFLICT(project,trace_id,revision) DO NOTHING`, h.Project, h.TraceID, h.Revision, h.StartNS); err != nil {
				return err
			}
		}
		end, err := nextWatchClock(ctx, tx, count)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		ctx = context.WithValue(ctx, watchVersionKey{}, watchReservation{Start: end - int64(count) + 1, End: end})
		tx, err = s.beginReservedWatchWrite(ctx)
		if errors.Is(err, errWatchReservationStale) {
			ctx, tx, err = s.beginVersionedWatchWrite(ctx, count)
		}
		if err != nil {
			return err
		}
		if err := checkWatchLease(ctx, tx, lease); err != nil {
			return err
		}
		// Retention can tombstone a head without moving the clock, so a
		// current reservation does not mean the heads are unchanged.
		if writes, heads, err = reprepare(ctx, tx); err != nil {
			return err
		}
	}
	if len(writes) > 0 {
		projections := map[[2]string]string{}
		retiredBefore := map[[2]string]int64{}
		liveLegacyRefs := map[[3]string]int64{}
		for _, h := range heads {
			key := [2]string{h.Project, h.TraceID}
			projections[key] = scoreBinding(h.Hash, h.ScoreRevision)
			var retired int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(retired_before_ns),0) FROM watch_publications WHERE project=? AND trace_id=? AND revision=0`, h.Project, h.TraceID).Scan(&retired); err != nil {
				return err
			}
			retiredBefore[key] = retired
			if retired > 0 {
				rows, err := tx.QueryContext(ctx, `SELECT record_hash,start_ns FROM watch_score_references WHERE project=? AND trace_id=? AND start_ns>=?`, h.Project, h.TraceID, retired)
				if err != nil {
					return err
				}
				for rows.Next() {
					var hash string
					var start int64
					if err := rows.Scan(&hash, &start); err != nil {
						_ = rows.Close()
						return err
					}
					liveLegacyRefs[[3]string{h.Project, h.TraceID, hash}] = start
				}
				if err := rows.Err(); err != nil {
					_ = rows.Close()
					return err
				}
				if err := rows.Close(); err != nil {
					return err
				}
			}
		}
		ctx = context.WithValue(ctx, watchProjectionKey{}, projections)
		ctx = context.WithValue(ctx, watchNoLegacyCopyKey{}, retiredBefore)
		ctx = context.WithValue(ctx, watchLiveLegacyRefsKey{}, liveLegacyRefs)
		if _, ok := s.traces.(*sqlTraces); ok {
			err = putSQLTraces(ctx, tx, writes)
		} else {
			err = s.traces.PutTraces(ctx, writes)
		}
		if err != nil {
			return err
		}
		for _, h := range heads {
			if _, err := tx.ExecContext(ctx, `UPDATE watch_trace_heads SET published=1,projection_hash=? WHERE project=? AND trace_id=? AND revision=? AND deleted=0`, scoreBinding(h.Hash, h.ScoreRevision), h.Project, h.TraceID, h.Revision); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM watch_publications WHERE project=? AND trace_id=? AND revision=?`, h.Project, h.TraceID, h.Revision); err != nil {
				return err
			}
		}
	}
	err = tx.Commit()
	tx = nil
	return err
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
	var start int64
	if err := tx.QueryRowContext(ctx, `SELECT start_ns FROM watch_pending WHERE project=? AND trace_id=? AND stream=? AND payload=?`, item.Project, item.TraceID, item.Stream, item.Payload).Scan(&start); err != nil {
		return err
	}
	if err := retainWatchScore(ctx, tx, item.Project, item.TraceID, binding, start); err != nil {
		return err
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
type watchNoLegacyCopyKey struct{}
type watchLiveLegacyRefsKey struct{}

func legacyCopyAllowed(ctx context.Context, project, id, hash string, start int64) bool {
	cutoffs, _ := ctx.Value(watchNoLegacyCopyKey{}).(map[[2]string]int64)
	cutoff := cutoffs[[2]string{project, id}]
	refs, _ := ctx.Value(watchLiveLegacyRefsKey{}).(map[[3]string]int64)
	start = max(start, refs[[3]string{project, id, hash}])
	return cutoff == 0 || start >= cutoff
}

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
	return snapshotHashFromRecordHash(RecordHash(record), summary)
}

func snapshotHashFromRecordHash(recHash string, summary *evalsiv1alpha1.TraceSummary) string {
	blob, err := (proto.MarshalOptions{Deterministic: true}).Marshal(summary)
	if err != nil {
		panic(err)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(recHash))
	_, _ = hash.Write(blob)
	return hex.EncodeToString(hash.Sum(nil))
}

// PutUnwatchedTraces stores traces when no online policy could score them,
// without queuing evaluation work. Heads, score bindings, retention and
// ClickHouse publication intents follow the same rules as PutWatchPending
// followed by PutWatchTraces, so a trace stored here is indistinguishable
// from one that went through the queue: an unchanged re-ingest keeps its
// score binding, and an already-published identical trace is not rewritten.
// Within one call, the last write of a trace wins.
func (s *Store) PutUnwatchedTraces(ctx context.Context, lease PolicyLease, writes []TraceWrite) error {
	last := make(map[[2]string]int, len(writes))
	for i, w := range writes {
		last[[2]string{w.Summary.GetProject(), w.Summary.GetTraceId()}] = i
	}
	unique := make([]TraceWrite, 0, len(last))
	for i, w := range writes {
		if last[[2]string{w.Summary.GetProject(), w.Summary.GetTraceId()}] == i {
			unique = append(unique, w)
		}
	}
	if len(unique) == 0 {
		return nil
	}
	// Hashing dominates for large traces; it needs no lock.
	pending := make([]WatchPending, len(unique))
	workers := min(len(unique), 8)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < len(unique); i += workers {
				t := unique[i]
				recHash := RecordHash(t.Record)
				pending[i] = WatchPending{
					Project: t.Summary.GetProject(), TraceID: t.Summary.GetTraceId(),
					RecordHash: recHash, SnapshotHash: snapshotHashFromRecordHash(recHash, t.Summary),
					StartNS: t.Summary.GetStartTime().AsTime().UnixNano(),
				}
			}
		}(w)
	}
	wg.Wait()

	tx, err := s.beginWatchWrite(ctx)
	if err != nil {
		return err
	}
	if err := checkWatchLease(ctx, tx, lease); err != nil {
		_ = tx.Rollback()
		return err
	}
	var out []TraceWrite
	var heads []watchHead
	for i, p := range pending {
		h, err := acceptWatchHead(ctx, tx, p)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		if h.Published {
			continue // this exact trace is already stored
		}
		out = append(out, unique[i])
		heads = append(heads, h)
	}
	// After the intents commit, keep only heads nothing has superseded or
	// retired since, as prepareWatchTraces does for queued work.
	reprepare := func(ctx context.Context, tx *txn) ([]TraceWrite, []watchHead, error) {
		var w []TraceWrite
		var hs []watchHead
		for i, h := range heads {
			cur, err := getWatchHead(ctx, tx, h.Project, h.TraceID)
			if isNoRows(err) {
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			if cur.Deleted || cur.Published || cur.Revision != h.Revision {
				continue
			}
			w = append(w, out[i])
			hs = append(hs, cur)
		}
		return w, hs, nil
	}
	return s.publishWatchHeads(ctx, tx, lease, len(out), out, heads, reprepare)
}
