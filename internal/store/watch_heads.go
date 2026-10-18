package store

import (
	"context"
	"time"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
	"google.golang.org/protobuf/proto"
)

type watchHead struct {
	Project, TraceID, Hash, ProjectionHash, SnapshotHash string
	Revision, ScoreRevision, StartNS                     int64
	Published, Deleted                                   bool
}

func nextWatchClock(ctx context.Context, q querier, count int) (int64, error) {
	var end int64
	now := time.Now().UnixNano()
	err := q.QueryRowContext(ctx, `UPDATE watch_write_lock SET version=CASE WHEN version>? THEN version ELSE ? END+? WHERE id=1 RETURNING version`, now, now, count).Scan(&end)
	return end, err
}

func getWatchHead(ctx context.Context, q querier, project, id string) (watchHead, error) {
	h := watchHead{Project: project, TraceID: id}
	err := q.QueryRowContext(ctx, `SELECT revision,record_hash,start_ns,published,deleted,projection_hash,snapshot_hash,score_revision FROM watch_trace_heads WHERE project=? AND trace_id=?`, project, id).Scan(&h.Revision, &h.Hash, &h.StartNS, &h.Published, &h.Deleted, &h.ProjectionHash, &h.SnapshotHash, &h.ScoreRevision)
	return h, err
}

func putWatchHead(ctx context.Context, q querier, h watchHead) error {
	_, err := q.ExecContext(ctx, `INSERT INTO watch_trace_heads(project,trace_id,revision,record_hash,start_ns,published,deleted,projection_hash,snapshot_hash,score_revision) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(project,trace_id) DO UPDATE SET revision=excluded.revision,record_hash=excluded.record_hash,start_ns=excluded.start_ns,published=excluded.published,deleted=excluded.deleted,projection_hash=excluded.projection_hash,snapshot_hash=excluded.snapshot_hash,score_revision=excluded.score_revision`, h.Project, h.TraceID, h.Revision, h.Hash, h.StartNS, h.Published, h.Deleted, h.ProjectionHash, h.SnapshotHash, h.ScoreRevision)
	return err
}

func acceptWatchHead(ctx context.Context, q querier, item WatchPending) (watchHead, error) {
	h, err := getWatchHead(ctx, q, item.Project, item.TraceID)
	if err != nil && !isNoRows(err) {
		return h, err
	}
	snapshot := item.SnapshotHash
	if snapshot == "" {
		snapshot = item.RecordHash
	}
	inputChanged := isNoRows(err) || h.Hash != item.RecordHash || h.Deleted
	if inputChanged || h.SnapshotHash != snapshot {
		h.Revision, err = nextWatchClock(ctx, q, 1)
		if err != nil {
			return h, err
		}
		if inputChanged {
			h.ScoreRevision = h.Revision
			h.ProjectionHash = scoreBinding(item.RecordHash, h.ScoreRevision)
		}
		h.Hash = item.RecordHash
		h.SnapshotHash = snapshot
		h.Published = false
		h.Deleted = false
	}
	h.StartNS = item.StartNS
	return h, putWatchHead(ctx, q, h)
}

// Old development queues lack publication order. Preserve an existing trace
// view as the head rather than letting an arbitrary legacy stream replace it.
func (s *Store) bindLegacyWatch(ctx context.Context, tx *txn, item WatchTraceWrite, revision, scoreRevision int64, hash string) (int64, int64, string, error) {
	if hash != "" {
		return revision, scoreRevision, hash, nil
	}
	hash = RecordHash(item.Trace.Record)
	h, err := getWatchHead(ctx, tx, item.Pending.Project, item.Pending.TraceID)
	if err != nil && !isNoRows(err) {
		return 0, 0, "", err
	}
	if isNoRows(err) {
		h = watchHead{Project: item.Pending.Project, TraceID: item.Pending.TraceID, Hash: hash, StartNS: item.Pending.StartNS}
		var summary *evalsiv1alpha1.TraceSummary
		var record *evalsiv1alpha1.Record
		if _, ok := s.traces.(*sqlTraces); ok {
			var sum, rec []byte
			err = tx.QueryRowContext(ctx, `SELECT summary,record,record_hash FROM traces WHERE project=? AND trace_id=?`, h.Project, h.TraceID).Scan(&sum, &rec, &h.ProjectionHash)
			if err == nil {
				summary = &evalsiv1alpha1.TraceSummary{}
				record = &evalsiv1alpha1.Record{}
				if err = proto.Unmarshal(sum, summary); err != nil {
					return 0, 0, "", err
				}
				if err = proto.Unmarshal(rec, record); err != nil {
					return 0, 0, "", err
				}
			}
		} else {
			summary, record, err = s.traces.GetTrace(ctx, h.Project, h.TraceID)
			if err == nil {
				if ch, ok := s.traces.(*clickhouse); ok {
					h.ProjectionHash, err = ch.currentRecordHash(ctx, h.Project, h.TraceID)
				}
			}
		}
		if err != nil && !isNoRows(err) && err != ErrNotFound {
			return 0, 0, "", err
		}
		if record != nil {
			h.Hash = RecordHash(record)
			h.SnapshotHash = SnapshotHash(summary, record)
			h.StartNS = summary.GetStartTime().AsTime().UnixNano()
			h.Published = true
		}
		h.Revision, err = nextWatchClock(ctx, tx, 1)
		h.ScoreRevision = h.Revision
		if err != nil {
			return 0, 0, "", err
		}
		if err = putWatchHead(ctx, tx, h); err != nil {
			return 0, 0, "", err
		}
	} else if h.Hash == "" {
		h.Hash = hash
		if h.ScoreRevision == 0 {
			h.ScoreRevision = h.Revision
		}
		if !h.Published {
			h.ProjectionHash = scoreBinding(hash, h.ScoreRevision)
		}
		h.SnapshotHash = SnapshotHash(item.Trace.Summary, item.Trace.Record)
		if err := putWatchHead(ctx, tx, h); err != nil {
			return 0, 0, "", err
		}
	}
	if h.Hash == hash && !h.Deleted {
		scoreRevision = h.ScoreRevision
		// Equal evaluator input can share scores while an older summary must
		// remain historical. Only an equal (or unknown) snapshot may publish.
		if h.SnapshotHash == "" || h.SnapshotHash == SnapshotHash(item.Trace.Summary, item.Trace.Record) {
			revision = h.Revision
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE watch_pending SET revision=?,score_revision=?,record_hash=? WHERE project=? AND trace_id=? AND stream=? AND payload=?`, revision, scoreRevision, hash, item.Pending.Project, item.Pending.TraceID, item.Pending.Stream, item.Pending.Payload)
	return revision, scoreRevision, hash, err
}

func loadWatchPending(ctx context.Context, q querier, item WatchPending) (int64, int64, string, error) {
	var revision, scoreRevision int64
	var hash string
	err := q.QueryRowContext(ctx, `SELECT revision,score_revision,record_hash FROM watch_pending WHERE project=? AND trace_id=? AND stream=? AND payload=?`, item.Project, item.TraceID, item.Stream, item.Payload).Scan(&revision, &scoreRevision, &hash)
	return revision, scoreRevision, hash, err
}
